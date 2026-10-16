package elevenlabs_test

import (
	"context"
	"encoding/json"
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
	return replay.LoadScript(t, replay.RecordingsDir(t, "elevenlabs"), name)
}

func itoa(n int) string { return strconv.Itoa(n) }

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

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

func drainTTS(t *testing.T, ch <-chan provider.TTSChunk) []provider.TTSChunk {
	t.Helper()
	var out []provider.TTSChunk
	deadline := time.After(5 * time.Second)
	for {
		select {
		case c, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, c)
		case <-deadline:
			t.Fatal("stream did not finish")
		}
	}
}

func totalBytes(cs []provider.TTSChunk) int {
	n := 0
	for _, c := range cs {
		n += len(c.PCM)
	}
	return n
}

// bodyField returns one top-level field of the JSON request body.
func bodyField(t *testing.T, body []byte, field string) (string, bool) {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(body, &m))
	v, ok := m[field]
	return string(v), ok
}
