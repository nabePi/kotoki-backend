package fake

import (
	"context"
	"encoding/binary"
	"math"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

const (
	ttsChunkBytes    = 960 // 20 ms of PCM16LE mono at 24 kHz
	ttsBytesPerMs    = provider.TTSSampleRate * 2 / 1000
	ttsMinDurationMs = 300
	ttsToneHz        = 220.0
	ttsToneAmplitude = 4634.0 // peak of a sine whose RMS is -20 dBFS
	ttsChunkBuffer   = 32
	maxTTSRunes      = 300
	defaultTTSModel  = "fake-tts"
	waveformSilence  = "silence"
)

// TTS is the fake provider.TTSProvider. It ignores Voice/VoiceID/Model from the request
// (D-015: fake mode does not read the character's provider_name/model_name/voice_id).
type TTS struct {
	name provider.Name
	cfg  CapConfig
	conf Config
	deps Deps
	dec  *decider
	jit  *jitterSource
}

var _ provider.TTSProvider = (*TTS)(nil)

// NewTTS builds a fake TTS; name is FakePrimary or FakeFallback.
func NewTTS(name provider.Name, c CapConfig, cfg Config, d Deps) *TTS {
	d = d.WithDefaults()
	return &TTS{name: name, cfg: c, conf: cfg, deps: d,
		dec: newDecider(d.Seed, name, provider.CapTTS, c), jit: newJitterSource(d.Seed, 0x545453)}
}

// Name implements provider.TTSProvider.
func (t *TTS) Name() provider.Name { return t.name }

type ttsStream struct {
	ch     chan provider.TTSChunk
	cancel context.CancelFunc
	once   sync.Once
}

func (s *ttsStream) Chunks() <-chan provider.TTSChunk { return s.ch }

func (s *ttsStream) Close() error {
	s.once.Do(s.cancel)
	return nil
}

// Synthesize implements provider.TTSProvider.
func (t *TTS) Synthesize(ctx context.Context, req provider.TTSRequest) (provider.TTSStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, cancelledErr(t.name, provider.CapTTS, err)
	}
	if n := utf8.RuneCountInString(req.Text); strings.TrimSpace(req.Text) == "" || n > maxTTSRunes {
		return nil, provider.NewError(t.name, provider.CapTTS, provider.KindInvalidRequest, "fake: text must be 1..300 runes")
	}
	fault, _ := t.deps.Faults.Current().Lookup(provider.CapTTS, t.name)
	fail, plan := t.dec.next(fault)
	if fail && plan.Phase == PhaseStart {
		return nil, errorFor(t.name, provider.CapTTS, plan.Kind)
	}
	sctx, cancel := context.WithCancel(ctx)
	s := &ttsStream{ch: make(chan provider.TTSChunk, ttsChunkBuffer), cancel: cancel}
	go t.run(sctx, s, req, fail, plan)
	return s, nil
}

// durationMs is max(300, runes × MsPerChar) / speed, speed = SpeakingRate (0 → 1).
func (t *TTS) durationMs(req provider.TTSRequest) float64 {
	perChar := t.conf.TTSMsPerChar
	if perChar <= 0 {
		perChar = 120
	}
	base := math.Max(ttsMinDurationMs, float64(utf8.RuneCountInString(req.Text)*perChar))
	speed := req.SpeakingRate
	if speed <= 0 {
		speed = 1
	}
	return base / speed
}

// pcm renders the whole utterance; the tone is phase-continuous across chunks.
func (t *TTS) pcm(totalBytes int) []byte {
	out := make([]byte, totalBytes)
	if t.conf.TTSWaveform == waveformSilence {
		return out
	}
	for i := 0; i < totalBytes/2; i++ {
		v := int16(ttsToneAmplitude * math.Sin(2*math.Pi*ttsToneHz*float64(i)/provider.TTSSampleRate))
		binary.LittleEndian.PutUint16(out[i*2:], uint16(v)) //nolint:gosec // two's complement reinterpretation of a PCM sample
	}
	return out
}

func (t *TTS) run(ctx context.Context, s *ttsStream, req provider.TTSRequest, fail bool, plan failPlan) {
	defer close(s.ch)
	defer s.cancel()
	model := req.Model
	if model == "" {
		model = defaultTTSModel
	}
	send := func(c provider.TTSChunk) bool {
		c.Provider, c.Model, c.ReceivedAt = t.name, model, t.deps.Clock.Now()
		select {
		case s.ch <- c:
			return true
		case <-ctx.Done():
			return false
		}
	}
	if fail && plan.Phase == PhaseHang {
		<-ctx.Done()
		return
	}
	wait := t.jit.delay(t.cfg.LatencyMS, t.cfg.JitterMS) + time.Duration(plan.ExtraDelayMs)*time.Millisecond
	if !sleep(ctx, wait) {
		return
	}
	if fail && plan.Phase == PhaseBeforeFirst {
		send(provider.TTSChunk{Err: errorFor(t.name, provider.CapTTS, plan.Kind)})
		return
	}
	dur := t.durationMs(req)
	total := int(dur*ttsBytesPerMs) &^ 1
	data := t.pcm(total)
	interval := time.Duration(t.conf.TTSChunkIntervalMS) * time.Millisecond
	sent := 0
	for chunks := 0; total-sent > ttsChunkBytes; chunks++ {
		if fail && plan.Phase == PhaseMid && chunks >= plan.After {
			send(provider.TTSChunk{Err: errorFor(t.name, provider.CapTTS, plan.Kind)})
			return
		}
		if chunks > 0 && !sleep(ctx, interval) {
			return
		}
		if !send(provider.TTSChunk{PCM: data[sent : sent+ttsChunkBytes : sent+ttsChunkBytes]}) {
			return
		}
		sent += ttsChunkBytes
	}
	if fail && plan.Phase == PhaseMid { // shorter than FailAfter chunks
		send(provider.TTSChunk{Err: errorFor(t.name, provider.CapTTS, plan.Kind)})
		return
	}
	if sent > 0 && !sleep(ctx, interval) {
		return
	}
	send(provider.TTSChunk{PCM: data[sent:total:total], Final: true,
		Usage: &provider.TTSUsage{Characters: utf8.RuneCountInString(req.Text), AudioMs: int64(total / ttsBytesPerMs)}})
}
