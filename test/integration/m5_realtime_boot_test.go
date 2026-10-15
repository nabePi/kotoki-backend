//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

const bootPod = "m5-boot-test"

// syncBuf is the output of the process: written by the exec goroutine, read by the test.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// realtimeProc is the real kotoki-realtime binary running against the test Postgres and Redis.
type realtimeProc struct {
	cmd      *exec.Cmd
	out      *syncBuf
	opsAddr  string
	httpAddr string
	exited   chan error
}

// startRealtime builds cmd/realtime (cgo) and starts it in PROVIDER_MODE=fake.
func startRealtime(t *testing.T, pgDSN, redisAddr string, extra map[string]string) *realtimeProc {
	t.Helper()
	root := testutil.ModuleRoot(t)
	bin := filepath.Join(t.TempDir(), "realtime")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", bin, "./cmd/realtime")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build ./cmd/realtime: %v\n%s", err, out)
	}

	p := &realtimeProc{out: &syncBuf{}, opsAddr: testutil.FreeAddr(t), httpAddr: testutil.FreeAddr(t), exited: make(chan error, 1)}
	env := testutil.MinimalEnv(t, map[string]string{
		"DB_URL": pgDSN, "REDIS_URL": "redis://" + redisAddr + "/0", "OPS_ADDR": p.opsAddr, "HTTP_ADDR": p.httpAddr,
		"LOG_FORMAT": "json", "PROVIDER_MODE": "fake", "REALTIME_POD_ID": bootPod,
		"WS_ALLOWED_ORIGINS": "http://localhost:3000", "WS_ALLOW_EMPTY_ORIGIN": "false",
	}, extra)
	p.cmd = exec.CommandContext(context.WithoutCancel(t.Context()), bin)
	p.cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir()}
	for k, v := range env {
		p.cmd.Env = append(p.cmd.Env, k+"="+v)
	}
	p.cmd.Stdout, p.cmd.Stderr = p.out, p.out
	require.NoError(t, p.cmd.Start())
	go func() { p.exited <- p.cmd.Wait() }()
	t.Cleanup(func() {
		if p.cmd.ProcessState == nil {
			_ = p.cmd.Process.Kill()
			<-p.exited
		}
	})
	return p
}

func (p *realtimeProc) get(t *testing.T, addr, path, origin string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+path, http.NoBody)
	require.NoError(t, err)
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp.StatusCode, b.String()
}

func (p *realtimeProc) waitReady(t *testing.T) {
	t.Helper()
	ok := assert.Eventually(t, func() bool {
		select {
		case err := <-p.exited:
			t.Fatalf("realtime exited before ready: %v\n%s", err, p.out.String())
		default:
		}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+p.opsAddr+"/readyz", http.NoBody)
		if err != nil {
			return false
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode == http.StatusOK
	}, 30*time.Second, 100*time.Millisecond)
	if !ok {
		t.Fatalf("realtime never became ready\n%s", p.out.String())
	}
}

func TestM5BootIntegration(t *testing.T) {
	pool := testutil.NewPostgres(t)
	rdb := testutil.NewRedis(t)
	// what `make run-realtime` sets up: the 16 kHz fixtures behind FAKE_STT_FIXTURE_DIR
	fixtures := t.TempDir()
	gen := exec.CommandContext(t.Context(), "go", "run", "./test/tools/genaudio", "-to16k", fixtures, "-src", "testdata/audio/synth")
	gen.Dir = testutil.ModuleRoot(t)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Fatalf("genaudio: %v\n%s", err, out)
	}
	p := startRealtime(t, pool.Config().ConnString(), rdb.Options().Addr, map[string]string{"FAKE_STT_FIXTURE_DIR": fixtures})
	p.waitReady(t)

	code, body := p.get(t, p.opsAddr, "/readyz", "")
	require.Equal(t, 200, code, body)
	code, _ = p.get(t, p.opsAddr, "/healthz", "")
	require.Equal(t, 200, code)
	code, body = p.get(t, p.opsAddr, "/metrics", "")
	require.Equal(t, 200, code)
	for _, want := range []string{"kotoki_realtime_session_capacity", "kotoki_provider_", "kotoki_db_pool_max_conns"} {
		require.Contains(t, body, want)
	}

	// Step 1 of the handshake (spec 7.3.1): a wrong Origin is refused before anything else.
	code, body = p.get(t, p.httpAddr, "/api/v1/realtime", "https://evil.example")
	require.Equal(t, 403, code, body)
	require.Contains(t, body, "ORIGIN_NOT_ALLOWED")

	// The whole stack answers on the upgraded socket: the ticket store (Redis) says no.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, _, err := websocket.Dial(ctx, "ws://"+p.httpAddr+"/api/v1/realtime?ticket="+strings.Repeat("A", 43), &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"http://localhost:3000"}}, Subprotocols: []string{"kotoki.v1"},
	})
	require.NoError(t, err)
	_, raw, err := c.Read(ctx)
	require.NoError(t, err)
	var ev struct{ Type string }
	require.NoError(t, json.Unmarshal(raw, &ev))
	require.Equal(t, "error", ev.Type)
	_, _, err = c.Read(ctx)
	require.Equal(t, websocket.StatusCode(4001), websocket.CloseStatus(err), "TICKET_INVALID")

	// podinfo is written while the pod runs.
	require.Eventually(t, func() bool {
		return rdb.HGet(ctx, "kotoki:realtime:podinfo:"+bootPod, "draining").Val() == "0"
	}, 5*time.Second, 50*time.Millisecond, "podinfo of the running pod")

	// SIGTERM: drain, then exit 0 within the 30 s budget; the draining podinfo is left behind.
	start := time.Now()
	require.NoError(t, p.cmd.Process.Signal(syscall.SIGTERM))
	select {
	case err := <-p.exited:
		require.NoError(t, err, "exit code of the drained pod\n%s", p.out.String())
	case <-time.After(35 * time.Second):
		t.Fatalf("realtime did not exit after SIGTERM\n%s", p.out.String())
	}
	require.Less(t, time.Since(start), 30*time.Second)
	require.Equal(t, "1", rdb.HGet(ctx, "kotoki:realtime:podinfo:"+bootPod, "draining").Val(), "podinfo draining=1 written by the drain")
	require.Contains(t, p.out.String(), `"msg":"stopped"`)
}

func TestM5RealtimeRefusesRealMode(t *testing.T) {
	pool := testutil.NewPostgres(t)
	rdb := testutil.NewRedis(t)
	p := startRealtime(t, pool.Config().ConnString(), rdb.Options().Addr, map[string]string{"PROVIDER_MODE": "real"})
	select {
	case err := <-p.exited:
		require.Error(t, err)
	case <-time.After(30 * time.Second):
		t.Fatalf("realtime keeps running in real mode\n%s", p.out.String())
	}
	require.Contains(t, p.out.String(), "M9")
	testutil.AssertNothingListening(t, p.opsAddr)
	testutil.AssertNothingListening(t, p.httpAddr)
}
