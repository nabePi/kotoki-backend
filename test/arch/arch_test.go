package arch_test

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestForbiddenImportsTable(t *testing.T) {
	const m = "github.com/nabePi/kotoki-backend/"
	tests := []struct {
		name    string
		pkg     string
		imports []string
		want    []string
	}{
		{"feature imports pgx", "internal/session", []string{"github.com/jackc/pgx/v5"}, []string{"github.com/jackc/pgx/v5"}},
		{"feature imports postgres", "internal/auth", []string{m + "internal/postgres"}, []string{m + "internal/postgres"}},
		{"handler imports postgres", "internal/httpapi", []string{m + "internal/postgres"}, []string{m + "internal/postgres"}},
		{"adapter imports handler", "internal/postgres", []string{m + "internal/httpapi"}, []string{m + "internal/httpapi"}},
		{"audio imports opus", "internal/conversation/audio", []string{"gopkg.in/hraban/opus.v2"}, nil},
		{"vendor imports websocket", "internal/provider/openai", []string{"github.com/coder/websocket"}, nil},
		{"cartesia imports websocket", "internal/provider/cartesia", []string{"github.com/coder/websocket"}, nil},
		{"elevenlabs imports websocket", "internal/provider/elevenlabs", []string{"github.com/coder/websocket"}, nil},
		{"deepseek imports websocket", "internal/provider/deepseek", []string{"github.com/coder/websocket"}, nil},
		{"feature imports cartesia adapter", "internal/session", []string{m + "internal/provider/cartesia"}, []string{m + "internal/provider/cartesia"}},
		{"feature imports deepseek adapter", "internal/session", []string{m + "internal/provider/deepseek"}, []string{m + "internal/provider/deepseek"}},
		{"feature imports elevenlabs adapter", "internal/session", []string{m + "internal/provider/elevenlabs"}, []string{m + "internal/provider/elevenlabs"}},
		{"provider core imports websocket", "internal/provider", []string{"github.com/coder/websocket"}, []string{"github.com/coder/websocket"}},
		{"postgres imports pgx", "internal/postgres", []string{"github.com/jackc/pgx/v5"}, nil},
		{"feature imports vendor adapter", "internal/session", []string{m + "internal/provider/openai"}, []string{m + "internal/provider/openai"}},
		{"non-audio imports opus", "internal/session", []string{"gopkg.in/hraban/opus.v2"}, []string{"gopkg.in/hraban/opus.v2"}},
		{"httpapi imports audio", "internal/httpapi", []string{m + "internal/conversation/audio"}, []string{m + "internal/conversation/audio"}},
		{"wsgateway imports audio", "internal/wsgateway", []string{m + "internal/conversation/audio"}, nil},
		{"jobs imports audio", "internal/jobs", []string{m + "internal/conversation/audio"}, []string{m + "internal/conversation/audio"}},
		{"audiofx imports audio", "internal/testutil/audiofx", []string{m + "internal/conversation/audio"}, nil},
		{"rtharness imports audio", "internal/testutil/rtharness", []string{m + "internal/conversation/audio"}, nil},
		{"testutil root imports audio", "internal/testutil", []string{m + "internal/conversation/audio"}, []string{m + "internal/conversation/audio"}},
		{"testutil fakes imports audio", "internal/testutil/fakes", []string{m + "internal/conversation/audio"}, []string{m + "internal/conversation/audio"}},
		{"unrelated imports", "internal/session", []string{"context", "log/slog"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.ElementsMatch(t, tt.want, forbiddenImports(m+tt.pkg, tt.imports))
		})
	}
}

func goList(t *testing.T, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = "../.."
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	require.NoError(t, err, stderr.String())
	return out
}

func TestCmdAPIWorkerGraphHasNoCgo(t *testing.T) {
	var targets []string
	for _, p := range []string{"cmd/api", "cmd/worker"} {
		cmd := exec.Command("go", "list", "./"+p)
		cmd.Dir = "../.."
		if err := cmd.Run(); err == nil {
			targets = append(targets, "./"+p)
		}
	}
	if len(targets) == 0 {
		t.Skip("cmd/api and cmd/worker do not exist yet (Task 23)")
	}
	out := goList(t, append([]string{"list", "-deps"}, targets...)...)
	for _, dep := range strings.Fields(string(out)) {
		for _, bad := range []string{"internal/conversation/audio", "hraban/opus", "go-webrtcvad"} {
			require.NotContains(t, dep, bad, "cgo package in api/worker graph")
		}
	}
}

func TestFeaturePackagesRespectLayout(t *testing.T) {
	out := goList(t, "list", "-json", "./internal/...")
	dec := json.NewDecoder(bytes.NewReader(out))
	checked := 0
	for dec.More() {
		var p struct {
			ImportPath string
			Imports    []string
		}
		require.NoError(t, dec.Decode(&p))
		checked++
		require.Empty(t, forbiddenImports(p.ImportPath, p.Imports), p.ImportPath)
	}
	require.Positive(t, checked)
}
