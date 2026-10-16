package bench

import (
	"context"
	"fmt"
	"time"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

// ttsBytesPerMs is PCM16LE mono at 24 kHz.
const ttsBytesPerMs = provider.TTSSampleRate * 2 / 1000

// ttsOutcome is what one synthesised sentence measured.
type ttsOutcome struct {
	firstAudioMs float64
	totalMs      float64
	bytes        int
}

// ttsOnce synthesises one sentence and drains the stream. The request carries model and voice as the
// router fills them from characters.model_name/voice_id: taken from the candidate as written.
func ttsOnce(ctx context.Context, p provider.TTSProvider, c Candidate, text, requestID string, clk clock.Clock, timeout time.Duration) (ttsOutcome, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	t0 := clk.Now()
	st, err := p.Synthesize(ctx, provider.TTSRequest{Text: text, Language: "ja", VoiceID: c.Voice, Model: c.Model, SpeakingRate: 1.0, RequestID: requestID})
	if err != nil {
		return ttsOutcome{}, err
	}
	defer func() { _ = st.Close() }()
	var out ttsOutcome
	first := false
	for {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case ch, ok := <-st.Chunks():
			if !ok {
				if out.bytes == 0 {
					return out, provider.NewError(p.Name(), provider.CapTTS, provider.KindInvalidResponse, "stream ended without audio")
				}
				out.totalMs = ms(clk.Now().Sub(t0))
				return out, nil
			}
			if ch.Err != nil {
				return out, ch.Err
			}
			if len(ch.PCM) > 0 {
				if !first {
					first, out.firstAudioMs = true, ms(clk.Now().Sub(t0))
				}
				out.bytes += len(ch.PCM)
			}
			if ch.Final {
				if out.bytes == 0 {
					return out, provider.NewError(p.Name(), provider.CapTTS, provider.KindInvalidResponse, "final chunk without any audio")
				}
				out.totalMs = ms(clk.Now().Sub(t0))
				return out, nil
			}
		}
	}
}

// RunTTS measures the TTS candidate, one (vendor, model, voice), on every sentence (ReferenceText),
// `repeat` times each: time to first audio, total time, bytes, audio duration and the real-time factor
// (total time over audio duration). One run serves every character the candidate is proposed for: the
// character-specific part is the voice id, which is the candidate itself (D-015). A vendor error
// becomes an error row.
func RunTTS(ctx context.Context, p provider.TTSProvider, c Candidate, items []Item, repeat int, clk clock.Clock, opts ...Option) []Result {
	o := newRunOpts(opts)
	var rs []Result
	for _, it := range items {
		for n := 1; n <= repeat; n++ {
			if ctx.Err() != nil {
				return rs
			}
			r := newResult(SuiteTTS, c, it, n)
			out, err := ttsOnce(ctx, p, c, it.ReferenceText, fmt.Sprintf("bench:%s:%d", it.ID, n), clk, o.timeout)
			if err != nil {
				rs = append(rs, r.failed(err))
				continue
			}
			audioMs := float64(out.bytes) / ttsBytesPerMs
			r.Values["first_audio_ms"] = out.firstAudioMs
			r.Values["total_ms"] = out.totalMs
			r.Values["bytes"] = float64(out.bytes)
			r.Values["audio_ms"] = audioMs
			if audioMs > 0 {
				r.Values["rtf"] = out.totalMs / audioMs
			}
			rs = append(rs, r)
		}
	}
	return rs
}
