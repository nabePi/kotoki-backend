package openai

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
)

const (
	sttInRate                  = provider.STTSampleRate
	sttOutRate                 = 24000
	sttBatchBytes              = 1280 // 2 x 20 ms at 16 kHz
	sttMaxQueuedBytes          = 64000
	sttBytesPerMs              = sttInRate * 2 / 1000
	defaultSyntheticFinalDelay = 400 * time.Millisecond
	sttCloseWait               = 500 * time.Millisecond
	sttReadLimit               = 1 << 20
)

// STT streams audio to the Realtime API in transcription mode (spec 8.4.2).
type STT struct {
	cfg Config
	hc  *http.Client // used for the WebSocket handshake (HTTP/1.1 only)
	log *slog.Logger
}

var _ provider.STTProvider = (*STT)(nil)

// NewSTT builds the STT adapter; APIKey, WSBaseURL and STTModel are required.
func NewSTT(cfg Config, hc *http.Client, log *slog.Logger) (*STT, error) {
	switch {
	case cfg.APIKey == "":
		return nil, errors.New("openai: APIKey is empty")
	case cfg.WSBaseURL == "":
		return nil, errors.New("openai: WSBaseURL is empty")
	case cfg.STTModel == "":
		return nil, errors.New("openai: STTModel is empty")
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

// Name is OPENAI.
func (*STT) Name() provider.Name { return provider.OpenAI }

// Model is the configured transcription model.
func (s *STT) Model() string { return s.cfg.STTModel }

type sttOp struct {
	pcm      []byte
	finalize bool
}

type msSpan struct{ start, end int64 }

// commitState is one input_audio_buffer.commit; answered once a completed event or the
// synthetic FINAL took it.
type commitState struct {
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
	finishOnce             sync.Once

	mu             sync.Mutex
	closed         bool
	userClosed     bool
	abortErr       *provider.Error
	ops            []sttOp
	queuedBytes    int
	acceptedBytes  int64
	committedBytes int64
	audioSent      int64 // bytes of 16 kHz audio written to the socket
	segStartMs     int64
	commits        []*commitState // one per commit, in send order
	synthOwed      int            // synthetic FINALs whose real completed event is still to come
	owed           []msSpan
	timers         []*time.Timer
	partials       map[string]string
	order          []string
	ready          map[string]string
	openedAt       time.Time
	firstEventAt   time.Time
}

// Open dials, sends session.update and returns once the vendor confirmed it. ctx is the
// session lifetime: cancelling it closes the connection.
func (s *STT) Open(ctx context.Context, cfg provider.STTSessionConfig) (provider.STTSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, httpx.ClassifyError(provider.OpenAI, provider.CapSTT, err)
	}
	ctx, span := httpx.StartSpan(ctx, provider.CapSTT, provider.OpenAI, s.cfg.STTModel)
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

func (s *STT) scrub(v string) string { return strings.ReplaceAll(v, s.cfg.APIKey, "[redacted]") }

func (s *STT) logFailure(err error) {
	if err == nil || provider.KindOf(err) == provider.KindCancelled {
		return
	}
	attrs := []any{"provider", string(provider.OpenAI), "capability", string(provider.CapSTT), "model", s.cfg.STTModel,
		"kind", string(provider.KindOf(err))}
	var pe *provider.Error
	if errors.As(err, &pe) {
		attrs = append(attrs, "http_status", pe.HTTPStatus, "vendor_code", pe.VendorCode)
	}
	s.log.Warn("provider call failed", attrs...)
}

func (s *STT) endpoint() (string, error) {
	u, err := url.Parse(s.cfg.WSBaseURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if q.Get("intent") == "" && q.Get("model") == "" {
		q.Set("intent", "transcription") // UNVERIFIED query (spec 8.4.2, deviations M9-T4)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (s *STT) sessionUpdate(cfg provider.STTSessionConfig) []byte {
	lang := cfg.Language
	if lang == "" {
		lang = "ja"
	}
	tr := map[string]any{"model": s.cfg.STTModel}
	if s.cfg.STTSingleLanguage {
		tr["language"] = lang
	} else {
		tr["languages"] = []string{lang}
	}
	if kw := sanitizeKeywords(cfg.Keyterms); len(kw) > 0 {
		tr["keywords"] = kw
	}
	b, _ := json.Marshal(map[string]any{"type": "session.update", "session": map[string]any{
		"type": "transcription",
		"audio": map[string]any{"input": map[string]any{
			"format":         map[string]any{"type": "audio/pcm", "rate": sttOutRate},
			"transcription":  tr,
			"turn_detection": nil,
		}},
	}})
	return b
}

// sanitizeKeywords drops keywords the vendor rejects (< > CR LF, documented) and empties.
func sanitizeKeywords(in []string) []string {
	var out []string
	for _, k := range in {
		k = strings.TrimSpace(k)
		if k == "" || strings.ContainsAny(k, "<>\r\n") {
			continue
		}
		out = append(out, k)
	}
	return out
}

func (s *STT) tap(dir string, text bool, data []byte) {
	if s.cfg.Tap != nil {
		s.cfg.Tap(dir, text, data)
	}
}

func (s *STT) open(ctx context.Context, cfg provider.STTSessionConfig) (*sttSession, error) {
	target, err := s.endpoint()
	if err != nil {
		return nil, provider.NewError(provider.OpenAI, provider.CapSTT, provider.KindInvalidRequest, "WSBaseURL is not a valid URL")
	}
	sctx, cancel := context.WithCancel(ctx)
	conn, resp, err := websocket.Dial(sctx, target, &websocket.DialOptions{
		HTTPClient: s.hc,
		HTTPHeader: http.Header{"Authorization": {"Bearer " + s.cfg.APIKey}},
	})
	if err != nil {
		cancel()
		return nil, s.dialError(ctx, resp, err)
	}
	conn.SetReadLimit(sttReadLimit)
	fail := func(e error) (*sttSession, error) {
		cancel()
		_ = conn.CloseNow()
		return nil, e
	}
	upd := s.sessionUpdate(cfg)
	s.tap("send", true, upd)
	if err := conn.Write(sctx, websocket.MessageText, upd); err != nil {
		return fail(s.ctxOr(ctx, err))
	}
	for confirmed := false; !confirmed; {
		typ, data, err := conn.Read(sctx)
		if err != nil {
			return fail(s.ctxOr(ctx, err))
		}
		s.tap("recv", typ == websocket.MessageText, data)
		var ev rtEvent
		if typ != websocket.MessageText || json.Unmarshal(data, &ev) != nil {
			continue
		}
		switch ev.Type {
		case evtSessionUpdated:
			confirmed = true
		case evtError:
			return fail(sttError(ev.Error, s.scrub))
		}
	}
	se := &sttSession{
		s: s, conn: conn, ctx: sctx, cancel: cancel, q: newEventQueue(), wake: make(chan struct{}, 1),
		readerDone: make(chan struct{}), writerDone: make(chan struct{}),
		partials: map[string]string{}, ready: map[string]string{}, openedAt: time.Now(),
	}
	return se, nil
}

// ctxOr maps a transport error, preferring the caller's context state (cancel/deadline).
func (s *STT) ctxOr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return httpx.ClassifyError(provider.OpenAI, provider.CapSTT, cerr)
	}
	e := httpx.ClassifyError(provider.OpenAI, provider.CapSTT, err)
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
		e := classify(provider.CapSTT, resp, body)
		e.Message = s.scrub(e.Message)
		e.VendorCode = s.scrub(e.VendorCode)
		return e
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
		return provider.NewError(provider.OpenAI, provider.CapSTT, provider.KindInvalidRequest, "audio length must be even")
	}
	se.mu.Lock()
	if se.closed {
		se.mu.Unlock()
		return provider.ErrSessionClosed
	}
	if len(pcm) == 0 {
		se.mu.Unlock()
		return nil
	}
	if se.queuedBytes+len(pcm) > sttMaxQueuedBytes {
		se.mu.Unlock()
		e := provider.NewError(provider.OpenAI, provider.CapSTT, provider.KindUnavailable, "audio buffer full")
		e.Retryable = true
		return e
	}
	if n := len(se.ops); n > 0 && !se.ops[n-1].finalize {
		se.ops[n-1].pcm = append(se.ops[n-1].pcm, pcm...)
	} else {
		se.ops = append(se.ops, sttOp{pcm: append([]byte(nil), pcm...)})
	}
	se.queuedBytes += len(pcm)
	se.acceptedBytes += int64(len(pcm))
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

// Finalize commits what was sent; exactly one FINAL{FromFinalize:true} follows.
func (se *sttSession) Finalize() error {
	se.mu.Lock()
	if se.closed {
		se.mu.Unlock()
		return provider.ErrSessionClosed
	}
	if se.acceptedBytes == se.committedBytes { // nothing since the last commit: the vendor would reject it
		now := se.audioSent / sttBytesPerMs
		se.emitLocked(provider.STTEvent{Type: provider.STTFinal, FromFinalize: true, StartMs: se.segStartMs, EndMs: now})
		se.mu.Unlock()
		return nil
	}
	se.committedBytes = se.acceptedBytes
	se.ops = append(se.ops, sttOp{finalize: true})
	se.mu.Unlock()
	se.signal()
	return nil
}

// Close is idempotent and waits at most 500 ms for the goroutines.
func (se *sttSession) Close() error {
	se.mu.Lock()
	se.userClosed = true
	se.closed = true
	se.mu.Unlock()
	se.cancel()
	t := time.NewTimer(sttCloseWait)
	defer t.Stop()
	for _, ch := range []chan struct{}{se.readerDone, se.writerDone} {
		select {
		case <-ch:
		case <-t.C:
			_ = se.conn.CloseNow()
			return nil
		}
	}
	return nil
}

func (se *sttSession) write(data []byte) error {
	se.s.tap("send", true, data)
	return se.conn.Write(se.ctx, websocket.MessageText, data)
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
	e := httpx.ClassifyError(provider.OpenAI, provider.CapSTT, err)
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
			select {
			case <-se.wake:
				continue
			case <-se.ctx.Done():
				return
			}
		}
		for _, op := range ops {
			if se.ctx.Err() != nil {
				return
			}
			if op.finalize {
				if len(batch) > 0 {
					if !se.sendAppend(batch) {
						return
					}
					batch = nil
				}
				if !se.sendCommit() {
					return
				}
				continue
			}
			batch = append(batch, op.pcm...)
			for len(batch) >= sttBatchBytes {
				if !se.sendAppend(batch[:sttBatchBytes]) {
					return
				}
				batch = batch[sttBatchBytes:]
			}
		}
	}
}

func (se *sttSession) sendAppend(pcm16k []byte) bool {
	up := provider.ResamplePCM16(pcm16k, sttInRate, sttOutRate)
	msg, _ := json.Marshal(map[string]string{"type": "input_audio_buffer.append", "audio": base64.StdEncoding.EncodeToString(up)})
	if err := se.write(msg); err != nil {
		se.writeFailed(err)
		return false
	}
	se.mu.Lock()
	se.audioSent += int64(len(pcm16k))
	se.mu.Unlock()
	return true
}

func (se *sttSession) sendCommit() bool {
	se.mu.Lock()
	now := se.audioSent / sttBytesPerMs
	se.commits = append(se.commits, &commitState{span: msSpan{se.segStartMs, now}})
	n := len(se.commits)
	se.segStartMs = now
	se.mu.Unlock()
	if err := se.write([]byte(`{"type":"input_audio_buffer.commit"}`)); err != nil {
		se.writeFailed(err)
		return false
	}
	d := se.s.cfg.SyntheticFinalDelay
	if d <= 0 {
		d = defaultSyntheticFinalDelay
	}
	se.mu.Lock()
	if !se.closed {
		se.timers = append(se.timers, time.AfterFunc(d, func() { se.syntheticFinal(n) }))
	}
	se.mu.Unlock()
	return true
}

// syntheticFinal answers commit n with an empty FINAL when the vendor stayed silent.
func (se *sttSession) syntheticFinal(n int) {
	se.mu.Lock()
	defer se.mu.Unlock()
	if se.closed || se.commits[n-1].answered {
		return
	}
	c := se.commits[n-1]
	c.answered = true
	se.synthOwed++
	se.owed = append(se.owed, c.span)
	se.emitLocked(provider.STTEvent{Type: provider.STTFinal, FromFinalize: true, StartMs: c.span.start, EndMs: c.span.end})
}

// oldestPendingLocked is the oldest commit nobody answered yet.
func (se *sttSession) oldestPendingLocked() *commitState {
	for _, c := range se.commits {
		if !c.answered {
			return c
		}
	}
	return nil
}

func (se *sttSession) popOwed() msSpan {
	if len(se.owed) == 0 {
		return msSpan{se.segStartMs, se.audioSent / sttBytesPerMs}
	}
	sp := se.owed[0]
	se.owed = se.owed[1:]
	return sp
}

// curSpanLocked is the audio span of the oldest unanswered commit, else the running segment.
func (se *sttSession) curSpanLocked() msSpan {
	if c := se.oldestPendingLocked(); c != nil {
		return c.span
	}
	return msSpan{se.segStartMs, se.audioSent / sttBytesPerMs}
}

func (se *sttSession) emitLocked(ev provider.STTEvent) {
	now := time.Now()
	ev.ReceivedAt = now
	ev.Provider = provider.OpenAI
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
		var ev rtEvent
		if json.Unmarshal(data, &ev) != nil {
			continue
		}
		if perr := se.handle(ev); perr != nil {
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
	return httpx.ClassifyWSClose(provider.OpenAI, provider.CapSTT, code)
}

func (se *sttSession) handle(ev rtEvent) *provider.Error {
	switch ev.Type {
	case evtError:
		return sttError(ev.Error, se.s.scrub)
	case evtCommitted:
		se.mu.Lock()
		se.order = append(se.order, ev.ItemID)
		se.mu.Unlock()
	case evtDelta:
		se.mu.Lock()
		buf := se.partials[ev.ItemID] + ev.Delta
		se.partials[ev.ItemID] = buf
		if text := provider.NormalizeJA(buf); text != "" {
			sp := se.curSpanLocked()
			se.emitLocked(provider.STTEvent{Type: provider.STTPartial, Text: text, StartMs: sp.start, EndMs: sp.end})
		}
		se.mu.Unlock()
	case evtCompleted:
		se.mu.Lock()
		delete(se.partials, ev.ItemID)
		se.completedLocked(ev.ItemID, ev.Transcript)
		se.mu.Unlock()
	}
	return nil
}

// completedLocked delivers transcripts in commit order (items that were never announced by a
// committed event go out as they arrive).
func (se *sttSession) completedLocked(item, text string) {
	idx := -1
	for i, id := range se.order {
		if id == item {
			idx = i
			break
		}
	}
	if idx < 0 {
		se.finalLocked(text)
		return
	}
	se.ready[item] = text
	for len(se.order) > 0 {
		t, ok := se.ready[se.order[0]]
		if !ok {
			return
		}
		delete(se.ready, se.order[0])
		se.order = se.order[1:]
		se.finalLocked(t)
	}
}

func (se *sttSession) finalLocked(transcript string) {
	text := provider.NormalizeJA(transcript)
	var from bool
	var sp msSpan
	c := se.oldestPendingLocked()
	switch {
	case se.synthOwed > 0: // late answer to a commit already answered synthetically
		se.synthOwed--
		sp = se.popOwed()
	case c != nil:
		c.answered = true
		from = true
		sp = c.span
	default:
		sp = msSpan{se.segStartMs, se.audioSent / sttBytesPerMs}
	}
	if !from && text == "" {
		return
	}
	se.emitLocked(provider.STTEvent{Type: provider.STTFinal, Text: text, FromFinalize: from, StartMs: sp.start, EndMs: sp.end})
}

// finish ends the session exactly once: optional ERROR, then CLOSED, then the channel closes.
func (se *sttSession) finish(err *provider.Error) {
	se.finishOnce.Do(func() {
		se.mu.Lock()
		se.closed = true
		if se.abortErr != nil {
			err = se.abortErr
		}
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
