package conversation

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/conversation/rtstate"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/session"
)

const (
	redisOpTimeout   = 200 * time.Millisecond // per Redis operation (§7.9.4)
	redisSyncTick    = 50 * time.Millisecond  // granularity of the coalesce windows below
	rtCoalesce       = 100 * time.Millisecond // rt: ≤ 1 HSET / 100 ms (§7.9.1)
	turnCoalesce     = 250 * time.Millisecond // turn: coalesce 250 ms (§7.9.1)
	stateTTLFloor    = 300 * time.Second      // TTL_S = max(300, expires_at − now + 300)
	preActiveTTLPlus = 600 * time.Second      // before ACTIVE: duration_seconds + 600
	maxPendingEvents = 500                    // never more than the stream keeps (MAXLEN ~ 500)

	defaultOwnerTTL      = 15 * time.Second
	defaultOwnerRefresh  = 5 * time.Second
	defaultHeartbeatTTL  = 60 * time.Second
	defaultHeartbeatTick = 5 * time.Second
)

// RedisSyncOption configures a RedisSync.
type RedisSyncOption func(*RedisSync)

// RedisSyncTuning takes OWNER_TTL, OWNER_REFRESH_INTERVAL, HEARTBEAT_INTERVAL and HEARTBEAT_TTL
// from the runtime configuration (zero values keep the 15 s / 5 s / 5 s / 60 s defaults).
func RedisSyncTuning(c config.RealtimeTuning) RedisSyncOption {
	return func(s *RedisSync) {
		s.ownerTTL = orDefault(c.OwnerTTL, s.ownerTTL)
		s.ownerEvery = orDefault(c.OwnerRefreshInterval, s.ownerEvery)
		s.beatEvery = orDefault(c.HeartbeatInterval, s.beatEvery)
		s.beatTTL = orDefault(c.HeartbeatTTL, s.beatTTL)
	}
}

func orDefault(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// RedisSyncSessionDuration is the reserved duration of the session: the key TTL before ACTIVE
// (rt.ExpiresAtMS == 0) is duration + 600 s.
func RedisSyncSessionDuration(d time.Duration) RedisSyncOption {
	return func(s *RedisSync) { s.duration = d }
}

// RedisSyncLastDurableSeq seeds prev_seq of the first stream entry (takeover: the newest seq of
// the stream or rt.last_durable_seq).
func RedisSyncLastDurableSeq(seq int64) RedisSyncOption {
	return func(s *RedisSync) { s.lastDurable = seq }
}

// RedisSyncOnFenceLost is called once when the owner script refuses this pod (a newer fence
// owns the session). See RedisSync for the rules a callback must follow.
func RedisSyncOnFenceLost(f func(error)) RedisSyncOption {
	return func(s *RedisSync) { s.onFenceLost = f }
}

// RedisSyncFenceLatch shares the runtime's fence-loss latch (default: one of its own).
func RedisSyncFenceLatch(l *FenceLatch) RedisSyncOption { return func(s *RedisSync) { s.latch = l } }

// RedisSyncTicker replaces the ticker of the sync loop (tests fire it by hand).
func RedisSyncTicker(f func(time.Duration) Ticker) RedisSyncOption {
	return func(s *RedisSync) { s.newTicker = f }
}

// RedisSyncTimeoutFunc replaces context.WithTimeout for the 200 ms operation bound.
func RedisSyncTimeoutFunc(f TimeoutFunc) RedisSyncOption {
	return func(s *RedisSync) { s.withTimeout = f }
}

// slot is the newest value of one key and whether Redis has it yet.
type slot[T any] struct {
	val     T
	set     bool
	ver     uint64
	written uint64
	since   time.Time // first change not yet written (the start of the coalesce window)
}

func (s *slot[T]) put(v T, now time.Time) {
	if !s.dirty() {
		s.since = now
	}
	s.val, s.set = v, true
	s.ver++
}

func (s *slot[T]) dirty() bool { return s.ver != s.written }

func (s *slot[T]) wrote(ver uint64, now time.Time) {
	if ver > s.written {
		s.written = ver
	}
	if s.dirty() {
		s.since = now // changed during the write: a new window starts
	}
}

type turnSlot struct {
	snap    rtstate.TurnSnap
	deleted bool
}

// syncPlan is one batch of writes taken from the state under the lock.
type syncPlan struct {
	owner, beat bool
	events      []rtstate.EventEntry
	rt          *rtstate.RT
	rtVer       uint64
	turn        *turnSlot
	turnVer     uint64
	summary     *rtstate.Summary
	sumVer      uint64
	ttl         time.Duration
}

// RedisSync owns the session's realtime Redis state (§7.9.1) for the runtime: the owner key
// (refreshed every 5 s, TTL 15 s), hb (every 5 s, TTL 60 s), the rt hash (≤ 1 write / 100 ms),
// the turn hash (250 ms; DEL when the turn is final), the summary hash and the durable-event
// replay stream. Every key is written with TTL_S (the stream's MAXLEN/EXPIRE belong to the store).
// The setters only touch memory and never block the loop; one goroutine does the I/O, each
// operation bounded by 200 ms.
//
// Redis down (§7.9.4): a failed write marks Redis down; the session keeps running. The newest
// snapshot stays in memory, durable events are dropped (never rewritten; prev_seq keeps
// advancing so a resume across the gap reports UNAVAILABLE), and every HEARTBEAT_INTERVAL the
// sync retries a full rewrite: owner via the Lua script, hb, rt, turn, summary.
//
// Fence: when the owner script refuses this pod, OnFenceLost fires once, the FenceLatch counts
// fence_lost_total, the loop goroutine exits and nothing is written again (Flush/Release/Acquire
// return session.ErrFenceLost).
//
// Callbacks: OnFenceLost runs on the sync goroutine, or on the caller of Acquire/Flush, while the
// Redis I/O lock is held. It must not call back into the RedisSync (Flush, Release, Acquire and
// Abort take that lock or wait for the goroutine: deadlock) and must not block: the loop signals
// itself with a non-blocking channel send.
//
// Loop usage (Tasks 18–23): NewRedisSync → Acquire(ctx) at start/takeover → Start(sessionCtx) →
// Update / UpdateTurn / ClearTurn / UpdateSummary (a Summarizer result kept because its
// coveredUntilSeq grew) / Event (every emitted server event with its envelope) → drain handoff:
// Flush(ctx) then later Release(ctx); finish(): Update(rt status COMPLETED) then Release(ctx);
// fence lost from the persister: Abort(). Both get the runtime's FenceLatch.
type RedisSync struct {
	store       rtstate.Store
	clk         clock.Clock
	sid         uuid.UUID
	pod         string
	fence       int64
	log         *slog.Logger
	newTicker   func(time.Duration) Ticker
	withTimeout TimeoutFunc
	onFenceLost func(error)
	passHook    func()
	latch       *FenceLatch
	duration    time.Duration
	ownerTTL    time.Duration
	ownerEvery  time.Duration
	beatTTL     time.Duration
	beatEvery   time.Duration

	io sync.Mutex // serialises Redis I/O of the loop, Acquire, Flush and Release

	mu          sync.Mutex
	rt          slot[rtstate.RT]
	turn        slot[turnSlot]
	summary     slot[rtstate.Summary]
	events      []rtstate.EventEntry
	lastDurable int64
	down        bool
	probeAt     time.Time
	nextOwner   time.Time
	nextBeat    time.Time
	lost        bool
	finished    bool // released or aborted: no more writes
	halted      bool // the loop must not run another pass
	started     bool
	cancel      context.CancelFunc
	done        chan struct{}
}

// NewRedisSync creates the sync of session sid owned by pod with fence. A nil log discards.
func NewRedisSync(store rtstate.Store, clk clock.Clock, sid uuid.UUID, pod string, fence int64, m *Metrics,
	log *slog.Logger, opts ...RedisSyncOption,
) *RedisSync {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	s := &RedisSync{store: store, clk: clk, sid: sid, pod: pod, fence: fence, log: log,
		newTicker: NewRealTicker, withTimeout: context.WithTimeout,
		ownerTTL: defaultOwnerTTL, ownerEvery: defaultOwnerRefresh, beatTTL: defaultHeartbeatTTL,
		beatEvery: defaultHeartbeatTick, latch: NewFenceLatch(m), done: make(chan struct{})}
	for _, o := range opts {
		o(s)
	}
	return s
}

// Start runs the sync loop until ctx ends, Release or Abort. The first pass (owner refresh and
// heartbeat) runs right away. Only the first call counts.
func (s *RedisSync) Start(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started || s.finished {
		return
	}
	s.started = true
	lctx, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	go s.loop(lctx)
}

// Acquire runs the owner script now (session start, takeover). A refusal is session.ErrFenceLost
// (OnFenceLost fires); a Redis error is returned as is.
func (s *RedisSync) Acquire(ctx context.Context) error {
	s.io.Lock()
	defer s.io.Unlock()
	if err := s.writable(); err != nil {
		return err
	}
	now := s.clk.Now()
	if err := s.refreshOwner(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	s.nextOwner = now.Add(s.ownerEvery)
	s.mu.Unlock()
	return nil
}

// Update replaces the rt snapshot; PodID, Fence and UpdatedAtMS are filled at write time.
func (s *RedisSync) Update(rt rtstate.RT) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rt.put(rt, s.clk.Now())
}

// UpdateTurn replaces the snapshot of the turn in progress.
func (s *RedisSync) UpdateTurn(t rtstate.TurnSnap) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turn.put(turnSlot{snap: t}, s.clk.Now())
}

// ClearTurn deletes the turn hash (the turn is final), without waiting for the coalesce window.
func (s *RedisSync) ClearTurn() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turn.put(turnSlot{deleted: true}, s.clk.Now())
}

// UpdateSummary replaces the rolling summary (HSET on the next pass). The loop calls it only for
// a result whose CoveredUntilSeq grew (§7.8.5).
func (s *RedisSync) UpdateSummary(sum rtstate.Summary) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.summary.put(sum, s.clk.Now())
}

// Event offers one emitted server event with its envelope JSON. Durable events (EventDurable:
// the §7.4.3 types plus a non-fatal error) go to the replay stream with prev_seq = the previous
// durable server_seq; transient and replayed events are ignored. While Redis is down the entry is
// lost, but prev_seq still advances.
func (s *RedisSync) Event(ev protocol.ServerEvent, env []byte) {
	if ev.Replay || !EventDurable(ev) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e := rtstate.EventEntry{Seq: ev.ServerSeq, PrevSeq: s.lastDurable, Type: ev.Type, Env: slices.Clone(env)}
	s.lastDurable = ev.ServerSeq
	if s.down || s.lost || s.finished {
		return
	}
	if len(s.events) >= maxPendingEvents {
		s.events = slices.Delete(s.events, 0, 1)
	}
	s.events = append(s.events, e)
}

// Flush writes everything now, ignoring the coalesce windows: owner refresh, hb, pending
// events, rt, turn and summary (drain handoff step 2, §7.12.1). While Redis is down it is the
// full rewrite. The error is informational: the caller logs it and goes on.
func (s *RedisSync) Flush(ctx context.Context) error {
	s.io.Lock()
	defer s.io.Unlock()
	if err := s.writable(); err != nil {
		return err
	}
	now := s.clk.Now()
	s.mu.Lock()
	plan := s.planLocked(now, planAll)
	s.mu.Unlock()
	if err := s.apply(ctx, now, plan); err != nil {
		return err
	}
	s.recovered(ctx)
	return nil
}

// Release stops the loop, writes what changed since the last pass (no heartbeat) and releases
// the owner key (drain handoff step 5, finish()). Nothing is written afterwards.
func (s *RedisSync) Release(ctx context.Context) error {
	s.stopLoop()
	s.io.Lock()
	defer s.io.Unlock()
	if err := s.writable(); err != nil {
		return err
	}
	now := s.clk.Now()
	s.mu.Lock()
	plan := s.planLocked(now, planDirty)
	s.finished = true
	s.mu.Unlock()
	werr := s.apply(ctx, now, plan)
	octx, cancel := s.withTimeout(ctx, redisOpTimeout)
	defer cancel()
	if err := s.store.ReleaseOwner(octx, s.sid, s.pod, s.fence); err != nil {
		return errors.Join(werr, fmt.Errorf("conversation: release owner: %w", err))
	}
	return werr
}

// Abort stops the loop without writing anything again (fence lost elsewhere, shutdown).
func (s *RedisSync) Abort() {
	s.mu.Lock()
	s.finished = true
	s.mu.Unlock()
	s.stopLoop()
}

// stopLoop waits for a pass in flight (taking the I/O lock), so the events it took are written
// rather than dropped by a cancellation, then stops the goroutine.
func (s *RedisSync) stopLoop() {
	s.io.Lock()
	s.mu.Lock()
	s.halted = true
	started, cancel := s.started, s.cancel
	s.mu.Unlock()
	s.io.Unlock()
	if started {
		cancel()
		<-s.done
	}
}

func (s *RedisSync) isHalted() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.halted
}

func (s *RedisSync) writable() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.lost:
		return session.ErrFenceLost
	case s.finished:
		return errors.New("conversation: redis sync released")
	}
	return nil
}

func (s *RedisSync) loop(ctx context.Context) {
	defer close(s.done)
	t := s.newTicker(redisSyncTick)
	defer t.Stop()
	for {
		s.pass(ctx)
		if s.passHook != nil {
			s.passHook()
		}
		if s.writable() != nil || s.isHalted() {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C():
		}
	}
}

// pass writes what is due; while Redis is down it only probes, with a full rewrite, every
// HEARTBEAT_INTERVAL.
func (s *RedisSync) pass(ctx context.Context) {
	s.io.Lock()
	defer s.io.Unlock()
	now := s.clk.Now()
	s.mu.Lock()
	if s.lost || s.finished || s.halted || (s.down && now.Before(s.probeAt)) {
		s.mu.Unlock()
		return
	}
	mode := planDue
	if s.down {
		mode = planAll
	}
	plan := s.planLocked(now, mode)
	s.mu.Unlock()
	if s.apply(ctx, now, plan) == nil {
		s.recovered(ctx)
	}
}

// planMode selects what planLocked takes.
type planMode int

const (
	planDue   planMode = iota // what is due: owner/hb timers and the coalesce windows (sync loop)
	planDirty                 // every change not yet written, no owner/hb (Release)
	planAll                   // everything known (Flush, the rewrite after an outage)
)

// planLocked takes the writes of mode. Pending events always leave the queue here.
func (s *RedisSync) planLocked(now time.Time, mode planMode) syncPlan {
	p := syncPlan{
		owner:  mode == planAll || mode == planDue && !now.Before(s.nextOwner),
		beat:   mode == planAll || mode == planDue && !now.Before(s.nextBeat),
		events: s.events,
		ttl:    s.stateTTLLocked(now),
	}
	s.events = nil
	if s.rt.set && takes(mode, s.rt.dirty(), now.Sub(s.rt.since) >= rtCoalesce) {
		v := s.rt.val
		p.rt, p.rtVer = &v, s.rt.ver
	}
	if s.turn.set && takes(mode, s.turn.dirty(), s.turn.val.deleted || now.Sub(s.turn.since) >= turnCoalesce) {
		v := s.turn.val
		p.turn, p.turnVer = &v, s.turn.ver
	}
	if s.summary.set && takes(mode, s.summary.dirty(), true) {
		v := s.summary.val
		p.summary, p.sumVer = &v, s.summary.ver
	}
	return p
}

func takes(mode planMode, dirty, windowOver bool) bool {
	switch mode {
	case planAll:
		return true
	case planDirty:
		return dirty
	}
	return dirty && windowOver
}

// stateTTLLocked is TTL_S (§7.9.1).
func (s *RedisSync) stateTTLLocked(now time.Time) time.Duration {
	if s.rt.set && s.rt.val.ExpiresAtMS > 0 {
		return max(stateTTLFloor, time.UnixMilli(s.rt.val.ExpiresAtMS).Sub(now)+stateTTLFloor)
	}
	return s.duration + preActiveTTLPlus
}

// apply runs plan in order (owner first, so a refused owner writes nothing). The first error
// marks Redis down and drops the remaining events.
func (s *RedisSync) apply(ctx context.Context, now time.Time, p syncPlan) error {
	if p.owner {
		if err := s.refreshOwner(ctx); err != nil {
			return err
		}
		s.mu.Lock()
		s.nextOwner = now.Add(s.ownerEvery)
		s.mu.Unlock()
	}
	if p.beat {
		if err := s.do(ctx, now, "hb", func(c context.Context) error {
			return s.store.WriteHeartbeat(c, s.sid, now, s.beatTTL)
		}); err != nil {
			return err
		}
		s.mu.Lock()
		s.nextBeat = now.Add(s.beatEvery)
		s.mu.Unlock()
	}
	for _, e := range p.events {
		if err := s.do(ctx, now, "events", func(c context.Context) error { return s.store.AppendEvent(c, s.sid, e) }); err != nil {
			return err
		}
	}
	if p.rt != nil {
		rt := *p.rt
		rt.PodID, rt.Fence, rt.UpdatedAtMS = s.pod, s.fence, now.UnixMilli()
		if err := s.do(ctx, now, "rt", func(c context.Context) error { return s.store.WriteRT(c, s.sid, rt, p.ttl) }); err != nil {
			return err
		}
		s.mu.Lock()
		s.rt.wrote(p.rtVer, now)
		s.mu.Unlock()
	}
	if p.turn != nil {
		if err := s.do(ctx, now, "turn", func(c context.Context) error {
			if p.turn.deleted {
				return s.store.DeleteTurn(c, s.sid)
			}
			t := p.turn.snap
			t.UpdatedAtMS = now.UnixMilli()
			return s.store.WriteTurn(c, s.sid, t, p.ttl)
		}); err != nil {
			return err
		}
		s.mu.Lock()
		s.turn.wrote(p.turnVer, now)
		s.mu.Unlock()
	}
	if p.summary != nil {
		sum := *p.summary
		if sum.UpdatedAtMS == 0 {
			sum.UpdatedAtMS = now.UnixMilli()
		}
		if err := s.do(ctx, now, "summary", func(c context.Context) error { return s.store.WriteSummary(c, s.sid, sum, p.ttl) }); err != nil {
			return err
		}
		s.mu.Lock()
		s.summary.wrote(p.sumVer, now)
		s.mu.Unlock()
	}
	return nil
}

// refreshOwner runs the owner script; a refusal is the fence loss.
func (s *RedisSync) refreshOwner(ctx context.Context) error {
	var ok bool
	if err := s.do(ctx, s.clk.Now(), "owner", func(c context.Context) error {
		var err error
		ok, err = s.store.AcquireOwner(c, s.sid, s.pod, s.fence, s.ownerTTL)
		return err
	}); err != nil {
		return err
	}
	if ok {
		return nil
	}
	err := fmt.Errorf("conversation: owner of session held by a newer fence: %w", session.ErrFenceLost)
	s.mu.Lock()
	first := !s.lost
	s.lost, s.events = true, nil
	s.mu.Unlock()
	if first {
		s.latch.trip()
		s.log.WarnContext(ctx, "redis owner refresh refused: fence lost", "session_id", s.sid, "fence", s.fence)
		if s.onFenceLost != nil {
			s.onFenceLost(err)
		}
	}
	return err
}

// do runs one Redis operation bounded by 200 ms; an error marks Redis down until the next probe.
func (s *RedisSync) do(ctx context.Context, now time.Time, op string, f func(context.Context) error) error {
	octx, cancel := s.withTimeout(ctx, redisOpTimeout)
	err := f(octx)
	cancel()
	if err == nil {
		return nil
	}
	s.mu.Lock()
	wasDown := s.down
	s.down, s.events = true, nil
	s.probeAt = now.Add(s.beatEvery)
	s.mu.Unlock()
	if ctx.Err() == nil {
		msg := "redis state write failed; keeping the snapshot in memory"
		if wasDown {
			msg = "redis still unavailable"
		}
		s.log.WarnContext(ctx, msg, "session_id", s.sid, "fence", s.fence, "op", op, "error", err)
	}
	return err
}

func (s *RedisSync) recovered(ctx context.Context) {
	s.mu.Lock()
	was := s.down
	s.down = false
	s.mu.Unlock()
	if was {
		s.log.InfoContext(ctx, "redis state rewritten after outage", "session_id", s.sid, "fence", s.fence)
	}
}
