package ratelimit_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/ratelimit"
)

var t0 = time.Date(2026, 10, 14, 9, 0, 0, 0, time.UTC)

func TestCatalog(t *testing.T) {
	want := map[string]ratelimit.Policy{
		"auth_register_ip":    {Capacity: 5, Refill: 5, Period: time.Hour},
		"auth_login_ip":       {Capacity: 20, Refill: 20, Period: time.Minute},
		"auth_login_email":    {Capacity: 5, Refill: 5, Period: 15 * time.Minute},
		"auth_refresh_ip":     {Capacity: 60, Refill: 60, Period: time.Minute},
		"auth_forgot_ip":      {Capacity: 5, Refill: 5, Period: time.Hour},
		"auth_forgot_email":   {Capacity: 3, Refill: 3, Period: time.Hour},
		"auth_reset_ip":       {Capacity: 10, Refill: 10, Period: time.Hour},
		"session_create_user": {Capacity: 5, Refill: 10, Period: time.Hour},
		"ticket_user":         {Capacity: 10, Refill: 30, Period: time.Minute},
		"ws_connect_ip":       {Capacity: 30, Refill: 30, Period: time.Minute},
		"ws_attach_user":      {Capacity: 10, Refill: 10, Period: time.Minute},
		"account_delete_user": {Capacity: 3, Refill: 3, Period: time.Hour},
		"admin_user":          {Capacity: 60, Refill: 120, Period: time.Minute},
		"api_user_default":    {Capacity: 60, Refill: 300, Period: time.Minute},
		"api_ip_default":      {Capacity: 120, Refill: 600, Period: time.Minute},
	}
	require.Len(t, ratelimit.Catalog, len(want))
	for name, p := range want {
		p.Name = name
		require.Equal(t, p, ratelimit.Get(name), name)
		require.Equal(t, p, ratelimit.Catalog[name], name)
	}
	require.Panics(t, func() { ratelimit.Get("nope") })
}

func TestSubjects(t *testing.T) {
	require.Equal(t, ratelimit.SubjectEmail("hana@example.com"), ratelimit.SubjectEmail("  HANA@Example.com "))
	require.Regexp(t, `^[0-9a-f]{32}$`, ratelimit.SubjectEmail("hana@example.com"))
	require.NotContains(t, ratelimit.SubjectEmail("hana@example.com"), "hana")
	require.Equal(t, "203.0.113.7", ratelimit.SubjectIP(netip.MustParseAddr("203.0.113.7")))
}

func TestPolicyRate(t *testing.T) {
	p := ratelimit.Policy{Capacity: 5, Refill: 5, Period: 15 * time.Minute}
	require.InDelta(t, 5.0/float64(15*time.Minute/time.Millisecond), p.RatePerMs(), 1e-12)
}

func TestLocalBurstAndRefill(t *testing.T) {
	clk := clock.NewFake(t0)
	l := ratelimit.NewLocal(clk, 100)
	p := ratelimit.Policy{Name: "x", Capacity: 5, Refill: 5, Period: time.Minute}
	ctx := context.Background()

	for i := 4; i >= 0; i-- {
		d, err := l.Allow(ctx, p, "s")
		require.NoError(t, err)
		require.True(t, d.Allowed)
		require.Equal(t, i, d.Remaining)
	}
	d, err := l.Allow(ctx, p, "s")
	require.NoError(t, err)
	require.False(t, d.Allowed)
	require.Greater(t, d.RetryAfter, time.Duration(0))
	require.LessOrEqual(t, d.RetryAfter, 12*time.Second+time.Millisecond, "one token takes Period/Refill = 12s")

	clk.Advance(12 * time.Second)
	d, err = l.Allow(ctx, p, "s")
	require.NoError(t, err)
	require.True(t, d.Allowed)

	clk.Advance(time.Hour) // never above capacity
	d, err = l.Allow(ctx, p, "s")
	require.NoError(t, err)
	require.Equal(t, 4, d.Remaining)
}

func TestLocalPerSubject(t *testing.T) {
	l := ratelimit.NewLocal(clock.NewFake(t0), 100)
	p := ratelimit.Policy{Name: "x", Capacity: 1, Refill: 1, Period: time.Hour}
	ctx := context.Background()
	d, _ := l.Allow(ctx, p, "a")
	require.True(t, d.Allowed)
	d, _ = l.Allow(ctx, p, "a")
	require.False(t, d.Allowed)
	d, _ = l.Allow(ctx, p, "b")
	require.True(t, d.Allowed, "another subject has its own bucket")
	other := ratelimit.Policy{Name: "y", Capacity: 1, Refill: 1, Period: time.Hour}
	d, _ = l.Allow(ctx, other, "a")
	require.True(t, d.Allowed, "another policy has its own bucket")
}

func TestLocalLRUEviction(t *testing.T) {
	l := ratelimit.NewLocal(clock.NewFake(t0), 2)
	p := ratelimit.Policy{Name: "x", Capacity: 1, Refill: 1, Period: time.Hour}
	ctx := context.Background()
	for _, s := range []string{"a", "b", "c"} {
		d, err := l.Allow(ctx, p, s)
		require.NoError(t, err)
		require.True(t, d.Allowed)
	}
	require.Equal(t, 2, l.Len())
	// "a" was evicted, so it starts from a full bucket again; "c" is still limited
	d, _ := l.Allow(ctx, p, "a")
	require.True(t, d.Allowed)
	d, _ = l.Allow(ctx, p, "c")
	require.False(t, d.Allowed)
}
