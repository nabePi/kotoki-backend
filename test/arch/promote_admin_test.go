package arch

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func TestPromoteAdminTargetExists(t *testing.T) {
	cmd := exec.Command("make", "-n", "promote-admin", "EMAIL=ops@kotoki.id", "DB_URL=postgres://x")
	cmd.Dir = testutil.ModuleRoot(t)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	require.Contains(t, string(out), "UPDATE users SET role='ADMIN'")
	require.Contains(t, string(out), "ops@kotoki.id")
}
