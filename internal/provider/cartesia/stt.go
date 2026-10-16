package cartesia

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
)

const (
	sttRate                    = provider.STTSampleRate
	sttBatchBytes              = 3200 // 5 x 20 ms at 16 kHz
	sttMaxQueuedBytes          = 64000
	sttBytesPerMs              = sttRate * 2 / 1000
	sttKeepAliveBytes          = 3200 // 100 ms of silence
	defaultSyntheticFinalDelay = 400 * time.Millisecond
	sttCloseWait               = 500 * time.Millisecond
	sttCloseAckWait            = 200 * time.Millisecond
	sttReadLimit               = 1 << 20
	sttPath                    = "/stt/websocket"
	maxErrorBody               = 64 << 10

	maxKeyterms     = 100
	maxKeytermChars = 1200

	// whisperMarker identifies the model family that takes `language` and has no keyterm
	// support (docs: language/min_volume/max_silence_duration_secs are "ink-whisper only",
	// keyterm is "ink-2 and ink-preview only").
	whisperMarker = "whisper"

	textFinalize = "finalize"
	textClose    = "close"
)

// STT streams audio to Cartesia Ink realtime (spec 8.4.6).
type STT struct {
	cfg Config
	hc  *http.Client // used for the WebSocket handshake (HTTP/1.1 only)
	log *slog.Logger
}

var _ provider.STTProvider = (*STT)(nil)

// NewSTT builds the STT adapter; APIKey and STTModel are required.
func NewSTT(cfg Config, hc *http.Client, log *slog.Logger) (*STT, error) {
	cfg = cfg.withDefaults()
	if err := cfg.validateSTT(); err != nil {
		return nil, err
	}
	if hc == nil {
		hc = httpx.NewClient()
	}
	if log == nil {
		log = slog.Default()
	}
	return &STT{cfg: cfg, hc: wsHTTPClient(hc), log: log}, nil
}

// wsHTTPClient returns a client that never negotiates HTTP/2: the WebSocket upgrade only
// exists in HTTP/1.1.
func wsHTTPClient(hc *http.Client) *http.Client {
	tr, ok := hc.Transport.(*http.Transport)
	if !ok {
		return hc
	}
	c := tr.Clone()
	c.ForceAttemptHTTP2 = false
	c.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	cp := *hc
	cp.Transport = c
	return &cp
}

// Name is CARTESIA.
func (*STT) Name() provider.Name { return provider.Cartesia }

// Model is the configured transcription model.
func (s *STT) Model() string { return s.cfg.STTModel }

type sttOp struct {
	pcm      []byte
	finalize bool
}

type msSpan struct{ start, end int64 }

// finState is one finalize; answered once flush_done (or the synthetic FINAL) took it.
type finState struct {
	span     msSpan
	answered bool
}

type sttSession struct {
	s    *STT
	conn *websocket.Conn
	ctx  context.Context
	//nolint:containedctx // session lifetime
	cancel context.CancelFunc
	q      *eventQueue
	span   *httpx.Span
	wake   chan struct{}

	readerDone, writerDone chan struct{}
	finishOnce, closeOnce  sync.Once

	mu           sync.Mutex
	closed       bool
	userClosed   bool
	failed       bool // finish emitted an ERROR: the vendor (or the wire) ended the session
	abortErr     *provider.Error
	ops          []sttOp
	queuedBytes  int
	audioSent    int64 // bytes of real audio accepted by SendAudio (silence excluded)
	segStartMs   int64
	fins         []*finState // one per Finalize, in send order
	finSent      int         // finalize frames written
	synthOwed    int         // synthetic FINALs whose flush_done is still to come
	finalText    strings.Builder
	finalWords   []provider.STTWord
	timers       []*time.Timer
	openedAt     time.Time
	firstEventAt time.Time
}

// Open dials and returns once the handshake succeeded (the vendor sends no ready message).
// ctx is the session lifetime: cancelling it closes the connection.
func (s *STT) Open(ctx context.Context, cfg provider.STTSessionConfig) (provider.STTSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, httpx.ClassifyError(provider.Cartesia, provider.CapSTT, err)
	}
	ctx, span := httpx.StartSpan(ctx, provider.CapSTT, provider.Cartesia, s.cfg.STTModel)
	se, err := s.open(ctx, cfg)
	if err != nil {
		span.End(spanOutcome(err), err)
		s.logFailure(err)
		return nil, err
	}
	se.span = span
	go se.run()
	return se, nil
}

func spanOutcome(err error) string {
	if provider.KindOf(err) == provider.KindCancelled {
		return httpx.OutcomeCancelled
	}
	return httpx.OutcomeError
}

func (s *STT) scrub(v string) string { return scrubKey(v, s.cfg.APIKey) }

func scrubKey(v, key string) string {
	if key == "" {
		return v
	}
	return strings.ReplaceAll(v, key, "[redacted]")
}

func (s *STT) logFailure(err error) {
	if err == nil || provider.KindOf(err) == provider.KindCancelled {
		return
	}
	attrs := []any{"provider", string(provider.Cartesia), "capability", string(provider.CapSTT), "model", s.cfg.STTModel,
		"kind", string(provider.KindOf(err))}
	var pe *provider.Error
	if errors.As(err, &pe) {
		attrs = append(attrs, "http_status", pe.HTTPStatus, "vendor_code", pe.VendorCode)
	}
	s.log.Warn("provider call failed", attrs...)
}

// keyterms limits the list to the documented bounds (100 terms, 1200 characters in all): terms
// that no longer fit are dropped, never cut in the middle.
func keyterms(in []string) []string {
	var out []string
	chars := 0
	for _, k := range in {
		n := utf8.RuneCountInString(k)
		if k == "" || len(out) >= maxKeyterms || chars+n > maxKeytermChars {
			continue
		}
		out = append(out, k)
		chars += n
	}
	return out
}

// queryEscape escapes a query value with %20 for spaces (the docs ask for %20 in keyterms).
func queryEscape(v string) string { return strings.ReplaceAll(url.QueryEscape(v), "+", "%20") }

func (s *STT) endpoint(cfg provider.STTSessionConfig) string {
	pairs := [][2]string{
		{"model", s.cfg.STTModel}, {"encoding", "pcm_s16le"}, {"sample_rate", "16000"}, {"cartesia_version", s.cfg.Version},
	}
	if strings.Contains(s.cfg.STTModel, whisperMarker) {
		lang := cfg.Language
		if lang == "" {
			lang = "ja"
		}
		pairs = append(pairs, [2]string{"language", lang})
	} else {
		for _, k := range keyterms(cfg.Keyterms) {
			pairs = append(pairs, [2]string{"keyterm", k})
		}
	}
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, p[0]+"="+queryEscape(p[1]))
	}
	return s.cfg.WSBaseURL + sttPath + "?" + strings.Join(parts, "&")
}

func (s *STT) tap(dir string, text bool, data []byte) {
	if s.cfg.Tap != nil {
		s.cfg.Tap(dir, text, data)
	}
}

func (s *STT) open(ctx context.Context, cfg provider.STTSessionConfig) (*sttSession, error) {
	sctx, cancel := context.WithCancel(ctx)
	conn, resp, err := websocket.Dial(sctx, s.endpoint(cfg), &websocket.DialOptions{
		HTTPClient: s.hc,
		HTTPHeader: http.Header{"X-API-Key": {s.cfg.APIKey}},
	})
	if err != nil {
		cancel()
		return nil, s.dialError(ctx, resp, err)
	}
	conn.SetReadLimit(sttReadLimit)
	return &sttSession{
		s: s, conn: conn, ctx: sctx, cancel: cancel, q: newEventQueue(), wake: make(chan struct{}, 1),
		readerDone: make(chan struct{}), writerDone: make(chan struct{}), openedAt: time.Now(),
	}, nil
}

// ctxOr maps a transport error, preferring the caller's context state (cancel/deadline).
func (s *STT) ctxOr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return httpx.ClassifyError(provider.Cartesia, provider.CapSTT, cerr)
	}
	e := httpx.ClassifyError(provider.Cartesia, provider.CapSTT, err)
	e.Message = s.scrub(e.Message)
	return e
}

func (s *STT) dialError(ctx context.Context, resp *http.Response, err error) error {
	if resp != nil && resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusSwitchingProtocols {
		var body []byte
		if resp.Body != nil {
			body, _ = io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
			_ = resp.Body.Close()
		}
		return classifyHTTP(provider.CapSTT, resp, body, s.scrub)
	}
	return s.ctxOr(ctx, err)
}

func (se *sttSession) run() {
	go se.writeLoop()
	go func() {
		defer close(se.readerDone)
		se.finish(se.readLoop())
	}()
}

// Events is buffered; see eventQueue.
func (se *sttSession) Events() <-chan provider.STTEvent { return se.q.out }

// Stats reports the audio sent (16 kHz sample basis) and timestamps.
func (se *sttSession) Stats() provider.STTStats {
	se.mu.Lock()
	defer se.mu.Unlock()
	return provider.STTStats{AudioMsSent: se.audioSent / sttBytesPerMs, OpenedAt: se.openedAt, FirstEventAt: se.firstEventAt}
}

// SendAudio queues PCM16LE mono 16 kHz; it never blocks and refuses more than 2 s of backlog.
func (se *sttSession) SendAudio(pcm []byte) error {
	if len(pcm)%2 != 0 {
		return provider.NewError(provider.Cartesia, provider.CapSTT, provider.KindInvalidRequest, "audio length must be even")
	}
	se.mu.Lock()
	if se.closed {
		gone := se.failed && !se.userClosed
		se.mu.Unlock()
		if gone { // the vendor ended the session: ERROR/CLOSED carry it; audio and Finalize are dropped
			return nil
		}
		return provider.ErrSessionClosed
	}
	if len(pcm) == 0 {
		se.mu.Unlock()
		return nil
	}
	if se.queuedBytes+len(pcm) > sttMaxQueuedBytes {
		se.mu.Unlock()
		e := provider.NewError(provider.Cartesia, provider.CapSTT, provider.KindUnavailable, "audio buffer full")
		e.Retryable = true
		return e
	}
	if n := len(se.ops); n > 0 && !se.ops[n-1].finalize {
		se.ops[n-1].pcm = append(se.ops[n-1].pcm, pcm...)
	} else {
		se.ops = append(se.ops, sttOp{pcm: append([]byte(nil), pcm...)})
	}
	se.queuedBytes += len(pcm)
	se.audioSent += int64(len(pcm))
	se.mu.Unlock()
	se.signal()
	return nil
}

func (se *sttSession) signal() {
	select {
	case se.wake <- struct{}{}:
	default:
	}
}

// Finalize asks the vendor to finalise what was sent; exactly one FINAL{FromFinalize:true}
// follows.
func (se *sttSession) Finalize() error {
	se.mu.Lock()
	if se.closed {
		gone := se.failed && !se.userClosed
		se.mu.Unlock()
		if gone { // the vendor ended the session: ERROR/CLOSED carry it; audio and Finalize are dropped
			return nil
		}
		return provider.ErrSessionClosed
	}
	now := se.audioSent / sttBytesPerMs
	se.fins = append(se.fins, &finState{span: msSpan{se.segStartMs, now}})
	se.ops = append(se.ops, sttOp{finalize: true})
	se.mu.Unlock()
	se.signal()
	return nil
}

// Close sends `close`, waits briefly for `done` (the vendor then ends the stream), and stops the session; idempotent, at most
// 500 ms.
func (se *sttSession) Close() error {
	se.closeOnce.Do(se.doClose)
	return nil
}

func (se *sttSession) doClose() {
	se.mu.Lock()
	se.userClosed = true
	se.closed = true
	se.mu.Unlock()
	t := time.NewTimer(sttCloseWait)
	defer t.Stop()
	if se.ctx.Err() == nil {
		wctx, wcancel := context.WithTimeout(context.Background(), sttCloseAckWait)
		if err := se.writeText(wctx, textClose); err == nil {
			select {
			case <-se.readerDone:
			case <-time.After(sttCloseAckWait):
			}
		}
		wcancel()
	}
	se.cancel()
	for _, ch := range []chan struct{}{se.readerDone, se.writerDone} {
		select {
		case <-ch:
		case <-t.C:
			_ = se.conn.CloseNow()
			return
		}
	}
}

func (se *sttSession) writeText(ctx context.Context, text string) error {
	se.s.tap("send", true, []byte(text))
	return se.conn.Write(ctx, websocket.MessageText, []byte(text))
}

func (se *sttSession) writeBinary(pcm []byte) error {
	se.s.tap("send", false, pcm)
	return se.conn.Write(se.ctx, websocket.MessageBinary, pcm)
}

func (se *sttSession) abort(err *provider.Error) {
	se.mu.Lock()
	if se.abortErr == nil {
		se.abortErr = err
	}
	se.mu.Unlock()
	se.cancel()
}

func (se *sttSession) writeFailed(err error) {
	if se.ctx.Err() != nil {
		return
	}
	e := httpx.ClassifyError(provider.Cartesia, provider.CapSTT, err)
	e.Retryable = true
	e.Kind = provider.KindUnavailable
	se.abort(e)
}

func (se *sttSession) writeLoop() {
	defer close(se.writerDone)
	var batch []byte
	for {
		se.mu.Lock()
		ops := se.ops
		se.ops, se.queuedBytes = nil, 0
		se.mu.Unlock()
		if len(ops) == 0 {
			if !se.idle() {
				return
			}
			continue
		}
		for _, op := range ops {
			if se.ctx.Err() != nil {
				return
			}
			if op.finalize {
				if !se.sendFinalize(batch) {
					return
				}
				batch = nil
				continue
			}
			batch = append(batch, op.pcm...)
			for len(batch) >= sttBatchBytes {
				if err := se.writeBinary(batch[:sttBatchBytes]); err != nil {
					se.writeFailed(err)
					return
				}
				batch = batch[sttBatchBytes:]
			}
		}
	}
}

// idle waits for work; with KeepAliveAfter set it sends 100 ms of silence after that much
// quiet. It returns false when the session ended.
func (se *sttSession) idle() bool {
	var ka <-chan time.Time
	if d := se.s.cfg.KeepAliveAfter; d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		ka = t.C
	}
	select {
	case <-se.wake:
		return true
	case <-ka:
		if err := se.writeBinary(make([]byte, sttKeepAliveBytes)); err != nil {
			se.writeFailed(err)
			return false
		}
		return true
	case <-se.ctx.Done():
		return false
	}
}

// sendFinalize flushes the held frames, then `finalize`, and arms the synthetic FINAL.
func (se *sttSession) sendFinalize(rest []byte) bool {
	if len(rest) > 0 {
		if err := se.writeBinary(rest); err != nil {
			se.writeFailed(err)
			return false
		}
	}
	if err := se.writeText(se.ctx, textFinalize); err != nil {
		se.writeFailed(err)
		return false
	}
	d := se.s.cfg.SyntheticFinalDelay
	if d <= 0 {
		d = defaultSyntheticFinalDelay
	}
	se.mu.Lock()
	defer se.mu.Unlock()
	if !se.closed {
		se.finSent++
		n := se.finSent
		se.timers = append(se.timers, time.AfterFunc(d, func() { se.syntheticFinal(n) }))
	}
	return true
}

// syntheticFinal answers finalize n when flush_done never came (safety net).
func (se *sttSession) syntheticFinal(n int) {
	se.mu.Lock()
	defer se.mu.Unlock()
	if se.closed || n > len(se.fins) || se.fins[n-1].answered {
		return
	}
	f := se.fins[n-1]
	f.answered = true
	se.synthOwed++
	se.emitFinalizeLocked(f)
}

// oldestPendingLocked is the oldest finalize nobody answered yet.
func (se *sttSession) oldestPendingLocked() *finState {
	for _, f := range se.fins {
		if !f.answered {
			return f
		}
	}
	return nil
}

// emitFinalizeLocked emits the FINAL answering f with the finals collected since the previous one.
func (se *sttSession) emitFinalizeLocked(f *finState) {
	text := provider.NormalizeJA(se.finalText.String())
	words := se.finalWords
	se.finalText.Reset()
	se.finalWords = nil
	se.segStartMs = f.span.end
	se.emitLocked(provider.STTEvent{Type: provider.STTFinal, Text: text, FromFinalize: true, Words: words, StartMs: f.span.start, EndMs: f.span.end})
}

func (se *sttSession) emitLocked(ev provider.STTEvent) {
	now := time.Now()
	ev.ReceivedAt = now
	ev.Provider = provider.Cartesia
	if se.firstEventAt.IsZero() {
		se.firstEventAt = now
	}
	se.q.push(ev)
}

func (se *sttSession) readLoop() *provider.Error {
	for {
		typ, data, err := se.conn.Read(se.ctx)
		if err != nil {
			return se.readError(err)
		}
		se.s.tap("recv", typ == websocket.MessageText, data)
		if typ != websocket.MessageText {
			continue
		}
		var m sttMessage
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if perr, stop := se.handle(m); stop {
			return perr
		}
	}
}

func (se *sttSession) readError(err error) *provider.Error {
	if se.ctx.Err() != nil {
		return nil // we (or the caller) closed it
	}
	code := int(websocket.CloseStatus(err))
	if code < 0 {
		code = 1006
	}
	return httpx.ClassifyWSClose(provider.Cartesia, provider.CapSTT, code)
}

// handle processes one server message; stop ends the read loop (with err for a vendor error,
// nil for the `done` acknowledgement).
func (se *sttSession) handle(m sttMessage) (*provider.Error, bool) {
	switch m.Type {
	case msgTranscript:
		se.mu.Lock()
		se.transcriptLocked(m)
		se.mu.Unlock()
	case msgFlushDone:
		se.mu.Lock()
		se.flushDoneLocked()
		se.mu.Unlock()
	case msgDone:
		return nil, true
	case msgError:
		return mapError(provider.CapSTT, m.vendorError, se.s.scrub), true
	}
	return nil, false
}

func (se *sttSession) transcriptLocked(m sttMessage) {
	now := se.audioSent / sttBytesPerMs
	if !m.IsFinal {
		if text := provider.NormalizeJA(m.Text); text != "" {
			se.emitLocked(provider.STTEvent{Type: provider.STTPartial, Text: text, StartMs: se.segStartMs, EndMs: now})
		}
		return
	}
	if se.oldestPendingLocked() != nil {
		// final text between finalize and flush_done: kept verbatim, no trimming or spacing
		se.finalText.WriteString(m.Text)
		se.finalWords = append(se.finalWords, toWords(m.Words)...)
		return
	}
	text := provider.NormalizeJA(m.Text)
	sp := msSpan{se.segStartMs, now}
	se.segStartMs = now
	if text == "" {
		return
	}
	se.emitLocked(provider.STTEvent{Type: provider.STTFinal, Text: text, Words: toWords(m.Words), StartMs: sp.start, EndMs: sp.end})
}

func (se *sttSession) flushDoneLocked() {
	if se.synthOwed > 0 { // the synthetic FINAL already answered this finalize
		se.synthOwed--
		return
	}
	f := se.oldestPendingLocked()
	if f == nil {
		return
	}
	f.answered = true
	se.emitFinalizeLocked(f)
}

// finish ends the session exactly once: optional ERROR, then CLOSED, then the channel closes.
func (se *sttSession) finish(err *provider.Error) {
	se.finishOnce.Do(func() {
		se.mu.Lock()
		se.closed = true
		if se.abortErr != nil {
			err = se.abortErr
		}
		se.failed = err != nil
		user := se.userClosed
		for _, t := range se.timers {
			t.Stop()
		}
		ctxDone := se.ctx.Err() != nil
		switch {
		case err != nil:
			se.span.End(httpx.OutcomeError, err)
			se.s.logFailure(err)
		case user:
			se.span.End(httpx.OutcomeSuccess, nil)
		default:
			se.span.End(httpx.OutcomeCancelled, nil)
		}
		if err != nil {
			se.emitLocked(provider.STTEvent{Type: provider.STTError, Err: err})
		}
		se.emitLocked(provider.STTEvent{Type: provider.STTClosed})
		se.mu.Unlock()
		se.cancel()
		_ = se.conn.CloseNow()
		se.q.close()
		if ctxDone || user {
			se.q.abandon()
		}
	})
}
