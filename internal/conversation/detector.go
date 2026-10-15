package conversation

import (
	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/prompt"
)

// FrameMs is the length of one VAD frame; detector time is frames × FrameMs (audio time).
const FrameMs = 20

// The voiced cluster: at least clusterVoiced voiced frames among the last clusterWindow
// (≥ 60 ms of voice in 100 ms). bargeOnsetBackMs dates a barge-in candidate back to the first
// frame of the cluster; bargeCancelMs of silence drops the candidate (spec 7.6.2 literals).
const (
	clusterVoiced    = 3
	clusterWindow    = 5
	bargeOnsetBackMs = clusterVoiced * FrameMs
	bargeCancelMs    = 300
)

// TurnState is the runtime turn state (D-075, spec 7.6).
type TurnState string

// Turn states.
const (
	StateListening    TurnState = "LISTENING"
	StateUserSpeaking TurnState = "USER_SPEAKING"
	StateProcessing   TurnState = "PROCESSING"
	StateAISpeaking   TurnState = "AI_SPEAKING"
)

// EventKind is what the detector reports to Runtime.loop (spec 7.6.1).
type EventKind string

// Detector events; the values are the spec names.
const (
	EventSpeechConfirmed  EventKind = "SPEECH_CONFIRMED"
	EventSpeechPause      EventKind = "SPEECH_PAUSE"
	EventSpeechResume     EventKind = "SPEECH_RESUME"
	EventSilenceReached   EventKind = "SILENCE_REACHED"
	EventMaxUtterance     EventKind = "MAX_UTTERANCE"
	EventBargeInConfirmed EventKind = "BARGE_IN_CONFIRMED"
)

// Event is one detector event. OnsetMs is set for SPEECH_CONFIRMED and BARGE_IN_CONFIRMED,
// ThresholdMs for SILENCE_REACHED; both are audio time in ms.
type Event struct {
	Kind        EventKind
	OnsetMs     int64
	ThresholdMs int64
}

// TextView is the STT text the detector reads (backed by sttState, spec 7.6.2).
type TextView interface {
	// TurnText is the final segments since the turn mark plus the running partial.
	TurnText() string
	// TextSince is the text received since the last commit with audio time >= onsetMs, or ""
	// while a DISCARD commit is pending.
	TextSince(onsetMs int64) string
	// FinalCoversLatest reports that the latest STT event of the turn is a final (D-076).
	FinalCoversLatest() bool
	// TurnOnsetMs is the onset of the current user turn.
	TurnOnsetMs() int64
}

// DetectorConfig holds the detector knobs; frame counts are in 20 ms frames.
type DetectorConfig struct {
	SpeechStartFrames  int // voiced frames needed in the start window (10)
	SpeechStartWindow  int // start window (15)
	BargeFrames        int // voiced frames needed in the barge-in window (15)
	BargeWindow        int // barge-in window (20)
	BargeMinChars      int // speakable runes since the candidate onset (2)
	SpeechEndMs        int64
	DefaultMs          int64
	FillerMs           int64
	CompleteMs         int64
	FillerOnlyMs       int64
	MaxUtteranceMs     int64
	FillerSuffixes     []string
	CompletionSuffixes []string
}

// DetectorConfigFrom maps the spec 7.2 tuning onto the detector: VAD_SPEECH_START_MS /
// _WINDOW_MS and BARGEIN_VAD_MS / _WINDOW_MS become frame counts (a window is never shorter
// than its count).
func DetectorConfigFrom(t config.RealtimeTuning) DetectorConfig {
	frames := func(ms int) int { return max(1, ms/FrameMs) }
	c := DetectorConfig{
		SpeechStartFrames:  frames(t.VADSpeechStartMS),
		SpeechStartWindow:  frames(t.VADSpeechStartWindowMS),
		BargeFrames:        frames(t.BargeinVADMS),
		BargeWindow:        frames(t.BargeinWindowMS),
		BargeMinChars:      t.BargeinMinPartialChars,
		SpeechEndMs:        int64(t.VADSpeechEndMS),
		DefaultMs:          int64(t.TurnSilenceDefaultMS),
		FillerMs:           int64(t.TurnSilenceFillerMS),
		CompleteMs:         int64(t.TurnSilenceCompleteMS),
		FillerOnlyMs:       int64(t.TurnSilenceFillerOnlyMS),
		MaxUtteranceMs:     int64(t.TurnMaxUtteranceMS),
		FillerSuffixes:     append([]string(nil), t.TurnFillerSuffixes...),
		CompletionSuffixes: append([]string(nil), t.TurnCompletionSuffixes...),
	}
	c.SpeechStartWindow = max(c.SpeechStartWindow, c.SpeechStartFrames)
	c.BargeWindow = max(c.BargeWindow, c.BargeFrames)
	return c
}

// Detector turns VAD frames and STT text into turn events (spec 7.6.2, D-076, D-077). It is
// pure: no goroutine and no clock, time is frames × 20 ms. Only Runtime.loop calls it, so it is
// not safe for concurrent use.
type Detector struct {
	cfg              DetectorConfig
	nowMs            int64    // audio time (frames × 20)
	win15, win20, w5 BoolRing // VAD history of the start, barge-in and cluster windows
	lastVoicedMs     int64    // updated while w5.Count() >= 3 (≥ 60 ms voiced in 100 ms)
	pauseEmitted     bool
	bargeOnsetMs     int64 // -1 = no candidate
}

// NewDetector returns a detector at audio time 0 with empty windows.
func NewDetector(cfg DetectorConfig) *Detector {
	return &Detector{
		cfg:          cfg,
		win15:        NewBoolRing(cfg.SpeechStartWindow),
		win20:        NewBoolRing(cfg.BargeWindow),
		w5:           NewBoolRing(clusterWindow),
		bargeOnsetMs: -1,
	}
}

// NowMs is the audio time of the last pushed frame.
func (d *Detector) NowMs() int64 { return d.nowMs }

// Push is called for every 20 ms frame, injected silence included.
func (d *Detector) Push(voiced bool, st TurnState, tx TextView) []Event {
	d.nowMs += FrameMs
	d.win15.Push(voiced)
	d.win20.Push(voiced)
	d.w5.Push(voiced)
	cluster := d.w5.Count() >= clusterVoiced
	if cluster {
		d.lastVoicedMs = d.nowMs
	}
	silence := d.nowMs - d.lastVoicedMs
	cfg := d.cfg
	var ev []Event

	switch st {
	case StateListening, StateProcessing:
		// ≥ 200 ms voiced in a 300 ms window (VAD_SPEECH_START_MS / _WINDOW_MS).
		if d.win15.Count() >= cfg.SpeechStartFrames {
			return []Event{{Kind: EventSpeechConfirmed, OnsetMs: d.nowMs - d.win15.SpanFromFirstTrue()}}
		}
	case StateUserSpeaking:
		if cluster && d.pauseEmitted {
			d.pauseEmitted = false
			return []Event{{Kind: EventSpeechResume}}
		}
		if d.nowMs-tx.TurnOnsetMs() >= cfg.MaxUtteranceMs {
			return []Event{{Kind: EventMaxUtterance}}
		}
		if !d.pauseEmitted && silence >= cfg.SpeechEndMs {
			d.pauseEmitted = true
			ev = append(ev, Event{Kind: EventSpeechPause})
		}
		th := d.Threshold(tx)
		if silence >= th {
			return append(ev, Event{Kind: EventSilenceReached, ThresholdMs: th})
		}
	case StateAISpeaking:
		if cluster && d.bargeOnsetMs < 0 {
			d.bargeOnsetMs = d.nowMs - bargeOnsetBackMs // candidate onset
		}
		if silence >= bargeCancelMs {
			d.bargeOnsetMs = -1 // candidate cancelled
		}
		// ≥ 300 ms voiced in 400 ms AND a partial of ≥ 2 runes since the candidate onset.
		// TextSince only sees text AFTER the last answered commit (S-8 (b)); until the FINAL of
		// a DISCARD commit arrives it is "" (the old partial is audio from before the commit).
		if d.bargeOnsetMs >= 0 && d.win20.Count() >= cfg.BargeFrames &&
			RuneLen(prompt.NormalizeSpeakable(tx.TextSince(d.bargeOnsetMs))) >= cfg.BargeMinChars {
			on := d.bargeOnsetMs
			d.bargeOnsetMs = -1
			return []Event{{Kind: EventBargeInConfirmed, OnsetMs: on}}
		}
	}
	return ev
}

// Threshold is the silence that ends the user turn for the current text (D-076). Completion
// is checked before filler: "ですが" ends in が → 1400, "ですか" ends in か → 500.
func (d *Detector) Threshold(tx TextView) int64 {
	cfg := d.cfg
	t := NormalizeTail(tx.TurnText())
	switch {
	case t == "":
		return cfg.DefaultMs // 800
	case IsFillerOnly(t):
		return cfg.FillerOnlyMs // 2000 [builder call]
	case HasSuffixAny(t, cfg.CompletionSuffixes) && tx.FinalCoversLatest():
		return cfg.CompleteMs // 500
	case HasSuffixAny(t, cfg.FillerSuffixes):
		return cfg.FillerMs // 1400
	default:
		return cfg.DefaultMs // 800
	}
}

// InjectSilence pushes ms/20 silent frames (watchdog starvation, spec 7.5) and returns every
// event they produced, in order.
func (d *Detector) InjectSilence(ms int64, st TurnState, tx TextView) []Event {
	var ev []Event
	for range ms / FrameMs {
		ev = append(ev, d.Push(false, st, tx)...)
	}
	return ev
}

// ResetForNewTurn clears the per-turn flags; the VAD windows and audio time are kept.
func (d *Detector) ResetForNewTurn() {
	d.pauseEmitted = false
	d.bargeOnsetMs = -1
}

// BoolRing is a fixed-size window of the most recent booleans.
type BoolRing struct {
	buf   []bool
	next  int // slot the next Push writes
	n     int // filled slots
	count int // true slots
}

// NewBoolRing returns an empty window of size slots (at least one).
func NewBoolRing(size int) BoolRing {
	return BoolRing{buf: make([]bool, max(1, size))}
}

// Push appends v, dropping the oldest value once the window is full.
func (r *BoolRing) Push(v bool) {
	if r.n == len(r.buf) {
		if r.buf[r.next] {
			r.count--
		}
	} else {
		r.n++
	}
	r.buf[r.next] = v
	if v {
		r.count++
	}
	r.next = (r.next + 1) % len(r.buf)
}

// Count is the number of true values in the window.
func (r *BoolRing) Count() int { return r.count }

// SpanFromFirstTrue is the audio length from the start of the oldest true frame in the window to
// the end of the newest frame, in ms (slots × 20, inclusive); 0 when the window holds no true.
// nowMs - SpanFromFirstTrue() is therefore the start time of that first true frame.
func (r *BoolRing) SpanFromFirstTrue() int64 {
	oldest := (r.next - r.n + len(r.buf)) % len(r.buf)
	for i := range r.n {
		if r.buf[(oldest+i)%len(r.buf)] {
			return int64(r.n-i) * FrameMs
		}
	}
	return 0
}
