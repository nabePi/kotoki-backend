//go:build integration || e2e

package testutil

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// FreeAddr returns a loopback address with a currently free port.
func FreeAddr(t testing.TB) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("testutil: free port: %v", err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().String()
}

func httpGet(addr, path string) (int, string, error) {
	resp, err := http.Get("http://" + addr + path) //nolint:noctx // short-lived test probe
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), err
}

// AssertOpsEndpoints waits (≤10 s) for /readyz=200 and then checks /healthz and /metrics.
func AssertOpsEndpoints(t testing.TB, addr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if code, _, err := httpGet(addr, "/readyz"); err == nil && code == http.StatusOK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("testutil: /readyz on %s not 200 within 10s", addr)
		}
		time.Sleep(50 * time.Millisecond)
	}
	code, body, err := httpGet(addr, "/healthz")
	if err != nil || code != http.StatusOK {
		t.Fatalf("testutil: /healthz = %d, %v", code, err)
	}
	var h struct{ Status string }
	if err := json.Unmarshal([]byte(body), &h); err != nil || h.Status != "ok" {
		t.Fatalf("testutil: /healthz body %q", body)
	}
	code, body, err = httpGet(addr, "/metrics")
	if err != nil || code != http.StatusOK {
		t.Fatalf("testutil: /metrics = %d, %v", code, err)
	}
	for _, want := range []string{"go_goroutines", "kotoki_db_pool_max_conns"} {
		if !strings.Contains(body, want) {
			t.Fatalf("testutil: /metrics lacks %s", want)
		}
	}
}

// AssertNothingListening fails if addr accepts TCP connections.
func AssertNothingListening(t testing.TB, addr string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("testutil: %s unexpectedly accepts connections", addr)
	}
}
