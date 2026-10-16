package errs_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/httpapi"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

func TestM7CodesHaveMessagesAndStatus(t *testing.T) {
	cases := []struct {
		code    errs.Code
		status  int
		message string
	}{
		{errs.CodeSessionNotEnded, 409, "Evaluasi baru tersedia setelah sesi selesai."},
		{errs.CodeEvaluationNotAvailable, 404, "Evaluasi tidak tersedia karena sesi ini tidak pernah dimulai."},
	}
	for _, c := range cases {
		require.Equal(t, c.message, errs.Messages[c.code], c.code)
		require.Equal(t, c.status, httpapi.StatusOf(c.code), c.code)
	}
}
