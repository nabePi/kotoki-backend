package logging_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/logging"
)

var allKeys = []string{
	"password", "new_password", "token", "access_token", "refresh_token", "ticket",
	"authorization", "cookie", "set-cookie", "api_key", "secret", "email", "phone",
	"user_text", "assistant_text", "assistant_spoken_text", "transcript", "partial",
	"text", "prompt", "audio",
}

func newLogger(t *testing.T, o logging.Options) (*slog.Logger, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	if o.Format == "" {
		o.Format = "json"
	}
	if o.Service == "" {
		o.Service = "kotoki-api"
	}
	return logging.New(&buf, o), &buf
}

func decode(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &m), buf.String())
	return m
}

func TestRedactAllKeys(t *testing.T) {
	require.Len(t, allKeys, 21)
	keys := append(append([]string{}, allKeys...), "PASSWORD", "Set-Cookie")
	for _, k := range keys {
		t.Run(k, func(t *testing.T) {
			l, buf := newLogger(t, logging.Options{})
			l.Info("m", slog.String(k, "secret-value"))
			require.Equal(t, "[REDACTED]", decode(t, buf)[k])
			require.NotContains(t, buf.String(), "secret-value")
		})
	}
	l, buf := newLogger(t, logging.Options{})
	l.Info("m", "request_id", "r1")
	require.Equal(t, "r1", decode(t, buf)["request_id"])
}

func TestRedactNestedGroup(t *testing.T) {
	l, buf := newLogger(t, logging.Options{})
	l.Info("m", slog.Group("req", slog.String("email", "a@b.c"), slog.Group("in", slog.String("Token", "t0k"))))
	m := decode(t, buf)
	req := m["req"].(map[string]any)
	require.Equal(t, "[REDACTED]", req["email"])
	require.Equal(t, "[REDACTED]", req["in"].(map[string]any)["Token"])

	l, buf = newLogger(t, logging.Options{})
	l.With(slog.String("email", "a@b.c")).WithGroup("g").Info("m", slog.String("phone", "123"), slog.String("ok", "v"))
	m = decode(t, buf)
	require.Equal(t, "[REDACTED]", m["email"])
	g := m["g"].(map[string]any)
	require.Equal(t, "[REDACTED]", g["phone"])
	require.Equal(t, "v", g["ok"])
	require.NotContains(t, buf.String(), "a@b.c")
}

func TestLogTranscriptsExempt(t *testing.T) {
	l, buf := newLogger(t, logging.Options{LogTranscripts: true})
	l.Info("m", "text", "hello", "prompt", "p", "user_text", "u",
		"password", "x", "token", "y", "email", "e@x.y", "audio", "zzz")
	m := decode(t, buf)
	require.Equal(t, "hello", m["text"])
	require.Equal(t, "p", m["prompt"])
	require.Equal(t, "u", m["user_text"])
	for _, k := range []string{"password", "token", "email", "audio"} {
		require.Equal(t, "[REDACTED]", m[k], k)
	}
	require.True(t, logging.IsSensitiveKey("text", false))
	require.False(t, logging.IsSensitiveKey("text", true))
	require.True(t, logging.IsSensitiveKey("AUDIO", true))
	require.False(t, logging.IsSensitiveKey("request_id", false))
}

func TestRequiredFields(t *testing.T) {
	l, buf := newLogger(t, logging.Options{Version: "v1", GitCommit: "abc", Env: "local"})
	l.Info("hello")
	m := decode(t, buf)
	require.Equal(t, "kotoki-api", m["service"])
	require.Equal(t, "v1", m["version"])
	require.Equal(t, "abc", m["git_commit"])
	require.Equal(t, "local", m["env"])
	require.Equal(t, "INFO", m["level"])
	require.Equal(t, "hello", m["msg"])
	require.True(t, strings.HasSuffix(m["time"].(string), "Z"), m["time"])
	require.NotContains(t, m, "trace_id")
}

func TestTextFormat(t *testing.T) {
	l, buf := newLogger(t, logging.Options{Format: "text"})
	l.Info("hello", "email", "a@b.c")
	require.Contains(t, buf.String(), "service=kotoki-api")
	require.Contains(t, buf.String(), "email=[REDACTED]")
	require.NotContains(t, buf.String(), "a@b.c")
}

func TestLevel(t *testing.T) {
	l, buf := newLogger(t, logging.Options{Level: slog.LevelWarn})
	l.Info("no")
	require.Empty(t, buf.String())
}

func TestTraceIDs(t *testing.T) {
	tp := sdktrace.NewTracerProvider()
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("x").Start(context.Background(), "op")
	defer span.End()
	l, buf := newLogger(t, logging.Options{})
	l.InfoContext(ctx, "m")
	m := decode(t, buf)
	require.Len(t, m["trace_id"], 32)
	require.Len(t, m["span_id"], 16)
	require.Equal(t, span.SpanContext().TraceID().String(), m["trace_id"])
}

func TestEmailHash(t *testing.T) {
	h := logging.EmailHash("a@b.c")
	require.Len(t, h, 32)
	require.Equal(t, h, logging.EmailHash("a@b.c"))
	require.NotEqual(t, "a@b.c", h)
	require.NotEqual(t, h, logging.EmailHash("x@b.c"))
}

func TestErrAttrs(t *testing.T) {
	require.Nil(t, logging.ErrAttrs(nil))
	err := errs.New(errs.CodeAuthRequired, "need auth")
	attrs := logging.ErrAttrs(err)
	m := map[string]string{}
	for _, a := range attrs {
		m[a.Key] = a.Value.String()
	}
	require.Equal(t, "AUTH_REQUIRED", m["error_code"])
	require.Contains(t, m["error"], "need auth")
	require.Regexp(t, `^internal/platform/logging/logging_test.go:\d+ `, m["error_location"])
	require.Contains(t, m["error_location"], "TestErrAttrs")
}

type strer struct{}

func (strer) String() string { return "stringer-ok" }

type lv struct{ v any }

func (l lv) LogValue() slog.Value { return slog.AnyValue(l.v) }

func TestComplexValuesDenied(t *testing.T) {
	type st struct{ Email string }
	cases := map[string]any{
		"map":    map[string]any{"email": "a@b.c", "text": "hi"},
		"struct": st{Email: "a@b.c"},
		"slice":  []string{"a@b.c"},
		"raw":    json.RawMessage(`{"email":"a@b.c"}`),
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			l, buf := newLogger(t, logging.Options{})
			l.Info("m", slog.Any("payload", v))
			require.Equal(t, "[REDACTED_COMPLEX]", decode(t, buf)["payload"])
			require.NotContains(t, buf.String(), "a@b.c")
		})
	}
	l, buf := newLogger(t, logging.Options{})
	l.Info("m", slog.Any("err", errors.New("boom")), slog.Any("s", strer{}), slog.Any("n", nil))
	m := decode(t, buf)
	require.Equal(t, "boom", m["err"])
	require.Equal(t, "stringer-ok", m["s"])
}

func TestSensitiveGroups(t *testing.T) {
	l, buf := newLogger(t, logging.Options{})
	l.Info("m", slog.Group("transcript", slog.String("a", "x1")), slog.Group("token", slog.Group("n", slog.String("b", "x2"))))
	m := decode(t, buf)
	require.Equal(t, "[REDACTED]", m["transcript"])
	require.Equal(t, "[REDACTED]", m["token"])
	require.NotContains(t, buf.String(), "x1")
	require.NotContains(t, buf.String(), "x2")

	l, buf = newLogger(t, logging.Options{})
	l.WithGroup("token").Info("m", "a", "x3", slog.Group("n", slog.String("b", "x4")))
	require.NotContains(t, buf.String(), "x3")
	require.NotContains(t, buf.String(), "x4")

	l, buf = newLogger(t, logging.Options{})
	l.With(slog.Group("req", slog.String("email", "x5")), slog.Group("secret", slog.String("k", "x6"))).Info("m")
	require.NotContains(t, buf.String(), "x5")
	require.NotContains(t, buf.String(), "x6")

	// transcript group is exempt when LogTranscripts is on
	l, buf = newLogger(t, logging.Options{LogTranscripts: true})
	l.Info("m", slog.Group("transcript", slog.String("a", "visible")))
	require.Contains(t, buf.String(), "visible")
}

func TestLogValuerResolvedThenRedacted(t *testing.T) {
	l, buf := newLogger(t, logging.Options{})
	l.Info("m", slog.Any("token", lv{"x7"}), slog.Any("p", lv{map[string]any{"email": "x8"}}), slog.Any("q", lv{"fine"}))
	m := decode(t, buf)
	require.Equal(t, "[REDACTED]", m["token"])
	require.Equal(t, "[REDACTED_COMPLEX]", m["p"])
	require.Equal(t, "fine", m["q"])
	require.NotContains(t, buf.String(), "x7")
	require.NotContains(t, buf.String(), "x8")
}

func TestTimeNormalizedToUTC(t *testing.T) {
	var buf bytes.Buffer
	l := logging.New(&buf, logging.Options{Format: "json"})
	loc := time.FixedZone("WIB", 7*3600)
	ts := time.Date(2026, 10, 13, 15, 4, 5, 123456789, loc)
	r := slog.NewRecord(ts, slog.LevelInfo, "m", 0)
	require.NoError(t, l.Handler().Handle(context.Background(), r))
	require.Equal(t, "2026-10-13T08:04:05.123456789Z", decode(t, &buf)["time"])
}
