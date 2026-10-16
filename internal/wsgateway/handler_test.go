package wsgateway_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
	"github.com/nabePi/kotoki-backend/internal/testutil/rtharness"
	"github.com/nabePi/kotoki-backend/internal/wsgateway"
)

func TestOriginRejected(t *testing.T) {
	for name, origin := range map[string]string{
		"other host":       "https://evil.test",
		"other scheme":     "http://app.kotoki.test",
		"other port":       "https://app.kotoki.test:8443",
		"opaque origin":    "null",
		"with a path":      "https://app.kotoki.test/x",
		"subdomain":        "https://x.app.kotoki.test",
		"suffix confusion": "https://app.kotoki.test.evil.test",
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			ticket := r.issue()
			resp := r.get(ticket, origin)
			require.Equal(t, http.StatusForbidden, resp.StatusCode)
			require.Equal(t, "ORIGIN_NOT_ALLOWED", restError(t, resp).Error.Code)
			require.Empty(t, r.Tickets.Consumed(), "the ticket is not consumed")
			require.Empty(t, r.Limiter.Calls(), "origin is checked before the limiter")
			require.Equal(t, 1.0, connCount(t, r.H.Reg, conversation.ConnRejectedOrigin))
			// The ticket is intact: the same one opens a connection from the allowed origin.
			c := r.mustDial(ticket)
			require.Equal(t, "session.ready", readEvent(t, c).Type)
		})
	}
}

func TestOriginAllowed(t *testing.T) {
	r := newRig(t, nil, func(r *rig) {
		r.Handler.Origins = []string{"https://app.kotoki.test", "http://localhost:3000/", "https://Other.Kotoki.test"}
	})
	for _, origin := range []string{"https://app.kotoki.test", "http://localhost:3000", "https://other.kotoki.test", "HTTPS://APP.KOTOKI.TEST"} {
		c, _, err := r.dial(r.issue(), origin)
		require.NoError(t, err, origin)
		require.Equal(t, "session.ready", readEvent(t, c).Type, origin)
		_ = c.CloseNow()
	}
}

func TestEmptyOriginPolicy(t *testing.T) {
	t.Run("refused when WS_ALLOW_EMPTY_ORIGIN is false", func(t *testing.T) {
		r := newRig(t, nil)
		ticket := r.issue()
		resp := r.get(ticket, "")
		require.Equal(t, http.StatusForbidden, resp.StatusCode)
		require.Equal(t, "ORIGIN_NOT_ALLOWED", restError(t, resp).Error.Code)
		_, resp2, err := r.dial(ticket, "")
		require.Error(t, err)
		require.Equal(t, http.StatusForbidden, resp2.StatusCode)
		require.Empty(t, r.Tickets.Consumed())
	})
	t.Run("accepted when it is true", func(t *testing.T) {
		r := newRig(t, nil, func(r *rig) { r.Handler.AllowEmptyOrigin = true })
		c, _, err := r.dial(r.issue(), "")
		require.NoError(t, err)
		defer func() { _ = c.CloseNow() }()
		require.Equal(t, "session.ready", readEvent(t, c).Type)
	})
	t.Run("a wrong origin is refused even then", func(t *testing.T) {
		r := newRig(t, nil, func(r *rig) { r.Handler.AllowEmptyOrigin = true })
		require.Equal(t, http.StatusForbidden, r.get(r.issue(), "https://evil.test").StatusCode)
	})
}

func TestRateLimitIP(t *testing.T) {
	r := newRig(t, nil, func(r *rig) { r.Limiter.deny = map[string]time.Duration{"ws_connect_ip": 7200 * time.Millisecond} })
	ticket := r.issue()
	resp := r.get(ticket, allowedOrigin)
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "8", resp.Header.Get("Retry-After"), "RetryAfter rounds up to whole seconds")
	require.Equal(t, "RATE_LIMITED", restError(t, resp).Error.Code)
	require.Equal(t, []limiterCall{{"ws_connect_ip", "127.0.0.1"}}, r.Limiter.Calls())
	require.Empty(t, r.Tickets.Consumed(), "the ticket is intact")
	require.Equal(t, 1.0, connCount(t, r.H.Reg, conversation.ConnRejectedRateLimit))

	// A sub-second wait is still at least one second.
	r.Limiter.mu.Lock()
	r.Limiter.deny["ws_connect_ip"] = 10 * time.Millisecond
	r.Limiter.mu.Unlock()
	require.Equal(t, "1", r.get(ticket, allowedOrigin).Header.Get("Retry-After"))
	r.Limiter.mu.Lock()
	r.Limiter.deny["ws_connect_ip"] = 0
	r.Limiter.mu.Unlock()
	require.Equal(t, "1", r.get(ticket, allowedOrigin).Header.Get("Retry-After"), "never 0")
}

func TestRateLimitIPPolicyAndSubject(t *testing.T) {
	r := newRig(t, nil, func(r *rig) {
		r.Handler.TrustedProxies = nil
		r.Limiter.deny = map[string]time.Duration{"ws_connect_ip": time.Second}
	})
	req, err := http.NewRequestWithContext(ctx5(t), http.MethodGet, r.Srv.URL+"/api/v1/realtime", http.NoBody)
	require.NoError(t, err)
	req.Header.Set("Origin", allowedOrigin)
	req.Header.Set("X-Forwarded-For", "203.0.113.9")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Equal(t, "127.0.0.1", r.Limiter.Calls()[0].Subject, "an untrusted X-Forwarded-For is ignored")
	require.Equal(t, ratelimit.Get("ws_connect_ip").Name, r.Limiter.Calls()[0].Policy)
}

func TestLimiterErrorFailsOpen(t *testing.T) {
	r := newRig(t, nil, func(r *rig) {
		r.Limiter.err = map[string]error{"ws_connect_ip": errors.New("redis down"), "ws_attach_user": errors.New("redis down")}
	})
	c := r.mustDial(r.issue())
	require.Equal(t, "session.ready", readEvent(t, c).Type)
	require.Contains(t, r.Logs.String(), "rate limiter error")
}

func TestDrainingBeforeTicket(t *testing.T) {
	r := newRig(t, nil)
	ticket := r.issue()
	drain(t, r.H.Mgr)

	for _, origin := range []string{allowedOrigin} {
		resp := r.get(ticket, origin)
		require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
		require.Equal(t, "1", resp.Header.Get("Retry-After"))
		require.Equal(t, []string{stickyClear}, resp.Header.Values("Set-Cookie"))
		require.Equal(t, "SERVER_DRAINING", restError(t, resp).Error.Code)
	}
	_, resp, err := r.dial(ticket, allowedOrigin)
	require.Error(t, err, "no upgrade")
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Empty(t, r.Tickets.Consumed(), "Tickets.Consume is not called")
	require.Equal(t, 2.0, connCount(t, r.H.Reg, conversation.ConnRejectedDraining))

	// The same ticket is still good on a pod that is not draining (Review Focus 5).
	other := newRig(t, nil, func(o *rig) { o.Tickets = r.Tickets; o.Handler.Tickets = r.Tickets })
	c := other.mustDial(ticket)
	require.Equal(t, "session.ready", readEvent(t, c).Type)
	require.Equal(t, []string{ticket}, r.Tickets.Consumed())
}

func TestAtCapacityBeforeTicket(t *testing.T) {
	r := newRig(t, []rtharness.Option{rtharness.WithLimits(conversation.Limits{ReadyMax: 1, Max: 1, HardMax: 1})})
	r.H.Connect(r.H.StartCmd())
	r.H.WaitFor("the session to hold a slot", func() bool { return r.H.Mgr.ActiveSessions() == 1 })
	ticket := r.issue()

	resp := r.get(ticket, allowedOrigin)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Equal(t, "1", resp.Header.Get("Retry-After"))
	require.Equal(t, []string{stickyClear}, resp.Header.Values("Set-Cookie"))
	require.Equal(t, "SERVER_AT_CAPACITY", restError(t, resp).Error.Code)
	require.Empty(t, r.Tickets.Consumed())
	require.Equal(t, 1.0, connCount(t, r.H.Reg, conversation.ConnRejectedCapacity))
	require.Zero(t, connCount(t, r.H.Reg, conversation.ConnRejectedDraining))
}

func TestStepOrder(t *testing.T) {
	// Each case breaks steps 1..3 at once from a given step on: the first failing step answers.
	r := newRig(t, nil, func(r *rig) { r.Limiter.deny = map[string]time.Duration{"ws_connect_ip": time.Second} })
	drain(t, r.H.Mgr)
	ticket := r.issue()

	resp := r.get(ticket, "https://evil.test") // origin, limiter and drain all fail
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Empty(t, r.Limiter.Calls(), "step 2 does not run after step 1 failed")
	require.Empty(t, resp.Header.Values("Set-Cookie"))

	resp = r.get(ticket, allowedOrigin) // limiter and drain fail
	require.Equal(t, http.StatusTooManyRequests, resp.StatusCode)
	require.Empty(t, resp.Header.Values("Set-Cookie"), "step 3 does not run after step 2 failed")
	require.Zero(t, connCount(t, r.H.Reg, conversation.ConnRejectedDraining))

	r.Limiter.mu.Lock()
	r.Limiter.deny = nil
	r.Limiter.mu.Unlock()
	resp = r.get(ticket, allowedOrigin) // only the drain fails
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
	require.Empty(t, r.Tickets.Consumed())
}

func TestTicketInvalidCloses4001(t *testing.T) {
	t.Run("unknown ticket", func(t *testing.T) {
		r := newRig(t, nil)
		c := r.mustDial(strings.Repeat("A", 43))
		p := expectRefusal(t, c, "TICKET_INVALID", 4001)
		require.True(t, p.Retryable)
		require.Equal(t, 1, len(r.Tickets.Consumed()))
		require.Equal(t, 1.0, connCount(t, r.H.Reg, conversation.ConnRejectedTicket))
		require.Zero(t, r.H.Mgr.ActiveSessions())
	})
	t.Run("missing or malformed ticket never reaches the store", func(t *testing.T) {
		r := newRig(t, nil)
		for _, tk := range []string{"", "short", strings.Repeat("A", 44), strings.Repeat("A", 42) + "+", strings.Repeat("A", 42) + "="} {
			c := r.mustDial(tk)
			expectRefusal(t, c, "TICKET_INVALID", 4001)
		}
		require.Empty(t, r.Tickets.Consumed())
	})
	t.Run("a ticket works once", func(t *testing.T) {
		r := newRig(t, nil)
		ticket := r.issue()
		c := r.mustDial(ticket)
		require.Equal(t, "session.ready", readEvent(t, c).Type)
		_ = c.CloseNow()
		expectRefusal(t, r.mustDial(ticket), "TICKET_INVALID", 4001)
	})
}

func TestTicketRedisError4503(t *testing.T) {
	r := newRig(t, nil, func(r *rig) { r.Tickets.fail = errors.New("redis: connection refused") })
	c := r.mustDial(r.issue())
	p := expectRefusal(t, c, "SERVICE_UNAVAILABLE", 4503)
	require.True(t, p.Retryable)
	require.Equal(t, 1.0, connCount(t, r.H.Reg, conversation.ConnRejectedUnavailable))
	require.Zero(t, connCount(t, r.H.Reg, conversation.ConnRejectedTicket))
	require.Zero(t, r.H.Mgr.ActiveSessions())
	require.Contains(t, r.Logs.String(), "ws ticket store failed")
}

func TestAttachRateLimit4029(t *testing.T) {
	r := newRig(t, nil, func(r *rig) { r.Limiter.deny = map[string]time.Duration{"ws_attach_user": time.Second} })
	ticket := r.issue()
	c := r.mustDial(ticket)
	p := expectRefusal(t, c, "RATE_LIMITED", 4029)
	require.True(t, p.Retryable)
	require.Equal(t, []limiterCall{{"ws_connect_ip", "127.0.0.1"}, {"ws_attach_user", r.H.Ticket.UserID.String()}}, r.Limiter.Calls())
	require.Equal(t, []string{ticket}, r.Tickets.Consumed(), "step 5 runs before step 6")
	require.Zero(t, r.H.Mgr.ActiveSessions(), "the connection never reached Attach")
	require.Equal(t, 1.0, connCount(t, r.H.Reg, conversation.ConnRejectedRateLimit))
}

func TestAttachRateLimitNeedsTheTicketFirst(t *testing.T) {
	r := newRig(t, nil, func(r *rig) { r.Limiter.deny = map[string]time.Duration{"ws_attach_user": time.Second} })
	expectRefusal(t, r.mustDial(strings.Repeat("B", 43)), "TICKET_INVALID", 4001)
	for _, c := range r.Limiter.Calls() {
		require.NotEqual(t, "ws_attach_user", c.Policy, "no user is known before the ticket")
	}
}

func TestHandshakeReachesAttach(t *testing.T) {
	r := newRig(t, nil)
	c := r.mustDial(r.issue())
	ev := readEvent(t, c)
	require.Equal(t, "session.ready", ev.Type, "Manager.Attach ran on the upgraded socket")
	require.Equal(t, 1.0, connCount(t, r.H.Reg, conversation.ConnAccepted))
	require.Equal(t, []limiterCall{{"ws_connect_ip", "127.0.0.1"}, {"ws_attach_user", r.H.Ticket.UserID.String()}}, r.Limiter.Calls())

	// A first command drives the session through the Manager.
	start := r.H.Cmd("session.start", map[string]any{"audio": map[string]any{
		"codec": "opus", "sample_rate": 48000, "channels": 1, "frame_ms": 20,
	}})
	raw, err := protocol.MarshalClient(start)
	require.NoError(t, err)
	require.NoError(t, c.Write(ctx5(t), websocket.MessageText, raw))
	for ev := readEvent(t, c); ev.Type != "session.started"; ev = readEvent(t, c) {
		require.NotEqual(t, "error", ev.Type)
	}
	require.Equal(t, 1, r.H.Mgr.ActiveSessions())
}

func TestSubprotocolEcho(t *testing.T) {
	r := newRig(t, nil)
	h := http.Header{"Origin": {allowedOrigin}}
	c, resp, err := websocket.Dial(ctx5(t), r.wsURL(r.issue()), &websocket.DialOptions{
		HTTPHeader: h, Subprotocols: []string{"other", wsgateway.Subprotocol}, CompressionMode: websocket.CompressionContextTakeover,
	})
	require.NoError(t, err)
	defer func() { _ = c.CloseNow() }()
	require.Equal(t, "kotoki.v1", c.Subprotocol())
	require.Equal(t, "kotoki.v1", resp.Header.Get("Sec-WebSocket-Protocol"))
	require.Empty(t, resp.Header.Get("Sec-WebSocket-Extensions"), "compression stays off even when the client offers it")
}

func TestReadLimit(t *testing.T) {
	t.Run("16384 bytes pass the transport", func(t *testing.T) {
		r := newRig(t, nil)
		c := r.mustDial(r.issue())
		require.Equal(t, "session.ready", readEvent(t, c).Type)
		c.SetReadLimit(1 << 20)
		require.NoError(t, c.Write(ctx5(t), websocket.MessageText, []byte(strings.Repeat("x", 16384))))
		ev := readEvent(t, c)
		require.Equal(t, "error", ev.Type)
		require.Contains(t, string(ev.Payload), "INVALID_MESSAGE", "the Manager read it and found it was not JSON")
	})
	t.Run("16385 bytes close the connection", func(t *testing.T) {
		r := newRig(t, nil)
		c := r.mustDial(r.issue())
		require.Equal(t, "session.ready", readEvent(t, c).Type)
		require.NoError(t, c.Write(ctx5(t), websocket.MessageText, []byte(strings.Repeat("x", 16385))))
		_, _, err := c.Read(ctx5(t))
		require.Error(t, err)
		require.Equal(t, websocket.StatusMessageTooBig, websocket.CloseStatus(err))
	})
}

func TestRequestLogNeverHoldsTheTicket(t *testing.T) {
	r := newRig(t, nil)
	ticket := r.issue()
	c := r.mustDial(ticket)
	require.Equal(t, "session.ready", readEvent(t, c).Type)
	_ = c.CloseNow()
	// Refusals log too: bad origin, unknown ticket.
	r.get(ticket, "https://evil.test")
	expectRefusal(t, r.mustDial(strings.Repeat("Z", 43)), "TICKET_INVALID", 4001)

	logs := r.Logs.String()
	require.Contains(t, logs, "ticket=REDACTED")
	require.NotContains(t, logs, ticket)
	require.NotContains(t, logs, strings.Repeat("Z", 43))
}

func TestNilOptionalDependenciesHaveDefaults(t *testing.T) {
	r := newRig(t, nil, func(r *rig) { r.Handler.Log, r.Handler.Metrics = nil, nil })
	expectRefusal(t, r.mustDial(strings.Repeat("C", 43)), "TICKET_INVALID", 4001)
	resp := r.get("", "https://evil.test")
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}
