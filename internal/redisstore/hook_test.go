package redisstore

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

func TestKeyPrefix(t *testing.T) {
	cases := map[string]string{
		"kotoki:session:0192f1a0-1111-7000-8000-000000000001:rt": "kotoki:session:*:rt",
		"kotoki:ws_ticket:" + strings.Repeat("ab12", 16):         "kotoki:ws_ticket:*",
		"kotoki:fake:faults":     "kotoki:fake:faults",
		"kotoki:rate:user:12345": "kotoki:rate:user:*",
		"":                       "",
	}
	for in, want := range cases {
		if got := KeyPrefix(in); got != want {
			t.Errorf("KeyPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewClientBadURLDoesNotLeakPassword(t *testing.T) {
	_, err := NewClient(config.Redis{URL: config.Secret("rediss://default:s3cr%zzet@host:6380/0")}, nil)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "s3cr") || strings.Contains(err.Error(), "host:6380") {
		t.Fatalf("error leaks the URL: %v", err)
	}
	if errs.CodeOf(err) != errs.CodeServiceUnavailable {
		t.Fatalf("code = %q", errs.CodeOf(err))
	}
}

func TestCommandKey(t *testing.T) {
	cases := []struct {
		name string
		cmd  redis.Cmder
		want string
	}{
		{"get", redis.NewCmd(context.Background(), "get", "kotoki:rate:user:12345"), "kotoki:rate:user:12345"},
		{"eval first key", redis.NewCmd(context.Background(), "eval", "return 1", 1, "kotoki:rate:user:12345", "arg"), "kotoki:rate:user:12345"},
		{"evalsha first key", redis.NewCmd(context.Background(), "evalsha", "abc123", 2, "kotoki:a", "kotoki:b"), "kotoki:a"},
		{"eval without keys", redis.NewCmd(context.Background(), "eval", "return 1", 0), ""},
		{"ping", redis.NewCmd(context.Background(), "ping"), ""},
	}
	for _, c := range cases {
		if got, _ := commandKey(c.cmd); got != c.want {
			t.Errorf("%s: commandKey = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestRedisErrorClassification(t *testing.T) {
	h := &traceHook{}
	cmd := func() redis.Cmder { return redis.NewCmd(context.Background(), "get", "k") }
	if got := h.fail(cmd(), context.Canceled); !errors.Is(got, context.Canceled) {
		t.Errorf("a cancelled context must pass through unwrapped, got %v", got)
	}
	if got := h.fail(cmd(), context.DeadlineExceeded); !errors.Is(got, context.DeadlineExceeded) {
		t.Errorf("a deadline must pass through unwrapped, got %v", got)
	}
	if code := errs.CodeOf(h.fail(cmd(), serverErr("WRONGTYPE Operation against a key"))); code != errs.CodeInternalError {
		t.Errorf("WRONGTYPE code = %q, want INTERNAL_ERROR", code)
	}
	if code := errs.CodeOf(h.fail(cmd(), serverErr("ERR user_script:1: boom"))); code != errs.CodeInternalError {
		t.Errorf("Lua error code = %q, want INTERNAL_ERROR", code)
	}
	for _, e := range []error{serverErr("LOADING Redis is loading"), serverErr("READONLY You can't write against a read only replica"), io.EOF, redis.ErrPoolTimeout} {
		if code := errs.CodeOf(h.fail(cmd(), e)); code != errs.CodeServiceUnavailable {
			t.Errorf("%v code = %q, want SERVICE_UNAVAILABLE", e, code)
		}
	}
}

type serverErr string

func (e serverErr) Error() string { return string(e) }
func (serverErr) RedisError()     {}
