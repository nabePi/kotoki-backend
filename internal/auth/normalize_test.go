package auth

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

func requireFieldError(t *testing.T, err error, field, reason string) {
	t.Helper()
	require.Error(t, err)
	require.Equal(t, errs.CodeValidationFailed, errs.CodeOf(err))
	require.Equal(t, map[string]any{"fields": []map[string]any{{"field": field, "reason": reason}}}, errs.DetailsOf(err))
}

func TestNormalizeEmail(t *testing.T) {
	got, err := NormalizeEmail("  Hana@Example.com ")
	require.NoError(t, err)
	require.Equal(t, "hana@example.com", got)

	for _, bad := range []string{`"N" <a@b.com>`, "a@b", "a@@b.com", strings.Repeat("a", 250) + "@b.com", ""} {
		_, err := NormalizeEmail(bad)
		requireFieldError(t, err, "email", "invalid_format")
	}
}

func TestNormalizePhone(t *testing.T) {
	got, err := NormalizePhone("+62 812-3456-789")
	require.NoError(t, err)
	require.Equal(t, "+628123456789", got)

	_, err = NormalizePhone("08123456789")
	requireFieldError(t, err, "phone_number", "invalid_format")
}

func TestNormalizeFullName(t *testing.T) {
	got, err := NormalizeFullName("  Café ")
	require.NoError(t, err)
	require.Equal(t, "Café", got)

	_, err = NormalizeFullName("Ha\x00na")
	requireFieldError(t, err, "full_name", "invalid_format")
	_, err = NormalizeFullName(strings.Repeat("a", 101))
	requireFieldError(t, err, "full_name", "invalid_format")
	_, err = NormalizeFullName(strings.Repeat("a", 100))
	require.NoError(t, err)
	_, err = NormalizeFullName("   ")
	requireFieldError(t, err, "full_name", "invalid_format")
}

func TestMaskEmail(t *testing.T) {
	require.Equal(t, "h***@example.com", MaskEmail("hana@example.com"))
}

func TestEmailHash(t *testing.T) {
	h := EmailHash("  Hana@Example.com ")
	require.Len(t, h, 32)
	require.Equal(t, h, EmailHash("hana@example.com"))
	require.Regexp(t, `^[0-9a-f]{32}$`, h)
}
