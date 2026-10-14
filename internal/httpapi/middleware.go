package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

// RequestID accepts a valid UUID X-Request-ID or creates a UUIDv7, stores it in
// the context and echoes it in the response (spec 13.8.2).
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if parsed, err := uuid.Parse(id); err != nil || len(id) != 36 {
			id = newRequestID()
		} else {
			id = parsed.String()
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}

// Recoverer turns a panic into a 500 INTERNAL_ERROR and logs it once with the panic site.
func Recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer recoverPanic(w, r, log)
			next.ServeHTTP(w, r)
		})
	}
}

// recoverPanic must be the deferred function itself so that recover() works.
func recoverPanic(w http.ResponseWriter, r *http.Request, log *slog.Logger) {
	rec := recover()
	if rec == nil {
		return
	}
	if rec == http.ErrAbortHandler { //nolint:errorlint // sentinel value compared by identity per net/http docs
		panic(rec)
	}
	log.ErrorContext(r.Context(), "panic recovered", "error_code", string(errs.CodeInternalError),
		"request_id", RequestIDFrom(r.Context()), "panic", fmt.Sprint(rec), "error_location", panicSite())
	WriteError(w, r, errs.New(errs.CodeInternalError, "panic"))
}

// panicSite returns the file:line of the innermost frame outside the runtime and this middleware.
func panicSite() string {
	pcs := make([]uintptr, 24)
	n := runtime.Callers(3, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	for {
		f, more := frames.Next()
		if !strings.HasPrefix(f.Function, "runtime.") && !strings.Contains(f.Function, "httpapi.Recoverer") {
			return fmt.Sprintf("%s:%d %s", f.File, f.Line, f.Function)
		}
		if !more {
			return "unknown"
		}
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusWriter) code() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
}

// routePattern returns the matched chi pattern, or "unmatched".
func routePattern(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil {
		if p := rc.RoutePattern(); p != "" {
			return p
		}
	}
	return "unmatched"
}

// AccessLog logs one INFO line per request: route pattern (never the raw path
// or query string, so tickets cannot leak), status and duration.
func AccessLog(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r.WithContext(WithLogger(r.Context(), log)))
			attrs := []any{"method", r.Method, "route", routePattern(r), "status", sw.code(),
				"duration_ms", float64(time.Since(start)) / float64(time.Millisecond), "request_id", RequestIDFrom(r.Context())}
			if ip, ok := ClientIPFrom(r.Context()); ok {
				attrs = append(attrs, "ip", ip.String())
			}
			log.InfoContext(r.Context(), "http request", attrs...)
		})
	}
}

// BodyLimit rejects bodies larger than limit bytes with 413.
func BodyLimit(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				WriteError(w, r, errs.New(errs.CodePayloadTooLarge, "request body too large"))
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// SecurityHeaders sets the fixed API headers; /auth/* and /me* are never cached.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/v1/auth/") || p == "/api/v1/me" || strings.HasPrefix(p, "/api/v1/me/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

func peerAddr(r *http.Request) netip.Addr {
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return ap.Addr().Unmap()
	}
	if a, err := netip.ParseAddr(r.RemoteAddr); err == nil {
		return a.Unmap()
	}
	return netip.Addr{}
}

func inPrefixes(a netip.Addr, prefixes []netip.Prefix) bool {
	for _, p := range prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// ClientIP resolves the caller: the peer address, unless the peer is a trusted
// proxy, in which case X-Forwarded-For is walked from the right past trusted hops.
func ClientIP(r *http.Request, trusted []netip.Prefix) netip.Addr {
	peer := peerAddr(r)
	if len(trusted) == 0 || !inPrefixes(peer, trusted) {
		return peer
	}
	hops := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			continue
		}
		if a = a.Unmap(); !inPrefixes(a, trusted) {
			return a
		}
	}
	return peer
}

// ClientIPMiddleware stores ClientIP(r, trusted) in the request context.
func ClientIPMiddleware(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(WithClientIP(r.Context(), ClientIP(r, trusted))))
		})
	}
}

// SubjectFunc derives the limiter subject of a request; an empty subject skips limiting.
type SubjectFunc func(*http.Request) string

// IPSubject limits by client address.
func IPSubject(trusted []netip.Prefix) SubjectFunc {
	return func(r *http.Request) string {
		ip, ok := ClientIPFrom(r.Context())
		if !ok {
			ip = ClientIP(r, trusted)
		}
		return ratelimit.SubjectIP(ip)
	}
}

// UserSubject limits by authenticated user id (empty without a principal).
func UserSubject(r *http.Request) string {
	if p, ok := auth.PrincipalFrom(r.Context()); ok {
		return p.UserID.String()
	}
	return ""
}

// RateLimit applies policy p. Limiter errors never block a request (fail-open).
func RateLimit(l ratelimit.Limiter, p ratelimit.Policy, subject SubjectFunc, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sub := subject(r)
			if sub == "" {
				next.ServeHTTP(w, r)
				return
			}
			d, err := l.Allow(r.Context(), p, sub)
			if err != nil {
				log.WarnContext(r.Context(), "rate limiter error, allowing request", "policy", p.Name, "error", err.Error())
				next.ServeHTTP(w, r)
				return
			}
			if !d.Allowed {
				secs := int((d.RetryAfter + time.Second - 1) / time.Second)
				w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
				WriteError(w, r, errs.New(errs.CodeRateLimited, "rate limited: "+p.Name))
				return
			}
			w.Header().Set("RateLimit-Remaining", strconv.Itoa(d.Remaining))
			next.ServeHTTP(w, r)
		})
	}
}

// Authn requires a valid Bearer access token and stores the Principal. No DB access.
func Authn(t auth.TokenIssuer, clk clock.Clock) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			scheme, token, found := strings.Cut(r.Header.Get("Authorization"), " ")
			if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
				WriteError(w, r, errs.New(errs.CodeAuthRequired, "bearer token required"))
				return
			}
			claims, err := t.ParseAccess(strings.TrimSpace(token), clk.Now())
			switch {
			case err == auth.ErrTokenExpired: //nolint:errorlint // sentinel returned directly by TokenIssuer
				WriteError(w, r, errs.New(errs.CodeTokenExpired, "token expired"))
				return
			case err != nil:
				WriteError(w, r, errs.New(errs.CodeTokenInvalid, "token invalid"))
				return
			}
			p := auth.Principal{UserID: claims.UserID, Role: claims.Role, FamilyID: claims.FamilyID}
			next.ServeHTTP(w, r.WithContext(auth.WithPrincipal(r.Context(), p)))
		})
	}
}

// AdminGate re-checks in the database that the caller is still an ACTIVE admin.
type AdminGate interface {
	IsActiveAdmin(ctx context.Context, userID uuid.UUID) (bool, error)
}

// RequireAdmin gates /admin/*: the token claim must say ADMIN and the DB must agree.
func RequireAdmin(g AdminGate, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, _ := auth.PrincipalFrom(r.Context())
			if p.Role != auth.RoleAdmin {
				denyAdmin(w, r, log, p)
				return
			}
			ok, err := g.IsActiveAdmin(r.Context(), p.UserID)
			if err != nil {
				WriteError(w, r, err)
				return
			}
			if !ok {
				denyAdmin(w, r, log, p)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func denyAdmin(w http.ResponseWriter, r *http.Request, log *slog.Logger, p auth.Principal) {
	log.InfoContext(r.Context(), "audit", "audit", true, "audit_event", "admin_access_denied", "result", "failure",
		"user_id", p.UserID.String(), "request_id", RequestIDFrom(r.Context()))
	WriteError(w, r, errs.New(errs.CodeForbidden, "admin access denied"))
}

// OriginCheck rejects cookie endpoints called from an origin outside the allowlist;
// a missing Origin (CLI, server-to-server) is allowed.
func OriginCheck(origins []string) func(http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, o := range origins {
		allowed[o] = true
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if o := r.Header.Get("Origin"); o != "" && !allowed[o] {
				WriteError(w, r, errs.New(errs.CodeOriginNotAllowed, "origin not allowed"))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
