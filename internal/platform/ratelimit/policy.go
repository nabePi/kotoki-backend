// Package ratelimit defines the rate-limit policies (spec 13.4.3), the Limiter
// port and the in-memory fallback limiter. The Redis implementation lives in
// internal/redisstore; middleware lives in internal/httpapi and wsgateway.
package ratelimit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

// Policy is a token bucket: Capacity is the burst, Refill tokens are added per Period.
type Policy struct {
	Name     string
	Capacity int
	Refill   int
	Period   time.Duration
}

// RatePerMs is the refill rate in tokens per millisecond (the Lua script's ARGV[2]).
func (p Policy) RatePerMs() float64 {
	return float64(p.Refill) / float64(p.Period/time.Millisecond)
}

// Decision is the outcome of one Allow call.
type Decision struct {
	Allowed    bool
	Remaining  int
	RetryAfter time.Duration
}

// Limiter decides whether subject may proceed under p.
type Limiter interface {
	Allow(ctx context.Context, p Policy, subject string) (Decision, error)
}

func policy(name string, capacity, refill int, period time.Duration) Policy {
	return Policy{Name: name, Capacity: capacity, Refill: refill, Period: period}
}

// Catalog holds every canonical policy by name (spec 13.4.3).
var Catalog = func() map[string]Policy {
	m := map[string]Policy{}
	for _, p := range []Policy{
		policy("auth_register_ip", 5, 5, time.Hour),
		policy("auth_login_ip", 20, 20, time.Minute),
		policy("auth_login_email", 5, 5, 15*time.Minute),
		policy("auth_refresh_ip", 60, 60, time.Minute),
		policy("auth_forgot_ip", 5, 5, time.Hour),
		policy("auth_forgot_email", 3, 3, time.Hour),
		policy("auth_reset_ip", 10, 10, time.Hour),
		policy("session_create_user", 5, 10, time.Hour),
		policy("ticket_user", 10, 30, time.Minute),
		policy("ws_connect_ip", 30, 30, time.Minute),
		policy("ws_attach_user", 10, 10, time.Minute),
		policy("account_delete_user", 3, 3, time.Hour),
		policy("admin_user", 60, 120, time.Minute),
		policy("api_user_default", 60, 300, time.Minute),
		policy("api_ip_default", 120, 600, time.Minute),
	} {
		m[p.Name] = p
	}
	return m
}()

// Get returns the named policy and panics on an unknown name (a programming error).
func Get(name string) Policy {
	p, ok := Catalog[name]
	if !ok {
		panic(fmt.Sprintf("ratelimit: unknown policy %q", name))
	}
	return p
}

// SubjectEmail is the first 32 hex chars of sha256(lowercase(trim(email))); raw
// emails never reach Redis.
func SubjectEmail(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(sum[:])[:32]
}

// SubjectIP renders a client address as a limiter subject.
func SubjectIP(ip netip.Addr) string { return ip.String() }
