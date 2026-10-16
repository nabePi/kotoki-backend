package elevenlabs

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
	sttRate                    = provider.STTSampleRate
	sttBatchBytes              = 1280 // 2 x 20 ms at 16 kHz
	sttSilenceBytes            = 640  // 20 ms of silence, carries a commit when nothing is buffered
	sttMaxQueuedBytes          = 64000
	sttBytesPerMs              = sttRate * 2 / 1000
	defaultSyntheticFinalDelay = 400 * time.Millisecond
	sttCloseWait               = 500 * time.Millisecond
	sttReadLimit               = 1 << 20
	sttPath                    = "/v1/speech-to-text/realtime"
	defaultKeepAliveAfter      = 5 * time.Second
	sttKeepAliveBytes          = 3200 // 100 ms of silence
)

// STT streams audio to Scribe realtime with manual commits (spec 8.4.1).
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
		return nil, errors.New("elevenlabs: APIKey is empty")
	case cfg.WSBaseURL == "":
		return nil, errors.New("elevenlabs: WSBaseURL is empty")
	case cfg.STTModel == "":
		return nil, errors.New("elevenlabs: STTModel is empty")
	}
	if hc == nil {
		hc = httpx.NewClient()
	}
	if log == nil {
		log = slog.Default()
	}
	cfg.WSBaseURL = strings.TrimRight(cfg.WSBaseURL, "/")
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

// Name is ELEVENLABS.
func (*STT) Name() provider.Name { return provider.ElevenLabs }

// Model is the configured transcription model.
func (s *STT) Model() string { return s.cfg.STTModel }

type sttOp struct {
	pcm      []byte
	finalize bool
}

type msSpan struct{ start, end int64 }

// commitState is one commit=true chunk; answered once a committed_transcript or the synthetic
// FINAL took it.
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

	mu           sync.Mutex
	closed       bool
	userClosed   bool
	abortErr     *provider.Error
	ops          []sttOp
	queuedBytes  int
	audioSent    int64 // bytes of real audio written to the socket (silence excluded)
	segStartMs   int64
	commits      []*commitState // one per commit, in send order
	synthOwed    int            // synthetic FINALs whose real committed_transcript is still to come
	owed         []msSpan
	timers       []*time.Timer
	openedAt     time.Time
	firstEventAt time.Time
}

// Open dials, waits for session_started and returns. ctx is the session lifetime: cancelling
// it closes the connection.
func (s *STT) Open(ctx context.Context, cfg provider.STTSessionConfig) (provider.STTSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, httpx.ClassifyError(provider.ElevenLabs, provider.CapSTT, err)
	}
	ctx, span := httpx.StartSpan(ctx, provider.CapSTT, provider.ElevenLabs, s.cfg.STTModel)
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
	attrs := []any{"provider", string(provider.ElevenLabs), "capability", string(provider.CapSTT), "model", s.cfg.STTModel,
		"kind", string(provider.KindOf(err))}
	var pe *provider.Error
	if errors.As(err, &pe) {
		attrs = append(attrs, "http_status", pe.HTTPStatus, "vendor_code", pe.VendorCode)
	}
	s.log.Warn("provider call failed", attrs...)
}

func (s *STT) endpoint(cfg provider.STTSessionConfig) string {
	lang := cfg.Language
	if lang == "" {
		lang = "ja"
	}
	q := url.Values{
		"model_id":           {s.cfg.STTModel},
		"language_code":      {lang},
		"audio_format":       {"pcm_16000"},
		"commit_strategy":    {"manual"},
		"include_timestamps": {"false"},
		"enable_logging":     {"false"},
	}
	return s.cfg.WSBaseURL + sttPath + "?" + q.Encode()
}

// keepAliveAfter is how long the session may stay silent before 100 ms of silence is sent
// (the server closes after 15 s without client messages, spec 8.4.1).
func (s *STT) keepAliveAfter() time.Duration {
	if s.cfg.KeepAliveAfter > 0 {
		return s.cfg.KeepAliveAfter
	}
	return defaultKeepAliveAfter
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
		HTTPHeader: http.Header{"xi-api-key": {s.cfg.APIKey}},
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
	for started := false; !started; {
		typ, data, err := conn.Read(sctx)
		if err != nil {
			return fail(s.ctxOr(ctx, err))
		}
		s.tap("recv", typ == websocket.MessageText, data)
		var m rtMessage
		if typ != websocket.MessageText || json.Unmarshal(data, &m) != nil {
			continue
		}
		switch {
		case m.MessageType == msgSessionStarted:
			started = true
		case errorTypes[m.MessageType]:
			return fail(sttError(m, s.scrub))
		}
	}
	return &sttSession{
		s: s, conn: conn, ctx: sctx, cancel: cancel, q: newEventQueue(), wake: make(chan struct{}, 1),
		readerDone: make(chan struct{}), writerDone: make(chan struct{}), openedAt: time.Now(),
	}, nil
}

// ctxOr maps a transport error, preferring the caller's context state (cancel/deadline).
func (s *STT) ctxOr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return httpx.ClassifyError(provider.ElevenLabs, provider.CapSTT, cerr)
	}
	e := httpx.ClassifyError(provider.ElevenLabs, provider.CapSTT, err)
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
		return provider.NewError(provider.ElevenLabs, provider.CapSTT, provider.KindInvalidRequest, "audio length must be even")
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
		e := provider.NewError(provider.ElevenLabs, provider.CapSTT, provider.KindUnavailable, "audio buffer full")
		e.Retryable = true
		return e
	}
	if n := len(se.ops); n > 0 && !se.ops[n-1].finalize {
		se.ops[n-1].pcm = append(se.ops[n-1].pcm, pcm...)
	} else {
		se.ops = append(se.ops, sttOp{pcm: append([]byte(nil), pcm...)})
	}
	se.queuedBytes += len(pcm)
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
	e := httpx.ClassifyError(provider.ElevenLabs, provider.CapSTT, err)
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
			ka := time.NewTimer(se.s.keepAliveAfter())
			select {
			case <-se.wake:
				ka.Stop()
				continue
			case <-ka.C:
				if !se.sendChunk(make([]byte, sttKeepAliveBytes), false, false) {
					return
				}
				continue
			case <-se.ctx.Done():
				ka.Stop()
				return
			}
		}
		for _, op := range ops {
			if se.ctx.Err() != nil {
				return
			}
			if op.finalize {
				if !se.sendCommit(batch) {
					return
				}
				batch = nil
				continue
			}
			batch = append(batch, op.pcm...)
			for len(batch) >= sttBatchBytes {
				if !se.sendChunk(batch[:sttBatchBytes], false, true) {
					return
				}
				batch = batch[sttBatchBytes:]
			}
		}
	}
}

// sendChunk writes one input_audio_chunk. counted is false for adapter-made silence, which does
// not count as audio sent.
func (se *sttSession) sendChunk(pcm []byte, commit, counted bool) bool {
	msg, _ := json.Marshal(map[string]any{
		"message_type": "input_audio_chunk", "audio_base_64": base64.StdEncoding.EncodeToString(pcm),
		"commit": commit, "sample_rate": sttRate,
	})
	se.mu.Lock()
	if counted {
		se.audioSent += int64(len(pcm))
	}
	se.mu.Unlock()
	if err := se.write(msg); err != nil {
		se.writeFailed(err)
		return false
	}
	return true
}

// sendCommit flushes the held frames with commit=true (20 ms of silence when none is held).
func (se *sttSession) sendCommit(rest []byte) bool {
	counted := len(rest) > 0
	if !counted {
		rest = make([]byte, sttSilenceBytes)
	}
	se.mu.Lock()
	if counted {
		se.audioSent += int64(len(rest))
	}
	now := se.audioSent / sttBytesPerMs
	se.commits = append(se.commits, &commitState{span: msSpan{se.segStartMs, now}})
	n := len(se.commits)
	se.segStartMs = now
	se.mu.Unlock()
	if !se.sendChunk(rest, true, false) {
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

func (se *sttSession) emitLocked(ev provider.STTEvent) {
	now := time.Now()
	ev.ReceivedAt = now
	ev.Provider = provider.ElevenLabs
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
		var m rtMessage
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if perr := se.handle(m); perr != nil {
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
	return httpx.ClassifyWSClose(provider.ElevenLabs, provider.CapSTT, code)
}

func (se *sttSession) handle(m rtMessage) *provider.Error {
	switch {
	case m.MessageType == msgPartialTranscript:
		se.mu.Lock()
		if text := provider.NormalizeJA(m.Text); text != "" {
			se.emitLocked(provider.STTEvent{Type: provider.STTPartial, Text: text, StartMs: se.segStartMs, EndMs: se.audioSent / sttBytesPerMs})
		}
		se.mu.Unlock()
	case m.MessageType == msgCommittedTranscript:
		se.mu.Lock()
		se.committedLocked(m.Text)
		se.mu.Unlock()
	case m.MessageType == msgWarning:
		// vendor warning: logged by identifiers only (the text may echo audio-derived content)
		se.s.log.Warn("provider warning", "provider", string(provider.ElevenLabs), "capability", string(provider.CapSTT),
			"model", se.s.cfg.STTModel, "vendor_code", msgWarning)
	case errorTypes[m.MessageType]:
		return sttError(m, se.s.scrub)
	}
	return nil
}

// committedLocked turns a committed_transcript into a FINAL: it answers the oldest pending
// commit (FromFinalize), a late answer to a synthetic FINAL, or a commit nobody asked for
// (vendor auto-commit): a plain FINAL, dropped when empty.
func (se *sttSession) committedLocked(transcript string) {
	text := provider.NormalizeJA(transcript)
	var from bool
	var sp msSpan
	c := se.oldestPendingLocked()
	switch {
	case se.synthOwed > 0:
		se.synthOwed--
		sp, se.owed = se.owed[0], se.owed[1:]
	case c != nil:
		c.answered = true
		from = true
		sp = c.span
	default:
		now := se.audioSent / sttBytesPerMs
		sp = msSpan{se.segStartMs, now}
		se.segStartMs = now
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
