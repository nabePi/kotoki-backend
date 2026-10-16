package deepseek_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

var ctx = context.Background()

func recording(t *testing.T, name string) *replay.Script {
	t.Helper()
	return replay.LoadScript(t, replay.RecordingsDir(t, "deepseek"), name)
}

func msgs() []provider.Message {
	return []provider.Message{{Role: provider.RoleSystem, Content: "S"}, {Role: provider.RoleUser, Content: "U"}}
}

func drain(t *testing.T, ch <-chan provider.LLMEvent) []provider.LLMEvent {
	t.Helper()
	var out []provider.LLMEvent
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-deadline:
			t.Fatal("stream did not finish")
		}
	}
}

func deltas(evs []provider.LLMEvent) []string {
	var out []string
	for _, e := range evs {
		if e.Type == provider.LLMDelta {
			out = append(out, e.Text)
		}
	}
	return out
}

// bodyField returns one top-level field of the JSON request body.
func bodyField(t *testing.T, body []byte, field string) string {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &m))
	return string(m[field])
}

func bodyHas(t *testing.T, body []byte, field string) bool {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &m))
	_, ok := m[field]
	return ok
}

type rlCall struct {
	P    provider.Name
	C    provider.Capability
	Kind string
	V    float64
}

type rlSink struct {
	mu    sync.Mutex
	calls []rlCall
}

func (s *rlSink) RateLimitRemaining(p provider.Name, c provider.Capability, kind string, v float64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, rlCall{p, c, kind, v})
}

// logBuf is a concurrency-safe log sink.
type logBuf struct {
	mu sync.Mutex
	b  []byte
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b = append(l.b, p...)
	return len(p), nil
}

func (l *logBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.b)
}

func bufLogger(l *logBuf) *slog.Logger {
	return slog.New(slog.NewTextHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func itoa(n int) string { return strconv.Itoa(n) }

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

func fmtV(v any) string { return fmt.Sprintf("%v %+v %#v", v, v, v) }
