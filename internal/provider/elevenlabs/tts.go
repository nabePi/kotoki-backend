package elevenlabs

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
)

const (
	maxErrorBody = 64 << 10
	readBufSize  = 8 << 10
	chunkBuffer  = 32

	// speed range accepted by ElevenLabs (spec 8.4.5); the docs page read at M9-T7 states no
	// min/max, so the adapter keeps the spec's 0.7-1.2 as a defensive clamp.
	minSpeed = 0.7
	maxSpeed = 1.2

	// pcm_24000 = PCM16 LE mono at 24 kHz = 48000 bytes/s = 48 bytes/ms.
	bytesPerMs = 48

	codeQuotaExceeded = "quota_exceeded"
)

// Rate-limit headers read when present (not documented by ElevenLabs, see Config.Metrics).
const (
	headerRemainingRequests = "x-ratelimit-remaining-requests"
	headerRemainingTokens   = "x-ratelimit-remaining-tokens" //nolint:gosec // header name, not a credential
)

// TTS synthesises one sentence per request over HTTP streaming (spec 8.4.5).
type TTS struct {
	cfg Config
	hc  *http.Client
	log *slog.Logger
}

var _ provider.TTSProvider = (*TTS)(nil)

// NewTTS builds the TTS adapter; APIKey and BaseURL are required.
func NewTTS(cfg Config, hc *http.Client, log *slog.Logger) (*TTS, error) {
	if cfg.APIKey == "" {
		return nil, errors.New("elevenlabs: APIKey is empty")
	}
	if cfg.BaseURL == "" {
		return nil, errors.New("elevenlabs: BaseURL is empty")
	}
	if hc == nil {
		hc = httpx.NewClient()
	}
	if log == nil {
		log = slog.Default()
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &TTS{cfg: cfg, hc: hc, log: log}, nil
}

// Name is ELEVENLABS.
func (*TTS) Name() provider.Name { return provider.ElevenLabs }

type voiceSettings struct {
	Speed float64 `json:"speed"` // the only voice setting sent (D-014)
}

type ttsBody struct {
	Text                   string        `json:"text"`
	ModelID                string        `json:"model_id"`
	LanguageCode           string        `json:"language_code,omitempty"`
	VoiceSettings          voiceSettings `json:"voice_settings"`
	PreviousText           string        `json:"previous_text,omitempty"`
	ApplyTextNormalization string        `json:"apply_text_normalization"`
}

// clampSpeed maps SpeakingRate to voice_settings.speed: 0 means 1.0, otherwise clamped to the
// vendor range.
func clampSpeed(r float64) float64 {
	switch {
	case r == 0:
		return 1.0
	case r < minSpeed:
		return minSpeed
	case r > maxSpeed:
		return maxSpeed
	}
	return r
}

// supportsLanguageCode is false for multilingual_v2 models ("not supported for multilingual_v2").
func supportsLanguageCode(model string) bool { return !strings.Contains(model, "multilingual_v2") }

// Synthesize returns once the vendor answered 200; failures before that are *provider.Error.
// Only VoiceID, Model, SpeakingRate, Text, PreviousText and Language of req are read (8.4.12).
func (t *TTS) Synthesize(ctx context.Context, req provider.TTSRequest) (provider.TTSStream, error) {
	body := ttsBody{
		Text: req.Text, ModelID: req.Model, VoiceSettings: voiceSettings{Speed: clampSpeed(req.SpeakingRate)},
		PreviousText: req.PreviousText, ApplyTextNormalization: "auto",
	}
	if supportsLanguageCode(req.Model) {
		body.LanguageCode = req.Language
	}
	ctx, span := httpx.StartSpan(ctx, provider.CapTTS, provider.ElevenLabs, req.Model)
	sctx, cancel := context.WithCancel(ctx)
	resp, err := t.post(sctx, req.VoiceID, body) //nolint:bodyclose // closed by run
	if err != nil {
		cancel()
		span.End(outcomeOf(err), err)
		t.logFailure(req, err)
		return nil, err
	}
	st := &ttsStream{ch: make(chan provider.TTSChunk, chunkBuffer), cancel: cancel}
	go t.run(sctx, resp, st, req, span)
	return st, nil
}

func outcomeOf(err error) string {
	if provider.KindOf(err) == provider.KindCancelled {
		return httpx.OutcomeCancelled
	}
	return httpx.OutcomeError
}

func (t *TTS) post(ctx context.Context, voiceID string, body ttsBody) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, provider.NewError(provider.ElevenLabs, provider.CapTTS, provider.KindInvalidRequest, "request could not be encoded")
	}
	u := t.cfg.BaseURL + "/v1/text-to-speech/" + url.PathEscape(voiceID) + "/stream?output_format=pcm_24000"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return nil, provider.NewError(provider.ElevenLabs, provider.CapTTS, provider.KindInvalidRequest, "request could not be built")
	}
	hreq.Header.Set("xi-api-key", t.cfg.APIKey)
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := t.hc.Do(hreq)
	if err != nil {
		e := httpx.ClassifyError(provider.ElevenLabs, provider.CapTTS, err)
		e.Message = t.scrub(e.Message)
		return nil, e
	}
	t.publishRateLimit(resp.Header)
	if resp.StatusCode/100 != 2 {
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		e := classify(provider.CapTTS, resp, b)
		e.Message = t.scrub(e.Message)
		e.VendorCode = t.scrub(e.VendorCode)
		return nil, e
	}
	return resp, nil
}

func (t *TTS) scrub(s string) string { return strings.ReplaceAll(s, t.cfg.APIKey, "[redacted]") }

// detailStatus reads {"detail":{"status":S,"message":M}}; detail may also be a validation-error
// list (422), which has no status.
func detailStatus(body []byte) (status, message string) {
	var doc struct {
		Detail json.RawMessage `json:"detail"`
	}
	if json.Unmarshal(body, &doc) != nil || len(doc.Detail) == 0 {
		return "", ""
	}
	var d struct {
		Status  string `json:"status"`
		Message string `json:"message"`
	}
	if json.Unmarshal(doc.Detail, &d) != nil {
		return "", ""
	}
	return d.Status, d.Message
}

// classify maps a non-2xx response: httpx.ClassifyHTTP plus detail.status as VendorCode;
// quota_exceeded is not retryable (spec 8.4.5).
func classify(c provider.Capability, resp *http.Response, body []byte) *provider.Error {
	e := httpx.ClassifyHTTP(provider.ElevenLabs, c, resp, body)
	if status, msg := detailStatus(body); status != "" {
		e.VendorCode = httpx.Redact(status)
		if msg != "" {
			e.Message = httpx.Redact(msg)
		}
	}
	if e.Kind == provider.KindRateLimited && e.VendorCode == codeQuotaExceeded {
		e.Retryable = false
	}
	return e
}

func (t *TTS) publishRateLimit(h http.Header) {
	if t.cfg.Metrics == nil {
		return
	}
	for kind, name := range map[string]string{"requests": headerRemainingRequests, "tokens": headerRemainingTokens} {
		if v, err := strconv.ParseFloat(strings.TrimSpace(h.Get(name)), 64); err == nil {
			t.cfg.Metrics.RateLimitRemaining(provider.ElevenLabs, provider.CapTTS, kind, v)
		}
	}
}

// logFailure logs identifiers and classification only: never keys, text or audio.
func (t *TTS) logFailure(req provider.TTSRequest, err error) {
	if err == nil || provider.KindOf(err) == provider.KindCancelled {
		return
	}
	attrs := []any{"provider", string(provider.ElevenLabs), "capability", string(provider.CapTTS), "model", req.Model,
		"request_id", req.RequestID, "kind", string(provider.KindOf(err))}
	var pe *provider.Error
	if errors.As(err, &pe) {
		attrs = append(attrs, "http_status", pe.HTTPStatus, "vendor_code", pe.VendorCode)
	}
	t.log.Warn("provider call failed", attrs...)
}

type ttsStream struct {
	ch     chan provider.TTSChunk
	cancel context.CancelFunc
	once   sync.Once
}

func (s *ttsStream) Chunks() <-chan provider.TTSChunk { return s.ch }

// Close cancels the request; idempotent.
func (s *ttsStream) Close() error {
	s.once.Do(s.cancel)
	return nil
}

func (t *TTS) run(ctx context.Context, resp *http.Response, st *ttsStream, req provider.TTSRequest, span *httpx.Span) {
	defer close(st.ch)
	defer st.cancel()
	defer func() { _ = resp.Body.Close() }()

	send := func(c provider.TTSChunk) bool {
		c.Provider, c.Model, c.ReceivedAt = provider.ElevenLabs, req.Model, time.Now()
		select {
		case st.ch <- c:
			return true
		case <-ctx.Done():
			return false
		}
	}
	var (
		buf   = make([]byte, readBufSize)
		carry []byte // odd byte held back at a chunk boundary
		total int64
	)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			data := append(append(make([]byte, 0, len(carry)+n), carry...), buf[:n]...)
			carry = carry[:0]
			if len(data)%2 == 1 {
				carry = append(carry, data[len(data)-1])
				data = data[:len(data)-1]
			}
			if len(data) > 0 {
				total += int64(len(data))
				if !send(provider.TTSChunk{PCM: data}) {
					span.End(httpx.OutcomeCancelled, nil)
					return
				}
			}
		}
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			span.End(httpx.OutcomeCancelled, nil)
			return
		}
		if errors.Is(err, io.EOF) {
			usage := &provider.TTSUsage{Characters: utf8.RuneCountInString(req.Text), AudioMs: total / bytesPerMs}
			if send(provider.TTSChunk{Final: true, Usage: usage}) {
				span.End(httpx.OutcomeSuccess, nil)
			} else {
				span.End(httpx.OutcomeCancelled, nil)
			}
			return
		}
		e := provider.NewError(provider.ElevenLabs, provider.CapTTS, provider.KindUnavailable, "audio stream interrupted")
		e.Retryable = true
		e.Cause = err
		t.logFailure(req, e)
		if send(provider.TTSChunk{Err: e}) {
			span.End(httpx.OutcomeError, e)
		} else {
			span.End(httpx.OutcomeCancelled, nil)
		}
		return
	}
}
