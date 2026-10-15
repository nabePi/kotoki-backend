package conversation

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/config"
)

// fakeText is a fixed TextView.
type fakeText struct {
	turn    string
	since   string
	covers  bool
	onsetMs int64
}

func (f fakeText) TurnText() string        { return f.turn }
func (f fakeText) TextSince(int64) string  { return f.since }
func (f fakeText) FinalCoversLatest() bool { return f.covers }
func (f fakeText) TurnOnsetMs() int64      { return f.onsetMs }
func newTestDetector() *Detector           { return NewDetector(DetectorConfigFrom(defaultTuning())) }
func kinds(ev []Event) (out []EventKind)   { return appendKinds(out, ev) }
func appendKinds(out []EventKind, ev []Event) []EventKind {
	for _, e := range ev {
		out = append(out, e.Kind)
	}
	return out
}

func defaultTuning() config.RealtimeTuning {
	return config.RealtimeTuning{
		VADSpeechStartMS: 200, VADSpeechStartWindowMS: 300, VADSpeechEndMS: 300,
		TurnSilenceDefaultMS: 800, TurnSilenceFillerMS: 1400, TurnSilenceCompleteMS: 500,
		TurnSilenceFillerOnlyMS: 2000, TurnMaxUtteranceMS: 45000,
		TurnFillerSuffixes:     []string{"えっと", "ええと", "あの", "その", "なんか", "まあ", "て", "で", "けど", "が", "から", "し", "とか", "って"},
		TurnCompletionSuffixes: []string{"。", "？", "?", "か", "ね", "よ", "です", "ます", "ました", "でした", "ません", "ください"},
		BargeinVADMS:           300, BargeinWindowMS: 400, BargeinMinPartialChars: 2,
	}
}

func TestDetectorConfigFrom(t *testing.T) {
	c := DetectorConfigFrom(defaultTuning())
	require.Equal(t, 10, c.SpeechStartFrames)
	require.Equal(t, 15, c.SpeechStartWindow)
	require.Equal(t, 15, c.BargeFrames)
	require.Equal(t, 20, c.BargeWindow)
	require.Equal(t, int64(300), c.SpeechEndMs)
	require.Equal(t, int64(800), c.DefaultMs)
	require.Equal(t, int64(1400), c.FillerMs)
	require.Equal(t, int64(500), c.CompleteMs)
	require.Equal(t, int64(2000), c.FillerOnlyMs)
	require.Equal(t, int64(45000), c.MaxUtteranceMs)
	require.Equal(t, 2, c.BargeMinChars)
	require.Len(t, c.FillerSuffixes, 14)
	require.Len(t, c.CompletionSuffixes, 12)
}

func TestSpeechStart(t *testing.T) {
	for _, st := range []TurnState{StateListening, StateProcessing} {
		d := newTestDetector()
		tx := fakeText{}
		for i := 1; i <= 9; i++ {
			require.Empty(t, d.Push(true, st, tx), "frame %d", i)
		}
		require.Empty(t, d.Push(false, st, tx))

		// 10 voiced of 15: the window now holds frames 6..20; the first voiced one in the window
		// is frame 6, which starts at 100 ms (frame i covers [(i-1)*20, i*20)).
		d = newTestDetector()
		for i := 1; i <= 5; i++ {
			require.Empty(t, d.Push(false, st, tx))
		}
		pattern := []bool{true, true, false, true, true, false, true, true, false, true, true, false, true, false, true}
		var got []Event
		for i, v := range pattern {
			ev := d.Push(v, st, tx)
			if i < len(pattern)-1 {
				require.Empty(t, ev, "pattern frame %d", i)
			}
			got = ev
		}
		require.Equal(t, []Event{{Kind: EventSpeechConfirmed, OnsetMs: 100}}, got)
		require.Equal(t, int64(400), d.NowMs())
	}

	// Scattered voiced frames below 10 of 15 never confirm speech: alternating frames keep at
	// most 8 voiced per window, 3-of-5 periods exactly 9.
	for _, pattern := range [][]bool{{true, false}, {true, false, true, false, true}} {
		d := newTestDetector()
		for i := range 300 {
			require.Empty(t, d.Push(pattern[i%len(pattern)], StateListening, fakeText{}), "pattern %v frame %d", pattern, i+1)
		}
	}

	// USER_SPEAKING and AI_SPEAKING never confirm speech.
	for _, st := range []TurnState{StateUserSpeaking, StateAISpeaking} {
		d := newTestDetector()
		for range 15 {
			for _, e := range d.Push(true, st, fakeText{}) {
				require.NotEqual(t, EventSpeechConfirmed, e.Kind)
			}
		}
	}
}

func TestSilenceThresholds(t *testing.T) {
	cases := []struct {
		text   string
		covers bool
		want   int64
	}{
		{"昨日会社に行きました", false, 800},
		{"昨日会社に行きました", true, 500},
		{"えっと", false, 2000},
		{"昨日は雨だったけど、", false, 1400},
		{"ですが", false, 1400},
		{"ですが", true, 1400},
		{"ですか", true, 500},
		{"", false, 800},
		{"", true, 800},
		{"。", true, 2000}, // punctuation-only text is filler-only (nothing left after removal)
		{"？", false, 2000},
		{"「」", false, 2000},
		{"あのー、えっと", false, 1400}, // ー is a letter, so not filler-only; ends in えっと
	}
	for _, c := range cases {
		t.Run(c.text, func(t *testing.T) {
			d := newTestDetector()
			tx := fakeText{turn: c.text, covers: c.covers}
			require.Equal(t, c.want, d.Threshold(tx))

			const k = 20
			for i := 1; i <= k; i++ {
				for _, e := range d.Push(true, StateUserSpeaking, tx) {
					require.NotEqual(t, EventSilenceReached, e.Kind, "voiced frame %d", i)
				}
			}
			n := int(c.want / 20)
			for f := k + 1; f <= k+2+n; f++ {
				ev := d.Push(false, StateUserSpeaking, tx)
				var reached []Event
				for _, e := range ev {
					if e.Kind == EventSilenceReached {
						reached = append(reached, e)
					}
				}
				if f < k+2+n {
					require.Empty(t, reached, "frame %d", f)
					continue
				}
				require.Equal(t, []Event{{Kind: EventSilenceReached, ThresholdMs: c.want}}, reached, "frame %d", f)
			}
		})
	}
}

func TestMaxUtterance(t *testing.T) {
	d := newTestDetector()
	for range 7 {
		require.Empty(t, d.Push(false, StateListening, fakeText{}))
	}
	var onset int64 = -1
	for i := 0; onset < 0; i++ {
		require.Less(t, i, 15, "SPEECH_CONFIRMED never fired")
		for _, e := range d.Push(true, StateListening, fakeText{}) {
			require.Equal(t, EventSpeechConfirmed, e.Kind)
			onset = e.OnsetMs
		}
	}
	require.Equal(t, int64(140), onset)

	tx := fakeText{turn: "えっと", onsetMs: onset}
	for i := 0; ; i++ {
		require.Less(t, i, 45000/20+1, "MAX_UTTERANCE never fired")
		ev := d.Push(true, StateUserSpeaking, tx)
		if d.NowMs()-onset < 45000 {
			require.Empty(t, ev, "now=%d", d.NowMs())
			continue
		}
		require.Equal(t, []Event{{Kind: EventMaxUtterance}}, ev)
		require.Equal(t, onset+45000, d.NowMs())
		break
	}
}

func TestPauseResume(t *testing.T) {
	d := newTestDetector()
	tx := fakeText{} // threshold 800: SILENCE_REACHED at k+2+40 (frame 62)
	const k = 20
	for range k {
		require.Empty(t, d.Push(true, StateUserSpeaking, tx))
	}
	var pauses []int
	for f := k + 1; f <= 50; f++ {
		for _, e := range d.Push(false, StateUserSpeaking, tx) {
			require.Equal(t, EventSpeechPause, e.Kind, "frame %d", f)
			pauses = append(pauses, f)
		}
	}
	require.Equal(t, []int{k + 2 + 15}, pauses, "SPEECH_PAUSE exactly once at silence >= 300 ms")

	// Voice again: the 3-of-5 cluster forms on the third voiced frame.
	require.Empty(t, d.Push(true, StateUserSpeaking, tx))
	require.Empty(t, d.Push(true, StateUserSpeaking, tx))
	require.Equal(t, []Event{{Kind: EventSpeechResume}}, d.Push(true, StateUserSpeaking, tx))
	for range 10 {
		require.Empty(t, d.Push(true, StateUserSpeaking, tx), "resume is emitted once")
	}

	// A new pause is possible after the resume.
	var got []EventKind
	for range 17 {
		got = appendKinds(got, d.Push(false, StateUserSpeaking, tx))
	}
	require.Equal(t, []EventKind{EventSpeechPause}, got)
}

func TestBargeInRules(t *testing.T) {
	t.Run("15 of 20 voiced and two runes since onset", func(t *testing.T) {
		d := newTestDetector()
		tx := fakeText{since: "あの"}
		for i := 1; i <= 14; i++ {
			require.Empty(t, d.Push(true, StateAISpeaking, tx), "frame %d", i)
		}
		// Candidate onset at the first cluster (frame 3) minus 60 ms = start of frame 1.
		require.Equal(t, []Event{{Kind: EventBargeInConfirmed, OnsetMs: 0}}, d.Push(true, StateAISpeaking, tx))
	})
	t.Run("one rune is not enough", func(t *testing.T) {
		d := newTestDetector()
		for range 40 {
			require.Empty(t, d.Push(true, StateAISpeaking, fakeText{since: "あ"}))
		}
	})
	t.Run("punctuation does not count", func(t *testing.T) {
		d := newTestDetector()
		for range 40 {
			require.Empty(t, d.Push(true, StateAISpeaking, fakeText{since: "あ。、"}))
		}
	})
	t.Run("14 of 20 voiced", func(t *testing.T) {
		d := newTestDetector()
		tx := fakeText{since: "あの"}
		// Repeating 7 voiced + 3 silent keeps every 20-frame window at 14 voiced and the
		// silence below 300 ms, so the candidate stays alive.
		for range 10 {
			for i := range 10 {
				require.Empty(t, d.Push(i < 7, StateAISpeaking, tx))
			}
		}
	})
	t.Run("echo before commit: empty TextSince", func(t *testing.T) {
		d := newTestDetector()
		for range 40 {
			require.Empty(t, d.Push(true, StateAISpeaking, fakeText{since: ""}))
		}
	})
	t.Run("silence of 300 ms cancels the candidate", func(t *testing.T) {
		d := newTestDetector()
		for range 10 {
			require.Empty(t, d.Push(true, StateAISpeaking, fakeText{}))
		}
		require.Equal(t, int64(0), d.bargeOnsetMs)
		// The cluster lasts 2 frames after the last voiced one, then 15 frames = 300 ms.
		for range 2 + 14 {
			require.Empty(t, d.Push(false, StateAISpeaking, fakeText{}))
		}
		require.Equal(t, int64(0), d.bargeOnsetMs, "silence 280 ms keeps the candidate")
		require.Empty(t, d.Push(false, StateAISpeaking, fakeText{}))
		require.Equal(t, int64(-1), d.bargeOnsetMs, "silence 300 ms cancels the candidate")

		// A new run forms a new candidate; the confirmed onset is the new one.
		newStart := d.NowMs() // start of the next frame
		tx := fakeText{since: "あの"}
		var got []Event
		for i := 0; got == nil; i++ {
			require.Less(t, i, 20, "BARGE_IN_CONFIRMED never fired")
			got = d.Push(true, StateAISpeaking, tx)
		}
		require.Equal(t, []Event{{Kind: EventBargeInConfirmed, OnsetMs: newStart}}, got)
	})
}

func TestInjectSilence(t *testing.T) {
	d := newTestDetector()
	tx := fakeText{}
	for range 20 {
		require.Empty(t, d.Push(true, StateUserSpeaking, tx))
	}
	require.Equal(t, int64(400), d.NowMs())

	ev := d.InjectSilence(19, StateUserSpeaking, tx)
	require.Empty(t, ev)
	require.Equal(t, int64(400), d.NowMs(), "less than one frame injects nothing")

	ev = d.InjectSilence(830, StateUserSpeaking, tx) // 41 frames: 21..61
	require.Equal(t, []EventKind{EventSpeechPause}, kinds(ev))
	require.Equal(t, int64(400+820), d.NowMs())

	ev = d.InjectSilence(20, StateUserSpeaking, tx) // frame 62 = k+2+40
	require.Equal(t, []Event{{Kind: EventSilenceReached, ThresholdMs: 800}}, ev)
	require.Equal(t, int64(62*20), d.NowMs())
}

func TestResetForNewTurn(t *testing.T) {
	d := newTestDetector()
	for range 20 {
		d.Push(true, StateAISpeaking, fakeText{})
	}
	require.Equal(t, int64(0), d.bargeOnsetMs)
	for range 17 {
		d.Push(false, StateUserSpeaking, fakeText{})
	}
	require.True(t, d.pauseEmitted)

	d.ResetForNewTurn()
	require.False(t, d.pauseEmitted)
	require.Equal(t, int64(-1), d.bargeOnsetMs)
	require.Equal(t, int64(37*20), d.NowMs(), "audio time is kept")
	require.Equal(t, 0, d.w5.Count())
	require.Equal(t, 3, d.win20.Count(), "VAD windows are kept")
}

func TestBoolRing(t *testing.T) {
	r := NewBoolRing(5)
	require.Equal(t, 0, r.Count())
	require.Equal(t, int64(0), r.SpanFromFirstTrue())
	for _, v := range []bool{false, true, false, false} {
		r.Push(v)
	}
	require.Equal(t, 1, r.Count())
	require.Equal(t, int64(60), r.SpanFromFirstTrue(), "first true is 3 slots back, inclusive")
	for _, v := range []bool{true, true, false} {
		r.Push(v)
	}
	// Window (oldest→newest): false false true true false; the old true fell out.
	require.Equal(t, 2, r.Count())
	require.Equal(t, int64(60), r.SpanFromFirstTrue())
	for range 5 {
		r.Push(false)
	}
	require.Equal(t, 0, r.Count())
	require.Equal(t, int64(0), r.SpanFromFirstTrue())
}
