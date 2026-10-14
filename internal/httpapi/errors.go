package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// statusByCode maps the REST error catalog (spec 5.10) to HTTP statuses. Codes
// that exist only in the WebSocket catalog are intentionally absent: a handler
// that returns one has a bug and the client gets a generic 500.
var statusByCode = map[errs.Code]int{
	errs.CodeInvalidJson:                 400,
	errs.CodeValidationFailed:            400,
	errs.CodeInvalidCursor:               400,
	errs.CodeIdempotencyKeyRequired:      400,
	errs.CodeIdempotencyKeyInvalid:       400,
	errs.CodePasswordTooWeak:             400,
	errs.CodeResetTokenInvalid:           400,
	errs.CodeAuthRequired:                401,
	errs.CodeTokenInvalid:                401,
	errs.CodeTokenExpired:                401,
	errs.CodeInvalidCredentials:          401,
	errs.CodeRefreshTokenInvalid:         401,
	errs.CodeBalanceInsufficient:         402,
	errs.CodeForbidden:                   403,
	errs.CodeAccountSuspended:            403,
	errs.CodeTermsNotAccepted:            403,
	errs.CodeOriginNotAllowed:            403,
	errs.CodeNotFound:                    404,
	errs.CodeUserNotFound:                404,
	errs.CodeSessionNotFound:             404,
	errs.CodeEvaluationNotAvailable:      404,
	errs.CodeMemoryNotFound:              404,
	errs.CodeMethodNotAllowed:            405,
	errs.CodeEmailAlreadyRegistered:      409,
	errs.CodePhoneAlreadyRegistered:      409,
	errs.CodeRefreshTokenRotated:         409,
	errs.CodeIdempotencyKeyReused:        409,
	errs.CodeSessionAlreadyOpen:          409,
	errs.CodeSessionNotCancellable:       409,
	errs.CodeSessionNotConnectable:       409,
	errs.CodeSessionStartDeadlinePassed:  409,
	errs.CodeSessionNotEnded:             409,
	errs.CodeAccountHasActiveSession:     409,
	errs.CodeBalanceAdjustmentNegative:   409,
	errs.CodeCannotModifySelf:            409,
	errs.CodeInvalidUserStatusTransition: 409,
	errs.CodeContentInvalidTransition:    409,
	errs.CodeContentVersionRequired:      409,
	errs.CodeTranscriptExpired:           410,
	errs.CodePayloadTooLarge:             413,
	errs.CodeUnsupportedMediaType:        415,
	errs.CodeTopicNotAvailable:           422,
	errs.CodeScenarioNotAvailable:        422,
	errs.CodeScenarioTopicMismatch:       422,
	errs.CodeCharacterNotAvailable:       422,
	errs.CodeContentValidationFailed:     422,
	errs.CodeRateLimited:                 429,
	errs.CodeInternalError:               500,
	errs.CodeContentMisconfigured:        500,
	errs.CodeServiceUnavailable:          503}

// StatusOf returns the HTTP status of code; unknown codes are 500.
func StatusOf(c errs.Code) int {
	if s, ok := statusByCode[c]; ok {
		return s
	}
	return http.StatusInternalServerError
}

// FieldError is a 400 VALIDATION_FAILED for one field (spec 5.8.2).
func FieldError(field, reason string) error {
	return errs.WithDetails(errs.New(errs.CodeValidationFailed, "invalid "+field),
		map[string]any{"fields": []map[string]any{{"field": field, "reason": reason}}})
}

type errorObject struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
	Details   any    `json:"details"`
}

type errorEnvelope struct {
	Error errorObject `json:"error"`
}

// WriteError renders err as the fixed error object (spec 5.8.2). Unknown codes
// become 500 INTERNAL_ERROR without leaking the original code, message or details.
// Internal errors are logged once at ERROR with their code and origin.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	code := errs.CodeOf(err)
	_, known := statusByCode[code]
	if !known {
		code = errs.CodeInternalError
	}
	status := StatusOf(code)
	reqID := RequestIDFrom(r.Context())
	if reqID == "" {
		reqID = newRequestID()
	}

	if !known || errs.KindOf(err) == errs.KindInternal {
		args := []any{"error_code", string(errs.CodeOf(err)), "request_id", reqID, "status", status, "error", err.Error()}
		if loc, ok := errs.LocationOf(err); ok {
			args = append(args, "error_location", fmt.Sprintf("%s:%d %s", loc.File, loc.Line, loc.Function))
		}
		LoggerFrom(r.Context()).ErrorContext(r.Context(), "request failed", args...)
	}

	var details any
	if known {
		details = errs.DetailsOf(err)
	}
	if status == http.StatusServiceUnavailable && w.Header().Get("Retry-After") == "" {
		w.Header().Set("Retry-After", "1")
	}
	WriteJSON(w, status, errorEnvelope{Error: errorObject{Code: string(code), Message: errs.Messages[code], RequestID: reqID, Details: details}})
}

// WriteJSON writes v as JSON with the REST content type.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	var buf []byte
	if v != nil {
		b, err := marshal(v)
		if err != nil {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"code":"INTERNAL_ERROR","message":"` + errs.Messages[errs.CodeInternalError] + `","request_id":"","details":null}}` + "\n"))
			return
		}
		buf = b
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}

func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
