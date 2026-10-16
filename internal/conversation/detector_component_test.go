package conversation

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/audio"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/testutil/audiofx"
	sttreplay "github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

// tailSilenceMs of digital silence is appended to every fixture: the synthetic speech ends
// 100–400 ms before the file does, and the longest threshold (filler-only) is 2000 ms.
const tailSilenceMs = 3000

// acted is a detector event the mini state machine acted on, at audio time AtMs.
type acted struct {
	Event
	AtMs int64
}

// fixtureRun is the outcome of feeding one fixture through VAD → detector with scripted STT.
type fixtureRun struct {
	raw       []Event // every event the detector returned
	acted     []acted
	onset     int64           // onset of the (first) user turn
	threshold map[int64]int64 // audio time → Detector.Threshold while USER_SPEAKING
}

func (r fixtureRun) count(k EventKind) int {
	n := 0
	for _, a := range r.acted {
		if a.Kind == k {
			n++
		}
	}
	return n
}

func (r fixtureRun) first(t *testing.T, k EventKind) acted {
	t.Helper()
	for _, a := range r.acted {
		if a.Kind == k {
			return a
		}
	}
	t.Fatalf("no %s event; acted %+v", k, r.acted)
	return acted{}
}

// loadFixture16k returns the fixture decimated to 16 kHz.
func loadFixture16k(t *testing.T, name string) []int16 {
	t.Helper()
	pcm, _ := audiofx.LoadFixture(t, name)
	require.Equal(t, 48000, pcm.Rate, name)
	return audio.Decimate48to16(pcm.Samples)
}

// runDetector feeds 16 kHz audio (plus tail silence) frame by frame to the real VAD (mode 2),
// the scripted STT and the detector, driving a mini Runtime.loop: LISTENING → USER_SPEAKING
// (rows 1, 5, 6) → PROCESSING (rows 3, 4, 9). Detector events repeat every frame until the
// state changes, so only the first event of a kind in a state is acted on.
func runDetector(t *testing.T, pcm16 []int16, tl sttreplay.Timeline) fixtureRun {
	t.Helper()
	vad, err := audio.NewVAD(2)
	require.NoError(t, err)
	stt := sttreplay.NewScriptedSTT(tl)
	t.Cleanup(func() { _ = stt.Close() })
	det := NewDetector(DetectorConfigFrom(defaultTuning()))
	st := newSTTState(det.NowMs)

	pcm16 = append(append([]int16(nil), pcm16...), make([]int16, tailSilenceMs*audio.UpstreamRate/1000)...)
	pump := func() {
		for {
			select {
			case ev := <-stt.Events():
				switch ev.Type {
				case provider.STTPartial:
					st.OnPartial(ev)
				case provider.STTFinal:
					st.OnFinal(ev)
				default:
					t.Fatalf("unexpected STT event %+v", ev)
				}
			default:
				return
			}
		}
	}

	run := fixtureRun{threshold: map[int64]int64{}}
	state, noMerge := StateListening, false
	buf := make([]byte, 0, audio.UpstreamFrame*2)
	for off := 0; off+audio.UpstreamFrame <= len(pcm16); off += audio.UpstreamFrame {
		frame := pcm16[off : off+audio.UpstreamFrame]
		buf = audio.Int16ToBytes(frame, buf)
		require.NoError(t, stt.SendAudio(buf))
		pump()
		evs := det.Push(vad.Process(frame), state, st)
		if state == StateUserSpeaking {
			run.threshold[det.NowMs()] = det.Threshold(st)
		}
		run.raw = append(run.raw, evs...)
		for _, ev := range evs {
			act := func() { run.acted = append(run.acted, acted{Event: ev, AtMs: det.NowMs()}) }
			switch {
			case state == StateListening && ev.Kind == EventSpeechConfirmed: // row 1
				act()
				run.onset = ev.OnsetMs
				st.MarkTurn(ev.OnsetMs)
				det.ResetForNewTurn()
				state = StateUserSpeaking
			case state == StateUserSpeaking && (ev.Kind == EventSpeechPause || ev.Kind == EventSpeechResume): // rows 5, 6
				act()
			case state == StateUserSpeaking && (ev.Kind == EventSilenceReached || ev.Kind == EventMaxUtterance): // rows 3, 4
				act()
				noMerge = ev.Kind == EventMaxUtterance
				st.Commit(CommitTurnEnd)
				require.NoError(t, stt.Finalize())
				pump()
				state = StateProcessing
			case state == StateProcessing && ev.Kind == EventSpeechConfirmed && !noMerge: // row 9: same turn resumes
				act()
				state = StateUserSpeaking
			}
		}
	}
	return run
}

// frameCeil rounds ms up to a frame end (detector times are multiples of FrameMs).
func frameCeil(ms int) int64 { return (int64(ms) + FrameMs - 1) / FrameMs * FrameMs }

func fixtureEntry(t *testing.T, name string) audiofx.Entry {
	t.Helper()
	for _, e := range audiofx.FixtureManifest(t) {
		if e.Name == name {
			return e
		}
	}
	t.Fatalf("fixture %s not in manifest", name)
	return audiofx.Entry{}
}

func fixtureTimeline(t *testing.T, name string) sttreplay.Timeline {
	t.Helper()
	p := audiofx.PartialsPath(t, name)
	if p == "" {
		return nil
	}
	tl, err := sttreplay.LoadTimeline(p)
	require.NoError(t, err)
	return tl
}

func runFixture(t *testing.T, name string) fixtureRun {
	t.Helper()
	return runDetector(t, loadFixture16k(t, name), fixtureTimeline(t, name))
}

// requireOneTurn asserts exactly one SPEECH_CONFIRMED and one SILENCE_REACHED, the latter
// after the last speech segment of the fixture (never inside a pause).
func requireOneTurn(t *testing.T, name string, run fixtureRun) acted {
	t.Helper()
	require.Equal(t, 1, run.count(EventSpeechConfirmed), "%s: acted %+v", name, run.acted)
	require.Equal(t, 1, run.count(EventSilenceReached), "%s: acted %+v", name, run.acted)
	require.Zero(t, run.count(EventMaxUtterance), name)
	segs := fixtureEntry(t, name).Segments
	sil := run.first(t, EventSilenceReached)
	t.Logf("%s: onset %d ms, SILENCE_REACHED at %d ms (threshold %d), last speech end %d ms",
		name, run.onset, sil.AtMs, sil.ThresholdMs, segs[len(segs)-1].EndMs)
	require.GreaterOrEqual(t, sil.AtMs, int64(segs[len(segs)-1].EndMs), "%s: turn ended inside the speech", name)
	return sil
}

func TestDetectorFixtures(t *testing.T) {
	t.Run("ja_filler_pause", func(t *testing.T) {
		// The 1.0 s pause follows "えっと" (filler-only → 2000 ms): no turn end there.
		run := runFixture(t, "ja_filler_pause")
		requireOneTurn(t, "ja_filler_pause", run)
		segs := fixtureEntry(t, "ja_filler_pause").Segments
		require.Len(t, segs, 2)
		require.Equal(t, 1000, segs[1].StartMs-segs[0].EndMs)
		for ms := frameCeil(segs[0].EndMs + 200); ms < int64(segs[1].StartMs); ms += FrameMs {
			require.Equal(t, int64(2000), run.threshold[ms], "filler-only threshold in the pause at %d ms", ms)
		}
		require.GreaterOrEqual(t, run.count(EventSpeechPause), 1, "the pause is seen (speech.ended) but does not end the turn")
	})
	t.Run("ja_conjunction_pause", func(t *testing.T) {
		// "…だったけど" (filler suffix → 1400 ms) outlasts the 1.1 s pause: one turn.
		run := runFixture(t, "ja_conjunction_pause")
		requireOneTurn(t, "ja_conjunction_pause", run)
		segs := fixtureEntry(t, "ja_conjunction_pause").Segments
		require.Len(t, segs, 2)
		require.Equal(t, 1100, segs[1].StartMs-segs[0].EndMs)
		for ms := frameCeil(segs[0].EndMs + 200); ms < int64(segs[1].StartMs); ms += FrameMs {
			require.Equal(t, int64(1400), run.threshold[ms], "filler-suffix threshold in the pause at %d ms", ms)
		}
	})
	t.Run("ja_question", func(t *testing.T) {
		// "…ですか？" with the FINAL received → complete (500 ms).
		run := runFixture(t, "ja_question")
		sil := requireOneTurn(t, "ja_question", run)
		require.Equal(t, int64(500), sil.ThresholdMs)
	})
	t.Run("ja_greeting_short", func(t *testing.T) {
		// "こんにちは" partial only → default (800 ms).
		run := runFixture(t, "ja_greeting_short")
		sil := requireOneTurn(t, "ja_greeting_short", run)
		require.Equal(t, int64(800), sil.ThresholdMs)
	})
	t.Run("ja_max_utterance", func(t *testing.T) {
		segs := fixtureEntry(t, "ja_max_utterance").Segments
		require.Len(t, segs, 1)
		require.Greater(t, segs[0].EndMs-segs[0].StartMs, 45000, "one speech segment longer than 45 s")

		run := runFixture(t, "ja_max_utterance")
		require.Equal(t, 1, run.count(EventSpeechConfirmed), "acted %+v", run.acted)
		require.Equal(t, 1, run.count(EventMaxUtterance), "acted %+v", run.acted)
		require.Zero(t, run.count(EventSilenceReached), "no second turn end after MAX_UTTERANCE")
		maxAt := run.first(t, EventMaxUtterance).AtMs
		require.InDelta(t, 45000, maxAt-run.onset, 40, "MAX_UTTERANCE 45 s after the onset")
	})
	for _, name := range []string{"noise_only", "silence_5s"} {
		t.Run(name, func(t *testing.T) {
			run := runFixture(t, name)
			require.Empty(t, run.raw, "%s: no detector event at all", name)
		})
	}
	// Speech start needs VAD active >= 200 ms (D-076: 10 voiced frames in 15). The cuts come from
	// INSIDE the speech of ja_greeting_short, not its leading digital silence. WebRTC VAD mode 2
	// keeps reporting voice after a burst (overhang: +80 ms after <= 100 ms of speech, +120 ms
	// after longer bursts), so a 100 ms cut is 9 voiced frames (180 ms VAD) and is ignored,
	// while a 150 ms cut is 13 voiced frames (260 ms VAD) and is a valid speech start.
	for _, tc := range []struct {
		cutMs, offsetMs int // offsetMs from the segment start
		speech          bool
	}{
		{cutMs: 100, offsetMs: 0, speech: false},
		{cutMs: 100, offsetMs: 300, speech: false},
		{cutMs: 100, offsetMs: 600, speech: false},
		{cutMs: 150, offsetMs: 0, speech: true},
	} {
		t.Run(fmt.Sprintf("speech_cut_%dms_at_%d", tc.cutMs, tc.offsetMs), func(t *testing.T) {
			seg := fixtureEntry(t, "ja_greeting_short").Segments[0]
			from := seg.StartMs + tc.offsetMs
			to := from + tc.cutMs
			require.True(t, fixtureEntry(t, "ja_greeting_short").SegmentCovers(from, to), "cut inside the speech")
			pcm16 := loadFixture16k(t, "ja_greeting_short")
			cut := pcm16[from*audio.UpstreamRate/1000 : to*audio.UpstreamRate/1000]
			voiced := vadVoicedFrames(t, cut)
			require.GreaterOrEqual(t, voiced, tc.cutMs/FrameMs, "the cut is voice, so the result means something")
			run := runDetector(t, cut, nil)
			if !tc.speech {
				require.Less(t, voiced, 10, "VAD active < 200 ms")
				require.Empty(t, run.raw, "VAD active < 200 ms is no speech start")
				return
			}
			require.GreaterOrEqual(t, voiced, 10, "overhang makes the VAD active >= 200 ms")
			require.Equal(t, 1, run.count(EventSpeechConfirmed), "acted %+v", run.acted)
		})
	}
}

// vadVoicedFrames counts the frames the real VAD (mode 2) marks voiced in pcm16 followed by
// the tail silence, overhang included.
func vadVoicedFrames(t *testing.T, pcm16 []int16) int {
	t.Helper()
	vad, err := audio.NewVAD(2)
	require.NoError(t, err)
	pcm16 = append(append([]int16(nil), pcm16...), make([]int16, tailSilenceMs*audio.UpstreamRate/1000)...)
	n := 0
	for off := 0; off+audio.UpstreamFrame <= len(pcm16); off += audio.UpstreamFrame {
		if vad.Process(pcm16[off : off+audio.UpstreamFrame]) {
			n++
		}
	}
	return n
}
