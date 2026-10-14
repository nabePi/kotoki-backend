package fake

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

const (
	sttFrameBytes       = provider.STTSampleRate * 2 * sttFrameMs / 1000 // 640
	sttBytesPerMs       = provider.STTSampleRate * 2 / 1000              // 32
	sttMaxPendingBytes  = 2 * 1000 * sttBytesPerMs                       // 2 s of audio
	sttSpeechStartRun   = 10                                             // 200 ms of speech starts a segment
	sttSilenceCloseRun  = 30                                             // 600 ms of silence closes it
	sttPartialEveryMs   = 200
	sttFixtureMinPrefix = 400
	sttMaxShiftFrames   = 10
	sttEventBuffer      = 64
	sttMsPerRune        = 120
	sttFinalConfidence  = 0.95
	sttDefaultModel     = "fake-stt"
	sttCloseDrain       = 500 * time.Millisecond
	modeFixture         = "fixture"
	modeEnergy          = "energy"
)

var defaultPhrases = []string{
	"はい、そうです。", "えっと、週末は友達と映画を見ました。", "日本語の勉強は楽しいです。",
	"すみません、もう一度お願いします。", "昨日は雨でしたね。",
}

type fixture struct {
	name  string
	text  string
	env   []float64
	durMs int
}

// STT is the fake provider.STTProvider: energy-based segmentation and a transcript taken
// from the best-matching fixture (envelope correlation) or a fixed phrase list.
type STT struct {
	name     provider.Name
	cfg      CapConfig
	conf     Config
	deps     Deps
	dec      *decider
	jit      *jitterSource
	mode     string
	phrases  []string
	fixtures []fixture
	thr      float64 // match threshold
	dbfs     float64 // speech threshold
}

var _ provider.STTProvider = (*STT)(nil)

// NewSTT builds a fake STT. It fails when FAKE_STT_FIXTURE_DIR is unreadable or holds a
// WAV that is not PCM16 mono 16 kHz.
func NewSTT(name provider.Name, c CapConfig, cfg Config, d Deps) (*STT, error) {
	d = d.WithDefaults()
	s := &STT{name: name, cfg: c, conf: cfg, deps: d, dec: newDecider(d.Seed, name, provider.CapSTT, c),
		jit: newJitterSource(d.Seed, 0x535454), thr: cfg.STTMatchThreshold, dbfs: cfg.STTEnergyDBFS, phrases: cfg.STTPhrases}
	if s.thr <= 0 {
		s.thr = 0.8
	}
	if s.dbfs == 0 {
		s.dbfs = -40
	}
	if len(s.phrases) == 0 {
		s.phrases = defaultPhrases
	}
	s.mode = cfg.STTMode
	if s.mode == "" {
		s.mode = modeEnergy
		if cfg.STTFixtureDir != "" {
			s.mode = modeFixture
		}
	}
	if s.mode == modeFixture && cfg.STTFixtureDir != "" {
		fx, err := loadFixtures(cfg.STTFixtureDir, s.dbfs)
		if err != nil {
			return nil, err
		}
		s.fixtures = fx
	}
	return s, nil
}

// Name implements provider.STTProvider.
func (s *STT) Name() provider.Name { return s.name }

// Model implements provider.STTProvider.
func (s *STT) Model() string { return sttDefaultModel }

func loadFixtures(dir string, dbfs float64) ([]fixture, error) {
	wavs, err := filepath.Glob(filepath.Join(dir, "*.wav"))
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("fake stt: fixture dir: %w", err)
	}
	sort.Strings(wavs)
	var out []fixture
	for _, w := range wavs {
		b, err := os.ReadFile(w) //nolint:gosec // operator-provided fixture directory (FAKE_STT_FIXTURE_DIR)
		if err != nil {
			return nil, fmt.Errorf("fake stt: %w", err)
		}
		pcm, rate, err := provider.DecodeWAV(b)
		if err != nil {
			return nil, fmt.Errorf("fake stt: %s: %w", filepath.Base(w), err)
		}
		if rate != provider.STTSampleRate {
			return nil, fmt.Errorf("fake stt: %s: sample rate %d, want %d", filepath.Base(w), rate, provider.STTSampleRate)
		}
		txt, err := os.ReadFile(strings.TrimSuffix(w, ".wav") + ".txt")
		if err != nil {
			return nil, fmt.Errorf("fake stt: %s has no .txt sidecar: %w", filepath.Base(w), err)
		}
		text, _, _ := strings.Cut(strings.TrimSpace(string(txt)), "\n")
		env := trimEnvelope(frameRMS(pcm, sttFrameBytes), dbfs)
		out = append(out, fixture{name: filepath.Base(w), text: strings.TrimSpace(text), env: env, durMs: len(env) * sttFrameMs})
	}
	return out, nil
}

// trimEnvelope drops leading and trailing frames below the speech threshold.
func trimEnvelope(env []float64, dbfs float64) []float64 {
	first, last := -1, -1
	for i, v := range env {
		if rmsDBFS(v) >= dbfs {
			if first < 0 {
				first = i
			}
			last = i
		}
	}
	if first < 0 {
		return nil
	}
	return env[first : last+1]
}

// bestFixture returns the fixture with the highest score (ties → smallest file name).
func (s *STT) bestFixture(env, prefixOf []float64) (fixture, float64, bool) {
	var best fixture
	bestScore, found := 0.0, false
	for _, f := range s.fixtures { // already sorted by name
		fe := f.env
		if prefixOf != nil && len(env) < len(fe) {
			fe = fe[:len(env)]
		}
		if sc := matchScore(env, fe, sttMaxShiftFrames); !found || sc > bestScore {
			best, bestScore, found = f, sc, true
		}
	}
	return best, bestScore, found
}

type sttItem struct {
	pcm      []byte
	finalize bool
}

type sttSession struct {
	s      *STT
	cfg    provider.STTSessionConfig
	events chan provider.STTEvent
	cancel context.CancelFunc
	ctx    context.Context

	mu       sync.Mutex
	q        []provider.STTEvent // events not yet handed to the public channel
	items    []sttItem
	pending  int
	closed   bool
	emitted  bool // CLOSED queued
	stats    provider.STTStats
	wake     chan struct{}
	evWake   chan struct{}
	closeOne sync.Once

	// worker state (touched only by the worker goroutine)
	rem        []byte
	frames     int
	inSeg      bool
	speechRun  int
	runEnv     []float64
	silRun     int
	segStart   int
	segEnv     []float64
	segIdx     int
	hangAtMs   int64
	errAtMs    int64
	errKind    provider.ErrorKind
	extraDelay time.Duration
	hung       bool
}

// Open implements provider.STTProvider.
func (s *STT) Open(ctx context.Context, cfg provider.STTSessionConfig) (provider.STTSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, cancelledErr(s.name, provider.CapSTT, err)
	}
	fault, _ := s.deps.Faults.Current().Lookup(provider.CapSTT, s.name)
	fail, plan := s.dec.next(fault)
	if fail && plan.Phase == PhaseStart {
		return nil, errorFor(s.name, provider.CapSTT, plan.Kind)
	}
	if !sleep(ctx, time.Duration(s.cfg.ConnectMS)*time.Millisecond+time.Duration(plan.ExtraDelayMs)*time.Millisecond) {
		return nil, cancelledErr(s.name, provider.CapSTT, ctx.Err())
	}
	wctx, cancel := context.WithCancel(context.WithoutCancel(ctx)) // the session outlives the Open call
	ss := &sttSession{s: s, cfg: cfg, events: make(chan provider.STTEvent, sttEventBuffer), cancel: cancel, ctx: wctx,
		wake: make(chan struct{}, 1), evWake: make(chan struct{}, 1), hangAtMs: -1, errAtMs: -1,
		extraDelay: time.Duration(plan.ExtraDelayMs) * time.Millisecond}
	ss.stats.OpenedAt = s.deps.Clock.Now()
	if fail {
		ss.errKind = plan.Kind
		switch plan.Phase {
		case PhaseHang:
			ss.hangAtMs = plan.AfterMs
		case PhaseBeforeFirst:
			ss.errAtMs = 0
		case PhaseMid:
			ss.errAtMs = int64(plan.After) * 1000
			if plan.Mode == ModeDisconnectMidStream {
				ss.errAtMs = plan.AfterMs
			}
		}
	}
	go ss.pump()
	go ss.work()
	return ss, nil
}

// SendAudio implements provider.STTSession.
func (ss *sttSession) SendAudio(pcm []byte) error {
	if len(pcm)%2 != 0 {
		return provider.NewError(ss.s.name, provider.CapSTT, provider.KindInvalidRequest, "fake: PCM length must be even")
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.closed {
		return provider.ErrSessionClosed
	}
	if ss.pending+len(pcm) > sttMaxPendingBytes {
		return provider.NewError(ss.s.name, provider.CapSTT, provider.KindUnavailable, "fake: audio buffer full")
	}
	ss.items = append(ss.items, sttItem{pcm: append([]byte(nil), pcm...)})
	ss.pending += len(pcm)
	ss.stats.AudioMsSent += int64(len(pcm) / sttBytesPerMs)
	ss.signal(ss.wake)
	return nil
}

// Finalize implements provider.STTSession.
func (ss *sttSession) Finalize() error {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.closed {
		return provider.ErrSessionClosed
	}
	ss.items = append(ss.items, sttItem{finalize: true})
	ss.signal(ss.wake)
	return nil
}

// Events implements provider.STTSession.
func (ss *sttSession) Events() <-chan provider.STTEvent { return ss.events }

// Stats implements provider.STTSession.
func (ss *sttSession) Stats() provider.STTStats {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	return ss.stats
}

// Close implements provider.STTSession; it is idempotent.
func (ss *sttSession) Close() error {
	ss.closeOne.Do(func() {
		ss.mu.Lock()
		ss.closed = true
		ss.mu.Unlock()
		ss.cancel()
		ss.queueClosed()
	})
	return nil
}

func (ss *sttSession) signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

// emit queues an event for the public channel. A new PARTIAL replaces queued PARTIALs;
// FINAL/ERROR/CLOSED are never dropped.
func (ss *sttSession) emit(ev provider.STTEvent) {
	ev.Provider, ev.ReceivedAt = ss.s.name, ss.s.deps.Clock.Now()
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.emitted {
		return
	}
	if ss.stats.FirstEventAt.IsZero() {
		ss.stats.FirstEventAt = ev.ReceivedAt
	}
	if ev.Type == provider.STTPartial {
		kept := ss.q[:0]
		for _, e := range ss.q {
			if e.Type != provider.STTPartial {
				kept = append(kept, e)
			}
		}
		ss.q = kept
	}
	ss.q = append(ss.q, ev)
	if ev.Type == provider.STTClosed {
		ss.emitted = true
	}
	ss.signal(ss.evWake)
}

func (ss *sttSession) queueClosed() { ss.emit(provider.STTEvent{Type: provider.STTClosed}) }

// pump forwards queued events to the public channel and closes it after CLOSED.
func (ss *sttSession) pump() {
	defer close(ss.events)
	for {
		ss.mu.Lock()
		if len(ss.q) == 0 {
			ss.mu.Unlock()
			<-ss.evWake
			continue
		}
		ev := ss.q[0]
		ss.q = ss.q[1:]
		closing := ss.closed
		ss.mu.Unlock()
		if closing { // the consumer may be gone: do not block forever
			select {
			case ss.events <- ev:
			case <-time.After(sttCloseDrain):
				return
			}
		} else {
			ss.events <- ev
		}
		if ev.Type == provider.STTClosed {
			return
		}
	}
}

func (ss *sttSession) work() {
	for {
		select {
		case <-ss.ctx.Done():
			return
		case <-ss.wake:
		}
		for {
			ss.mu.Lock()
			if len(ss.items) == 0 {
				ss.mu.Unlock()
				break
			}
			it := ss.items[0]
			ss.items = ss.items[1:]
			ss.mu.Unlock()
			if it.finalize {
				ss.onFinalize()
			} else {
				ss.onAudio(it.pcm)
				ss.mu.Lock()
				ss.pending -= len(it.pcm)
				ss.mu.Unlock()
			}
			if ss.ctx.Err() != nil {
				return
			}
		}
	}
}

func (ss *sttSession) onAudio(pcm []byte) {
	ss.rem = append(ss.rem, pcm...)
	for len(ss.rem) >= sttFrameBytes {
		frame := ss.rem[:sttFrameBytes]
		ss.rem = ss.rem[sttFrameBytes:]
		ss.onFrame(frame)
		if ss.ctx.Err() != nil {
			return
		}
	}
}

func (ss *sttSession) posMs() int64 { return int64(ss.frames) * sttFrameMs }

func (ss *sttSession) onFrame(frame []byte) {
	idx := ss.frames
	ss.frames++
	if ss.hung {
		return
	}
	if ss.hangAtMs >= 0 && ss.posMs() > ss.hangAtMs {
		ss.hung = true // stop emitting anything and never answer Finalize
		return
	}
	if ss.errAtMs >= 0 && ss.posMs() >= ss.errAtMs {
		ss.emit(provider.STTEvent{Type: provider.STTError, Err: errorFor(ss.s.name, provider.CapSTT, ss.errKind)})
		ss.queueClosed()
		ss.mu.Lock()
		ss.closed = true
		ss.mu.Unlock()
		ss.cancel()
		return
	}
	rms := rmsLinear(frame)
	speech := rmsDBFS(rms) >= ss.s.dbfs
	if !ss.inSeg {
		if !speech {
			ss.speechRun, ss.runEnv = 0, nil
			return
		}
		ss.speechRun++
		ss.runEnv = append(ss.runEnv, rms)
		if ss.speechRun == sttSpeechStartRun {
			ss.inSeg, ss.silRun = true, 0
			ss.segStart = idx - sttSpeechStartRun + 1
			ss.segEnv, ss.runEnv = ss.runEnv, nil
			ss.partial()
		}
		return
	}
	ss.segEnv = append(ss.segEnv, rms)
	if speech {
		ss.silRun = 0
	} else {
		ss.silRun++
	}
	if elapsed := int64(len(ss.segEnv)) * sttFrameMs; elapsed%sttPartialEveryMs == 0 {
		ss.partial()
	}
	if ss.silRun >= sttSilenceCloseRun {
		ss.closeSegment(false)
	}
}

// candidate picks the transcript and expected duration for the segment so far.
func (ss *sttSession) candidate(env []float64, prefix bool) (text string, expectedMs int) {
	s := ss.s
	if s.mode == modeFixture && len(s.fixtures) > 0 && (!prefix || len(env)*sttFrameMs >= sttFixtureMinPrefix) {
		var pre []float64
		if prefix {
			pre = env
		}
		if f, score, ok := s.bestFixture(env, pre); ok && score >= s.thr {
			return f.text, f.durMs
		}
	}
	phrase := s.phrases[ss.segIdx%len(s.phrases)]
	return phrase, utf8.RuneCountInString(phrase) * sttMsPerRune
}

func (ss *sttSession) activeEnv() []float64 {
	if ss.silRun > 0 && ss.silRun <= len(ss.segEnv) {
		return ss.segEnv[:len(ss.segEnv)-ss.silRun]
	}
	return ss.segEnv
}

func (ss *sttSession) partial() {
	elapsed := int64(len(ss.segEnv)) * sttFrameMs
	text, expected := ss.candidate(ss.activeEnv(), true)
	runes := []rune(text)
	n := ceilDiv(int64(len(runes))*elapsed, int64(max(expected, 1)))
	n = min(n, int64(len(runes)))
	if n <= 0 {
		return
	}
	startMs := int64(ss.segStart) * sttFrameMs
	ss.emit(provider.STTEvent{Type: provider.STTPartial, Text: provider.NormalizeJA(string(runes[:n])), StartMs: startMs, EndMs: startMs + elapsed})
}

func ceilDiv(a, b int64) int64 { return (a + b - 1) / b }

func (ss *sttSession) closeSegment(fromFinalize bool) {
	env := ss.activeEnv()
	text, _ := ss.candidate(env, false)
	startMs := int64(ss.segStart) * sttFrameMs
	endMs := startMs + int64(len(env))*sttFrameMs
	ss.inSeg, ss.speechRun, ss.runEnv, ss.silRun, ss.segEnv = false, 0, nil, 0, nil
	ss.segIdx++
	ss.final(provider.NormalizeJA(text), startMs, endMs, fromFinalize)
}

func (ss *sttSession) final(text string, startMs, endMs int64, fromFinalize bool) {
	if !sleep(ss.ctx, ss.s.jit.delay(ss.s.cfg.LatencyMS, ss.s.cfg.JitterMS)+ss.extraDelay) {
		return
	}
	conf := sttFinalConfidence
	if text == "" {
		conf = 0
	}
	ev := provider.STTEvent{Type: provider.STTFinal, Text: text, StartMs: startMs, EndMs: endMs, FromFinalize: fromFinalize}
	if text != "" {
		ev.Confidence = &conf
	}
	ss.emit(ev)
}

func (ss *sttSession) onFinalize() {
	if ss.hung {
		return
	}
	if ss.inSeg {
		ss.closeSegment(true)
		return
	}
	pos := ss.posMs()
	ss.final("", pos, pos, true)
}
