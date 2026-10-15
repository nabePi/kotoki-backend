// Package wsgateway is the HTTP side of the realtime endpoint: steps 1–6 of §7.3.1 (origin,
// rate limit per IP, admission, upgrade, ticket, rate limit per user) and the adapter that hands
// the upgraded socket to conversation.Manager.Attach (steps 7–11). It is the only server-side
// package that imports coder/websocket (D-005) and it never touches Redis itself: the limiter and
// the ticket store are ports.
package wsgateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/httpapi"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

const (
	// Subprotocol is the WebSocket subprotocol of the v1 protocol (§7.4.1).
	Subprotocol = "kotoki.v1"
	// stickyCookie is the cookie of the consistent-hash stickiness (Bab 13 §13.10.6); a refusal
	// before the upgrade clears it so that the retry is hashed again to another pod.
	stickyCookie = "kotoki_rt"
	// ticketLen is the length of a base64url ticket of 32 random bytes (§7.3).
	ticketLen = 43
)

// clearStickyCookie is the exact Set-Cookie of step 3 (§7.3.1).
const clearStickyCookie = stickyCookie + "=; Max-Age=0; Path=/api/v1/realtime; Secure; HttpOnly"

// Tickets is what the gateway needs of auth.TicketStore: the one-time read of the ticket (GETDEL,
// §7.3.1 step 5). It returns auth.ErrTicketInvalid for a ticket that is absent, expired, used or
// malformed; any other error means the store is unavailable.
type Tickets interface {
	Consume(ctx context.Context, plaintext string) (auth.WSTicket, error)
}

// Handler serves GET /api/v1/realtime. It is a plain http.Handler, not httpapi.Handle: it writes
// Retry-After and Set-Cookie itself and renders the D-013 body with httpapi.WriteError.
type Handler struct {
	Tuning           config.RealtimeTuning
	Origins          []string // WS_ALLOWED_ORIGINS: scheme://host[:port], exact match
	AllowEmptyOrigin bool     // WS_ALLOW_EMPTY_ORIGIN
	TrustedProxies   []netip.Prefix
	Limiter          ratelimit.Limiter
	Tickets          Tickets
	Mgr              *conversation.Manager
	Log              *slog.Logger
	Metrics          *conversation.Metrics // nil-safe
	IDs              idgen.Generator       // default: UUIDv7
	Clock            clock.Clock           // default: system clock
}

var _ http.Handler = (*Handler)(nil)

func (h *Handler) log() *slog.Logger {
	if h.Log != nil {
		return h.Log
	}
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func (h *Handler) ids() idgen.Generator {
	if h.IDs != nil {
		return h.IDs
	}
	return idgen.NewV7()
}

func (h *Handler) now() time.Time {
	if h.Clock != nil {
		return h.Clock.Now()
	}
	return clock.System{}.Now()
}

// ServeHTTP runs steps 1–6 of §7.3.1 in that order. Steps 1–3 refuse with a plain HTTP response
// before the upgrade and before the ticket is touched (R-22); steps 5–6 refuse with an error
// event and a close code.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := h.log().With("path", r.URL.Path, "query", RedactQuery(r.URL.RawQuery)) // never the raw query

	// 1. Origin.
	if !h.originAllowed(r.Header.Get("Origin")) {
		h.Metrics.Connection(conversation.ConnRejectedOrigin)
		log.WarnContext(ctx, "ws refused: origin", "origin", r.Header.Get("Origin"))
		httpapi.WriteError(w, r, errs.New(errs.CodeOriginNotAllowed, "origin not allowed"))
		return
	}

	// 2. Rate limit per IP.
	ip := httpapi.ClientIP(r, h.TrustedProxies)
	if d, ok := h.allow(ctx, "ws_connect_ip", ratelimit.SubjectIP(ip)); !ok {
		h.Metrics.Connection(conversation.ConnRejectedRateLimit)
		secs := int((d.RetryAfter + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
		httpapi.WriteError(w, r, errs.New(errs.CodeRateLimited, "rate limited: ws_connect_ip"))
		return
	}

	// 3. Draining or at hard capacity: before the upgrade and before the ticket is consumed, so the
	// ticket stays valid for the retry. The cookie is cleared so that the retry may land elsewhere.
	if err := h.Mgr.Admission(); err != nil {
		if errs.CodeOf(err) == errs.CodeServerAtCapacity {
			h.Metrics.Connection(conversation.ConnRejectedCapacity)
		} else {
			h.Metrics.Connection(conversation.ConnRejectedDraining)
		}
		log.InfoContext(ctx, "ws refused: admission", "code", string(errs.CodeOf(err)))
		w.Header().Set("Retry-After", "1")
		w.Header().Add("Set-Cookie", clearStickyCookie)
		httpapi.WriteError(w, r, err)
		return
	}

	// 4. Upgrade.
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		Subprotocols:    []string{Subprotocol},
		OriginPatterns:  h.originHosts(),
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil { // Accept has written the HTTP error itself
		log.WarnContext(ctx, "ws upgrade failed", "error", err.Error())
		return
	}
	c.SetReadLimit(h.Tuning.WSMaxJSONBytes)
	ws := NewConn(c)

	// 5. Ticket (GETDEL).
	ticket, err := h.consume(ctx, r.URL.Query().Get("ticket"))
	if err != nil {
		code := errs.CodeTicketInvalid
		if !errors.Is(err, auth.ErrTicketInvalid) { // 5b: the store failed
			code = errs.CodeServiceUnavailable
			h.Metrics.Connection(conversation.ConnRejectedUnavailable)
			log.ErrorContext(ctx, "ws ticket store failed", "error", err.Error())
		} else {
			h.Metrics.Connection(conversation.ConnRejectedTicket)
		}
		h.refuse(ctx, ws, uuid.Nil, code)
		return
	}

	// 6. Rate limit per user.
	if _, ok := h.allow(ctx, "ws_attach_user", ticket.UserID.String()); !ok {
		h.Metrics.Connection(conversation.ConnRejectedRateLimit)
		h.refuse(ctx, ws, ticket.SessionID, errs.CodeRateLimited)
		return
	}

	h.Metrics.Connection(conversation.ConnAccepted)
	err = h.Mgr.Attach(ctx, ws, conversation.AttachTicket{UserID: ticket.UserID, SessionID: ticket.SessionID})
	if err != nil {
		var ae *conversation.AttachError
		if errors.As(err, &ae) {
			log.InfoContext(ctx, "ws attach refused", "code", string(ae.Code))
		} else {
			log.WarnContext(ctx, "ws attach ended", "error", err.Error())
		}
	}
	_ = c.CloseNow() // idempotent: Attach has closed the socket on every path that matters
}

// consume reads the ticket once. A value that cannot be a ticket never reaches the store.
func (h *Handler) consume(ctx context.Context, raw string) (auth.WSTicket, error) {
	if !wellFormed(raw) {
		return auth.WSTicket{}, auth.ErrTicketInvalid
	}
	return h.Tickets.Consume(ctx, raw)
}

// wellFormed reports whether s has the shape of an issued ticket: 43 base64url characters.
func wellFormed(s string) bool {
	if len(s) != ticketLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// allow asks the limiter. A limiter error never blocks a connection (fail-open, like
// httpapi.RateLimit).
func (h *Handler) allow(ctx context.Context, policy, subject string) (ratelimit.Decision, bool) {
	d, err := h.Limiter.Allow(ctx, ratelimit.Get(policy), subject)
	if err != nil {
		h.log().WarnContext(ctx, "rate limiter error, allowing connection", "policy", policy, "error", err.Error())
		return ratelimit.Decision{}, true
	}
	return d, d.Allowed
}

// refuse answers a refusal after the upgrade (steps 5–6): the error event of the code, then the
// close with the code of the catalog (§7.4.8). Both are best effort: the peer may be gone.
func (h *Handler) refuse(ctx context.Context, ws conversation.Conn, sid uuid.UUID, code errs.Code) {
	ev := protocol.ServerEvent{
		Type: "error", Version: protocol.Version, EventID: h.ids().New(), ServerSeq: 1, SessionID: sid, TS: h.now(),
		Payload: protocol.NewError(code, h.ids().New(), nil, nil),
	}
	if b, err := protocol.MarshalServer(ev); err == nil {
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.Tuning.WSWriteTimeout)
		_ = ws.Write(wctx, conversation.MessageText, b)
		cancel()
	}
	_ = ws.Close(protocol.CloseCode(code), string(code))
	h.Metrics.WSClose(protocol.CloseCode(code))
}

// originAllowed is step 1: the Origin must be one of WS_ALLOWED_ORIGINS (exact scheme, host and
// port); without an Origin only when WS_ALLOW_EMPTY_ORIGIN is set.
func (h *Handler) originAllowed(origin string) bool {
	if origin == "" {
		return h.AllowEmptyOrigin
	}
	want, ok := normalizeOrigin(origin)
	if !ok {
		return false
	}
	for _, o := range h.Origins {
		if got, ok := normalizeOrigin(o); ok && got == want {
			return true
		}
	}
	return false
}

// normalizeOrigin lower-cases scheme and host and drops a trailing slash; it rejects values that
// are not scheme://host[:port] ("null", paths, queries).
func normalizeOrigin(o string) (string, bool) {
	u, err := url.Parse(strings.TrimSuffix(strings.TrimSpace(o), "/"))
	if err != nil || u.Scheme == "" || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return "", false
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Host), true
}

// originHosts are the host patterns of AcceptOptions.OriginPatterns: the library checks the host
// of the Origin once more, after the exact check of step 1.
func (h *Handler) originHosts() []string {
	hosts := make([]string, 0, len(h.Origins))
	for _, o := range h.Origins {
		if u, err := url.Parse(o); err == nil && u.Host != "" {
			hosts = append(hosts, u.Host)
		}
	}
	return hosts
}
