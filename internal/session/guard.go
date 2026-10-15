package session

import (
	"sync"
	"time"
)

// heartbeatTimeout is SWEEPER_HEARTBEAT_TIMEOUT: a heartbeat older than this counts as lost, and
// Redis has to be reachable this long before heartbeats are trusted again (spec 9.9.2).
const heartbeatTimeout = 45 * time.Second

// HeartbeatGuard is safeguard 1 of rule (b): while Redis cannot be reached, and until it has been
// reachable without interruption for heartbeatTimeout, a missing heartbeat proves nothing and no
// session may be failed because of it. It never reads the wall clock; callers pass the time.
type HeartbeatGuard struct {
	mu           sync.Mutex
	healthySince time.Time
}

// Observe records the outcome of one Redis interaction: a failure resets the healthy period, a
// success starts it when it has not started yet.
func (g *HeartbeatGuard) Observe(ok bool, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	switch {
	case !ok:
		g.healthySince = time.Time{}
	case g.healthySince.IsZero():
		g.healthySince = now
	}
}

// Enabled reports whether rule (b) may run: Redis has been reachable for at least heartbeatTimeout.
// A fresh guard starts disabled.
func (g *HeartbeatGuard) Enabled(now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.healthySince.IsZero() && now.Sub(g.healthySince) >= heartbeatTimeout
}
