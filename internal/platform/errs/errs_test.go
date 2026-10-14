package errs

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func lineOf() int { _, _, l, _ := runtime.Caller(1); return l }

func TestNewRecordsCallerLocation(t *testing.T) {
	line := lineOf() + 1
	err := New(CodeBalanceInsufficient, "x")
	loc, ok := LocationOf(err)
	if !ok {
		t.Fatal("no location")
	}
	if loc.File != "internal/platform/errs/errs_test.go" {
		t.Errorf("file = %q", loc.File)
	}
	if loc.Line != line {
		t.Errorf("line = %d want %d", loc.Line, line)
	}
	if !strings.Contains(loc.Function, "TestNewRecordsCallerLocation") {
		t.Errorf("function = %q", loc.Function)
	}
	if err.Error() != "x" {
		t.Errorf("Error() = %q", err.Error())
	}
}

func TestLocationThroughWrapLayers(t *testing.T) {
	e1 := errors.New("pgx")
	lineA := lineOf() + 1
	w := Wrap(e1, CodeInternalError, "insert")
	outer := fmt.Errorf("svc: %w", fmt.Errorf("repo: %w", w))
	loc, ok := LocationOf(outer)
	if !ok || loc.Line != lineA {
		t.Fatalf("loc = %+v ok=%v want line %d", loc, ok, lineA)
	}
	w2 := Wrap(w, CodeEmailAlreadyRegistered, "y")
	loc2, _ := LocationOf(w2)
	if loc2.Line != lineA {
		t.Errorf("wrap overwrote location: %d want %d", loc2.Line, lineA)
	}
	if !errors.Is(w, e1) || !errors.Is(outer, e1) || !errors.Is(w2, e1) {
		t.Error("errors.Is chain broken")
	}
	if CodeOf(outer) != CodeInternalError {
		t.Errorf("CodeOf through %%w = %q", CodeOf(outer))
	}
	if CodeOf(w2) != CodeEmailAlreadyRegistered {
		t.Errorf("outer wrap code = %q", CodeOf(w2))
	}
	if w.Error() != "insert: pgx" {
		t.Errorf("Error() = %q", w.Error())
	}
	if _, ok := LocationOf(e1); ok {
		t.Error("plain error should have no location")
	}
	if _, ok := LocationOf(nil); ok {
		t.Error("nil should have no location")
	}
	if Wrap(nil, CodeInternalError, "x") == nil {
		t.Error("Wrap(nil) should still produce an error")
	}
}

func TestKindOfAllCodes(t *testing.T) {
	internal := map[Code]bool{CodeInternalError: true, CodeContentMisconfigured: true,
		CodeServiceUnavailable: true, CodeProviderFailure: true, CodeAiGenerationFailed: true}
	for _, c := range AllCodes() {
		want := KindBusiness
		if internal[c] {
			want = KindInternal
		}
		if got := KindOf(New(c, "m")); got != want {
			t.Errorf("KindOf(%s) = %v want %v", c, got, want)
		}
	}
	if KindOf(errors.New("x")) != KindInternal || KindOf(nil) != KindInternal {
		t.Error("unknown must be KindInternal")
	}
	if KindOf(New(Code("NOPE"), "x")) != KindInternal {
		t.Error("unregistered code must be KindInternal")
	}
	if CodeOf(errors.New("x")) != "" || CodeOf(nil) != "" {
		t.Error("CodeOf plain must be empty")
	}
}

func pascal(code string) string {
	var b strings.Builder
	for _, p := range strings.Split(code, "_") {
		b.WriteString(strings.ToUpper(p[:1]) + strings.ToLower(p[1:]))
	}
	return "Code" + b.String()
}

func TestCodeConstantNames(t *testing.T) {
	table := []struct {
		c Code
		s string
	}{
		{CodeAccountHasActiveSession, "ACCOUNT_HAS_ACTIVE_SESSION"},
		{CodeAccountSuspended, "ACCOUNT_SUSPENDED"},
		{CodeAiGenerationFailed, "AI_GENERATION_FAILED"},
		{CodeAudioFrameInvalid, "AUDIO_FRAME_INVALID"},
		{CodeAuthRequired, "AUTH_REQUIRED"},
		{CodeBalanceAdjustmentNegative, "BALANCE_ADJUSTMENT_NEGATIVE"},
		{CodeBalanceInsufficient, "BALANCE_INSUFFICIENT"},
		{CodeCannotModifySelf, "CANNOT_MODIFY_SELF"},
		{CodeCharacterNotAvailable, "CHARACTER_NOT_AVAILABLE"},
		{CodeCommandRejected, "COMMAND_REJECTED"},
		{CodeContentInvalidTransition, "CONTENT_INVALID_TRANSITION"},
		{CodeContentMisconfigured, "CONTENT_MISCONFIGURED"},
		{CodeContentValidationFailed, "CONTENT_VALIDATION_FAILED"},
		{CodeContentVersionRequired, "CONTENT_VERSION_REQUIRED"},
		{CodeEmailAlreadyRegistered, "EMAIL_ALREADY_REGISTERED"},
		{CodeEvaluationNotAvailable, "EVALUATION_NOT_AVAILABLE"},
		{CodeForbidden, "FORBIDDEN"},
		{CodeHandshakeTimeout, "HANDSHAKE_TIMEOUT"},
		{CodeHeartbeatTimeout, "HEARTBEAT_TIMEOUT"},
		{CodeIdempotencyKeyInvalid, "IDEMPOTENCY_KEY_INVALID"},
		{CodeIdempotencyKeyRequired, "IDEMPOTENCY_KEY_REQUIRED"},
		{CodeIdempotencyKeyReused, "IDEMPOTENCY_KEY_REUSED"},
		{CodeInternalError, "INTERNAL_ERROR"},
		{CodeInvalidCredentials, "INVALID_CREDENTIALS"},
		{CodeInvalidCursor, "INVALID_CURSOR"},
		{CodeInvalidJson, "INVALID_JSON"},
		{CodeInvalidMessage, "INVALID_MESSAGE"},
		{CodeInvalidUserStatusTransition, "INVALID_USER_STATUS_TRANSITION"},
		{CodeMemoryNotFound, "MEMORY_NOT_FOUND"},
		{CodeMethodNotAllowed, "METHOD_NOT_ALLOWED"},
		{CodeNotFound, "NOT_FOUND"},
		{CodeOriginNotAllowed, "ORIGIN_NOT_ALLOWED"},
		{CodePasswordTooWeak, "PASSWORD_TOO_WEAK"},
		{CodePayloadTooLarge, "PAYLOAD_TOO_LARGE"},
		{CodePhoneAlreadyRegistered, "PHONE_ALREADY_REGISTERED"},
		{CodeProtocolViolation, "PROTOCOL_VIOLATION"},
		{CodeProviderFailure, "PROVIDER_FAILURE"},
		{CodeRateLimited, "RATE_LIMITED"},
		{CodeRefreshTokenInvalid, "REFRESH_TOKEN_INVALID"},
		{CodeRefreshTokenRotated, "REFRESH_TOKEN_ROTATED"},
		{CodeResetTokenInvalid, "RESET_TOKEN_INVALID"},
		{CodeScenarioNotAvailable, "SCENARIO_NOT_AVAILABLE"},
		{CodeScenarioTopicMismatch, "SCENARIO_TOPIC_MISMATCH"},
		{CodeServerAtCapacity, "SERVER_AT_CAPACITY"},
		{CodeServerDraining, "SERVER_DRAINING"},
		{CodeServiceUnavailable, "SERVICE_UNAVAILABLE"},
		{CodeSessionAlreadyOpen, "SESSION_ALREADY_OPEN"},
		{CodeSessionNotCancellable, "SESSION_NOT_CANCELLABLE"},
		{CodeSessionNotConnectable, "SESSION_NOT_CONNECTABLE"},
		{CodeSessionNotEnded, "SESSION_NOT_ENDED"},
		{CodeSessionNotFound, "SESSION_NOT_FOUND"},
		{CodeSessionNotResumable, "SESSION_NOT_RESUMABLE"},
		{CodeSessionStartDeadlinePassed, "SESSION_START_DEADLINE_PASSED"},
		{CodeSessionStateConflict, "SESSION_STATE_CONFLICT"},
		{CodeSessionTakenOver, "SESSION_TAKEN_OVER"},
		{CodeSlowConsumer, "SLOW_CONSUMER"},
		{CodeTermsNotAccepted, "TERMS_NOT_ACCEPTED"},
		{CodeTicketInvalid, "TICKET_INVALID"},
		{CodeTokenExpired, "TOKEN_EXPIRED"},
		{CodeTokenInvalid, "TOKEN_INVALID"},
		{CodeTopicNotAvailable, "TOPIC_NOT_AVAILABLE"},
		{CodeTranscriptExpired, "TRANSCRIPT_EXPIRED"},
		{CodeUnsupportedAudioFormat, "UNSUPPORTED_AUDIO_FORMAT"},
		{CodeUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE"},
		{CodeUnsupportedProtocolVersion, "UNSUPPORTED_PROTOCOL_VERSION"},
		{CodeUserNotFound, "USER_NOT_FOUND"},
		{CodeValidationFailed, "VALIDATION_FAILED"},
	}
	if len(table) != 67 {
		t.Fatalf("table has %d pairs", len(table))
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "codes.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{} // value -> const name
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, id := range vs.Names {
			if bl, ok := vs.Values[i].(*ast.BasicLit); ok {
				names[strings.Trim(bl.Value, `"`)] = id.Name
			}
		}
		return true
	})
	var got []string
	for _, p := range table {
		if string(p.c) != p.s {
			t.Errorf("%s != %s", p.c, p.s)
		}
		if names[p.s] != pascal(p.s) {
			t.Errorf("const name for %s = %q want %q", p.s, names[p.s], pascal(p.s))
		}
		got = append(got, p.s)
	}
	if len(names) != 67 {
		t.Errorf("codes.go declares %d constants", len(names))
	}
	var all []string
	for _, c := range AllCodes() {
		all = append(all, string(c))
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(all, ",") {
		t.Error("table != AllCodes()")
	}
	if !sort.StringsAreSorted(all) {
		t.Error("AllCodes not sorted")
	}
}

func TestMessagesComplete(t *testing.T) {
	if len(AllCodes()) != 67 || len(Messages) != 67 {
		t.Fatalf("codes=%d messages=%d", len(AllCodes()), len(Messages))
	}
	for _, c := range AllCodes() {
		if Messages[c] == "" {
			t.Errorf("empty message for %s", c)
		}
	}
	want := map[Code]string{
		CodeBalanceInsufficient: "Saldo menit Anda tidak cukup untuk durasi yang dipilih.",
		CodeInternalError:       "Terjadi kesalahan pada sistem. Silakan coba lagi.",
		CodeValidationFailed:    "Data yang dikirim tidak valid. Periksa kembali isian Anda.",
		CodeTicketInvalid:       "Koneksi tidak valid atau sudah kedaluwarsa. Menyambungkan ulang…",
	}
	for c, m := range want {
		if Messages[c] != m {
			t.Errorf("Messages[%s] = %q", c, Messages[c])
		}
	}
}

func TestDetails(t *testing.T) {
	d := map[string]any{"available_seconds": 600}
	err := fmt.Errorf("ctx: %w", WithDetails(New(CodeBalanceInsufficient, "x"), d))
	got, ok := DetailsOf(err).(map[string]any)
	if !ok || got["available_seconds"] != 600 {
		t.Errorf("details = %v", DetailsOf(err))
	}
	if DetailsOf(nil) != nil || DetailsOf(errors.New("x")) != nil {
		t.Error("expected nil details")
	}
	plain := WithDetails(errors.New("p"), 1)
	if DetailsOf(plain) != 1 {
		t.Error("WithDetails on plain error lost details")
	}
	if WithDetails(nil, 1) != nil {
		t.Error("WithDetails(nil) must be nil")
	}
	if CodeOf(WithDetails(New(CodeNotFound, "x"), 1)) != CodeNotFound {
		t.Error("code lost through WithDetails")
	}
}

func TestRemovedContentCodesAbsent(t *testing.T) {
	removed := []string{"CONVERSATION_STYLE_NOT_AVAILABLE", "EXPRESSION_PROFILE_NOT_AVAILABLE", "CONTENT_IN_USE_AS_DEFAULT"}
	for _, c := range AllCodes() {
		for _, r := range removed {
			if string(c) == r {
				t.Errorf("AllCodes still contains %s", r)
			}
		}
	}
	for c := range Messages {
		for _, r := range removed {
			if string(c) == r {
				t.Errorf("Messages still contains %s", r)
			}
		}
	}
	f, err := parser.ParseFile(token.NewFileSet(), "codes.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			switch id.Name {
			case "CodeConversationStyleNotAvailable", "CodeExpressionProfileNotAvailable", "CodeContentInUseAsDefault":
				t.Errorf("codes.go still declares %s", id.Name)
			}
		}
		return true
	})
}

// wsOnly are the codes that appear only in the WebSocket catalog (spec 7.4.8).
var wsOnly = map[Code]bool{
	CodeAiGenerationFailed: true, CodeAudioFrameInvalid: true, CodeCommandRejected: true, CodeHandshakeTimeout: true,
	CodeHeartbeatTimeout: true, CodeInvalidMessage: true, CodeProtocolViolation: true, CodeProviderFailure: true,
	CodeServerAtCapacity: true, CodeServerDraining: true, CodeSessionNotResumable: true, CodeSessionStateConflict: true,
	CodeSessionTakenOver: true, CodeSlowConsumer: true, CodeTicketInvalid: true, CodeUnsupportedAudioFormat: true,
	CodeUnsupportedProtocolVersion: true,
}

func TestRestAndWSCounts(t *testing.T) {
	rest, ws := 0, 0
	for _, c := range AllCodes() {
		if wsOnly[c] {
			ws++
		} else {
			rest++
		}
	}
	if rest != 50 || ws != 17 {
		t.Fatalf("rest=%d ws-only=%d, want 50 and 17", rest, ws)
	}
}
