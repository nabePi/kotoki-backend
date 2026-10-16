package wsgateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
	"github.com/nabePi/kotoki-backend/internal/testutil/rtharness"
	"github.com/nabePi/kotoki-backend/internal/wsgateway"
)

const (
	wsPkg         = "github.com/nabePi/kotoki-backend/internal/wsgateway"
	allowedOrigin = "https://app.kotoki.test"
	stickyClear   = "kotoki_rt=; Max-Age=0; Path=/api/v1/realtime; Secure; HttpOnly"
)

// stubLimiter answers per policy and records what it was asked.
type stubLimiter struct {
	mu      sync.Mutex
	deny    map[string]time.Duration // policy → RetryAfter of the refusal
	err     map[string]error
	calls   []limiterCall
	allowed int
}

type limiterCall struct{ Policy, Subject string }

func (l *stubLimiter) Allow(_ context.Context, p ratelimit.Policy, subject string) (ratelimit.Decision, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls = append(l.calls, limiterCall{p.Name, subject})
	if err := l.err[p.Name]; err != nil {
		return ratelimit.Decision{}, err
	}
	if ra, ok := l.deny[p.Name]; ok {
		return ratelimit.Decision{Allowed: false, RetryAfter: ra}, nil
	}
	l.allowed++
	return ratelimit.Decision{Allowed: true, Remaining: 1}, nil
}

func (l *stubLimiter) Calls() []limiterCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]limiterCall(nil), l.calls...)
}

// countingTickets wraps the in-memory store: it counts Consume and can fail it.
type countingTickets struct {
	*fakes.TicketStore
	mu       sync.Mutex
	consumed []string
	fail     error
}

func (c *countingTickets) Consume(ctx context.Context, plaintext string) (auth.WSTicket, error) {
	c.mu.Lock()
	c.consumed = append(c.consumed, plaintext)
	fail := c.fail
	c.mu.Unlock()
	if fail != nil {
		return auth.WSTicket{}, fail
	}
	return c.TicketStore.Consume(ctx, plaintext)
}

func (c *countingTickets) Consumed() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.consumed...)
}

// rig is a Handler over a real Manager of the harness, behind httptest.
type rig struct {
	t       *testing.T
	H       *rtharness.H
	Handler *wsgateway.Handler
	Limiter *stubLimiter
	Tickets *countingTickets
	Srv     *httptest.Server
	Logs    *syncBuffer
}

type rigOpt func(*rig)

func newRig(t *testing.T, harnessOpts []rtharness.Option, opts ...rigOpt) *rig {
	t.Helper()
	testutil.NoGoroutineLeak(t, wsPkg)
	r := &rig{t: t, Limiter: &stubLimiter{}, Tickets: &countingTickets{TicketStore: fakes.NewTicketStore()}, Logs: &syncBuffer{}}
	r.H = rtharness.New(t, harnessOpts...)
	r.Handler = &wsgateway.Handler{
		Tuning: rtharness.DefaultTuning(t), Origins: []string{allowedOrigin}, Limiter: r.Limiter, Tickets: r.Tickets, Mgr: r.H.Mgr,
		Log:     slog.New(slog.NewJSONHandler(r.Logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Metrics: r.H.Metrics,
	}
	for _, f := range opts {
		f(r)
	}
	r.Srv = httptest.NewServer(r.Handler)
	t.Cleanup(r.Srv.Close)
	return r
}

// syncBuffer is a log sink the handler goroutines write to while the test reads it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// issue stores a ticket of the harness session and returns it.
func (r *rig) issue() string {
	r.t.Helper()
	tk, err := r.Tickets.Issue(context.Background(), auth.WSTicket{UserID: r.H.Ticket.UserID, SessionID: r.H.Ticket.SessionID}, time.Minute)
	require.NoError(r.t, err)
	return tk
}

func (r *rig) wsURL(ticket string) string {
	u := "ws" + strings.TrimPrefix(r.Srv.URL, "http") + "/api/v1/realtime"
	if ticket != "" {
		u += "?ticket=" + ticket
	}
	return u
}

func ctx5(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// dial opens a WebSocket with the given Origin ("" sends none).
func (r *rig) dial(ticket, origin string) (*websocket.Conn, *http.Response, error) {
	r.t.Helper()
	h := http.Header{}
	if origin != "" {
		h.Set("Origin", origin)
	}
	return websocket.Dial(ctx5(r.t), r.wsURL(ticket), &websocket.DialOptions{HTTPHeader: h, Subprotocols: []string{wsgateway.Subprotocol}})
}

// mustDial dials with the allowed origin and fails if the upgrade does not happen.
func (r *rig) mustDial(ticket string) *websocket.Conn {
	r.t.Helper()
	c, _, err := r.dial(ticket, allowedOrigin)
	require.NoError(r.t, err)
	r.t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

// get is a plain (non-upgrade) request: steps 1–3 answer it before any upgrade.
func (r *rig) get(ticket, origin string) *http.Response {
	r.t.Helper()
	url := r.Srv.URL + "/api/v1/realtime"
	if ticket != "" {
		url += "?ticket=" + ticket
	}
	req, err := http.NewRequestWithContext(ctx5(r.t), http.MethodGet, url, http.NoBody)
	require.NoError(r.t, err)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(r.t, err)
	r.t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

type errBody struct {
	Error struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"request_id"`
		Details   any    `json:"details"`
	} `json:"error"`
}

// restError decodes the D-013 body of an HTTP refusal and checks that all four fields exist.
func restError(t *testing.T, resp *http.Response) errBody {
	t.Helper()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var fields map[string]map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw, &fields))
	for _, k := range []string{"code", "message", "request_id", "details"} {
		require.Contains(t, fields["error"], k, "D-013 field %s: %s", k, raw)
	}
	var b errBody
	require.NoError(t, json.Unmarshal(raw, &b))
	require.NotEmpty(t, b.Error.Message)
	require.NotEmpty(t, b.Error.RequestID)
	return b
}

type wsEvent struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type wsErrorPayload struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	Retryable bool `json:"retryable"`
	Fatal     bool `json:"fatal"`
}

func readEvent(t *testing.T, c *websocket.Conn) wsEvent {
	t.Helper()
	typ, b, err := c.Read(ctx5(t))
	require.NoError(t, err)
	require.Equal(t, websocket.MessageText, typ)
	var ev wsEvent
	require.NoError(t, json.Unmarshal(b, &ev))
	return ev
}

// expectRefusal reads the error event the gateway sends and then the close frame.
func expectRefusal(t *testing.T, c *websocket.Conn, code string, closeCode websocket.StatusCode) wsErrorPayload {
	t.Helper()
	ev := readEvent(t, c)
	require.Equal(t, "error", ev.Type)
	var p wsErrorPayload
	require.NoError(t, json.Unmarshal(ev.Payload, &p))
	require.Equal(t, code, p.Error.Code)
	require.True(t, p.Fatal)
	require.NotEmpty(t, p.Error.Message)
	_, _, err := c.Read(ctx5(t))
	require.Equal(t, closeCode, websocket.CloseStatus(err), "close of %s: %v", code, err)
	var ce websocket.CloseError
	require.True(t, errors.As(err, &ce))
	require.Equal(t, code, ce.Reason)
	return p
}

// connCount reads connections_total{result} of the harness registry.
func connCount(t *testing.T, reg *prometheus.Registry, result string) float64 {
	t.Helper()
	mfs, err := reg.Gather()
	require.NoError(t, err)
	for _, mf := range mfs {
		if mf.GetName() != "kotoki_realtime_connections_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "result" && l.GetValue() == result {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func drain(t *testing.T, mgr *conversation.Manager) {
	t.Helper()
	require.NoError(t, mgr.Drain(ctx5(t)))
	require.True(t, mgr.Draining())
}
