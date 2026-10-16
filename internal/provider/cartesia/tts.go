package cartesia

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
)

const (
	chunkBuffer = 32

	// generation_config.speed range (docs api-reference/tts/websocket: 0.6 to 1.5 inclusive).
	minSpeed = 0.6
	maxSpeed = 1.5

	// raw pcm_s16le mono at 24 kHz = 48000 bytes/s = 48 bytes/ms.
	bytesPerMs = 48

	ttsPath            = "/tts/websocket"
	defaultCancelGrace = 500 * time.Millisecond
	cancelWriteTimeout = 50 * time.Millisecond

	msgChunk = "chunk"
)

// TTS synthesises one sentence per context over a pool of persistent WebSocket connections
// (spec 8.4.14).
type TTS struct {
	cfg  Config
	hc   *http.Client
	log  *slog.Logger
	pool *pool
	seq  atomic.Uint64
}

var _ provider.TTSProvider = (*TTS)(nil)

// NewTTS builds the TTS adapter; APIKey is required.
func NewTTS(cfg Config, hc *http.Client, log *slog.Logger) (*TTS, error) {
	cfg = cfg.withDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if hc == nil {
		hc = httpx.NewClient()
	}
	if log == nil {
		log = slog.Default()
	}
	t := &TTS{cfg: cfg, hc: wsHTTPClient(hc), log: log}
	t.pool = newPool(t, cfg.PoolSize)
	return t, nil
}

// Name is CARTESIA.
func (*TTS) Name() provider.Name { return provider.Cartesia }

// Close closes the idle pooled connections; connections of calls in flight close when those end.
func (t *TTS) Close() error {
	t.pool.close()
	return nil
}

func (t *TTS) scrub(v string) string { return scrubKey(v, t.cfg.APIKey) }

func (t *TTS) tap(dir string, text bool, data []byte) {
	if t.cfg.Tap != nil {
		t.cfg.Tap(dir, text, data)
	}
}

type outputFormat struct {
	Container  string `json:"container"`
	Encoding   string `json:"encoding"`
	SampleRate int    `json:"sample_rate"`
}

type generationConfig struct {
	Speed float64 `json:"speed"` // the only generation setting sent (D-014: no volume/emotion)
}

// generateMsg is the one message sent per sentence. No `locale` (only one of language/locale
// may be set), no previous_text, no pronunciation dictionary.
type generateMsg struct {
	ModelID          string           `json:"model_id"`
	Transcript       string           `json:"transcript"`
	Voice            string           `json:"voice"`
	Language         string           `json:"language,omitempty"`
	ContextID        string           `json:"context_id"`
	Continue         bool             `json:"continue"`
	OutputFormat     outputFormat     `json:"output_format"`
	GenerationConfig generationConfig `json:"generation_config"`
}

// clampSpeed maps SpeakingRate to generation_config.speed: 0 means 1.0, otherwise clamped to
// the vendor range.
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

func (t *TTS) contextID(req provider.TTSRequest) string {
	if req.RequestID != "" {
		return req.RequestID
	}
	return "ctx-" + strconv.FormatUint(t.seq.Add(1), 10)
}

// Synthesize returns once the generate message was written; failures before that (no slot,
// handshake refused) are *provider.Error, later ones (vendor `error`, dropped socket) arrive as
// the last chunk's Err. Only VoiceID, Model, SpeakingRate, Text, Language and RequestID of req
// are read (8.4.12); the model is sent exactly as given.
func (t *TTS) Synthesize(ctx context.Context, req provider.TTSRequest) (provider.TTSStream, error) {
	id := t.contextID(req)
	raw, err := json.Marshal(generateMsg{
		ModelID: req.Model, Transcript: req.Text, Voice: req.VoiceID, Language: req.Language, ContextID: id,
		OutputFormat:     outputFormat{Container: "raw", Encoding: "pcm_s16le", SampleRate: 24000},
		GenerationConfig: generationConfig{Speed: clampSpeed(req.SpeakingRate)},
	})
	if err != nil {
		return nil, provider.NewError(provider.Cartesia, provider.CapTTS, provider.KindInvalidRequest, "request could not be encoded")
	}
	ctx, span := httpx.StartSpan(ctx, provider.CapTTS, provider.Cartesia, req.Model)
	wc, err := t.pool.acquire(ctx)
	if err != nil {
		span.End(outcomeOf(err), err)
		t.logFailure(req, err)
		return nil, err
	}
	cl := newCall(id)
	wc.setActive(cl)
	if err = wc.write(ctx, raw); err != nil && wc.reused && ctx.Err() == nil {
		// the idle connection went stale: one transparent retry on a fresh one
		if wc, err = t.pool.replace(ctx, wc); err == nil {
			wc.setActive(cl)
			err = wc.write(ctx, raw)
		}
	}
	if err != nil {
		if wc != nil {
			t.pool.release(wc, false)
		}
		pe := t.writeError(ctx, err)
		cl.finish()
		span.End(outcomeOf(pe), pe)
		t.logFailure(req, pe)
		return nil, pe
	}
	sctx, cancel := context.WithCancel(ctx)
	st := &ttsStream{ch: make(chan provider.TTSChunk, chunkBuffer), cancel: cancel}
	r := &run{t: t, req: req, raw: raw, wc: wc, cl: cl, st: st, ctx: sctx, span: span}
	go r.loop()
	return st, nil
}

func (t *TTS) writeError(ctx context.Context, err error) *provider.Error {
	var pe *provider.Error
	if errors.As(err, &pe) {
		return pe
	}
	if cerr := ctx.Err(); cerr != nil {
		return httpx.ClassifyError(provider.Cartesia, provider.CapTTS, cerr)
	}
	e := provider.NewError(provider.Cartesia, provider.CapTTS, provider.KindUnavailable, "request could not be sent")
	e.Retryable = true
	e.Cause = err
	return e
}

func outcomeOf(err error) string {
	if provider.KindOf(err) == provider.KindCancelled {
		return httpx.OutcomeCancelled
	}
	return httpx.OutcomeError
}

// logFailure logs identifiers and classification only: never keys, text or audio.
func (t *TTS) logFailure(req provider.TTSRequest, err error) {
	if err == nil || provider.KindOf(err) == provider.KindCancelled {
		return
	}
	attrs := []any{"provider", string(provider.Cartesia), "capability", string(provider.CapTTS), "model", req.Model,
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

// Close cancels the generation; idempotent.
func (s *ttsStream) Close() error {
	s.once.Do(s.cancel)
	return nil
}

// run is the goroutine of one generation.
type run struct {
	t    *TTS
	req  provider.TTSRequest
	raw  []byte
	wc   *wsConn
	cl   *call
	st   *ttsStream
	ctx  context.Context //nolint:containedctx // generation lifetime
	span *httpx.Span

	carry []byte // odd byte held back at a chunk boundary
	total int64
	got   bool // any message of this context arrived
}

func (r *run) send(c provider.TTSChunk) bool {
	c.Provider, c.Model, c.ReceivedAt = provider.Cartesia, r.req.Model, time.Now()
	if r.ctx.Err() != nil { // cancellation wins over a ready channel slot: no chunk after cancel
		return false
	}
	select {
	case r.st.ch <- c:
		return true
	case <-r.ctx.Done():
		return false
	}
}

func (r *run) loop() {
	defer r.st.cancel()
	for {
		if r.ctx.Err() != nil {
			r.cancelled(r.ctx)
			return
		}
		select {
		case ev := <-r.cl.events:
			if r.handle(ev) {
				return
			}
		case <-r.ctx.Done():
			r.cancelled(r.ctx)
			return
		}
	}
}

// handle processes one event; true ends the generation.
func (r *run) handle(ev event) bool {
	if ev.err != nil {
		return r.connLost(ev.err)
	}
	r.got = true
	m := ev.msg
	switch m.Type {
	case msgChunk:
		return !r.audio(m.Data)
	case msgDone:
		r.finishOK()
		return true
	case msgError:
		e := mapError(provider.CapTTS, m.vendorError, r.t.scrub)
		r.fail(e, true)
		return true
	}
	return false // flush_done, timestamps, phoneme_timestamps: not used
}

// audio forwards one chunk; false means the consumer left (cancelled).
func (r *run) audio(b64 string) bool {
	pcm, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		e := httpx.ClassifyBody(provider.Cartesia, provider.CapTTS, err)
		r.fail(e, false)
		return false
	}
	data := append(append(make([]byte, 0, len(r.carry)+len(pcm)), r.carry...), pcm...)
	r.carry = r.carry[:0]
	if len(data)%2 == 1 {
		r.carry = append(r.carry, data[len(data)-1])
		data = data[:len(data)-1]
	}
	if len(data) == 0 { // an empty vendor frame (or one lone odd byte) yields no chunk
		return true
	}
	r.total += int64(len(data))
	if !r.send(provider.TTSChunk{PCM: data}) {
		r.cancelled(r.ctx)
		return false
	}
	return true
}

func (r *run) finishOK() {
	usage := &provider.TTSUsage{Characters: utf8.RuneCountInString(r.req.Text), AudioMs: r.total / bytesPerMs}
	if r.send(provider.TTSChunk{Final: true, Usage: usage}) {
		r.span.End(httpx.OutcomeSuccess, nil)
	} else {
		r.span.End(httpx.OutcomeCancelled, nil)
	}
	r.end(true)
}

// fail emits the error chunk and ends the generation; reuse says whether the connection is still
// good (a vendor `error` ends only its context).
func (r *run) fail(e *provider.Error, reuse bool) {
	r.t.logFailure(r.req, e)
	if r.send(provider.TTSChunk{Err: e}) {
		r.span.End(httpx.OutcomeError, e)
	} else {
		r.span.End(httpx.OutcomeCancelled, nil)
	}
	r.end(reuse)
}

func (r *run) end(reuse bool) {
	r.cl.finish()
	r.t.pool.release(r.wc, reuse)
	close(r.st.ch)
}

// connLost handles a dead connection: before any message of this context arrived on a reused
// connection it is a stale idle socket and the sentence is retried once on a fresh one.
func (r *run) connLost(err error) bool {
	if !r.got && r.wc.reused && r.ctx.Err() == nil {
		if wc, rerr := r.t.pool.replace(r.ctx, r.wc); rerr == nil {
			wc.setActive(r.cl)
			if werr := wc.write(r.ctx, r.raw); werr == nil {
				r.wc = wc
				return false
			}
			r.wc = wc
		} else {
			r.wc = nil
			r.failLost(err)
			return true
		}
	}
	r.failLost(err)
	return true
}

func (r *run) failLost(err error) {
	code := int(websocket.CloseStatus(err))
	if code < 0 {
		code = 1006
	}
	e := httpx.ClassifyWSClose(provider.Cartesia, provider.CapTTS, code)
	e.Message = "audio stream interrupted"
	e.Cause = err
	r.t.logFailure(r.req, e)
	if r.send(provider.TTSChunk{Err: e}) {
		r.span.End(httpx.OutcomeError, e)
	} else {
		r.span.End(httpx.OutcomeCancelled, nil)
	}
	r.cl.finish()
	if r.wc != nil { // nil: the replacement dial failed and already gave the slot back
		r.t.pool.release(r.wc, false)
	}
	close(r.st.ch)
}

// cancelled ends the stream at once (no chunk follows), then tells the vendor to stop and keeps
// the connection only if the context ends within CancelGrace: the docs say a generation already
// running "will continue sending responses until completion", so the connection is drained, not
// assumed idle.
func (r *run) cancelled(ctx context.Context) {
	r.span.End(httpx.OutcomeCancelled, nil)
	close(r.st.ch)
	wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), cancelWriteTimeout)
	msg, _ := json.Marshal(map[string]any{"context_id": r.cl.id, "cancel": true})
	werr := r.wc.write(wctx, msg)
	wcancel()
	if werr != nil {
		r.cl.finish()
		r.t.pool.release(r.wc, false)
		return
	}
	grace := r.t.cfg.CancelGrace
	if grace <= 0 {
		grace = defaultCancelGrace
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	for {
		select {
		case ev := <-r.cl.events:
			if ev.err != nil {
				r.cl.finish()
				r.t.pool.release(r.wc, false)
				return
			}
			if ev.msg.Type == msgDone || ev.msg.Type == msgError {
				r.cl.finish()
				r.t.pool.release(r.wc, true)
				return
			}
		case <-timer.C:
			r.cl.finish()
			r.t.pool.release(r.wc, false)
			return
		}
	}
}
