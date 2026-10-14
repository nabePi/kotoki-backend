package fake

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/providertest"
)

type faultsFn func() *FaultSet

func (f faultsFn) Current() *FaultSet { return f() }

func newTestLLMWith(capCfg CapConfig, cfg Config, d Deps) *LLM {
	if d.Clock == nil {
		d.Clock = clock.NewFake(t0)
	}
	d.Seed = 42
	return NewLLM(provider.FakePrimary, capCfg, cfg, d)
}

func newTestLLM(t *testing.T, capCfg CapConfig) *LLM {
	t.Helper()
	return newTestLLMWith(capCfg, Config{}, Deps{})
}

func collect(t *testing.T, l *LLM, req provider.LLMRequest) (string, []provider.LLMEvent) {
	t.Helper()
	s, err := l.Stream(context.Background(), req)
	require.NoError(t, err)
	var b strings.Builder
	var evs []provider.LLMEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return b.String(), evs
			}
			evs = append(evs, ev)
			if ev.Type == provider.LLMDelta {
				b.WriteString(ev.Text)
			}
		case <-timeout:
			t.Fatal("stream did not finish")
		}
	}
}

func user(s string) []provider.Message {
	return []provider.Message{{Role: provider.RoleUser, Content: s}}
}

func TestFakeLLMConformance(t *testing.T) {
	providertest.LLMSuite(t, func(_ *testing.T) provider.LLMProvider {
		return newTestLLMWith(CapConfig{LatencyMS: 300}, Config{LLMTokenIntervalMS: 10}, Deps{})
	})
}

func TestFakeLLMDeterministicTemplate(t *testing.T) {
	l := newTestLLM(t, CapConfig{})
	req := provider.LLMRequest{Messages: user("こんにちは")}
	a, _ := collect(t, l, req)
	b, _ := collect(t, l, req)
	require.Equal(t, a, b)
	require.Contains(t, defaultTemplates, a)
}

func TestFakeLLMOpeningTemplate(t *testing.T) {
	l := newTestLLM(t, CapConfig{})
	text, _ := collect(t, l, provider.LLMRequest{Messages: []provider.Message{{Role: provider.RoleSystem, Content: "s"}}})
	require.Equal(t, "こんにちは！今日はどんな一日でしたか？", text)
	require.NotContains(t, text, "<")
}

func TestFakeLLMLongRepeatsFiveTimes(t *testing.T) {
	l := newTestLLM(t, CapConfig{})
	text, _ := collect(t, l, provider.LLMRequest{Messages: user("#long")})
	var base string
	for _, tpl := range defaultTemplates {
		if strings.Count(text, tpl) == 5 {
			base = tpl
		}
	}
	require.NotEmpty(t, base, "the answer is one template repeated 5 times")
}

func TestFakeLLMDeltasAreThreeRunes(t *testing.T) {
	l := newTestLLM(t, CapConfig{})
	_, evs := collect(t, l, provider.LLMRequest{Messages: user("こんにちは")})
	for _, ev := range evs[:len(evs)-2] {
		require.Equal(t, 3, utf8.RuneCountInString(ev.Text))
	}
	require.LessOrEqual(t, utf8.RuneCountInString(evs[len(evs)-2].Text), 3)
}

func TestFakeLLMUsage(t *testing.T) {
	l := newTestLLM(t, CapConfig{})
	msgs := []provider.Message{{Role: provider.RoleSystem, Content: "あい"}, {Role: provider.RoleUser, Content: "うえお"}}
	text, evs := collect(t, l, provider.LLMRequest{Messages: msgs})
	u := evs[len(evs)-1].Usage
	require.Equal(t, 5, u.InputTokens)
	require.Equal(t, utf8.RuneCountInString(text), u.OutputTokens)
	require.False(t, u.Estimated)
	require.Equal(t, provider.FinishStop, evs[len(evs)-1].FinishReason)
	require.True(t, evs[0].ReceivedAt.Equal(t0), "ReceivedAt comes from the injected clock")
	require.Equal(t, "fake-llm", evs[0].Model)
}

func TestFakeLLMModelEchoed(t *testing.T) {
	l := newTestLLM(t, CapConfig{})
	_, evs := collect(t, l, provider.LLMRequest{Model: "m-x", Messages: user("a")})
	require.Equal(t, "m-x", evs[0].Model)
}

func TestFakeLLMFailurePhases(t *testing.T) {
	t.Run("start", func(t *testing.T) {
		l := newTestLLM(t, CapConfig{FailEveryN: 1, FailureKind: provider.KindRateLimited, FailurePhase: PhaseStart})
		_, err := l.Stream(context.Background(), provider.LLMRequest{Messages: user("a")})
		var pe *provider.Error
		require.ErrorAs(t, err, &pe)
		require.Equal(t, provider.KindRateLimited, pe.Kind)
		require.True(t, pe.Retryable)
	})
	t.Run("before_first", func(t *testing.T) {
		l := newTestLLM(t, CapConfig{FailEveryN: 1, FailureKind: provider.KindUnavailable, FailurePhase: PhaseBeforeFirst})
		_, evs := collect(t, l, provider.LLMRequest{Messages: user("a")})
		require.Len(t, evs, 1)
		require.Equal(t, provider.LLMError, evs[0].Type)
		require.Equal(t, provider.KindUnavailable, evs[0].Err.Kind)
	})
	t.Run("mid", func(t *testing.T) {
		l := newTestLLM(t, CapConfig{FailEveryN: 1, FailureKind: provider.KindUnavailable, FailurePhase: PhaseMid, FailAfter: 2})
		_, evs := collect(t, l, provider.LLMRequest{Messages: user("a")})
		require.Len(t, evs, 3)
		require.Equal(t, provider.LLMDelta, evs[0].Type)
		require.Equal(t, provider.LLMDelta, evs[1].Type)
		require.Equal(t, provider.LLMError, evs[2].Type)
	})
	t.Run("hang", func(t *testing.T) {
		l := newTestLLM(t, CapConfig{FailEveryN: 1, FailurePhase: PhaseHang})
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		defer cancel()
		s, err := l.Stream(ctx, provider.LLMRequest{Messages: user("a")})
		require.NoError(t, err)
		select {
		case ev := <-s.Events():
			t.Fatalf("a hanging stream sent %v before ctx ended", ev.Type)
		case <-time.After(40 * time.Millisecond):
		}
		<-ctx.Done()
		var last provider.LLMEvent
		for ev := range s.Events() {
			last = ev
		}
		require.Equal(t, provider.FinishCancelled, last.FinishReason)
	})
}

func TestFakeLLMHangAfterSomeDeltas(t *testing.T) {
	l := newTestLLM(t, CapConfig{FailEveryN: 1, FailurePhase: PhaseHang, HangAfter: 2})
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	s, err := l.Stream(ctx, provider.LLMRequest{Messages: user("a")})
	require.NoError(t, err)
	var types []provider.LLMEventType
	for ev := range s.Events() {
		types = append(types, ev.Type)
	}
	require.Equal(t, []provider.LLMEventType{provider.LLMDelta, provider.LLMDelta, provider.LLMDone}, types, "two deltas, then silence until ctx ended")

	faults := faultsFn(func() *FaultSet {
		return ParseFaultSet([]byte(`{"LLM.FAKE_PRIMARY":{"mode":"timeout","rate":1,"after_events":1}}`), t0, nil)
	})
	l2 := newTestLLMWith(CapConfig{}, Config{}, Deps{Faults: faults})
	ctx2, cancel2 := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel2()
	s2, err := l2.Stream(ctx2, provider.LLMRequest{Messages: user("a")})
	require.NoError(t, err)
	n := 0
	for ev := range s2.Events() {
		if ev.Type == provider.LLMDelta {
			n++
		}
	}
	require.Equal(t, 1, n, "the runtime timeout fault honours after_events")
}

func TestFakeLLMRuntimeSlowFault(t *testing.T) {
	faults := faultsFn(func() *FaultSet {
		return ParseFaultSet([]byte(`{"LLM.FAKE_PRIMARY":{"mode":"slow","delay_ms":60}}`), t0, nil)
	})
	l := newTestLLMWith(CapConfig{}, Config{}, Deps{Faults: faults})
	start := time.Now()
	s, err := l.Stream(context.Background(), provider.LLMRequest{Messages: user("a")})
	require.NoError(t, err)
	<-s.Events()
	require.GreaterOrEqual(t, time.Since(start), 55*time.Millisecond)
	_ = s.Close()
}

func TestFakeLLMRuntimeErrorFault(t *testing.T) {
	faults := faultsFn(func() *FaultSet {
		return ParseFaultSet([]byte(`{"LLM.FAKE_PRIMARY":{"mode":"error","rate":1,"status":429}}`), t0, nil)
	})
	l := newTestLLMWith(CapConfig{}, Config{}, Deps{Faults: faults})
	_, err := l.Stream(context.Background(), provider.LLMRequest{Messages: user("a")})
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	require.Equal(t, provider.KindRateLimited, pe.Kind)
}

func TestFakeLLMTemplatesFileOverride(t *testing.T) {
	p := filepath.Join(t.TempDir(), "t.txt")
	require.NoError(t, os.WriteFile(p, []byte("ひとつ目です。\n\nふたつ目です。\n"), 0o600))
	l := newTestLLMWith(CapConfig{}, Config{LLMTemplatesFile: p}, Deps{})
	text, _ := collect(t, l, provider.LLMRequest{Messages: user("x")})
	require.Contains(t, []string{"ひとつ目です。", "ふたつ目です。"}, text)

	broken := newTestLLMWith(CapConfig{}, Config{LLMTemplatesFile: filepath.Join(t.TempDir(), "missing")}, Deps{})
	text, _ = collect(t, broken, provider.LLMRequest{Messages: user("x")})
	require.Contains(t, defaultTemplates, text, "an unreadable file falls back to the defaults")
}

func TestFakeLLMCancelMidwayUsageEstimated(t *testing.T) {
	l := newTestLLMWith(CapConfig{}, Config{LLMTokenIntervalMS: 20}, Deps{})
	ctx, cancel := context.WithCancel(context.Background())
	s, err := l.Stream(ctx, provider.LLMRequest{Messages: user("#long")})
	require.NoError(t, err)
	<-s.Events()
	cancel()
	var last provider.LLMEvent
	for ev := range s.Events() {
		last = ev
	}
	require.Equal(t, provider.FinishCancelled, last.FinishReason)
	require.True(t, last.Usage.Estimated)
}
