package auth

import (
	"bufio"
	_ "embed" // common_passwords.txt
	"io"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// commonPasswordsFile is the 10k most common passwords, lowercase, one per line.
// Source: danielmiessler/SecLists Passwords/Common-Credentials/10k-most-common.txt (MIT).
//
//go:embed common_passwords.txt
var commonPasswordsFile string

const (
	minPasswordLen = 12
	maxPasswordLen = 128
)

// PasswordPolicy validates passwords against spec 3.4.
type PasswordPolicy struct{ common map[string]struct{} }

// NewPasswordPolicy loads the common-password list from r (one lowercase entry per line).
func NewPasswordPolicy(common io.Reader) *PasswordPolicy {
	p := &PasswordPolicy{common: map[string]struct{}{}}
	sc := bufio.NewScanner(common)
	for sc.Scan() {
		if w := strings.TrimSpace(sc.Text()); w != "" {
			p.common[strings.ToLower(w)] = struct{}{}
		}
	}
	return p
}

var defaultPolicy = NewPasswordPolicy(strings.NewReader(commonPasswordsFile))

func weak(reason string) error {
	return errs.WithDetails(errs.New(errs.CodePasswordTooWeak, "password too weak"), map[string]any{"reason": reason})
}

// Validate checks pw (NFC-normalized, not trimmed) for the account email.
func (p *PasswordPolicy) Validate(pw, email string) error {
	pw = norm.NFC.String(pw)
	switch n := utf8.RuneCountInString(pw); {
	case n > maxPasswordLen:
		return weak("TOO_LONG")
	case n < minPasswordLen:
		return weak("TOO_SHORT")
	}
	if strings.IndexFunc(pw, func(r rune) bool { return !unicode.IsSpace(r) }) < 0 {
		return weak("BLANK")
	}
	lower := strings.ToLower(pw)
	email = strings.ToLower(strings.TrimSpace(email))
	local := email
	if at := strings.IndexByte(email, '@'); at >= 0 {
		local = email[:at]
	}
	if lower == email || (local != "" && lower == local) {
		return weak("CONTAINS_EMAIL")
	}
	if _, ok := p.common[lower]; ok {
		return weak("COMMON_PASSWORD")
	}
	return nil
}

// ValidatePassword validates pw with the embedded common-password list.
func ValidatePassword(pw, email string) error { return defaultPolicy.Validate(pw, email) }
