package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/conversation/rtstate"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/session"
)

const (
	loopTick                    = 100 * time.Millisecond // the loop ticker of §7.7.2 (every deadline is checked on it)
	queueSampleEvery            = time.Second            // outbound_queue_depth sampling (§7.14)
	defaultProviderSessionGrace = 15 * time.Second       // PROVIDER_SESSION_GRACE
	ioTimeout                   = 5 * time.Second        // one fenced Postgres call / flush from the loop
)

// Limits are the per-pod capacity limits (config.Realtime, spec §7.12.2).
type Limits struct {
	ReadyMax int // REALTIME_READY_MAX_SESSIONS: /readyz 503 from this many sessions (95)
	Max      int // REALTIME_MAX_SESSIONS: session.start is refused 4503 from here (100)
	HardMax  int // REALTIME_HARD_MAX_SESSIONS: the gateway refuses the upgrade (120)
}

// Deps is everything the Manager and its Runtimes consume (spec §7.13.1); all of them are
// injected by cmd/realtime (D-004) or by rtharness.
type Deps struct {
	Cfg     config.RealtimeTuning
	Limits  Limits
	Clock   clock.Clock
	IDs     idgen.Generator
	Log     *slog.Logger
	Metrics *Metrics

	Session SessionRuntime
	Turns   TurnRepository
	Events  SessionEventWriter
	Store   RuntimeStore
	Configs ConfigLoader
	Learner LearnerContextProvider
	Memory  memory.RetrievalService
	Router  provider.RealtimeRouter
	Builder PromptBuilder

	// NewTicker makes every ticker of the runtime graph (loop, redisSync, heartbeat, handshake);
	// tests inject a ManualTicker. Default NewRealTicker.
	NewTicker func(time.Duration) Ticker
	// Apology returns the APOLOGY packets; nil → the embedded clip (audio.ApologyFrames).
	Apology func() ([][]byte, error)

	// Sleep is the pacer's wait (see genParams.Sleep); nil → real time. The harness advances its
	// fake clock instead.
	Sleep func(ctx context.Context, d time.Duration) error
	// ProviderSessionGrace is PROVIDER_SESSION_GRACE: a generation is cut at
	// expires_at + this (§7.7.4). Zero → 15 s.
	ProviderSessionGrace time.Duration
	// STTFinalTimeout is PROVIDER_TIMEOUT_STT_FINAL: the turn goes on with the running text when
	// the FINAL of the TURN_END commit takes longer (§7.6.2). Zero → 1.5 s.
	STTFinalTimeout time.Duration
}

// withDefaults fills what a partial wiring leaves out; the tuning keeps the spec §7.2 defaults
// for the knobs the Manager and Runtime read directly (the components default their own).
func (d Deps) withDefaults() Deps {
	if d.Clock == nil {
		d.Clock = clock.System{}
	}
	if d.IDs == nil {
		d.IDs = idgen.NewV7()
	}
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	if d.Builder == nil {
		d.Builder = NewPromptBuilder()
	}
	if d.NewTicker == nil {
		d.NewTicker = NewRealTicker
	}
	if d.ProviderSessionGrace <= 0 {
		d.ProviderSessionGrace = defaultProviderSessionGrace
	}
	if d.STTFinalTimeout <= 0 {
		d.STTFinalTimeout = defaultSTTFinal
	}
	c := &d.Cfg
	c.WSHandshakeTimeout = orDur(c.WSHandshakeTimeout, 10*time.Second)
	c.SessionStartTimeout = orDur(c.SessionStartTimeout, 10*time.Second)
	c.BalanceTickInterval = orDur(c.BalanceTickInterval, 10*time.Second)
	c.SessionExpiryGrace = orDur(c.SessionExpiryGrace, 10*time.Second)
	c.ReconnectWindow = orDur(c.ReconnectWindow, 30*time.Second)
	c.PlaybackFinishGraceMS = orInt(c.PlaybackFinishGraceMS, 1500)
	c.AudioMaxLeadMS = orInt(c.AudioMaxLeadMS, defaultMaxLeadMS)
	c.MaxConsecutiveFailedTurns = orInt(c.MaxConsecutiveFailedTurns, 3)
	if len(c.SessionWarningOffsets) == 0 {
		c.SessionWarningOffsets = []time.Duration{60 * time.Second, 10 * time.Second}
	}
	if c.PodID == "" {
		c.PodID = "realtime-0"
	}
	return d
}

func orDur(v, def time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return def
}

// panicError is a recovered panic of one goroutine of the runtime or of a connection; the loop
// turns it into FATAL{SERVER_ERROR} (§7.7.2: a panic never takes the pod down).
type panicError struct {
	who   string
	value any
	stack []byte
}

func (e *panicError) Error() string {
	return fmt.Sprintf("conversation: panic in %s: %v", e.who, e.value)
}

// ctlFn is a closure the loop runs on its own goroutine: the one way for another goroutine to
// read or change state that only the loop owns (Runtime.call, Runtime.post).
type ctlFn func(r *Runtime)

// sttMsg is one STT event on its way to the loop, or the marker of Runtime.Barrier.
type sttMsg struct {
	ev     provider.STTEvent
	marker chan struct{}
}

// deadlines are the points in time Runtime.loop checks on every tick against clock.Now(): there
// is no time.Timer in the loop, so the fake clock of the tests drives every one of them
// (spec 7.7.2). A zero time is unarmed. Adding a deadline is: a field here, a branch in
// checkDeadlines and a handler; the tasks that extend the runtime do exactly that:
//
//	Task 19: sttFinal (STT_FINAL_TIMEOUT 1.5 s after the TURN_END commit; armed in endUserTurn)
//	Task 21: reconnect (RECONNECT_WINDOW 30 s after a disconnect)
//	Task 22: grace (expires_at + SESSION_EXPIRY_GRACE) and the flush deadline of finish()
//	Task 23: safepoint (DRAIN_SAFEPOINT_TIMEOUT)
type deadlines struct {
	startBy  time.Time   // STARTING must be ACTIVE by now + SESSION_START_TIMEOUT (§7.6.3 step 4)
	warn     []time.Time // session.ending WARNING points (expires_at − offset), consumed in order
	warnOff  []time.Duration
	expiry   time.Time // expires_at (§7.11.2)
	balance  time.Time // next balance.tick (aligned to elapsed multiples of 10 s)
	playback time.Time // row 14: estimated end of playback + PLAYBACK_FINISH_GRACE_MS
	sttFinal time.Time // STT_FINAL_TIMEOUT after the TURN_END commit (rows 3 → 7)
}

// timedCancel is a context cancellation armed against the runtime clock (see cancelAt).
type timedCancel struct {
	id     uint64
	at     time.Time
	cancel context.CancelCauseFunc
}

// Runtime is the actor of one session (spec §7.7.2): Runtime.loop is the only goroutine that
// reads or writes the session, turn and generation state, so none of it is guarded by a mutex.
// Everything else talks to the loop through channels: commands, notices and audio come from the
// connection's reader, STT events from the sttReceiver, generation events from the goroutines
// of a Generation (each carrying its generation number: the loop drops events of a generation
// that is no longer r.gen), writer acks from the Writer, and closures (ctl) from other
// goroutines. A Runtime outlives its connections: a connState is adopted on attach and dropped on
// disconnect while the session, the STT/generation state, the persister and the redisSync stay.
//
// Lifecycle: Manager.startRuntime builds it and calls begin; the startup goroutine runs
// §7.6.3 steps 3–6 and hands the result to the loop (startDone); the loop then plays steps 7–9.
// The loop ends in exit, which is the only place resources are released.
//
// Time: the loop reads time only from Deps.Clock. Deadlines are time.Time values checked on the
// 100 ms tick (deadlines); contexts that must expire on that clock come from cancelAt/ctxWithDeadline.
//
// Extension points for the later tasks (each is marked with its task in the code):
//   - Turn engine (Task 19): audio pipeline in connState.startAudio, user turns in
//     runtime_turn.go (rows 1–9), onSTTEvent, post-turn tasks in closeTurn.
//   - Barge-in and failures (Task 20): onGenFailed branches for REPLY/APOLOGY, bargeIn.
//   - Disconnect, resume, takeover (Task 21): detachConn, Manager.attachResume/takeover.
//   - End (Task 22): endSession/fatal/finish in runtime_end.go (grace, USER_SPEAKING/PROCESSING
//     handling, expiry), panic recovery for generation goroutines.
//   - Manager (Task 23): admission/drain around Manager.Attach and Runtime.drain.
type Runtime struct {
	d   Deps
	log *slog.Logger
	sid uuid.UUID
	uid uuid.UUID
	pod string
	// fence is runtime_fence of this ownership; every fenced write carries it.
	fence int64
	// rec is the session row as of ClaimRuntime/MarkStarting (immutable columns only).
	rec session.RuntimeRecord

	ctx    context.Context // sessionCtx: ends the runtime
	cancel context.CancelFunc
	doneCh chan struct{} // closed when exit has finished
	onExit func(*Runtime)

	persister *Persister
	sync      *RedisSync
	latch     *FenceLatch
	seen      *EventIDCache // client event_id dedupe, survives reconnects (§7.4.2)

	// Loop inputs that do not belong to a connection.
	ctl         chan ctlFn
	startDone   chan startResult       // startup goroutine → loop (cap 1)
	fenceLost   chan error             // persister/redisSync callbacks → loop (cap 1, non-blocking send)
	fatalCh     chan error             // persister fatal callback → loop (cap 1, non-blocking send)
	frameEvents chan frameEvent        // audio pipeline → loop (cap 64)
	sttPipe     atomic.Pointer[sttBox] // the STT stream for the audio goroutine; the loop stores it
	sttEvents   chan sttMsg            // sttReceiver → loop (cap 64)
	sttSync     chan chan struct{}
	genEvents   chan GenEvent // generation goroutines → loop (cap 128)
	wg          sync.WaitGroup

	// firstConn is the conn_id of the connection the runtime was created with (the startup goroutine
	// writes it into the first rt hash); startSnapOK/startSTTOpen tell the loop how far the
	// startup got when it times out.
	firstConn    string
	startSnapOK  atomic.Bool
	startSTTOpen atomic.Bool

	cmu       sync.Mutex // guards cancels (registered from the startup goroutine too)
	cancels   []*timedCancel
	cancelSeq uint64

	// ---- owned by Runtime.loop from here on ----

	// Inputs of the current connection; nil channels (no connection) never become ready.
	cs         *connState
	cmdCh      <-chan protocol.ClientEvent
	notices    <-chan ReaderNotice
	writerAcks <-chan WriterAck
	connExits  <-chan connExit

	status         session.Status
	metricState    string // label of sessions_active this runtime is counted under
	turnState      TurnState
	accepting      bool // acceptingTurns (§7.6)
	connected      bool
	disconnectedAt time.Time
	serverSeq      int64 // last allocated server_seq (per session, §7.4.3)
	lastDurable    int64
	generation     uint32 // r.generation: raised when a generation starts and when it is cancelled
	nextTurnSeq    int
	turnCount      int
	handled        uint64 // inputs handled, for Runtime.Barrier's quiescence check
	pendingEnd     bool   // session.end received while STARTING (§7.11.3)
	ending         bool
	stopped        bool
	startAborted   bool // the loop gave up on the startup (timeout) before it reported

	snap       ConfigSnapshot
	voice      provider.VoiceSelection // voiceSelection(snapshot character), once per session (D-015)
	learnerCtx prompt.LearnerContext
	mem        memory.MemoryContext
	adaptive   AdaptiveState
	startedAt  time.Time
	expiresAt  time.Time

	stt       provider.RoutedSTTSession
	sttCancel context.CancelFunc
	sttSt     *sttState
	det       *Detector // the turn detector (§7.6.2): audio time is its frames × 20 ms
	audioMs   int64     // audio clock of sttSt = det.NowMs()
	// lastFrameAt is when the reader received the last upstream frame the loop handled (the
	// starvation watchdog of §7.5.2).
	lastFrameAt   time.Time
	lastPartialAt time.Time // stt.partial throttle
	partialDirty  bool      // a partial was held back by the throttle
	sttAudioBase  int64     // Stats().AudioMsSent at the end of the previous turn (stt_audio_ms)
	failedTurns   int       // consecutiveFailedTurns (§7.6.1); Task 20 raises it, a good turn resets it

	// Post-turn state (§7.7.5, §7.8): the recentTurns ring, the rolling summary and the memory jobs.
	recent         []recentTurn
	summary        string
	coveredUntil   int
	summarizer     *Summarizer
	summaryRunning bool
	memBusy        bool
	// discardAt remembers which boundary (ai_speaking, listening) each DISCARD commit was for.
	discardAt map[CommitID]string

	turn *turnRun
	gen  *genRun
	// retired are cancelled generations whose goroutines exit() still awaits (kept for the life of
	// the session; Task 20 may prune the ones that have finished).
	retired []*Generation

	dl         deadlines
	nextSample time.Time
}

// newRuntime builds the Runtime of session sid owned with fence; cs is its first connection.
// onExit is called once, from the loop goroutine, when the runtime has released everything.
func newRuntime(parent context.Context, d Deps, rec session.RuntimeRecord, fence int64, cs *connState, seen *EventIDCache, onExit func(*Runtime)) *Runtime {
	ctx, cancel := context.WithCancel(parent)
	r := &Runtime{
		d: d, sid: rec.ID, uid: rec.UserID, pod: d.Cfg.PodID, fence: fence, rec: rec,
		ctx: ctx, cancel: cancel, doneCh: make(chan struct{}), onExit: onExit, seen: seen,
		latch:       NewFenceLatch(d.Metrics),
		ctl:         make(chan ctlFn, 16),
		startDone:   make(chan startResult, 1),
		fenceLost:   make(chan error, 1),
		fatalCh:     make(chan error, 1),
		frameEvents: make(chan frameEvent, 64),
		sttEvents:   make(chan sttMsg, 64),
		sttSync:     make(chan chan struct{}),
		genEvents:   make(chan GenEvent, 128),
		status:      session.StatusStarting,
		turnState:   StateListening,
		adaptive:    AdaptiveState{Current: parseAdaptive(rec.AdaptiveDifficulty)},
		discardAt:   map[CommitID]string{},
		det:         NewDetector(DetectorConfigFrom(d.Cfg)),
	}
	r.firstConn = cs.id
	r.log = d.Log.With("session_id", rec.ID.String(), "conn_id", cs.id)
	r.persister = NewPersister(rec.ID, d.Turns, d.Events, d.Session, fence, d.Metrics, d.Log,
		PersisterClock(d.Clock), PersisterFenceLatch(r.latch),
		PersisterOnFenceLost(func(err error) { signal(r.fenceLost, err) }),
		PersisterOnFatal(func(err error) { signal(r.fatalCh, err) }))
	r.sync = NewRedisSync(d.Store, d.Clock, rec.ID, r.pod, fence, d.Metrics, d.Log,
		RedisSyncTuning(d.Cfg), RedisSyncSessionDuration(time.Duration(rec.DurationSeconds)*time.Second),
		RedisSyncFenceLatch(r.latch), RedisSyncTicker(d.NewTicker),
		RedisSyncOnFenceLost(func(err error) { signal(r.fenceLost, err) }))
	r.sttSt = newSTTState(func() int64 { return r.audioMs })
	r.adoptConn(cs)
	return r
}

// signal is the non-blocking send of a persister/redisSync callback to the loop: a full channel
// means the loop was already told.
func signal(ch chan<- error, err error) {
	select {
	case ch <- err:
	default:
	}
}

// parseAdaptive decodes sessions.adaptive_difficulty; an empty or invalid document is the zero
// value (the prompt builder rejects what it cannot use, the generation clamps its sentences).
func parseAdaptive(raw []byte) learner.AdaptiveDifficulty {
	var ad learner.AdaptiveDifficulty
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &ad) // a bad document degrades to defaults; the snapshot is immutable and validated by M3
	}
	return ad
}

// begin starts the goroutines of the runtime: the persister, the loop and the startup (§7.6.3
// steps 3–6). The caller has already run ClaimRuntime and MarkStarting.
func (r *Runtime) begin() {
	r.dl.startBy = r.d.Clock.Now().Add(r.d.Cfg.SessionStartTimeout)
	r.metricState = SessionStarting
	r.d.Metrics.SessionState("", SessionStarting)
	r.persister.Start(r.ctx)
	startCtx, startCancel := r.ctxWithDeadline(r.ctx, r.d.Cfg.SessionStartTimeout)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer startCancel(nil)
		r.startDone <- r.startupSafe(startCtx)
	}()
	go r.loop()
}

// Done is closed once the runtime has stopped and released everything.
func (r *Runtime) Done() <-chan struct{} { return r.doneCh }

// ---- loop ----

// loop is Runtime.loop. It never blocks on I/O except in the terminal paths (finish, failStart).
func (r *Runtime) loop() {
	defer r.exit()
	defer r.recoverLoop()
	tk := r.d.NewTicker(loopTick)
	defer tk.Stop()
	for !r.stopped {
		select {
		case <-r.ctx.Done():
			r.onHardStop()
			return
		case <-tk.C():
			r.onTick(r.d.Clock.Now())
		case fn := <-r.ctl:
			fn(r)
		case res := <-r.startDone:
			r.onStartDone(res)
		case err := <-r.fenceLost:
			r.onFenceLost(err)
		case err := <-r.fatalCh:
			r.onFatalSignal(err)
		case ev := <-r.cmdCh:
			r.onCommand(ev)
		case n := <-r.notices:
			r.onNotice(n)
		case fe := <-r.frameEvents:
			r.onFrameEvent(fe)
		case m := <-r.sttEvents:
			r.onSTTMsg(m)
		case ge := <-r.genEvents:
			r.onGenEvent(ge)
		case a := <-r.writerAcks:
			r.onWriterAck(a)
		case x := <-r.connExits:
			r.onConnExit(x)
		}
	}
}

// recoverLoop turns a panic of the loop itself into the SERVER_ERROR end of §7.11.4, as far as the
// state allows: the end is attempted under its own recover and the runtime stops either way.
func (r *Runtime) recoverLoop() {
	v := recover()
	if v == nil {
		return
	}
	pe := &panicError{who: "runtime loop", value: v, stack: debug.Stack()}
	r.log.Error("panic in the runtime loop", "error", pe.Error(), "stack", string(pe.stack))
	func() {
		defer func() { _ = recover() }() // the state may be what panicked: stopping matters more than ending cleanly
		r.fatal(session.EndServerError, errs.CodeInternalError, pe)
	}()
	r.stopped = true
}

// drain handles every input that is ready right now, until none is (Runtime.Barrier). It is the
// loop's select without the blocking: keep the two in step when adding an input.
func (r *Runtime) drain() {
	for !r.stopped {
		select {
		case res := <-r.startDone:
			r.onStartDone(res)
		case err := <-r.fenceLost:
			r.onFenceLost(err)
		case err := <-r.fatalCh:
			r.onFatalSignal(err)
		case ev := <-r.cmdCh:
			r.onCommand(ev)
		case n := <-r.notices:
			r.onNotice(n)
		case fe := <-r.frameEvents:
			r.onFrameEvent(fe)
		case m := <-r.sttEvents:
			r.onSTTMsg(m)
		case ge := <-r.genEvents:
			r.onGenEvent(ge)
		case a := <-r.writerAcks:
			r.onWriterAck(a)
		case x := <-r.connExits:
			r.onConnExit(x)
		default:
			return
		}
	}
}

// onTick runs on every loop tick: clock-driven context cancellations, deadlines, sampling.
func (r *Runtime) onTick(now time.Time) {
	r.fireCancels(now)
	r.checkDeadlines(now)
	r.checkStarvation(now)
	r.flushPartial(now)
	r.flushDueText(now)
	if r.cs != nil && !now.Before(r.nextSample) {
		r.nextSample = now.Add(queueSampleEvery)
		r.d.Metrics.OutboundDepth(r.cs.out.ControlLen(), r.cs.out.AudioLen())
	}
}

// checkDeadlines fires what is due, in priority order. A handler may arm or clear deadlines.
func (r *Runtime) checkDeadlines(now time.Time) {
	due := func(t time.Time) bool { return !t.IsZero() && !now.Before(t) }
	switch {
	case r.stopped:
		return
	case r.status == session.StatusStarting && due(r.dl.startBy):
		r.dl.startBy = time.Time{}
		r.onStartTimeout()
	}
	if r.stopped || r.status != session.StatusActive {
		return
	}
	if due(r.dl.expiry) {
		r.dl.expiry = time.Time{}
		r.onExpiry()
		return
	}
	for len(r.dl.warn) > 0 && due(r.dl.warn[0]) {
		off := r.dl.warnOff[0]
		r.dl.warn, r.dl.warnOff = r.dl.warn[1:], r.dl.warnOff[1:]
		r.onWarning(off)
	}
	if due(r.dl.balance) {
		r.onBalanceTick(now)
	}
	if due(r.dl.playback) {
		r.dl.playback = time.Time{}
		r.onPlaybackDeadline()
	}
	if due(r.dl.sttFinal) {
		r.dl.sttFinal = time.Time{}
		r.onSTTFinalTimeout()
	}
}

// ---- clock-driven contexts ----

// ctxWithDeadline derives a context cancelled when the runtime clock passes now + d (cause
// context.DeadlineExceeded) or when parent ends; the CancelCauseFunc releases it early. Because
// it follows Deps.Clock, a test fires it with AdvanceClock + Tick, never with a real sleep.
func (r *Runtime) ctxWithDeadline(parent context.Context, d time.Duration) (context.Context, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	id := r.cancelAt(r.d.Clock.Now().Add(d), func() { cancel(context.DeadlineExceeded) })
	return ctx, func(cause error) {
		r.dropCancel(id)
		cancel(cause)
	}
}

// cancelAt arms f to run on the first tick at or after at; it returns an id for dropCancel.
func (r *Runtime) cancelAt(at time.Time, f func()) uint64 {
	r.cmu.Lock()
	defer r.cmu.Unlock()
	r.cancelSeq++
	tc := &timedCancel{id: r.cancelSeq, at: at, cancel: func(error) { f() }}
	r.cancels = append(r.cancels, tc)
	return tc.id
}

func (r *Runtime) dropCancel(id uint64) {
	r.cmu.Lock()
	defer r.cmu.Unlock()
	for i, tc := range r.cancels {
		if tc.id == id {
			r.cancels = append(r.cancels[:i], r.cancels[i+1:]...)
			return
		}
	}
}

// fireCancels runs (outside the lock) every armed cancellation that is due.
func (r *Runtime) fireCancels(now time.Time) {
	r.cmu.Lock()
	var due []*timedCancel
	kept := r.cancels[:0]
	for _, tc := range r.cancels {
		if now.Before(tc.at) {
			kept = append(kept, tc)
			continue
		}
		due = append(due, tc)
	}
	r.cancels = kept
	r.cmu.Unlock()
	for _, tc := range due {
		tc.cancel(nil)
	}
}

// ---- calls from other goroutines ----

// post runs fn on the loop; false when the runtime has stopped.
func (r *Runtime) post(ctx context.Context, fn ctlFn) bool {
	select {
	case r.ctl <- fn:
		return true
	case <-r.doneCh:
		return false
	case <-ctx.Done():
		return false
	}
}

// call runs fn on the loop and waits for it to finish.
func (r *Runtime) call(ctx context.Context, fn ctlFn) error {
	done := make(chan struct{})
	if !r.post(ctx, func(r *Runtime) { fn(r); close(done) }) {
		return errRuntimeStopped
	}
	select {
	case <-done:
		return nil
	case <-r.doneCh:
		return errRuntimeStopped
	case <-ctx.Done():
		return ctx.Err()
	}
}

var errRuntimeStopped = errors.New("conversation: runtime stopped")

// View is a copy of the state the loop owns, for tests and diagnostics (Runtime.View).
type View struct {
	Status      session.Status
	TurnState   TurnState
	Accepting   bool
	Connected   bool
	Generation  uint32
	NextTurnSeq int
	ServerSeq   int64
	CurrentTurn uuid.UUID // uuid.Nil: no turn in progress
	PendingEnd  bool
	// OutboundDrained is true when the connection's queue and writer hold nothing (true with no
	// connection).
	OutboundDrained bool
	// Handled counts the inputs the loop has handled; two equal values around a Barrier mean
	// nothing arrived in between.
	Handled uint64
}

// View returns a consistent copy of the loop state.
func (r *Runtime) View(ctx context.Context) (View, error) {
	var v View
	err := r.call(ctx, func(r *Runtime) {
		v = View{Status: r.status, TurnState: r.turnState, Accepting: r.accepting, Connected: r.connected,
			Generation: r.generation, NextTurnSeq: r.nextTurnSeq, ServerSeq: r.serverSeq, PendingEnd: r.pendingEnd,
			Handled: r.handled, OutboundDrained: r.cs == nil || r.cs.out.Drained()}
		if r.turn != nil {
			v.CurrentTurn = r.turn.id
		}
	})
	return v, err
}

// Barrier returns once everything that reached the loop's inputs before the call has been
// handled: a marker travels through the audio pipeline and through the STT receiver in order
// with their events, then the loop drains the rest of its inputs. Events a generation goroutine
// has not produced yet are not covered (wait for the event you expect). It is the seam of
// rtharness.H.Sync.
func (r *Runtime) Barrier(ctx context.Context) error {
	var cs *connState
	var haveSTT bool
	if err := r.call(ctx, func(r *Runtime) { cs, haveSTT = r.cs, r.stt != nil }); err != nil {
		return err
	}
	if cs != nil {
		ab := make(chan struct{})
		select {
		case cs.audioIn <- AudioIn{Barrier: ab}:
			select {
			case <-ab:
			case <-r.doneCh:
				return errRuntimeStopped
			case <-ctx.Done():
				return ctx.Err()
			}
		case <-cs.ctx.Done(): // connection gone: nothing to wait for on this path
		case <-r.doneCh:
			return errRuntimeStopped
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if haveSTT {
		marker := make(chan struct{})
		select {
		case r.sttSync <- marker:
			select {
			case <-marker:
			case <-r.doneCh:
				return errRuntimeStopped
			case <-ctx.Done():
				return ctx.Err()
			}
		case <-r.doneCh:
			return errRuntimeStopped
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return r.call(ctx, func(r *Runtime) { r.drain() })
}

// ---- connection ----

// adoptConn makes cs the connection of the runtime: the loop starts reading its channels and the
// writer is told the active generation. Before the loop runs this is called by newRuntime; after
// it (a reconnect, Task 21) it runs on the loop.
func (r *Runtime) adoptConn(cs *connState) {
	r.cs = cs
	r.cmdCh, r.notices, r.writerAcks, r.connExits = cs.cmdCh, cs.notices, cs.acks, cs.exits
	cs.activeGen.Store(r.generation)
	r.connected = true
	r.disconnectedAt = time.Time{}
	r.startAudio(cs)
	r.log = r.d.Log.With("session_id", r.sid.String(), "conn_id", cs.id)
}

// onConnExit handles the end of the current connection. A fatal violation found by the reader is
// answered first (error event, PROTOCOL_VIOLATION row, close 1008); a slow consumer or a dead
// peer just detaches.
func (r *Runtime) onConnExit(x connExit) {
	r.handled++
	cs := r.cs
	if cs == nil {
		return
	}
	var pan *panicError
	if errors.As(x.err, &pan) {
		r.fatalPanic(pan)
		return
	}
	var pe *ProtocolError
	if errors.As(x.err, &pe) {
		r.protocolViolation(pe)
	}
	r.log.Info("connection ended", "who", x.who, "failed", x.failed())
	r.detachConn(cs, x.who)
}

// fatalPanic ends the session after a recovered panic (SERVER_ERROR, INTERNAL_ERROR, close 1011).
func (r *Runtime) fatalPanic(pe *panicError) {
	r.log.Error("panic recovered", "error", pe.Error(), "stack", string(pe.stack))
	r.fatal(session.EndServerError, errs.CodeInternalError, pe)
}

// exitCloseCode is the best-effort close code for a connection whose goroutine "who" ended: the
// peer is normally gone already, so this only matters when it is not.
func exitCloseCode(who string) int {
	switch who {
	case "heartbeat":
		return protocol.CloseCode(errs.CodeHeartbeatTimeout)
	case "writer":
		return protocol.CloseCode(errs.CodeSlowConsumer)
	}
	return 1000
}

func closeReason(code int) string {
	switch code {
	case protocol.CloseCode(errs.CodeHeartbeatTimeout):
		return string(errs.CodeHeartbeatTimeout)
	case protocol.CloseCode(errs.CodeSlowConsumer):
		return string(errs.CodeSlowConsumer)
	}
	return ""
}

// protocolViolation answers a fatal ProtocolError: the event the client may still read, the
// audit row (no frame content) and the 1008 close.
func (r *Runtime) protocolViolation(pe *ProtocolError) {
	r.emitError(pe.Code, nil, nil)
	_ = r.persister.Submit(SessionEvent{Type: string(session.EventProtocol), Payload: map[string]string{"code": string(pe.Code), "detail": pe.Detail}})
	if r.cs != nil {
		r.cs.closeAfterDrain(pe.CloseCode(), string(pe.Code))
	}
}

// detachConn drops the connection cs. Task 21 extends it with §7.10.1 (cancel the generation,
// fail the turn in progress, close STT, start the reconnect timer); Task 18 only releases the
// connection and records that the client is gone.
func (r *Runtime) detachConn(cs *connState, who string) {
	if r.cs != cs {
		return
	}
	code := exitCloseCode(who)
	cs.closeNow(code, closeReason(code)) // idempotent: the first close wins (a violation or slow-consumer close is already under way)
	r.cs, r.cmdCh, r.notices, r.writerAcks, r.connExits = nil, nil, nil, nil, nil
	r.connected = false
	r.disconnectedAt = r.d.Clock.Now()
	r.moveState(SessionDetached)
	r.pushRT()
}

// moveState moves this runtime between the labels of sessions_active; "" removes it.
func (r *Runtime) moveState(to string) {
	if r.metricState == to {
		return
	}
	r.d.Metrics.SessionState(r.metricState, to)
	r.metricState = to
}

func (r *Runtime) onNotice(n ReaderNotice) {
	r.handled++
	r.emitError(n.Code, n.RefEventID, nil)
}

// ---- emit ----

// evMeta is what binds an event to a turn and a generation (§7.4.5 columns "turn" and "gen").
type evMeta struct {
	turn *turnRun
	gen  *uint32
}

func genMeta(t *turnRun, g uint32) evMeta { return evMeta{turn: t, gen: &g} }

// emit allocates the next server_seq, stamps the event and sends it: to the redisSync (durable
// events go to the replay stream even with no client attached, so a resume can replay them) and
// to the connection's Outbound (none while detached). A durable event that does not fit the
// queue is a slow consumer (D-082): the connection is closed 4101 and the runtime goes on.
func (r *Runtime) emit(typ string, m evMeta, payload any) {
	r.serverSeq++
	ev := protocol.ServerEvent{
		Type: typ, Version: protocol.Version, EventID: r.d.IDs.New(), ServerSeq: r.serverSeq, SessionID: r.sid,
		TS: r.d.Clock.Now(), Payload: payload,
	}
	if m.turn != nil {
		id, seq := m.turn.id, m.turn.seq
		ev.TurnID, ev.TurnSeq = &id, &seq
	}
	ev.Generation = m.gen
	env, err := protocol.MarshalServer(ev)
	if err != nil {
		r.log.Error("marshal server event", "type", typ, "error", err.Error())
		return
	}
	durable := EventDurable(ev)
	if durable {
		r.lastDurable = ev.ServerSeq
	}
	r.sync.Event(ev, env)
	if r.cs == nil {
		return
	}
	if err := r.cs.out.EnqueueControl(env, durable); errors.Is(err, ErrSlowConsumer) {
		r.log.Warn("slow consumer: closing the connection", "type", typ)
		r.cs.closeNow(protocol.CloseCode(errs.CodeSlowConsumer), string(errs.CodeSlowConsumer))
	}
}

// emitError sends an `error` event of the catalog (§7.4.8); a non-fatal one is durable and a
// fatal one is not (EventDurable decides from the payload).
func (r *Runtime) emitError(code errs.Code, ref *uuid.UUID, details any, opts ...protocol.ErrorOption) {
	r.emit("error", evMeta{}, protocol.NewError(code, r.d.IDs.New(), ref, details, opts...))
}

// pushRT hands the current rt snapshot to the redisSync (coalesced ≤ 1 write / 100 ms).
func (r *Runtime) pushRT() {
	r.sync.Update(r.rtSnapshot())
}

func (r *Runtime) rtSnapshot() rtstate.RT {
	rt := rtstate.RT{
		Status: string(r.status), TurnState: string(r.turnState), AcceptingTurns: r.accepting, Connected: r.connected,
		Generation: int(r.generation), NextTurnSeq: r.nextTurnSeq, LastServerSeq: r.serverSeq, LastDurableSeq: r.lastDurable,
		Adaptive: r.adaptive.Current, TurnsSinceAdjust: r.adaptive.TurnsSinceAdjust,
	}
	if r.cs != nil {
		rt.ConnID = r.cs.id
	}
	if !r.disconnectedAt.IsZero() {
		rt.DisconnectedAtMS = r.disconnectedAt.UnixMilli()
	}
	if r.turn != nil {
		rt.CurrentTurnID = r.turn.id.String()
	}
	if !r.expiresAt.IsZero() {
		rt.ExpiresAtMS = r.expiresAt.UnixMilli()
	}
	return rt
}

// lg returns the logger with the turn and generation of the moment (§7.14 log fields).
func (r *Runtime) lg() *slog.Logger {
	l := r.log.With("generation", r.generation)
	if r.turn != nil {
		l = l.With("turn_id", r.turn.id.String())
	}
	return l
}

// remainingSeconds is ceil(expires_at − now), never negative.
func (r *Runtime) remainingSeconds() int {
	if r.expiresAt.IsZero() {
		return 0
	}
	d := r.expiresAt.Sub(r.d.Clock.Now())
	if d <= 0 {
		return 0
	}
	return int((d + time.Second - 1) / time.Second)
}

// ioCtx is the context of one fenced call the loop makes itself (Session, flush): independent of
// the session context, so the terminal path still runs after a hard stop, bounded by ioTimeout.
func (r *Runtime) ioCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(r.ctx), ioTimeout)
}

// onCommand dispatches one client command (§7.4.4) the session accepts after the first one.
func (r *Runtime) onCommand(ev protocol.ClientEvent) {
	r.handled++
	switch p := ev.Payload.(type) {
	case protocol.SessionEnd:
		r.onSessionEnd()
	case protocol.ClientPlaybackFinished:
		r.onPlaybackFinished(p)
	case protocol.ClientBargeIn:
		// Row 18: a hint only. Counting it as hint_confirmed/hint_unconfirmed needs the barge-in
		// bookkeeping of Task 20.
	case protocol.Ping:
		r.emit("pong", evMeta{}, protocol.Pong{ClientTS: p.ClientTS, ServerTS: r.d.Clock.Now().UnixMilli()})
	case protocol.SessionStart, protocol.SessionResume:
		ref := ev.EventID
		r.emitError(errs.CodeCommandRejected, &ref, nil) // §7.4.8: a second session.start is rejected
	}
}

// onSTTMsg handles one message of the STT receiver.
func (r *Runtime) onSTTMsg(m sttMsg) {
	if m.marker != nil { // a Barrier marker is not input: it must not move Handled
		close(m.marker)
		return
	}
	r.handled++
	r.onSTTEvent(m.ev)
}

// startSTTReceiver forwards the STT stream to the loop (§7.7.2 sttReceiver). A Barrier request
// is answered in order with the events: the receiver first moves what the stream already holds.
func (r *Runtime) startSTTReceiver(stt provider.RoutedSTTSession) {
	r.goSafe("stt receiver", func() {
		events := stt.Events()
		forward := func(ev provider.STTEvent) bool {
			select {
			case r.sttEvents <- sttMsg{ev: ev}:
				return true
			case <-r.ctx.Done():
				return false
			}
		}
		for {
			select {
			case <-r.ctx.Done():
				return
			case ev, ok := <-events:
				if !ok {
					events = nil // closed: keep answering barrier markers until the runtime ends
					continue
				}
				if !forward(ev) {
					return
				}
			case marker := <-r.sttSync:
				for moved := true; moved; {
					select {
					case ev, ok := <-events:
						if !ok {
							events, moved = nil, false
							continue
						}
						if !forward(ev) {
							close(marker)
							return
						}
					default:
						moved = false
					}
				}
				select {
				case r.sttEvents <- sttMsg{marker: marker}:
				case <-r.ctx.Done():
					close(marker)
					return
				}
			}
		}
	})
}

// goSafe runs fn on a goroutine the runtime waits for in exit. A panic is recovered and reported to
// the loop as a fatal error (§7.7.2, §7.11.4 SERVER_ERROR).
func (r *Runtime) goSafe(who string, fn func()) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() {
			if v := recover(); v != nil {
				signal(r.fatalCh, &panicError{who: who, value: v, stack: debug.Stack()})
			}
		}()
		fn()
	}()
}
