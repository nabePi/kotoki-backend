package fake

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/providertest"
)

func newTestTTSWith(name provider.Name, capCfg CapConfig, cfg Config, d Deps) *TTS {
	if d.Clock == nil {
		d.Clock = clock.NewFake(t0)
	}
	d.Seed = 42
	return NewTTS(name, capCfg, cfg, d)
}

func newTestTTS(_ *testing.T) *TTS {
	return newTestTTSWith(provider.FakePrimary, CapConfig{}, Config{TTSChunkIntervalMS: 0, TTSMsPerChar: 120}, Deps{})
}

func mustSynth(t *testing.T, p *TTS, req provider.TTSRequest) provider.TTSStream {
	t.Helper()
	s, err := p.Synthesize(context.Background(), req)
	require.NoError(t, err)
	return s
}

func drain(t *testing.T, s provider.TTSStream) []byte {
	t.Helper()
	var pcm []byte
	timeout := time.After(5 * time.Second)
	for {
		select {
		case c, ok := <-s.Chunks():
			if !ok {
				return pcm
			}
			pcm = append(pcm, c.PCM...)
		case <-timeout:
			t.Fatal("stream did not finish")
		}
	}
}

func chunksOf(t *testing.T, s provider.TTSStream) []provider.TTSChunk {
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
			t.Fatal("stream did not finish")
		}
	}
}

func TestFakeTTSConformance(t *testing.T) {
	providertest.TTSSuite(t, func(_ *testing.T) provider.TTSProvider {
		return newTestTTSWith(provider.FakePrimary, CapConfig{LatencyMS: 20}, Config{TTSChunkIntervalMS: 5, TTSMsPerChar: 120}, Deps{})
	}, func() provider.TTSRequest {
		return provider.TTSRequest{Text: "こんにちは、今日はいい天気ですね。", Language: "ja", SpeakingRate: 1}
	})
}

func TestFakeTTSDurationFollowsSpeakingRate(t *testing.T) {
	req := provider.TTSRequest{Text: "こんにちは。", SpeakingRate: 1.05}
	pcm := drain(t, mustSynth(t, newTestTTS(t), req))
	// 6 runes → max(300, 6×120) = 720 ms; speed 1.05 → 686 ms of 24 kHz PCM16
	require.InDelta(t, 686, float64(len(pcm))/48.0, 20)
}

func TestFakeTTSZeroRateIsOne(t *testing.T) {
	pcm := drain(t, mustSynth(t, newTestTTS(t), provider.TTSRequest{Text: "こんにちは。"}))
	require.InDelta(t, 720, float64(len(pcm))/48.0, 1)
}

func TestFakeTTSMinimumDuration(t *testing.T) {
	pcm := drain(t, mustSynth(t, newTestTTS(t), provider.TTSRequest{Text: "あ"}))
	require.InDelta(t, 300, float64(len(pcm))/48.0, 1)
}

func TestFakeTTSChunksAre20ms(t *testing.T) {
	cs := chunksOf(t, mustSynth(t, newTestTTS(t), provider.TTSRequest{Text: "こんにちは。"}))
	for _, c := range cs[:len(cs)-1] {
		require.Len(t, c.PCM, 960)
	}
	last := cs[len(cs)-1]
	require.True(t, last.Final)
	require.LessOrEqual(t, len(last.PCM), 960)
	require.EqualValues(t, 720, last.Usage.AudioMs)
	require.Equal(t, 6, last.Usage.Characters)
}

func TestFakeTTSWaveform(t *testing.T) {
	tone := drain(t, mustSynth(t, newTestTTS(t), provider.TTSRequest{Text: "こんにちは。"}))
	require.InDelta(t, -20, provider.RMSdBFS(tone), 1.5)

	silent := newTestTTSWith(provider.FakePrimary, CapConfig{}, Config{TTSWaveform: "silence"}, Deps{})
	for _, b := range drain(t, mustSynth(t, silent, provider.TTSRequest{Text: "こんにちは。"})) {
		require.Zero(t, b)
	}
}

func TestFakeTTSIgnoresVoiceAndModelFields(t *testing.T) {
	req := provider.TTSRequest{Text: "こんにちは。", Voice: provider.VoiceSelection{Primary: provider.VoiceRef{Provider: provider.Cartesia, VoiceID: "PLACEHOLDER_x"}}, VoiceID: "PLACEHOLDER_x", Model: "PLACEHOLDER_m"}
	cs := chunksOf(t, mustSynth(t, newTestTTS(t), req))
	require.True(t, cs[len(cs)-1].Final)
	require.Equal(t, "PLACEHOLDER_m", cs[0].Model, "the router-provided model is echoed, never interpreted")
}

func TestFakeTTSRejectsBadText(t *testing.T) {
	p := newTestTTS(t)
	for _, text := range []string{"", "  "} {
		_, err := p.Synthesize(context.Background(), provider.TTSRequest{Text: text})
		var pe *provider.Error
		require.ErrorAs(t, err, &pe)
		require.Equal(t, provider.KindInvalidRequest, pe.Kind)
	}
}

func TestFakeTTSFailAfterFirstAudio(t *testing.T) {
	faults := faultsFn(func() *FaultSet {
		return ParseFaultSet([]byte(`{"TTS.FAKE_PRIMARY":{"mode":"fail_after_first_audio","rate":1}}`), t0, nil)
	})
	p := newTestTTSWith(provider.FakePrimary, CapConfig{}, Config{}, Deps{Faults: faults})
	cs := chunksOf(t, mustSynth(t, p, provider.TTSRequest{Text: "こんにちは、今日はいい天気ですね。"}))
	require.Len(t, cs, 2)
	require.NotEmpty(t, cs[0].PCM)
	require.Nil(t, cs[0].Err)
	require.NotNil(t, cs[1].Err)
	require.Equal(t, provider.KindUnavailable, cs[1].Err.Kind)
}

func TestFakeTTSFallbackInstanceHasOwnFaults(t *testing.T) {
	faults := faultsFn(func() *FaultSet {
		return ParseFaultSet([]byte(`{"TTS.FAKE_PRIMARY":{"mode":"error","rate":1}}`), t0, nil)
	})
	primary := newTestTTSWith(provider.FakePrimary, CapConfig{}, Config{}, Deps{Faults: faults})
	fallback := newTestTTSWith(provider.FakeFallback, CapConfig{}, Config{}, Deps{Faults: faults})
	_, err := primary.Synthesize(context.Background(), provider.TTSRequest{Text: "あ"})
	require.Error(t, err)
	s, err := fallback.Synthesize(context.Background(), provider.TTSRequest{Text: "あ"})
	require.NoError(t, err, "the fallback slot is unaffected by the primary's fault")
	cs := chunksOf(t, s)
	require.Equal(t, provider.FakeFallback, cs[0].Provider)
}

func TestFakeTTSHang(t *testing.T) {
	p := newTestTTSWith(provider.FakePrimary, CapConfig{FailEveryN: 1, FailurePhase: PhaseHang}, Config{}, Deps{})
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	s, err := p.Synthesize(ctx, provider.TTSRequest{Text: "あ"})
	require.NoError(t, err)
	select {
	case c, ok := <-s.Chunks():
		t.Fatalf("a hanging stream sent %v (open=%v)", c, ok)
	case <-time.After(40 * time.Millisecond):
	}
	<-ctx.Done()
	for range s.Chunks() {
		t.Fatal("no chunk may arrive after ctx ended")
	}
}

func TestFakeTTSChunkInterval(t *testing.T) {
	p := newTestTTSWith(provider.FakePrimary, CapConfig{}, Config{TTSChunkIntervalMS: 20, TTSMsPerChar: 120}, Deps{})
	start := time.Now()
	cs := chunksOf(t, mustSynth(t, p, provider.TTSRequest{Text: "こんにちは。"}))
	require.GreaterOrEqual(t, time.Since(start), time.Duration(len(cs)-1)*15*time.Millisecond, "chunks are paced by FAKE_TTS_CHUNK_INTERVAL_MS")
}
