package redisstore

import (
	"strings"
	"testing"
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
