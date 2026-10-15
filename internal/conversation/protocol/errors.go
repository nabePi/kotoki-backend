package protocol

import (
	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// Spec adalah satu baris katalog error WS (spec §7.4.8).
//
// Fatal menentukan dua hal bagi pemanggil: apakah koneksi ditutup dengan Close,
// dan apakah event `error` durable (§7.4.3: durable hanya bila non-fatal).
// IsDurable("error") sengaja selalu false karena keputusan itu bergantung pada
// Fatal; pemanggil memakai `!Catalog[code].Fatal` (atau ErrorPayload.Fatal).
type Spec struct {
	Fatal     bool
	Close     int // close code WS; 0 = tidak ada close (error non-fatal)
	Retryable bool
}

// Catalog adalah tabel §7.4.8. SESSION_STATE_CONFLICT.Retryable = false di sini;
// pemanggil menimpa dengan WithRetryable(true) hanya bila status sesi STARTING.
var Catalog = map[errs.Code]Spec{
	errs.CodeTicketInvalid:              {true, 4001, true},
	errs.CodeForbidden:                  {true, 4003, false},
	errs.CodeSessionNotFound:            {true, 4004, false},
	errs.CodeHandshakeTimeout:           {true, 4008, true},
	errs.CodeSessionStateConflict:       {true, 4009, false},
	errs.CodeSessionNotResumable:        {true, 4010, false},
	errs.CodeSessionStartDeadlinePassed: {true, 4010, false},
	errs.CodeSessionTakenOver:           {true, 4011, false},
	errs.CodeRateLimited:                {true, 4029, true},
	errs.CodeServerDraining:             {true, 4100, true},
	errs.CodeSlowConsumer:               {true, 4101, true},
	errs.CodeHeartbeatTimeout:           {true, 4102, true},
	errs.CodeProviderFailure:            {true, 4500, false},
	errs.CodeServerAtCapacity:           {true, 4503, true},
	errs.CodeServiceUnavailable:         {true, 4503, true},
	errs.CodeProtocolViolation:          {true, 1008, false},
	errs.CodeUnsupportedProtocolVersion: {true, 1008, false},
	errs.CodeUnsupportedAudioFormat:     {true, 1008, false},
	errs.CodeInternalError:              {true, 1011, true},
	errs.CodeInvalidMessage:             {false, 0, false},
	errs.CodeCommandRejected:            {false, 0, false},
	errs.CodeAudioFrameInvalid:          {false, 0, false},
	errs.CodeAiGenerationFailed:         {false, 0, false},
}

// CloseCode mengembalikan close code kode error, atau 0 bila non-fatal/tak dikenal.
func CloseCode(code errs.Code) int { return Catalog[code].Close }

// ErrorOption mengubah payload error yang dibangun NewError.
type ErrorOption func(*ErrorPayload)

// WithRetryable menimpa `retryable` dari katalog (mis. SESSION_STATE_CONFLICT
// pada status STARTING).
func WithRetryable(v bool) ErrorOption {
	return func(p *ErrorPayload) { p.Retryable = v }
}

// NewError membangun payload event `error` dengan empat field D-013 yang selalu
// ada. request_id = ref bila ada, selain itu requestID; ref_event_id = ref.
// Pesan dari errs.Messages; retryable/fatal dari Catalog.
func NewError(code errs.Code, requestID uuid.UUID, ref *uuid.UUID, details any, opts ...ErrorOption) ErrorPayload {
	spec := Catalog[code]
	p := ErrorPayload{
		Error: ErrorBody{
			Code:      string(code),
			Message:   errs.Messages[code],
			RequestID: requestID.String(),
			Details:   details,
		},
		Retryable: spec.Retryable,
		Fatal:     spec.Fatal,
	}
	if ref != nil {
		s := ref.String()
		p.Error.RequestID = s
		p.RefEventID = &s
	}
	for _, o := range opts {
		o(&p)
	}
	return p
}
