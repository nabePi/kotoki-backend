package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/auth"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

const maxBodyBytes = 64 << 10

// effects collects response metadata a handler wants (cookies, headers, status).
// They are applied on both the success and the error path; this is the only
// mechanism handlers have to influence the response beyond their return value.
type effects struct {
	cookies []*http.Cookie
	headers [][2]string
	status  int
}

func (e *effects) apply(w http.ResponseWriter) {
	for _, c := range e.cookies {
		http.SetCookie(w, c)
	}
	for _, h := range e.headers {
		w.Header().Set(h[0], h[1])
	}
}

// Request is what a typed handler receives.
type Request[B any] struct {
	Body           B
	Principal      auth.Principal // zero in the public group
	RequestID      string
	IdempotencyKey string
	ClientIP       netip.Addr
	UserAgent      string
	raw            *http.Request
	fx             *effects
}

// PathUUID parses a UUID path parameter; an invalid value is a 404 (spec 5.8.1).
func (r Request[B]) PathUUID(name string) (uuid.UUID, error) {
	id, err := uuid.Parse(chi.URLParam(r.raw, name))
	if err != nil {
		return uuid.Nil, errs.New(errs.CodeNotFound, "invalid path parameter "+name)
	}
	return id, nil
}

// Query returns the URL query.
func (r Request[B]) Query() url.Values { return r.raw.URL.Query() }

// Cookie returns the named request cookie's value, or "".
func (r Request[B]) Cookie(name string) string {
	c, err := r.raw.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}

// SetCookie adds a Set-Cookie to the response (success and error alike).
func (r Request[B]) SetCookie(c *http.Cookie) { r.fx.cookies = append(r.fx.cookies, c) }

// SetHeader sets a response header (success and error alike).
func (r Request[B]) SetHeader(k, v string) { r.fx.headers = append(r.fx.headers, [2]string{k, v}) }

// SetStatus overrides the status given to Handle on the success path.
func (r Request[B]) SetStatus(status int) { r.fx.status = status }

// Validator is implemented by request DTOs that validate themselves.
type Validator interface{ Validate() error }

// NoContent as a response type produces 204 with no body.
type NoContent struct{}

func newRequest[B any](r *http.Request) Request[B] {
	ip, ok := ClientIPFrom(r.Context())
	if !ok {
		if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
			ip = ap.Addr()
		}
	}
	reqID := RequestIDFrom(r.Context())
	if reqID == "" {
		reqID = newRequestID()
	}
	p, _ := auth.PrincipalFrom(r.Context())
	return Request[B]{
		Principal: p, RequestID: reqID, IdempotencyKey: r.Header.Get("Idempotency-Key"),
		ClientIP: ip, UserAgent: r.UserAgent(), raw: r, fx: &effects{},
	}
}

// Handle decodes a JSON body into B (Content-Type required, unknown fields rejected,
// 64 KiB limit), validates it, runs fn and writes the result or the error.
func Handle[B, R any](status int, fn func(ctx context.Context, req Request[B]) (R, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req := newRequest[B](r)
		ctx := WithRequestID(r.Context(), req.RequestID)
		r = r.WithContext(ctx)
		req.raw = r
		if err := decodeBody(w, r, &req.Body); err != nil {
			WriteError(w, r, err)
			return
		}
		if v, ok := any(req.Body).(Validator); ok {
			if err := v.Validate(); err != nil {
				WriteError(w, r, err)
				return
			}
		}
		respond(w, r, req.fx, status, func() (R, error) { return fn(ctx, req) })
	}
}

// HandleNoBody is Handle for endpoints without a request body.
func HandleNoBody[R any](status int, fn func(ctx context.Context, req Request[struct{}]) (R, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req := newRequest[struct{}](r)
		ctx := WithRequestID(r.Context(), req.RequestID)
		r = r.WithContext(ctx)
		req.raw = r
		respond(w, r, req.fx, status, func() (R, error) { return fn(ctx, req) })
	}
}

func respond[R any](w http.ResponseWriter, r *http.Request, fx *effects, status int, call func() (R, error)) {
	resp, err := call()
	fx.apply(w)
	if err != nil {
		WriteError(w, r, err)
		return
	}
	if fx.status != 0 {
		status = fx.status
	}
	if _, ok := any(resp).(NoContent); ok {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	WriteJSON(w, status, resp)
}

func decodeBody(w http.ResponseWriter, r *http.Request, into any) error {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return errs.New(errs.CodeUnsupportedMediaType, "content type must be application/json")
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return classifyDecodeError(err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return errs.New(errs.CodePayloadTooLarge, "request body too large")
		}
		return errs.New(errs.CodeInvalidJson, "trailing data after JSON body")
	}
	return nil
}

func classifyDecodeError(err error) error {
	var tooBig *http.MaxBytesError
	var typeErr *json.UnmarshalTypeError
	var syntaxErr *json.SyntaxError
	switch {
	case errors.As(err, &tooBig):
		return errs.New(errs.CodePayloadTooLarge, "request body too large")
	case errors.As(err, &typeErr):
		return FieldError(typeErr.Field, "invalid_type")
	case errors.As(err, &syntaxErr), errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return errs.New(errs.CodeInvalidJson, "invalid JSON")
	}
	if msg := err.Error(); strings.HasPrefix(msg, "json: unknown field ") {
		return FieldError(strings.Trim(strings.TrimPrefix(msg, "json: unknown field "), `"`), "unknown_field")
	}
	return errs.New(errs.CodeInvalidJson, "invalid JSON")
}
