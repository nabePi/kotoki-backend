package replay_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

var frame20ms = make([]byte, 640)

// drain returns every event already queued on the session, without waiting.
func drain(s *replay.ScriptedSTT) []provider.STTEvent {
	var out []provider.STTEvent
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		default:
			return out
		}
	}
}

func fillerTimeline() replay.Timeline {
	return replay.Timeline{
		{AtMs: 400, Text: "えっと"},
		{AtMs: 600, Text: "えっと週末"},
		{AtMs: 800, Text: "えっと週末は。", Final: true},
		{AtMs: 1000, Text: "映画"},
	}
}

// script runs one fixed call sequence and returns everything the session emitted.
func script(t *testing.T) []provider.STTEvent {
	t.Helper()
	s := replay.NewScriptedSTT(fillerTimeline())
	t.Cleanup(func() { _ = s.Close() })
	var all []provider.STTEvent
	for range 19 {
		require.NoError(t, s.SendAudio(frame20ms))
	}
	require.Empty(t, drain(s), "380 ms of audio: nothing yet")
	require.NoError(t, s.SendAudio(frame20ms))
	got := drain(s)
	require.Len(t, got, 1, "partial exactly when total audio reaches 400 ms")
	require.Equal(t, provider.STTPartial, got[0].Type)
	require.Equal(t, "えっと", got[0].Text)
	all = append(all, got...)

	for range 25 { // up to 900 ms
		require.NoError(t, s.SendAudio(frame20ms))
	}
	got = drain(s)
	require.Len(t, got, 2)
	require.Equal(t, provider.STTPartial, got[0].Type)
	require.Equal(t, provider.STTFinal, got[1].Type)
	require.False(t, got[1].FromFinalize)
	require.Equal(t, "えっと週末は。", got[1].Text)
	all = append(all, got...)

	require.NoError(t, s.Finalize())
	got = drain(s)
	require.Len(t, got, 1, "Finalize yields exactly one FINAL")
	require.Equal(t, provider.STTFinal, got[0].Type)
	require.True(t, got[0].FromFinalize)
	require.Empty(t, got[0].Text, "the final already closed the segment")
	require.Equal(t, 1, s.FinalizeCalls())
	all = append(all, got...)

	for range 5 { // up to 1000 ms
		require.NoError(t, s.SendAudio(frame20ms))
	}
	all = append(all, drain(s)...)
	require.NoError(t, s.Finalize())
	got = drain(s)
	require.Len(t, got, 1)
	require.Equal(t, "映画", got[0].Text, "Finalize text = last partial")
	all = append(all, got...)
	require.Equal(t, int64(1000), s.Stats().AudioMsSent)
	return all
}

func TestScriptedSTTDeterministic(t *testing.T) {
	first := script(t)
	second := script(t)
	require.Equal(t, first, second)
	require.Len(t, first, 6)
}

func TestScriptedSTTFinalizeTextHoldAndBarrier(t *testing.T) {
	s := replay.NewScriptedSTT(nil, replay.WithProvider(provider.FakePrimary))
	t.Cleanup(func() { _ = s.Close() })
	require.Equal(t, provider.FakePrimary, s.Provider())

	s.SetFinalizeText("げま")
	require.NoError(t, s.Finalize())
	got := drain(s)
	require.Len(t, got, 1)
	require.Equal(t, "げま", got[0].Text)
	require.Equal(t, provider.FakePrimary, got[0].Provider)
	require.NoError(t, s.Finalize())
	require.Empty(t, drain(s)[0].Text, "SetFinalizeText applies to the next Finalize only")

	s.HoldFinalize(true)
	s.SetFinalizeText("一")
	require.NoError(t, s.Finalize())
	s.SetFinalizeText("二")
	require.NoError(t, s.Finalize())
	id := s.Barrier()
	got = drain(s)
	require.Len(t, got, 1, "held FINALs are not emitted; the barrier is")
	bid, ok := replay.BarrierID(got[0])
	require.True(t, ok)
	require.Equal(t, id, bid)
	_, ok = replay.BarrierID(provider.STTEvent{Type: provider.STTFinal})
	require.False(t, ok)

	s.HoldFinalize(false)
	got = drain(s)
	require.Len(t, got, 2, "release in call order")
	require.Equal(t, "一", got[0].Text)
	require.Equal(t, "二", got[1].Text)
	require.Equal(t, 4, s.FinalizeCalls())

	s.ReportFinalTimeout()
	require.Equal(t, 1, s.FinalTimeoutReports())
}

func TestScriptedSTTClose(t *testing.T) {
	s := replay.NewScriptedSTT(nil)
	require.Error(t, s.SendAudio(make([]byte, 3)), "odd length")
	require.NoError(t, s.Close())
	require.NoError(t, s.Close(), "idempotent")
	got := drain(s)
	require.Len(t, got, 1)
	require.Equal(t, provider.STTClosed, got[0].Type)
	_, open := <-s.Events()
	require.False(t, open, "Events is closed after CLOSED")
	require.ErrorIs(t, s.SendAudio(frame20ms), provider.ErrSessionClosed)
	require.ErrorIs(t, s.Finalize(), provider.ErrSessionClosed)
}

func TestLoadTimeline(t *testing.T) {
	tl, err := replay.LoadTimeline(filepath.Join("..", "..", "..", "testdata", "audio", "synth", "ja_filler_pause.partials.json"))
	require.NoError(t, err)
	require.Equal(t, replay.Step{AtMs: 400, Text: "えっと"}, tl[0])
	require.True(t, tl[len(tl)-1].Final)

	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.json")
	require.NoError(t, os.WriteFile(bad, []byte(`[{"at_ms":500,"text":"a"},{"at_ms":400,"text":"b"}]`), 0o600))
	_, err = replay.LoadTimeline(bad)
	require.Error(t, err, "at_ms must not go backwards")
	_, err = replay.LoadTimeline(filepath.Join(dir, "missing.json"))
	require.Error(t, err)

	for name, body := range map[string]string{
		"unknown key":        `[{"at_ms":100,"text":"a","fianl":true}]`,
		"empty partial text": `[{"at_ms":100,"text":""}]`,
		"missing text":       `[{"at_ms":100}]`,
		"trailing data":      `[{"at_ms":100,"text":"a"}] []`,
		"not an array":       `{"at_ms":100,"text":"a"}`,
	} {
		f := filepath.Join(dir, "case.json")
		require.NoError(t, os.WriteFile(f, []byte(body), 0o600))
		_, err := replay.LoadTimeline(f)
		require.Error(t, err, name)
	}
	ok := filepath.Join(dir, "ok.json")
	require.NoError(t, os.WriteFile(ok, []byte(`[{"at_ms":100,"text":"a"},{"at_ms":200,"text":"","final":true}]`), 0o600))
	tl, err = replay.LoadTimeline(ok)
	require.NoError(t, err, "an empty FINAL is allowed")
	require.Len(t, tl, 2)
}

// collect reads Events until it is closed, failing after a second.
func collect(t *testing.T, s *replay.ScriptedSTT) []provider.STTEvent {
	t.Helper()
	var out []provider.STTEvent
	timeout := time.After(time.Second)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatalf("Events not closed; got %+v", out)
		}
	}
}

func texts(evs []provider.STTEvent) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, string(ev.Type)+":"+ev.Text)
	}
	return out
}

func TestScriptedSTTFullBufferDropsOldestPartial(t *testing.T) {
	tl := replay.Timeline{{AtMs: 20, Text: "p1"}, {AtMs: 40, Text: "p2"}, {AtMs: 60, Text: "p3"}, {AtMs: 80, Text: "p4"}}
	s := replay.NewScriptedSTT(tl, replay.WithEventBuffer(3))
	require.Equal(t, replay.DefaultEventBuffer, 64, "port contract: Events buffered (64)")
	for range 3 {
		require.NoError(t, s.SendAudio(frame20ms))
	}
	require.NoError(t, s.Finalize()) // buffer full of PARTIALs: the oldest goes, the FINAL stays
	require.NoError(t, s.SendAudio(frame20ms))
	require.Equal(t, 2, s.DroppedPartials())
	require.Equal(t, []string{"PARTIAL:p3", "FINAL:p3", "PARTIAL:p4"}, texts(drain(s)))
}

func TestScriptedSTTFullBufferNeverDropsFinalOrClosed(t *testing.T) {
	s := replay.NewScriptedSTT(nil, replay.WithEventBuffer(2))
	for _, txt := range []string{"一", "二", "三"} {
		s.SetFinalizeText(txt)
		require.NoError(t, s.Finalize())
	}
	require.NotZero(t, s.Barrier())
	require.NoError(t, s.Close(), "Close neither blocks nor panics on an undrained buffer")
	got := texts(collect(t, s))
	require.Equal(t, []string{"FINAL:一", "FINAL:二", "FINAL:三", "BARRIER:1", "CLOSED:"}, got)
	require.Zero(t, s.DroppedPartials())
}

func TestScriptedSTTBarrierAfterClose(t *testing.T) {
	s := replay.NewScriptedSTT(nil)
	require.Equal(t, uint64(1), s.Barrier())
	require.NoError(t, s.Close())
	require.Zero(t, s.Barrier(), "no sentinel after Close: 0, nothing to wait for")
	require.Equal(t, []string{"BARRIER:1", "CLOSED:"}, texts(collect(t, s)))
}
