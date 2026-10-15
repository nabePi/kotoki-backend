package fake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

var t0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

func TestDeciderEveryNDeterministic(t *testing.T) {
	d := newDecider(42, provider.FakePrimary, provider.CapLLM, CapConfig{FailEveryN: 3})
	var got []bool
	for i := 0; i < 6; i++ {
		f, _ := d.next(nil)
		got = append(got, f)
	}
	require.Equal(t, []bool{false, false, true, false, false, true}, got)
}

func TestDeciderRateDeterministic(t *testing.T) {
	run := func() []bool {
		d := newDecider(42, provider.FakePrimary, provider.CapTTS, CapConfig{FailureRate: 0.5, FailureKind: provider.KindTimeout, FailurePhase: PhaseMid, FailAfter: 2})
		var out []bool
		for i := 0; i < 200; i++ {
			f, plan := d.next(nil)
			if f {
				require.Equal(t, provider.KindTimeout, plan.Kind)
				require.Equal(t, PhaseMid, plan.Phase)
				require.Equal(t, 2, plan.After)
			}
			out = append(out, f)
		}
		return out
	}
	a, b := run(), run()
	require.Equal(t, a, b, "same seed, same sequence")
	n := 0
	for _, f := range a {
		if f {
			n++
		}
	}
	require.InDelta(t, 100, n, 30, "close to the configured rate")
	other := newDecider(7, provider.FakePrimary, provider.CapTTS, CapConfig{FailureRate: 0.5})
	var c []bool
	for i := 0; i < 200; i++ {
		f, _ := other.next(nil)
		c = append(c, f)
	}
	require.NotEqual(t, a, c, "a different seed changes the sequence")
}

func TestDeciderEveryNWinsOverRate(t *testing.T) {
	d := newDecider(42, provider.FakePrimary, provider.CapLLM, CapConfig{FailEveryN: 2, FailureRate: 1})
	f1, _ := d.next(nil)
	f2, _ := d.next(nil)
	require.False(t, f1)
	require.True(t, f2)
}

func TestDeciderZeroConfigNeverFails(t *testing.T) {
	d := newDecider(42, provider.FakePrimary, provider.CapLLM, CapConfig{})
	for i := 0; i < 100; i++ {
		f, _ := d.next(nil)
		require.False(t, f)
	}
}

func TestDeciderRuntimeFaultReplacesEnv(t *testing.T) {
	d := newDecider(42, provider.FakePrimary, provider.CapTTS, CapConfig{})
	fail, plan := d.next(&Fault{Mode: ModeFailAfterFirstAudio, Rate: 1})
	require.True(t, fail)
	require.Equal(t, PhaseMid, plan.Phase)
	require.Equal(t, 1, plan.After)
	require.Equal(t, provider.KindUnavailable, plan.Kind)

	fail, plan = d.next(&Fault{Mode: ModeTimeout, Rate: 1})
	require.True(t, fail)
	require.Equal(t, PhaseHang, plan.Phase)

	fail, plan = d.next(&Fault{Mode: ModeSlow, Rate: 1, DelayMs: 20000})
	require.False(t, fail, "slow is latency, not a failure")
	require.Equal(t, 20000, plan.ExtraDelayMs)

	fail, _ = d.next(&Fault{Mode: ModeError, Rate: 0})
	require.False(t, fail)
	fail, plan = d.next(&Fault{Mode: ModeError, Rate: 1, Kind: provider.KindRateLimited})
	require.True(t, fail)
	require.Equal(t, PhaseStart, plan.Phase)
	require.Equal(t, provider.KindRateLimited, plan.Kind)
}

func TestPollerClearsAfterStaleWindow(t *testing.T) { // review focus 5
	ctx := context.Background()
	clk := clock.NewFake(t0)
	fail := false
	p := NewPoller(func(context.Context) ([]byte, bool, error) {
		if fail {
			return nil, false, errors.New("redis down")
		}
		return []byte(`{"LLM.FAKE_PRIMARY":{"mode":"error","rate":1}}`), true, nil
	}, time.Second, clk, nil)
	p.PollOnce(ctx)
	_, ok := p.Current().Lookup(provider.CapLLM, provider.FakePrimary)
	require.True(t, ok)
	fail = true
	clk.Advance(9 * time.Second)
	p.PollOnce(ctx)
	_, ok = p.Current().Lookup(provider.CapLLM, provider.FakePrimary)
	require.True(t, ok, "kept while the outage is young")
	clk.Advance(2 * time.Second)
	p.PollOnce(ctx)
	_, ok = p.Current().Lookup(provider.CapLLM, provider.FakePrimary)
	require.False(t, ok, "cleared once the last success is older than 10 s")
}

func TestPollerKeyRemovedClearsFaults(t *testing.T) {
	ctx := context.Background()
	found := true
	p := NewPoller(func(context.Context) ([]byte, bool, error) {
		return []byte(`{"LLM.FAKE_PRIMARY":{"mode":"error"}}`), found, nil
	}, time.Second, clock.NewFake(t0), nil)
	p.PollOnce(ctx)
	require.Equal(t, 1, p.Current().Len())
	found = false
	p.PollOnce(ctx)
	require.Equal(t, 0, p.Current().Len())
}

func TestPollerInitialErrorStaysEmpty(t *testing.T) {
	p := NewPoller(func(context.Context) ([]byte, bool, error) { return nil, false, errors.New("down") }, time.Second, clock.NewFake(t0), nil)
	p.PollOnce(context.Background())
	require.Equal(t, 0, p.Current().Len())
}

func TestPollerWarnsAtMostOncePerMinute(t *testing.T) {
	clk := clock.NewFake(t0)
	var warns int
	p := NewPoller(func(context.Context) ([]byte, bool, error) { return []byte(`not json`), true, nil }, time.Second, clk, nil)
	p.log = newCountingLogger(&warns)
	for i := 0; i < 5; i++ {
		p.PollOnce(context.Background())
		clk.Advance(10 * time.Second)
	}
	require.Equal(t, 1, warns)
	clk.Advance(time.Minute)
	p.PollOnce(context.Background())
	require.Equal(t, 2, warns)
}

func TestParseFaultSet(t *testing.T) {
	var warns []string
	warn := func(s string) { warns = append(warns, s) }

	s := ParseFaultSet([]byte(`{"LLM.FAKE_PRIMARY":{"mode":"error","rate":0.5,"status":429},"until":"2026-10-14T10:00:00Z"}`), t0, warn)
	f, ok := s.Lookup(provider.CapLLM, provider.FakePrimary)
	require.True(t, ok)
	require.Equal(t, provider.KindRateLimited, f.Kind)
	require.Equal(t, 0.5, f.Rate)

	s = ParseFaultSet([]byte(`{"LLM.FAKE_PRIMARY":{"mode":"error","status":503}}`), t0, warn)
	f, _ = s.Lookup(provider.CapLLM, provider.FakePrimary)
	require.Equal(t, provider.KindUnavailable, f.Kind)
	require.Equal(t, 1.0, f.Rate, "rate defaults to 1")

	s = ParseFaultSet([]byte(`{"LLM.FAKE_PRIMARY":{"mode":"error","status":429,"kind":"AUTH"}}`), t0, warn)
	f, _ = s.Lookup(provider.CapLLM, provider.FakePrimary)
	require.Equal(t, provider.KindAuth, f.Kind, "an explicit kind overrides the status")

	s = ParseFaultSet([]byte(`{"LLM.FAKE_PRIMARY":{"mode":"error"},"until":"2026-10-14T08:00:00Z"}`), t0, warn)
	require.Equal(t, 0, s.Len(), "an expired until empties the document")

	warns = nil
	s = ParseFaultSet([]byte(`{not json`), t0, warn)
	require.Equal(t, 0, s.Len())
	require.Len(t, warns, 1)

	warns = nil
	s = ParseFaultSet([]byte(`{"TTS.FAKE_PRIMARY":{"mode":"disconnect_mid_stream"},"STT.FAKE_FALLBACK":{"mode":"error"},"LLM.FAKE_PRIMARY":{"mode":"error"},"X.Y":{"mode":"error"}}`), t0, warn)
	require.Equal(t, 1, s.Len(), "wrong mode/capability combos and unknown instances are dropped")
	require.Len(t, warns, 3)

	s = ParseFaultSet([]byte(`{"TTS.FAKE_FALLBACK":{"mode":"error"}}`), t0, warn)
	_, ok = s.Lookup(provider.CapTTS, provider.FakeFallback)
	require.True(t, ok, "the TTS fallback slot is addressable")

	var nilSet *FaultSet
	_, ok = nilSet.Lookup(provider.CapTTS, provider.FakePrimary)
	require.False(t, ok)
	_, ok = NoFaults.Current().Lookup(provider.CapTTS, provider.FakePrimary)
	require.False(t, ok)
}

func TestLoadCapConfig(t *testing.T) {
	look := func(m map[string]string) func(string) (string, bool) {
		return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
	}
	c, err := LoadCapConfig(look(nil), provider.FakePrimary, provider.CapLLM)
	require.NoError(t, err)
	require.Equal(t, CapConfig{LatencyMS: 300, FailureKind: provider.KindUnavailable, FailurePhase: PhaseStart, FailAfter: 3}, c)
	for cp, want := range map[provider.Capability]int{provider.CapSTT: 150, provider.CapTTS: 150, provider.CapEvaluation: 500, provider.CapEmbedding: 20} {
		c, err = LoadCapConfig(look(nil), provider.FakePrimary, cp)
		require.NoError(t, err)
		require.Equal(t, want, c.LatencyMS, cp)
	}
	c, _ = LoadCapConfig(look(nil), provider.FakePrimary, provider.CapSTT)
	require.Equal(t, 50, c.ConnectMS)

	c, err = LoadCapConfig(look(map[string]string{
		"FAKE_PRIMARY_LLM_FAILURE_RATE": "0.05", "FAKE_PRIMARY_LLM_FAIL_EVERY_N": "4", "FAKE_PRIMARY_LLM_FAILURE_KIND": "timeout",
		"FAKE_PRIMARY_LLM_FAILURE_PHASE": "MID", "FAKE_PRIMARY_LLM_LATENCY_MS": "450", "FAKE_PRIMARY_LLM_JITTER_MS": "150",
	}), provider.FakePrimary, provider.CapLLM)
	require.NoError(t, err)
	require.Equal(t, 0.05, c.FailureRate)
	require.Equal(t, 4, c.FailEveryN)
	require.Equal(t, provider.KindTimeout, c.FailureKind)
	require.Equal(t, PhaseMid, c.FailurePhase)
	require.Equal(t, 450, c.LatencyMS)
	require.Equal(t, 150, c.JitterMS)

	c, err = LoadCapConfig(look(map[string]string{"FAKE_FALLBACK_TTS_LATENCY_MS": "10"}), provider.FakeFallback, provider.CapTTS)
	require.NoError(t, err)
	require.Equal(t, 10, c.LatencyMS)

	_, err = LoadCapConfig(look(nil), provider.FakeFallback, provider.CapLLM)
	require.Error(t, err, "FALLBACK exists only for TTS")
	_, err = LoadCapConfig(look(nil), provider.OpenAI, provider.CapLLM)
	require.Error(t, err)

	for env, v := range map[string]string{
		"FAKE_PRIMARY_LLM_FAILURE_RATE": "abc", "FAKE_PRIMARY_LLM_LATENCY_MS": "-1", "FAKE_PRIMARY_LLM_FAILURE_KIND": "BOGUS",
		"FAKE_PRIMARY_LLM_FAILURE_PHASE": "sometimes", "FAKE_PRIMARY_LLM_FAIL_EVERY_N": "x",
	} {
		_, err = LoadCapConfig(look(map[string]string{env: v}), provider.FakePrimary, provider.CapLLM)
		require.ErrorContains(t, err, env)
	}
	_, err = LoadCapConfig(look(map[string]string{"FAKE_PRIMARY_LLM_FAILURE_RATE": "1.5"}), provider.FakePrimary, provider.CapLLM)
	require.Error(t, err)
}
