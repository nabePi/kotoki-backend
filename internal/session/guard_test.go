package session_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/session"
)

func TestHeartbeatGuard(t *testing.T) {
	var g session.HeartbeatGuard
	now := tenAM
	require.False(t, g.Enabled(now), "a fresh guard is disabled")

	g.Observe(true, now)
	require.False(t, g.Enabled(now), "Redis was just seen")
	require.False(t, g.Enabled(now.Add(30*time.Second)), "30 s of health is not enough")
	require.False(t, g.Enabled(now.Add(44*time.Second)))
	require.True(t, g.Enabled(now.Add(45*time.Second)), "45 s of uninterrupted health")
	require.True(t, g.Enabled(now.Add(46*time.Second)))

	g.Observe(true, now.Add(40*time.Second))
	require.True(t, g.Enabled(now.Add(46*time.Second)), "further successes do not restart the period")

	g.Observe(false, now.Add(50*time.Second))
	require.False(t, g.Enabled(now.Add(50*time.Second)), "an error disables it at once")
	require.False(t, g.Enabled(now.Add(120*time.Second)), "and stays disabled until Redis answers again")

	g.Observe(true, now.Add(130*time.Second))
	require.False(t, g.Enabled(now.Add(160*time.Second)), "the period starts over after the error")
	require.True(t, g.Enabled(now.Add(175*time.Second)))
}
