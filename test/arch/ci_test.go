package arch

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func readCI(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(testutil.ModuleRoot(t), ".github", "workflows", "ci.yml"))
	require.NoError(t, err)
	return string(b)
}

func TestCIWorkflowShape(t *testing.T) {
	ci := readCI(t)

	for _, job := range []string{"lint", "unit", "integration", "e2e", "build-images"} {
		require.Regexp(t, `(?m)^  `+job+`:$`, ci, "job %s", job)
	}

	pinned := regexp.MustCompile(`@[0-9a-f]{40}(\s|$)`)
	uses := regexp.MustCompile(`(?m)^\s*(?:-\s+)?uses:\s*(\S+)`)
	matches := uses.FindAllStringSubmatch(ci, -1)
	require.NotEmpty(t, matches)
	for _, m := range matches {
		require.Regexp(t, pinned, m[1]+"\n", "action %s must be pinned to a commit SHA", m[1])
	}

	for _, cmd := range []string{
		"make tidy-check sqlc-check migrations-check modelname-check", "make vuln", "make arch-check",
		"make test-cover", "make test-integration", "make test-e2e",
	} {
		require.Contains(t, ci, "run: "+cmd, cmd)
	}

	head, _, ok := strings.Cut(ci, "\njobs:")
	require.True(t, ok)
	require.Regexp(t, `(?m)^permissions:\n  contents: read$`, head)
	require.NotContains(t, head, "packages: write")
	before, after, ok := strings.Cut(ci, "\n  build-images:")
	require.True(t, ok)
	require.NotContains(t, before, "packages: write")
	require.Contains(t, after, "packages: write")
	require.Contains(t, ci, "image: [api, realtime, worker, migrate, seed]")
}

// buildImagesJob returns the text of the build-images job (through the end of the file: it is the last job).
func buildImagesJob(t *testing.T) string {
	t.Helper()
	_, job, ok := strings.Cut(readCI(t), "\n  build-images:")
	require.True(t, ok)
	return job
}

func TestCIBuildImagesMatrix(t *testing.T) {
	job := buildImagesJob(t)

	m := regexp.MustCompile(`(?m)^\s+image: \[([^\]]*)\]\s*$`).FindAllStringSubmatch(job, -1)
	require.Len(t, m, 1, "exactly one image matrix")
	var got []string
	for _, n := range strings.Split(m[0][1], ",") {
		got = append(got, strings.TrimSpace(n))
	}
	require.Equal(t, []string{"api", "realtime", "worker", "migrate", "seed"}, got)

	require.Contains(t, job, "file: deploy/docker/${{ matrix.image }}.Dockerfile")
	require.Contains(t, job, "tags: ghcr.io/nabepi/kotoki-backend/${{ matrix.image }}:${{ github.sha }}")
	require.NotRegexp(t, `:latest\b`, job, "images are tagged by git SHA only")

	require.Regexp(t, `uses: anchore/sbom-action@[0-9a-f]{40}`, job)
	require.Contains(t, job, "format: cyclonedx-json")
	require.Contains(t, job, "artifact-name: sbom-${{ matrix.image }}.cdx.json")
	require.Contains(t, job, "image: ghcr.io/nabepi/kotoki-backend/${{ matrix.image }}:${{ github.sha }}")

	require.Contains(t, job, "docker buildx imagetools inspect")
	require.Contains(t, job, "name: digests-${{ matrix.image }}")
	require.Contains(t, job, "${{ matrix.image }}.digest")
	require.Contains(t, job, "aquasecurity/trivy-action@")
	require.Contains(t, job, "severity: CRITICAL,HIGH")
	require.Contains(t, job, `exit-code: "1"`)
}
