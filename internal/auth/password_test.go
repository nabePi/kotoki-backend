package auth

import (
	"bufio"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

func requireWeak(t *testing.T, err error, reason string) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, errs.CodePasswordTooWeak, errs.CodeOf(err))
	require.Equal(t, map[string]any{"reason": reason}, errs.DetailsOf(err))
}

func TestValidatePassword(t *testing.T) {
	policy := NewPasswordPolicy(strings.NewReader("qwertyuiop12\n"))
	email := "hanatanaka01@example.com"

	requireWeak(t, policy.Validate(strings.Repeat("a", 11), email), "TOO_SHORT")
	requireWeak(t, policy.Validate(strings.Repeat("a", 129), email), "TOO_LONG")
	requireWeak(t, policy.Validate(strings.Repeat(" ", 12), email), "BLANK")
	requireWeak(t, policy.Validate("HANATANAKA01", email), "CONTAINS_EMAIL")
	requireWeak(t, policy.Validate(email, email), "CONTAINS_EMAIL")
	requireWeak(t, policy.Validate("QwertyUIOP12", email), "COMMON_PASSWORD")

	// decomposed e + U+0301 counts as one code point after NFC
	require.NoError(t, policy.Validate(strings.Repeat("é", 12), email))
	requireWeak(t, policy.Validate(strings.Repeat("é", 11), email), "TOO_SHORT")

	require.NoError(t, policy.Validate("correct horse battery", email))
	require.NoError(t, policy.Validate(strings.Repeat("a", 128), email))
}

func TestValidatePasswordPackageFunc(t *testing.T) {
	require.NoError(t, ValidatePassword("correct horse battery", "x@example.com"))
	requireWeak(t, ValidatePassword("unbelievable", "x@example.com"), "COMMON_PASSWORD")
}

func TestCommonPasswordsFile(t *testing.T) {
	sc := bufio.NewScanner(strings.NewReader(commonPasswordsFile))
	n := 0
	for sc.Scan() {
		line := sc.Text()
		require.Equal(t, strings.ToLower(line), line)
		n++
	}
	require.GreaterOrEqual(t, n, 9000)
	require.LessOrEqual(t, n, 10500)
}
