package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"net/mail"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

var phoneRE = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)

func fieldError(field string) error {
	return errs.WithDetails(
		errs.New(errs.CodeValidationFailed, "invalid "+field),
		map[string]any{"fields": []map[string]any{{"field": field, "reason": "invalid_format"}}},
	)
}

// NormalizeEmail trims, validates and lowercases an email (spec 3.5).
func NormalizeEmail(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 254 {
		return "", fieldError("email")
	}
	addr, err := mail.ParseAddress(s)
	if err != nil || addr.Address != s || strings.Count(s, "@") != 1 {
		return "", fieldError("email")
	}
	at := strings.IndexByte(s, '@')
	if !strings.Contains(s[at+1:], ".") {
		return "", fieldError("email")
	}
	return strings.ToLower(s), nil
}

// NormalizePhone strips spaces and dashes and requires E.164.
func NormalizePhone(s string) (string, error) {
	s = strings.NewReplacer(" ", "", "-", "").Replace(s)
	if !phoneRE.MatchString(s) {
		return "", fieldError("phone_number")
	}
	return s, nil
}

// NormalizeFullName trims, NFC-normalizes and bounds a display name.
func NormalizeFullName(s string) (string, error) {
	s = norm.NFC.String(strings.TrimSpace(s))
	n := utf8.RuneCountInString(s)
	if n < 1 || n > 100 {
		return "", fieldError("full_name")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return "", fieldError("full_name")
		}
	}
	return s, nil
}

// MaskEmail returns h***@example.com for log output.
func MaskEmail(s string) string {
	at := strings.IndexByte(s, '@')
	if at <= 0 {
		return "***"
	}
	_, size := utf8.DecodeRuneInString(s)
	return s[:size] + "***" + s[at:]
}

// EmailHash returns 32 hex chars of sha256(lowercase(trim(email))) for logs.
func EmailHash(s string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(s))))
	return hex.EncodeToString(sum[:])[:32]
}
