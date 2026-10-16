package cartesia

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
)

// vendorError is the documented error message (STT and TTS WebSocket) and, as far as the
// docs show, the HTTP error body: status_code, title, message are required; error_code,
// doc_url, request_id optional.
type vendorError struct {
	StatusCode int    `json:"status_code"`
	Title      string `json:"title"`
	Message    string `json:"message"`
	ErrorCode  string `json:"error_code"`
}

// codeSessionLimit is the vendor code mapped to a rotation signal. GUESS (M9-T10): the docs
// state no session duration limit and no code for it; the value reuses the code
// provider.rotationTriggers already knows, so rotatingSTT reconnects if the vendor ever sends it.
const codeSessionLimit = "session_time_limit_exceeded"

// quotaHints are substrings of error_code that mean "credits exhausted" (GUESS: the pricing
// docs name no code; with overages disabled requests "fail until renewal").
var quotaHints = []string{"quota", "credit", "insufficient", "limit_exceeded_plan"}

func isQuotaCode(code string) bool {
	code = strings.ToLower(code)
	for _, h := range quotaHints {
		if strings.Contains(code, h) {
			return true
		}
	}
	return false
}

// mapError normalises a vendor error: Kind from status_code (spec 8.3), VendorCode =
// error_code verbatim. scrub removes the API key from texts.
func mapError(c provider.Capability, ve vendorError, scrub func(string) string) *provider.Error {
	kind := provider.ClassifyHTTPStatus(ve.StatusCode)
	msg := ve.Message
	if msg == "" {
		msg = ve.Title
	}
	e := provider.NewError(provider.Cartesia, c, kind, httpx.Redact(scrub(msg)))
	e.HTTPStatus = ve.StatusCode
	e.VendorCode = httpx.Redact(scrub(ve.ErrorCode))
	switch {
	case ve.ErrorCode == codeSessionLimit:
		e.Kind, e.Retryable = provider.KindUnavailable, true
	case kind == provider.KindRateLimited && isQuotaCode(ve.ErrorCode):
		e.Retryable = false
	case kind == provider.KindTimeout, kind == provider.KindRateLimited, kind == provider.KindUnavailable:
		e.Retryable = true
	}
	return e
}

// classifyHTTP maps a non-2xx handshake/HTTP response.
func classifyHTTP(c provider.Capability, resp *http.Response, body []byte, scrub func(string) string) *provider.Error {
	var ve vendorError
	_ = json.Unmarshal(body, &ve)
	if ve.StatusCode == 0 {
		ve.StatusCode = resp.StatusCode
	}
	if ve.Message == "" && ve.Title == "" {
		ve.Message = http.StatusText(resp.StatusCode) // never echo a raw body
	}
	e := mapError(c, ve, scrub)
	e.HTTPStatus = resp.StatusCode
	e.RetryAfter = httpx.ClassifyHTTP(provider.Cartesia, c, resp, nil).RetryAfter
	return e
}
