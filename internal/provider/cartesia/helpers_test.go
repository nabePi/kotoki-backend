package cartesia_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"sync"
	"testing"

	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

var ctx = context.Background()

const testKey = "ct-test-key"

func recording(t *testing.T, name string) *replay.Script {
	t.Helper()
	return replay.LoadScript(t, replay.RecordingsDir(t, "cartesia"), name)
}

func itoa(n int) string { return strconv.Itoa(n) }

func quote(s string) string { b, _ := json.Marshal(s); return string(b) }

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
