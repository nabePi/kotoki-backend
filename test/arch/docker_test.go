//go:build integration

package arch

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func containerEngine(t *testing.T) string {
	t.Helper()
	for _, e := range []string{"docker", "podman"} {
		if p, err := exec.LookPath(e); err == nil {
			return p
		}
	}
	t.Skip("no docker or podman on PATH")
	return ""
}

func run(t *testing.T, name string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command(name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

func TestDockerImagesBuild(t *testing.T) {
	engine := containerEngine(t)
	root := testutil.ModuleRoot(t)
	for _, b := range []string{"api", "realtime", "worker", "migrate", "seed"} {
		t.Run("build "+b, func(t *testing.T) {
			_, stderr, err := run(t, engine, "build", "-f", root+"/deploy/docker/"+b+".Dockerfile",
				"--build-arg", "VERSION=test", "--build-arg", "GIT_COMMIT=test", "-t", "kotoki/"+b+":test", root)
			require.NoError(t, err, stderr)
		})
	}

	_, stderr, err := run(t, engine, "run", "--rm", "kotoki/api:test")
	require.Error(t, err)
	require.Contains(t, stderr, "DB_URL")

	for _, b := range []string{"api", "worker"} {
		_, _, err := run(t, engine, "run", "--rm", "--entrypoint", "sh", "kotoki/"+b+":test", "-c", "true")
		require.Error(t, err, b+" image must not contain a shell")
		user, stderr, err := run(t, engine, "inspect", "--format", "{{.Config.User}}", "kotoki/"+b+":test")
		require.NoError(t, err, stderr)
		require.Equal(t, "65532:65532", strings.TrimSpace(user), b)
	}
}
