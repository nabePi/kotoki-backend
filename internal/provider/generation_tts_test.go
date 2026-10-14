package provider_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/fake"
)

const sentence = "こんにちは。"

func ttsReq(text string) provider.TTSRequest {
	return provider.TTSRequest{Text: text, Language: "ja", SpeakingRate: 1.0, RequestID: "s:1:0"}
}

func drainTTS(t *testing.T, s provider.TTSStream) []provider.TTSChunk {
	t.Helper()
	var out []provider.TTSChunk
	timeout := time.After(5 * time.Second)
	for {
		select {
		case c, ok := <-s.Chunks():
			if !ok {
				return out
			}
			out = append(out, c)
		case <-timeout:
			t.Fatal("TTS stream did not finish")
		}
	}
}

func fakeVoice(withFallback bool) provider.VoiceSelection {
	v := provider.VoiceSelection{Primary: provider.VoiceRef{Provider: provider.FakePrimary}}
	if withFallback {
		v.Fallback = &provider.VoiceRef{Provider: provider.FakeFallback}
	}
	return v
}

func epochNow(g provider.Generation) (int, bool) {
	select {
	case e := <-g.TTSEpochChanged():
		return e, true
	default:
		return 0, false
	}
}

func failBeforeFirst() fake.CapConfig {
	return fake.CapConfig{FailEveryN: 1, FailurePhase: fake.PhaseBeforeFirst, FailureKind: provider.KindUnavailable}
}

// scriptTTS is a scripted TTS adapter that records the requests it receives.
type scriptTTS struct {
	name  provider.Name
	fail  bool // error before the first chunk
	hold  bool // never answer until the context ends
	calls atomic.Int32
	mu    sync.Mutex
	got   []provider.TTSRequest
}

func (s *scriptTTS) Name() provider.Name { return s.name }

type scriptTTSStream struct{ ch chan provider.TTSChunk }

func (s scriptTTSStream) Chunks() <-chan provider.TTSChunk { return s.ch }
func (scriptTTSStream) Close() error                       { return nil }

func (s *scriptTTS) Synthesize(ctx context.Context, req provider.TTSRequest) (provider.TTSStream, error) {
	s.calls.Add(1)
	s.mu.Lock()
	s.got = append(s.got, req)
	s.mu.Unlock()
	ch := make(chan provider.TTSChunk, 8)
	go func() {
		defer close(ch)
		switch {
		case s.hold:
			<-ctx.Done()
		case s.fail:
			ch <- provider.TTSChunk{Err: &provider.Error{Provider: s.name, Capability: provider.CapTTS, Kind: provider.KindUnavailable}}
		default:
			ch <- provider.TTSChunk{PCM: make([]byte, 960)}
			ch <- provider.TTSChunk{Final: true, Usage: &provider.TTSUsage{Characters: utf8.RuneCountInString(req.Text), AudioMs: 20}}
		}
	}()
	return scriptTTSStream{ch}, nil
}

func (s *scriptTTS) last() provider.TTSRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.got[len(s.got)-1]
}

func realCfg(c *provider.RouterConfig) { c.Mode = provider.ModeReal }

func TestTTSSwitchesToFallbackBeforeAudioAndBumpsEpoch(t *testing.T) {
	r, _ := newTestRouter(t, withTTSCap(provider.FakePrimary, failBeforeFirst()))
	g := r.NewGeneration(uuid.New(), 1, fakeVoice(true))
	s, err := g.Synthesize(context.Background(), ttsReq(sentence))
	require.NoError(t, err)
	chunks := drainTTS(t, s)
	last := chunks[len(chunks)-1]
	require.NotNil(t, last.Err)
	require.Equal(t, "tts_provider_switched", last.Err.VendorCode)
	e, ok := epochNow(g)
	require.True(t, ok)
	require.Equal(t, 1, e)

	s2, err := g.Synthesize(context.Background(), ttsReq(sentence)) // the runtime resends
	require.NoError(t, err)
	for _, c := range drainTTS(t, s2) {
		require.Nil(t, c.Err)
		require.Equal(t, 1, c.Epoch)
		require.Equal(t, provider.FakeFallback, c.Provider)
	}
	sum := g.Summary()
	require.True(t, sum.UsedFallback)
	require.Equal(t, provider.FakeFallback, sum.TTSProvider)
	require.Equal(t, "FAKE", sum.TTSVoiceID)
	require.Equal(t, "fake-tts", sum.TTSModel)
}

func TestTTSNoEpochSwitchAfterAudioSent(t *testing.T) { // review focus 2
	r, _ := newTestRouter(t, withTTSCap(provider.FakePrimary, failBeforeFirst()))
	g := r.NewGeneration(uuid.New(), 1, fakeVoice(true))
	g.MarkAudioSent()
	s, err := g.Synthesize(context.Background(), ttsReq(sentence))
	require.NoError(t, err)
	chunks := drainTTS(t, s)
	last := chunks[len(chunks)-1]
	require.NotNil(t, last.Err)
	require.NotEqual(t, "tts_provider_switched", last.Err.VendorCode, "after audio the error is final")
	require.Equal(t, provider.KindUnavailable, last.Err.Kind)
	require.Zero(t, last.Epoch)
	_, ok := epochNow(g)
	require.False(t, ok)
	require.False(t, g.Summary().UsedFallback)
}

func TestTTSVendorPinnedPerGeneration(t *testing.T) {
	r, env := newTestRouter(t, withTTSCap(provider.FakePrimary, failBeforeFirst()))
	g := r.NewGeneration(uuid.New(), 1, fakeVoice(true))
	first, _ := g.Synthesize(context.Background(), ttsReq(sentence))
	drainTTS(t, first)
	primaryCalls := counter(t, env.Reg, "kotoki_provider_requests_total", map[string]string{"provider": "FAKE_PRIMARY", "capability": "TTS"})
	require.Equal(t, 1.0, primaryCalls)

	var wg sync.WaitGroup
	results := make([][]provider.TTSChunk, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := g.Synthesize(context.Background(), ttsReq(sentence))
			if err == nil {
				results[i] = drainTTS(t, s)
			}
		}(i)
	}
	wg.Wait()
	for _, chunks := range results {
		require.NotEmpty(t, chunks)
		for _, c := range chunks {
			require.Equal(t, provider.FakeFallback, c.Provider, "two parallel sentences never mix vendors")
		}
	}
	require.Equal(t, primaryCalls, counter(t, env.Reg, "kotoki_provider_requests_total", map[string]string{"provider": "FAKE_PRIMARY", "capability": "TTS"}),
		"after the switch the primary is not tried again")
}

func TestTTSSwitchOnlyOncePerGeneration(t *testing.T) {
	r, _ := newTestRouter(t, withTTSCap(provider.FakePrimary, failBeforeFirst()), withTTSCap(provider.FakeFallback, failBeforeFirst()))
	g := r.NewGeneration(uuid.New(), 1, fakeVoice(true))
	s, _ := g.Synthesize(context.Background(), ttsReq(sentence))
	c1 := drainTTS(t, s)
	require.Equal(t, "tts_provider_switched", c1[len(c1)-1].Err.VendorCode)
	e, ok := epochNow(g)
	require.True(t, ok)
	require.Equal(t, 1, e)

	s2, _ := g.Synthesize(context.Background(), ttsReq(sentence))
	c2 := drainTTS(t, s2)
	last := c2[len(c2)-1]
	require.NotNil(t, last.Err)
	require.NotEqual(t, "tts_provider_switched", last.Err.VendorCode, "the fallback failing too is final")
	_, ok = epochNow(g)
	require.False(t, ok, "the epoch does not rise again")
}

func TestTTSNoFallbackConfiguredFailsGeneration(t *testing.T) { // D-042
	r, _ := newTestRouter(t, withTTSCap(provider.FakePrimary, failBeforeFirst()))
	g := r.NewGeneration(uuid.New(), 1, fakeVoice(false))
	s, _ := g.Synthesize(context.Background(), ttsReq(sentence))
	chunks := drainTTS(t, s)
	last := chunks[len(chunks)-1]
	require.NotNil(t, last.Err)
	require.NotEqual(t, "tts_provider_switched", last.Err.VendorCode)
	_, ok := epochNow(g)
	require.False(t, ok)
	require.False(t, g.Summary().UsedFallback)
}

func TestTTSStartsOnFallbackWhenPrimaryBreakerOpen(t *testing.T) {
	r, env := newTestRouter(t)
	env.openBreaker(t, r, provider.FakePrimary, provider.CapTTS)
	g := r.NewGeneration(uuid.New(), 1, fakeVoice(true))
	s, err := g.Synthesize(context.Background(), ttsReq(sentence))
	require.NoError(t, err)
	for _, c := range drainTTS(t, s) {
		require.Nil(t, c.Err)
		require.Equal(t, provider.FakeFallback, c.Provider)
		require.Zero(t, c.Epoch, "starting on the fallback is not a switch")
	}
	require.True(t, g.Summary().UsedFallback)
	_, ok := epochNow(g)
	require.False(t, ok)

	g2 := r.NewGeneration(uuid.New(), 2, fakeVoice(false))
	s2, _ := g2.Synthesize(context.Background(), ttsReq(sentence))
	chunks := drainTTS(t, s2)
	require.Len(t, chunks, 1)
	require.Equal(t, "circuit_open", chunks[0].Err.VendorCode)
}

func TestTTSFakeVendorSkipsVoice(t *testing.T) {
	r, _ := newTestRouter(t)
	v := provider.VoiceSelection{Primary: provider.VoiceRef{Provider: provider.Cartesia, ModelName: "PLACEHOLDER_m", VoiceID: "PLACEHOLDER_v"}}
	g := r.NewGeneration(uuid.New(), 1, v)
	req := ttsReq(sentence)
	req.SpeakingRate = 1.05
	s, err := g.Synthesize(context.Background(), req)
	require.NoError(t, err)
	chunks := drainTTS(t, s)
	require.Equal(t, "fake-tts", chunks[0].Model)
	require.Equal(t, provider.FakePrimary, chunks[0].Provider)
	require.Equal(t, "FAKE", g.Summary().TTSVoiceID)
	var n int
	for _, c := range chunks {
		n += len(c.PCM)
	}
	require.InDelta(t, 686, float64(n)/48.0, 20, "SpeakingRate reaches the fake unchanged")
}

func TestTTSVoiceBindingMissingIsInvalidRequestNotBreaker(t *testing.T) {
	eleven := &scriptTTS{name: provider.ElevenLabs}
	cartesia := &scriptTTS{name: provider.Cartesia}
	newReal := func(t *testing.T) (*provider.Router, *testEnv) {
		return newTestRouter(t, withTTSVendors(), withTTS(provider.ElevenLabs, eleven), withTTS(provider.Cartesia, cartesia), withCfg(realCfg),
			withCfg(func(c *provider.RouterConfig) { c.CBFailureThreshold = 1 }))
	}
	t.Run("empty voice id switches to the fallback", func(t *testing.T) {
		r, _ := newReal(t)
		v := provider.VoiceSelection{
			Primary:  provider.VoiceRef{Provider: provider.Cartesia, ModelName: "sonic", VoiceID: ""},
			Fallback: &provider.VoiceRef{Provider: provider.ElevenLabs, ModelName: "eleven", VoiceID: "v2"},
		}
		g := r.NewGeneration(uuid.New(), 1, v)
		s, _ := g.Synthesize(context.Background(), ttsReq(sentence))
		chunks := drainTTS(t, s)
		last := chunks[len(chunks)-1]
		require.Equal(t, "tts_provider_switched", last.Err.VendorCode)
		var cause *provider.Error
		require.ErrorAs(t, last.Err.Cause, &cause)
		require.Equal(t, "voice_binding_missing", cause.VendorCode)
		require.Equal(t, provider.BreakerClosed, r.BreakerForTest(provider.Cartesia, provider.CapTTS).State())
		require.Zero(t, cartesia.calls.Load(), "the adapter is never called for a missing binding")
	})
	t.Run("unregistered vendor without fallback is final", func(t *testing.T) {
		r, _ := newReal(t)
		g := r.NewGeneration(uuid.New(), 1, provider.VoiceSelection{Primary: provider.VoiceRef{Provider: provider.OpenAI, VoiceID: "v"}})
		s, _ := g.Synthesize(context.Background(), ttsReq(sentence))
		chunks := drainTTS(t, s)
		require.Len(t, chunks, 1)
		require.Equal(t, "voice_binding_missing", chunks[0].Err.VendorCode)
		require.Equal(t, provider.KindInvalidRequest, chunks[0].Err.Kind)
	})
}

func TestTTSRequestVoiceFromSnapshot(t *testing.T) {
	eleven := &scriptTTS{name: provider.ElevenLabs, fail: true}
	cartesia := &scriptTTS{name: provider.Cartesia}
	r, _ := newTestRouter(t, withTTSVendors(), withTTS(provider.ElevenLabs, eleven), withTTS(provider.Cartesia, cartesia), withCfg(realCfg))
	v := provider.VoiceSelection{
		Primary:  provider.VoiceRef{Provider: provider.ElevenLabs, ModelName: "Eleven v3 Conversational", VoiceID: "v1"},
		Fallback: &provider.VoiceRef{Provider: provider.Cartesia, ModelName: "sonic 3.6", VoiceID: "v2"},
	}
	g := r.NewGeneration(uuid.New(), 1, v)
	s, _ := g.Synthesize(context.Background(), ttsReq(sentence))
	chunks := drainTTS(t, s)
	require.Equal(t, "tts_provider_switched", chunks[len(chunks)-1].Err.VendorCode)
	require.Equal(t, "v1", eleven.last().VoiceID)
	require.Equal(t, "Eleven v3 Conversational", eleven.last().Model, "taken verbatim from the character, spaces included")

	s2, _ := g.Synthesize(context.Background(), ttsReq(sentence))
	drainTTS(t, s2)
	require.Equal(t, "v2", cartesia.last().VoiceID)
	require.Equal(t, "sonic 3.6", cartesia.last().Model)
	sum := g.Summary()
	require.Equal(t, provider.Cartesia, sum.TTSProvider)
	require.Equal(t, "sonic 3.6", sum.TTSModel)
	require.Equal(t, "v2", sum.TTSVoiceID)
	require.EqualValues(t, 1, eleven.calls.Load(), "the primary vendor was not tried again")
}

func TestTTSTwoCharactersDifferentVendorsConcurrently(t *testing.T) {
	eleven := &scriptTTS{name: provider.ElevenLabs}
	cartesia := &scriptTTS{name: provider.Cartesia}
	r, env := newTestRouter(t, withTTSVendors(), withTTS(provider.ElevenLabs, eleven), withTTS(provider.Cartesia, cartesia), withCfg(realCfg))
	a := r.NewGeneration(uuid.New(), 1, provider.VoiceSelection{Primary: provider.VoiceRef{Provider: provider.ElevenLabs, ModelName: "m", VoiceID: "a"}})
	b := r.NewGeneration(uuid.New(), 1, provider.VoiceSelection{Primary: provider.VoiceRef{Provider: provider.Cartesia, ModelName: "m", VoiceID: "b"}})
	var wg sync.WaitGroup
	var ca, cb []provider.TTSChunk
	wg.Add(2)
	go func() {
		defer wg.Done()
		s, _ := a.Synthesize(context.Background(), ttsReq(sentence))
		ca = drainTTS(t, s)
	}()
	go func() {
		defer wg.Done()
		s, _ := b.Synthesize(context.Background(), ttsReq(sentence))
		cb = drainTTS(t, s)
	}()
	wg.Wait()
	require.Equal(t, provider.ElevenLabs, ca[0].Provider)
	require.Equal(t, provider.Cartesia, cb[0].Provider)
	require.EqualValues(t, 1, eleven.calls.Load())
	require.EqualValues(t, 1, cartesia.calls.Load())

	// the breaker is per (vendor, TTS): two Cartesia characters share it
	env.openBreaker(t, r, provider.Cartesia, provider.CapTTS)
	for _, vid := range []string{"x", "y"} {
		g := r.NewGeneration(uuid.New(), 2, provider.VoiceSelection{Primary: provider.VoiceRef{Provider: provider.Cartesia, ModelName: "m", VoiceID: vid}})
		s, _ := g.Synthesize(context.Background(), ttsReq(sentence))
		chunks := drainTTS(t, s)
		require.Equal(t, "circuit_open", chunks[0].Err.VendorCode)
	}
}

func TestTTSSpeakingRateValidation(t *testing.T) {
	r, _ := newTestRouter(t)
	g := r.NewGeneration(uuid.New(), 1, fakeVoice(false))
	for _, rate := range []float64{0.79, 1.06, -1} {
		req := ttsReq(sentence)
		req.SpeakingRate = rate
		_, err := g.Synthesize(context.Background(), req)
		var pe *provider.Error
		require.ErrorAs(t, err, &pe, rate)
		require.Equal(t, provider.KindInvalidRequest, pe.Kind)
	}
	req := ttsReq(sentence)
	req.SpeakingRate = 0
	s, err := g.Synthesize(context.Background(), req)
	require.NoError(t, err, "0 means 1.0")
	n := 0
	for _, c := range drainTTS(t, s) {
		n += len(c.PCM)
	}
	require.InDelta(t, 720, float64(n)/48.0, 5)
	for _, rate := range []float64{0.80, 1.05} {
		req.SpeakingRate = rate
		s, err := g.Synthesize(context.Background(), req)
		require.NoError(t, err)
		drainTTS(t, s)
	}
}

func TestTTSFirstAudioTimeoutSwitches(t *testing.T) {
	r, _ := newTestRouter(t, withTTSCap(provider.FakePrimary, fake.CapConfig{FailEveryN: 1, FailurePhase: fake.PhaseHang}),
		withCfg(func(c *provider.RouterConfig) { c.TimeoutTTSFirstAudio = 40 * time.Millisecond }))
	g := r.NewGeneration(uuid.New(), 1, fakeVoice(true))
	s, _ := g.Synthesize(context.Background(), ttsReq(sentence))
	chunks := drainTTS(t, s)
	last := chunks[len(chunks)-1]
	require.Equal(t, "tts_provider_switched", last.Err.VendorCode)
	var cause *provider.Error
	require.ErrorAs(t, last.Err.Cause, &cause)
	require.Equal(t, provider.KindTimeout, cause.Kind)
	e, ok := epochNow(g)
	require.True(t, ok)
	require.Equal(t, 1, e)
}

func TestTTSCharactersSummed(t *testing.T) {
	r, _ := newTestRouter(t, withTTSCap(provider.FakePrimary, failBeforeFirst()))
	g := r.NewGeneration(uuid.New(), 1, fakeVoice(true))
	s, _ := g.Synthesize(context.Background(), ttsReq(sentence))
	drainTTS(t, s)
	s2, _ := g.Synthesize(context.Background(), ttsReq(sentence))
	drainTTS(t, s2)
	require.Equal(t, 2*utf8.RuneCountInString(sentence), g.Summary().TTSCharacters, "every request counts, restarts included")
	require.Positive(t, g.Summary().TTSFirstAudioMs+1)
}

func TestTTSSemaphoreFullSwitches(t *testing.T) {
	eleven := &scriptTTS{name: provider.ElevenLabs, hold: true}
	cartesia := &scriptTTS{name: provider.Cartesia}
	r, _ := newTestRouter(t, withTTSVendors(), withTTS(provider.ElevenLabs, eleven), withTTS(provider.Cartesia, cartesia), withCfg(realCfg),
		withCfg(func(c *provider.RouterConfig) { c.Concurrency.ElevenLabsTTS = 1 }))
	v := provider.VoiceSelection{
		Primary:  provider.VoiceRef{Provider: provider.ElevenLabs, ModelName: "m", VoiceID: "a"},
		Fallback: &provider.VoiceRef{Provider: provider.Cartesia, ModelName: "m", VoiceID: "b"},
	}
	g1 := r.NewGeneration(uuid.New(), 1, v)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s1, _ := g1.Synthesize(ctx, ttsReq(sentence))
	defer func() { _ = s1.Close() }()
	require.Eventually(t, func() bool { return eleven.calls.Load() == 1 }, 2*time.Second, 5*time.Millisecond)

	g2 := r.NewGeneration(uuid.New(), 2, v)
	s2, _ := g2.Synthesize(context.Background(), ttsReq(sentence))
	chunks := drainTTS(t, s2)
	last := chunks[len(chunks)-1]
	require.Equal(t, "tts_provider_switched", last.Err.VendorCode)
	var cause *provider.Error
	require.ErrorAs(t, last.Err.Cause, &cause)
	require.Equal(t, "local_concurrency", cause.VendorCode)
	require.Equal(t, provider.BreakerClosed, r.BreakerForTest(provider.ElevenLabs, provider.CapTTS).State())
}

func TestTTSVendorCartesiaRegistered(t *testing.T) {
	cartesia := &scriptTTS{name: provider.Cartesia}
	r, _ := newTestRouter(t, withTTSVendors(), withTTS(provider.Cartesia, cartesia), withCfg(realCfg),
		withCfg(func(c *provider.RouterConfig) { c.Concurrency.CartesiaTTS = 7 }))
	require.Equal(t, []provider.Name{provider.Cartesia}, r.TTSVendorsForTest())
	for _, row := range r.HealthSnapshot() {
		if row.Provider == provider.Cartesia && row.Capability == provider.CapTTS {
			require.Equal(t, 7, row.ConcurrencyLimit)
			return
		}
	}
	t.Fatal("CARTESIA/TTS not registered")
}

func TestTTSCallerCloseEndsWithoutError(t *testing.T) {
	eleven := &scriptTTS{name: provider.ElevenLabs, hold: true}
	r, _ := newTestRouter(t, withTTSVendors(), withTTS(provider.ElevenLabs, eleven), withCfg(realCfg),
		withCfg(func(c *provider.RouterConfig) { c.CBFailureThreshold = 1 }))
	g := r.NewGeneration(uuid.New(), 1, provider.VoiceSelection{Primary: provider.VoiceRef{Provider: provider.ElevenLabs, ModelName: "m", VoiceID: "a"}})
	s, _ := g.Synthesize(context.Background(), ttsReq(sentence))
	require.Eventually(t, func() bool { return eleven.calls.Load() == 1 }, 2*time.Second, 5*time.Millisecond)
	require.NoError(t, s.Close())
	for c := range s.Chunks() {
		t.Fatalf("a closed stream must not deliver chunks, got %+v", c)
	}
	require.Equal(t, provider.BreakerClosed, r.BreakerForTest(provider.ElevenLabs, provider.CapTTS).State(), "a cancellation is not a failure")
}
