package errs

// Code is a stable machine-readable error code (spec 5.10 and 7.4.8).
type Code string

// Canonical constant name: "Code" + each "_" segment capitalised, rest lower-case.
const (
	CodeAccountHasActiveSession       Code = "ACCOUNT_HAS_ACTIVE_SESSION"
	CodeAccountSuspended              Code = "ACCOUNT_SUSPENDED"
	CodeAiGenerationFailed            Code = "AI_GENERATION_FAILED"
	CodeAudioFrameInvalid             Code = "AUDIO_FRAME_INVALID"
	CodeAuthRequired                  Code = "AUTH_REQUIRED"
	CodeBalanceAdjustmentNegative     Code = "BALANCE_ADJUSTMENT_NEGATIVE"
	CodeBalanceInsufficient           Code = "BALANCE_INSUFFICIENT"
	CodeCannotModifySelf              Code = "CANNOT_MODIFY_SELF"
	CodeCharacterNotAvailable         Code = "CHARACTER_NOT_AVAILABLE"
	CodeCommandRejected               Code = "COMMAND_REJECTED"
	CodeContentInvalidTransition      Code = "CONTENT_INVALID_TRANSITION"
	CodeContentInUseAsDefault         Code = "CONTENT_IN_USE_AS_DEFAULT"
	CodeContentMisconfigured          Code = "CONTENT_MISCONFIGURED"
	CodeContentValidationFailed       Code = "CONTENT_VALIDATION_FAILED"
	CodeContentVersionRequired        Code = "CONTENT_VERSION_REQUIRED"
	CodeConversationStyleNotAvailable Code = "CONVERSATION_STYLE_NOT_AVAILABLE"
	CodeEmailAlreadyRegistered        Code = "EMAIL_ALREADY_REGISTERED"
	CodeEvaluationNotAvailable        Code = "EVALUATION_NOT_AVAILABLE"
	CodeExpressionProfileNotAvailable Code = "EXPRESSION_PROFILE_NOT_AVAILABLE"
	CodeForbidden                     Code = "FORBIDDEN"
	CodeHandshakeTimeout              Code = "HANDSHAKE_TIMEOUT"
	CodeHeartbeatTimeout              Code = "HEARTBEAT_TIMEOUT"
	CodeIdempotencyKeyInvalid         Code = "IDEMPOTENCY_KEY_INVALID"
	CodeIdempotencyKeyRequired        Code = "IDEMPOTENCY_KEY_REQUIRED"
	CodeIdempotencyKeyReused          Code = "IDEMPOTENCY_KEY_REUSED"
	CodeInternalError                 Code = "INTERNAL_ERROR"
	CodeInvalidCredentials            Code = "INVALID_CREDENTIALS" //nolint:gosec // G101: error code identifier, not a credential
	CodeInvalidCursor                 Code = "INVALID_CURSOR"
	CodeInvalidJson                   Code = "INVALID_JSON" //nolint:revive // name fixed by spec error-code catalog
	CodeInvalidMessage                Code = "INVALID_MESSAGE"
	CodeInvalidUserStatusTransition   Code = "INVALID_USER_STATUS_TRANSITION"
	CodeMemoryNotFound                Code = "MEMORY_NOT_FOUND"
	CodeMethodNotAllowed              Code = "METHOD_NOT_ALLOWED"
	CodeNotFound                      Code = "NOT_FOUND"
	CodeOriginNotAllowed              Code = "ORIGIN_NOT_ALLOWED"
	CodePasswordTooWeak               Code = "PASSWORD_TOO_WEAK"
	CodePayloadTooLarge               Code = "PAYLOAD_TOO_LARGE"
	CodePhoneAlreadyRegistered        Code = "PHONE_ALREADY_REGISTERED"
	CodeProtocolViolation             Code = "PROTOCOL_VIOLATION"
	CodeProviderFailure               Code = "PROVIDER_FAILURE"
	CodeRateLimited                   Code = "RATE_LIMITED"
	CodeRefreshTokenInvalid           Code = "REFRESH_TOKEN_INVALID"
	CodeRefreshTokenRotated           Code = "REFRESH_TOKEN_ROTATED" //nolint:gosec // G101: error code identifier, not a credential
	CodeResetTokenInvalid             Code = "RESET_TOKEN_INVALID"   //nolint:gosec // G101: error code identifier, not a credential
	CodeScenarioNotAvailable          Code = "SCENARIO_NOT_AVAILABLE"
	CodeScenarioTopicMismatch         Code = "SCENARIO_TOPIC_MISMATCH"
	CodeServerAtCapacity              Code = "SERVER_AT_CAPACITY"
	CodeServerDraining                Code = "SERVER_DRAINING"
	CodeServiceUnavailable            Code = "SERVICE_UNAVAILABLE"
	CodeSessionAlreadyOpen            Code = "SESSION_ALREADY_OPEN"
	CodeSessionNotCancellable         Code = "SESSION_NOT_CANCELLABLE"
	CodeSessionNotConnectable         Code = "SESSION_NOT_CONNECTABLE"
	CodeSessionNotEnded               Code = "SESSION_NOT_ENDED"
	CodeSessionNotFound               Code = "SESSION_NOT_FOUND"
	CodeSessionNotResumable           Code = "SESSION_NOT_RESUMABLE"
	CodeSessionStartDeadlinePassed    Code = "SESSION_START_DEADLINE_PASSED"
	CodeSessionStateConflict          Code = "SESSION_STATE_CONFLICT"
	CodeSessionTakenOver              Code = "SESSION_TAKEN_OVER"
	CodeSlowConsumer                  Code = "SLOW_CONSUMER"
	CodeTermsNotAccepted              Code = "TERMS_NOT_ACCEPTED"
	CodeTicketInvalid                 Code = "TICKET_INVALID"
	CodeTokenExpired                  Code = "TOKEN_EXPIRED"
	CodeTokenInvalid                  Code = "TOKEN_INVALID"
	CodeTopicNotAvailable             Code = "TOPIC_NOT_AVAILABLE"
	CodeTranscriptExpired             Code = "TRANSCRIPT_EXPIRED"
	CodeUnsupportedAudioFormat        Code = "UNSUPPORTED_AUDIO_FORMAT"
	CodeUnsupportedMediaType          Code = "UNSUPPORTED_MEDIA_TYPE"
	CodeUnsupportedProtocolVersion    Code = "UNSUPPORTED_PROTOCOL_VERSION"
	CodeUserNotFound                  Code = "USER_NOT_FOUND"
	CodeValidationFailed              Code = "VALIDATION_FAILED"
)

// AllCodes returns every registered code, sorted.
func AllCodes() []Code {
	return []Code{
		CodeAccountHasActiveSession,
		CodeAccountSuspended,
		CodeAiGenerationFailed,
		CodeAudioFrameInvalid,
		CodeAuthRequired,
		CodeBalanceAdjustmentNegative,
		CodeBalanceInsufficient,
		CodeCannotModifySelf,
		CodeCharacterNotAvailable,
		CodeCommandRejected,
		CodeContentInvalidTransition,
		CodeContentInUseAsDefault,
		CodeContentMisconfigured,
		CodeContentValidationFailed,
		CodeContentVersionRequired,
		CodeConversationStyleNotAvailable,
		CodeEmailAlreadyRegistered,
		CodeEvaluationNotAvailable,
		CodeExpressionProfileNotAvailable,
		CodeForbidden,
		CodeHandshakeTimeout,
		CodeHeartbeatTimeout,
		CodeIdempotencyKeyInvalid,
		CodeIdempotencyKeyRequired,
		CodeIdempotencyKeyReused,
		CodeInternalError,
		CodeInvalidCredentials,
		CodeInvalidCursor,
		CodeInvalidJson,
		CodeInvalidMessage,
		CodeInvalidUserStatusTransition,
		CodeMemoryNotFound,
		CodeMethodNotAllowed,
		CodeNotFound,
		CodeOriginNotAllowed,
		CodePasswordTooWeak,
		CodePayloadTooLarge,
		CodePhoneAlreadyRegistered,
		CodeProtocolViolation,
		CodeProviderFailure,
		CodeRateLimited,
		CodeRefreshTokenInvalid,
		CodeRefreshTokenRotated,
		CodeResetTokenInvalid,
		CodeScenarioNotAvailable,
		CodeScenarioTopicMismatch,
		CodeServerAtCapacity,
		CodeServerDraining,
		CodeServiceUnavailable,
		CodeSessionAlreadyOpen,
		CodeSessionNotCancellable,
		CodeSessionNotConnectable,
		CodeSessionNotEnded,
		CodeSessionNotFound,
		CodeSessionNotResumable,
		CodeSessionStartDeadlinePassed,
		CodeSessionStateConflict,
		CodeSessionTakenOver,
		CodeSlowConsumer,
		CodeTermsNotAccepted,
		CodeTicketInvalid,
		CodeTokenExpired,
		CodeTokenInvalid,
		CodeTopicNotAvailable,
		CodeTranscriptExpired,
		CodeUnsupportedAudioFormat,
		CodeUnsupportedMediaType,
		CodeUnsupportedProtocolVersion,
		CodeUserNotFound,
		CodeValidationFailed,
	}
}
