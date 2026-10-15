package balance_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/balance"
)

var day = time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)

// at builds 10:MM:SS.mmm of the test day.
func at(minute, sec, ms int) time.Time {
	return day.Add(10*time.Hour + time.Duration(minute)*time.Minute + time.Duration(sec)*time.Second + time.Duration(ms)*time.Millisecond)
}

func tp(t time.Time) *time.Time { return &t }

func TestComputeChargeExamples(t *testing.T) { // spec 4.5, the 11 worked examples
	cases := []struct {
		name         string
		reserved     int64
		ending       *time.Time
		ended        time.Time
		expires      time.Time
		disconnected int64
		fault        bool
		wantEnd      time.Time
		wantCharged  int64
	}{
		{"1 user ended, flush after ending not billed", 600, tp(at(7, 0, 400)), at(7, 2, 100), at(10, 0, 0), 0, false, at(7, 0, 400), 421},
		{"2 duration elapsed, grace not billed", 900, tp(at(15, 0, 0)), at(15, 8, 200), at(15, 0, 0), 0, false, at(15, 0, 0), 900},
		{"3 disconnected seconds subtracted", 900, tp(at(10, 0, 0)), at(10, 1, 500), at(15, 0, 0), 12, false, at(10, 0, 0), 588},
		{"4 reconnect timeout", 600, tp(at(5, 30, 200)), at(5, 31, 0), at(10, 0, 0), 30, false, at(5, 30, 200), 301},
		{"5 provider failure under a minute", 300, tp(at(0, 45, 0)), at(0, 46, 0), at(5, 0, 0), 0, true, at(0, 45, 0), 0},
		{"6 server error minus 30 s", 900, tp(at(5, 0, 500)), at(5, 1, 200), at(15, 0, 0), 0, true, at(5, 0, 500), 271},
		{"7 sweeper late: billable end cut at expires_at", 300, tp(at(5, 16, 0)), at(5, 16, 40), at(5, 0, 0), 20, false, at(5, 0, 0), 280},
		{"8 ended at start", 300, tp(at(0, 0, 0)), at(0, 0, 300), at(5, 0, 0), 0, false, at(0, 0, 0), 0},
		{"9 ACTIVE→FAILED by the sweeper, no ending_at", 900, nil, at(5, 0, 0), at(15, 0, 0), 0, true, at(5, 0, 0), 270},
		{"10 handoff drain, billing stops at handoff_at", 600, tp(at(3, 0, 0)), at(3, 31, 20), at(10, 0, 0), 0, false, at(3, 0, 0), 180},
		{"11 finalised by sweeper (c) long after ENDING", 600, tp(at(7, 0, 0)), at(8, 5, 0), at(10, 0, 0), 0, false, at(7, 0, 0), 420},
	}
	started := at(0, 0, 0)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			end := balance.BillableEnd(c.ending, c.ended, c.expires)
			require.True(t, c.wantEnd.Equal(end), "billable end %v, want %v", end, c.wantEnd)
			require.Equal(t, c.wantCharged, balance.ComputeCharge(c.reserved, started, end, c.disconnected, c.fault))
			released := c.reserved - balance.ComputeCharge(c.reserved, started, end, c.disconnected, c.fault)
			require.GreaterOrEqual(t, released, int64(0))
		})
	}
}

func TestComputeChargeBoundaries(t *testing.T) {
	start := at(0, 0, 0)
	ms := func(n int64) time.Time { return start.Add(time.Duration(n) * time.Millisecond) }

	require.EqualValues(t, 0, balance.ComputeCharge(900, start, ms(59_999), 0, true), "failure under 60 s active")
	require.EqualValues(t, 30, balance.ComputeCharge(900, start, ms(60_000), 0, true), "60 s active: 60 − 30")
	require.EqualValues(t, 60, balance.ComputeCharge(900, start, ms(60_000), 0, false), "a normal end bills the full 60 s")

	require.EqualValues(t, 600, balance.ComputeCharge(600, start, ms(600_000-1), 0, false), "ceil of 599.999 s = 600")
	require.EqualValues(t, 600, balance.ComputeCharge(600, start, ms(600_000), 0, false))
	require.EqualValues(t, 600, balance.ComputeCharge(600, start, ms(600_000+1), 0, false), "clamped to the reservation")
	require.EqualValues(t, 1, balance.ComputeCharge(600, start, ms(1), 0, false), "one millisecond bills one second")
	require.EqualValues(t, 0, balance.ComputeCharge(600, start, ms(0), 0, false))

	expires := ms(10_000)
	earlier := ms(9_999)
	later := ms(10_001)
	require.True(t, balance.BillableEnd(&earlier, ms(20_000), expires).Equal(earlier), "ending_at just before expires_at wins")
	require.True(t, balance.BillableEnd(&later, ms(20_000), expires).Equal(expires), "ending_at just after expires_at is cut")
	require.True(t, balance.BillableEnd(nil, ms(7_000), expires).Equal(ms(7_000)), "no ending_at: ended_at")
	require.True(t, balance.BillableEnd(nil, ms(30_000), expires).Equal(expires))

	require.EqualValues(t, 0, balance.ComputeCharge(600, start, ms(10_000), 11, false), "more disconnected than active: nothing billed")
	require.EqualValues(t, 0, balance.ComputeCharge(600, start, ms(10_000), 11, true))
	require.EqualValues(t, 0, balance.ComputeCharge(600, start, ms(-5_000), 0, false), "a billable end before the start is zero, never negative")
}
