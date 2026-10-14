package httpx

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

const maxMessage = 512

var (
	skKeyRE   = regexp.MustCompile(`sk[-_][A-Za-z0-9_\-]{6,}`)
	bearerRE  = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=\-]+`)
	apiKeyRE  = regexp.MustCompile(`(?i)(api[_-]?key|xi-api-key|authorization)["']?\s*[:=]\s*["']?[A-Za-z0-9._~+/=\-]+`)
	redaction = "[redacted]"
)

// Redact removes API-key-looking substrings (sk-…, Bearer …, api_key=…) from vendor text so it
// can safely go into provider.Error.Message.
func Redact(s string) string {
	s = skKeyRE.ReplaceAllString(s, redaction)
	s = bearerRE.ReplaceAllString(s, redaction)
	return apiKeyRE.ReplaceAllString(s, redaction)
}

func cut(s string) string {
	if len(s) <= maxMessage {
		return s
	}
	n := maxMessage
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// retryableByKind follows the default column of the taxonomy table (spec 8.3).
func retryableByKind(k provider.ErrorKind) bool {
	switch k {
	case provider.KindTimeout, provider.KindRateLimited, provider.KindUnavailable, provider.KindInvalidResponse:
		return true
	}
	return false
}

// vendorFields pulls the vendor error code/type and message out of the common JSON error
// shapes ({"error":{"code","type","message"}} and {"code","message"}).
func vendorFields(body []byte) (code, message string) {
	var doc struct {
		Error json.RawMessage `json:"error"`
		Code  any             `json:"code"`
		Msg   string          `json:"message"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", ""
	}
	str := func(v any) string {
		switch x := v.(type) {
		case string:
			return x
		case float64:
			return strconv.FormatFloat(x, 'f', -1, 64)
		}
		return ""
	}
	if len(doc.Error) > 0 {
		var inner struct {
			Code    any    `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		}
		if json.Unmarshal(doc.Error, &inner) == nil {
			code = str(inner.Code)
			if code == "" {
				code = inner.Type
			}
			message = inner.Message
		} else {
			var s string
			if json.Unmarshal(doc.Error, &s) == nil {
				message = s
			}
		}
	}
	if code == "" {
		code = str(doc.Code)
	}
	if message == "" {
		message = doc.Msg
	}
	return code, message
}

// retryAfter reads Retry-After as seconds or as an HTTP-date relative to the response's Date
// header (no wall clock is used); 0 when absent or unusable.
func retryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseFloat(v, 64); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs * float64(time.Second))
	}
	at, err := http.ParseTime(v)
	if err != nil {
		return 0
	}
	date, err := http.ParseTime(h.Get("Date"))
	if err != nil {
		return 0
	}
	if d := at.Sub(date); d > 0 {
		return d
	}
	return 0
}

// ClassifyHTTP maps a non-2xx response to a provider.Error (spec 8.3 table). Quota exhaustion
// (vendor code insufficient_quota) is not retryable.
func ClassifyHTTP(p provider.Name, c provider.Capability, resp *http.Response, body []byte) *provider.Error {
	kind := provider.ClassifyHTTPStatus(resp.StatusCode)
	code, msg := vendorFields(body)
	if msg == "" {
		msg = http.StatusText(resp.StatusCode) // never echo a raw body: it may carry request text
	}
	e := &provider.Error{
		Provider: p, Capability: c, Kind: kind, Retryable: retryableByKind(kind), HTTPStatus: resp.StatusCode,
		VendorCode: Redact(code), RetryAfter: retryAfter(resp.Header), Message: cut(Redact(strings.TrimSpace(msg))),
	}
	if kind == provider.KindRateLimited && strings.Contains(strings.ToLower(code), "insufficient_quota") {
		e.Retryable = false
	}
	return e
}

// ClassifyBody reports a 2xx body that cannot be parsed (INVALID_RESPONSE, retryable once).
func ClassifyBody(p provider.Name, c provider.Capability, cause error) *provider.Error {
	e := provider.NewError(p, c, provider.KindInvalidResponse, "response could not be parsed")
	e.Retryable = true
	e.Cause = cause
	return e
}

// ClassifyError maps a transport error: context.Canceled → CANCELLED, deadline or
// net.Error.Timeout → TIMEOUT, everything else (DNS, TLS, reset) → UNAVAILABLE.
func ClassifyError(p provider.Name, c provider.Capability, err error) *provider.Error {
	var pe *provider.Error
	if errors.As(err, &pe) {
		return pe
	}
	kind := provider.KindUnavailable
	var ne net.Error
	switch {
	case errors.Is(err, context.Canceled):
		kind = provider.KindCancelled
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		kind = provider.KindTimeout
	}
	e := provider.NewError(p, c, kind, "transport error")
	e.Retryable = retryableByKind(kind)
	e.Cause = err
	return e
}

// ClassifyWSClose maps an abnormal WebSocket close code (1006, 1011, 1012, 1013 and any other
// unexpected code) to UNAVAILABLE.
func ClassifyWSClose(p provider.Name, c provider.Capability, code int) *provider.Error {
	e := provider.NewError(p, c, provider.KindUnavailable, "websocket closed with code "+strconv.Itoa(code))
	e.Retryable = true
	e.VendorCode = "ws_close_" + strconv.Itoa(code)
	return e
}
