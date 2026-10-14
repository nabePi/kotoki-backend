package redisstore

import (
	"strings"
	"testing"

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
