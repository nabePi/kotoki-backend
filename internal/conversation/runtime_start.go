package conversation

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"slices"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/sync/errgroup"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/conversation/rtstate"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/otel"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// Timeouts of §7.6.3 step 4.
const (
	learnerTimeout     = time.Second     // c: learner prompt context; failure degrades
	memoryTimeout      = 2 * time.Second // d: memory context; failure or Degraded degrades (S-15)
	sttConnectTimeout  = 3 * time.Second // e: OpenSTT connect; failure is fatal (D-017: no other vendor)
	maxKeyterms        = 50              // provider.STTSessionConfig.Keyterms
	maxScenarioRunes   = 500             // memory.SessionContextInput.ScenarioText
	scenarioSeparators = "\n"
)

// startResult is what the startup goroutine hands to the loop (startDone).
type startResult struct {
	snap      ConfigSnapshot
	learner   prompt.LearnerContext
	mem       memory.MemoryContext
	stt       provider.RoutedSTTSession
	sttCancel context.CancelFunc
	startedAt time.Time
	expiresAt time.Time

	err       error
	reason    session.EndReason // PROVIDER_FAILURE or SERVER_ERROR (§7.11.4)
	code      errs.Code         // the `error` event of session.failed: PROVIDER_FAILURE or INTERNAL_ERROR
	fenceLost bool
	// timedOut: a step ended because a deadline passed (the start deadline or a connect limit); the
	// loop attributes it with timeoutFailure, the same way it does when it hits the deadline first.
	timedOut bool
	// takeover is what the loads of a takeover add (nil for a start).
	takeover *takeoverResult
}

// goSafe is g.Go with the panic recovery of every runtime goroutine: a panic fails the start with
// SERVER_ERROR (§7.11.4) instead of taking the pod down.
func goSafe(g *errgroup.Group, f func() error) {
	g.Go(func() (err error) {
		defer func() {
			if v := recover(); v != nil {
				err = serverFailure(&panicError{who: "session start", value: v, stack: debug.Stack()})
			}
		}()
		return f()
	})
}

// startFailure carries the end reason and error code of a failed start step out of the errgroup.
type startFailure struct {
	reason session.EndReason
	code   errs.Code
	err    error
}

func (f *startFailure) Error() string { return f.err.Error() }
func (f *startFailure) Unwrap() error { return f.err }

func providerFailure(err error) error {
	return &startFailure{reason: session.EndProviderFailure, code: errs.CodeProviderFailure, err: err}
}

func serverFailure(err error) error {
	return &startFailure{reason: session.EndServerError, code: errs.CodeInternalError, err: err}
}

// errStartTimeout means the 10 s of SESSION_START_TIMEOUT passed before the runtime was ready.
var errStartTimeout = errors.New("conversation: session start timed out")

// startupSafe is startup with the panic recovery of every runtime goroutine: a panic fails the
// start with SERVER_ERROR instead of taking the pod down.
func (r *Runtime) startupSafe(ctx context.Context) (res startResult) {
	defer func() {
		if v := recover(); v != nil {
			pe := &panicError{who: "session start", value: v, stack: debug.Stack()}
			r.log.Error("panic recovered", "error", pe.Error(), "stack", string(pe.stack))
			res = startFailed(serverFailure(pe))
		}
	}()
	return r.startup(ctx)
}

// startup runs §7.6.3 steps 3–6 on its own goroutine: it owns no loop state (it reads only what
// newRuntime set) and answers with one startResult. ctx ends at the start deadline (a clock-driven
// context) or when the runtime stops.
func (r *Runtime) startup(ctx context.Context) startResult {
	ctx, span := otel.Start(ctx, "session.start",
		attribute.String("session.id", r.sid.String()), attribute.Int64("fence", r.fence))
	var err error
	defer otel.End(span, &err)
	var res startResult
	if r.takeover != nil {
		res = r.runTakeover(ctx)
	} else {
		res = r.runStartup(ctx)
	}
	err = res.err
	result := "ok"
	if err != nil {
		result = "error"
	}
	span.SetAttributes(attribute.String("result", result)) // a constant: nothing for the redaction of otel.Start to hide
	return res
}

func (r *Runtime) runStartup(ctx context.Context) startResult {
	// Step 3: owner key, rt hash, OWNER_CHANGED. A Redis error is not fatal (§7.9.4): the session
	// runs from memory and the redisSync retries; a refused owner is a lost fence.
	if err := r.sync.Acquire(ctx); err != nil {
		if errors.Is(err, session.ErrFenceLost) {
			return startResult{err: err, fenceLost: true}
		}
		r.log.Warn("owner key not set at start", "error", err.Error())
	}
	r.sync.Update(r.startingRT())
	r.sync.Start(r.ctx) //nolint:contextcheck // the sync loop lives as long as the runtime, not as long as the start
	if err := r.persister.Submit(SessionEvent{Type: string(session.EventOwner), Payload: ownerChanged{Reason: "START", ToPod: r.pod, Fence: r.fence}}); err != nil {
		return startFailed(serverFailure(fmt.Errorf("conversation: submit OWNER_CHANGED: %w", err)))
	}

	// Step 4: the loads, in parallel under the start deadline.
	var res startResult
	snapOK, sttOpen := &r.startSnapOK, &r.startSTTOpen
	g, gctx := errgroup.WithContext(ctx)
	goSafe(g, func() error { res.learner = r.loadLearner(gctx); return nil })
	goSafe(g, func() error {
		snap, err := r.d.Configs.LoadSnapshot(gctx, r.rec) // a: snapshot + scenario/prompt + OPENING (b)
		if err != nil {
			return serverFailure(fmt.Errorf("conversation: load session config: %w", err))
		}
		res.snap = snap
		snapOK.Store(true)
		g2, g2ctx := errgroup.WithContext(gctx)
		goSafe(g2, func() error { res.mem = r.loadMemory(g2ctx, snap); return nil }) // d
		goSafe(g2, func() error {                                                    // e
			stt, cancel, err := r.openSTT(g2ctx, snap)
			if err != nil {
				return providerFailure(err)
			}
			res.stt, res.sttCancel = stt, cancel
			sttOpen.Store(true)
			return nil
		})
		return g2.Wait()
	})
	if err := g.Wait(); err != nil {
		r.closeSTT(res.stt, res.sttCancel)
		if isCancel(err) || errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
			return startResult{err: err, timedOut: true}
		}
		return startFailed(err)
	}

	// Step 6: STARTING → ACTIVE.
	startedAt, expiresAt, err := r.d.Session.Activate(ctx, r.sid, r.fence)
	if err != nil {
		r.closeSTT(res.stt, res.sttCancel)
		if errors.Is(err, session.ErrFenceLost) {
			return startResult{err: err, fenceLost: true}
		}
		if isCancel(err) {
			return startResult{err: err, timedOut: true}
		}
		return startFailed(serverFailure(fmt.Errorf("conversation: activate: %w", err)))
	}
	res.startedAt, res.expiresAt = startedAt, expiresAt
	return res
}

func startFailed(err error) startResult {
	var f *startFailure
	if !errors.As(err, &f) {
		f = &startFailure{reason: session.EndServerError, code: errs.CodeInternalError, err: err}
	}
	return startResult{err: f.err, reason: f.reason, code: f.code}
}

func isCancel(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// startingRT is the rt hash written at step 3, built from immutable fields only.
func (r *Runtime) startingRT() rtstate.RT {
	return rtstate.RT{
		Status: string(session.StatusStarting), TurnState: string(StateListening), Connected: true, ConnID: r.firstConn,
		Adaptive: r.adaptive.Current,
	}
}

// ownerChanged is the payload of OWNER_CHANGED (§7.13.1).
type ownerChanged struct {
	Reason  string `json:"reason"`
	FromPod string `json:"from_pod"`
	ToPod   string `json:"to_pod"`
	Fence   int64  `json:"fence"`
}

// loadLearner is step 4c: the learner block of the prompt, degraded to empty on any failure.
func (r *Runtime) loadLearner(ctx context.Context) prompt.LearnerContext {
	if r.d.Learner == nil {
		return prompt.LearnerContext{}
	}
	lctx, cancel := r.ctxWithDeadline(ctx, learnerTimeout)
	defer cancel(nil)
	lc, err := r.d.Learner.PromptContext(lctx, r.uid, r.adaptive.Current)
	if err != nil {
		r.log.Warn("learner context unavailable: starting without it", "error", err.Error())
		return prompt.LearnerContext{}
	}
	return lc
}

// loadMemory is step 4d: the memory context, an empty one when the service fails or reports
// Degraded (S-15).
func (r *Runtime) loadMemory(ctx context.Context, snap ConfigSnapshot) memory.MemoryContext {
	if r.d.Memory == nil {
		return memory.MemoryContext{SessionID: r.sid}
	}
	mctx, cancel := r.ctxWithDeadline(ctx, memoryTimeout)
	defer cancel(nil)
	mc, err := r.d.Memory.BuildSessionContext(mctx, memory.SessionContextInput{
		UserID: r.uid, SessionID: r.sid, ScenarioText: scenarioText(snap),
	})
	if err != nil || mc.Degraded {
		if err != nil {
			r.log.Warn("memory context unavailable: starting without it", "error", err.Error())
		} else {
			r.log.Warn("memory context degraded: starting without it")
		}
		return memory.MemoryContext{SessionID: r.sid}
	}
	return mc
}

// scenarioText is the retrieval query of step 4d: topic and scenario situation, ≤ 500 runes. The
// scenario title is not part of content.SessionConfig, so the situation text stands for it.
func scenarioText(snap ConfigSnapshot) string {
	t := snap.Topic.NameJA
	if t == "" {
		t = snap.Topic.Name
	}
	text := t + scenarioSeparators + snap.ScenarioVersion.Situation
	if rs := []rune(text); len(rs) > maxScenarioRunes {
		text = string(rs[:maxScenarioRunes])
	}
	return text
}

// openSTT is step 4e: the STT stream on the active vendor, connect bounded by 3 s on the runtime
// clock. The stream lives on its own context (sttCtx, §7.7.2), which the returned cancel ends; it
// is also ended when the start is abandoned (gctx).
func (r *Runtime) openSTT(gctx context.Context, snap ConfigSnapshot) (provider.RoutedSTTSession, context.CancelFunc, error) {
	sctx, cancel := context.WithCancel(r.ctx)
	timer := r.cancelAt(r.d.Clock.Now().Add(sttConnectTimeout), cancel)
	stop := context.AfterFunc(gctx, cancel)
	stt, err := r.d.Router.OpenSTT(sctx, provider.STTSessionConfig{ //nolint:contextcheck // the stream outlives the start step: sttCtx derives from the runtime context (§7.7.2)
		SessionID: r.sid, Language: ttsLanguage, SampleRate: provider.STTSampleRate, Keyterms: keyterms(snap),
	})
	r.dropCancel(timer)
	if err != nil {
		stop()
		cancel()
		return nil, nil, fmt.Errorf("conversation: open stt: %w", err)
	}
	if !stop() { // gctx ended while the stream was opening: the start is abandoned
		cancel()
		_ = stt.Close()
		return nil, nil, fmt.Errorf("conversation: open stt: %w", context.Cause(gctx))
	}
	return stt, cancel, nil
}

// keyterms are the scenario vocabulary the STT may use as hints (≤ 50).
func keyterms(snap ConfigSnapshot) []string {
	var out []string
	for _, h := range snap.ScenarioVersion.VocabHints {
		if h.TermJA != "" {
			out = append(out, h.TermJA)
		}
	}
	if len(out) > maxKeyterms {
		out = out[:maxKeyterms]
	}
	return out
}

func (r *Runtime) closeSTT(stt provider.RoutedSTTSession, cancel context.CancelFunc) {
	if stt != nil {
		_ = stt.Close()
	}
	if cancel != nil {
		cancel()
	}
}

// ---- loop side of the start ----

// onStartDone receives the result of the startup goroutine.
func (r *Runtime) onStartDone(res startResult) {
	r.handled++
	if r.takeover != nil {
		r.onTakeoverDone(res)
		return
	}
	if r.startAborted { // the loop already failed the start at the deadline
		r.closeSTT(res.stt, res.sttCancel)
		return
	}
	switch {
	case res.fenceLost:
		r.closeSTT(res.stt, res.sttCancel)
		r.onFenceLost(res.err)
	case res.timedOut:
		r.onStartTimeout()
	case res.err != nil:
		r.closeSTT(res.stt, res.sttCancel)
		r.failStart(res.reason, res.code, res.err)
	default:
		r.onStarted(res)
	}
}

// onStartTimeout fails the start when a deadline passed: the loop hit the start deadline before
// the startup goroutine answered (whatever it is stuck in, the loop does not wait for it), or the
// goroutine reported a step that ran into a deadline. The deadline is attributed to the step that
// had not finished: STT not open → the provider (PROVIDER_FAILURE), otherwise the server
// (config/DB: SERVER_ERROR), §7.11.4.
func (r *Runtime) onStartTimeout() {
	r.startAborted = true
	reason, code := session.EndServerError, errs.CodeInternalError
	if r.startSnapOK.Load() && !r.startSTTOpen.Load() {
		reason, code = session.EndProviderFailure, errs.CodeProviderFailure
	}
	r.failStart(reason, code, errStartTimeout)
}

// onStarted plays §7.6.3 steps 7–9 after Activate succeeded.
func (r *Runtime) onStarted(res startResult) {
	r.snap, r.learnerCtx, r.mem = res.snap, res.learner, res.mem
	r.voice = voiceSelection(res.snap.Snapshot.Character)
	r.stt, r.sttCancel = res.stt, res.sttCancel
	r.sttPipe.Store(&sttBox{s: res.stt})
	r.startSTTReceiver(res.stt)
	r.startedAt, r.expiresAt = res.startedAt, res.expiresAt
	r.status, r.turnState, r.accepting, r.nextTurnSeq = session.StatusActive, StateListening, true, 1
	r.dl.startBy = time.Time{}
	if r.connected {
		r.moveState(SessionActive)
	} else { // the client left while the session was starting: §7.10.1 as for any disconnect
		r.moveState(SessionDetached)
		r.enterDetached(r.disconnectedAt)
	}
	r.armTimers()
	r.pushRT()
	r.emit("session.started", genMeta(nil, r.generation), protocol.SessionStarted{
		Status: string(session.StatusActive), ConversationStartMode: string(r.rec.ConversationStartMode),
		StartedAt: protocol.Timestamp{Time: r.startedAt}, ExpiresAt: protocol.Timestamp{Time: r.expiresAt},
		DurationSeconds: r.rec.DurationSeconds, RemainingSeconds: r.remainingSeconds(), TurnState: string(r.turnState),
	})
	r.log.Info("session started", "mode", string(r.rec.ConversationStartMode), "fence", r.fence)
	if r.pendingEnd { // §7.11.3: session.end arrived while STARTING
		r.userEnd()
		return
	}
	if r.rec.ConversationStartMode == session.StartAIFirst {
		r.startOpening()
	}
}

// armTimers is §7.6.3 step 7: the warnings, the expiry and the balance.tick; the heartbeat and
// the owner refresh belong to the redisSync.
func (r *Runtime) armTimers() {
	now := r.d.Clock.Now()
	r.dl.expiry = r.expiresAt
	r.dl.warn, r.dl.warnOff = nil, nil
	offs := slices.Clone(r.d.Cfg.SessionWarningOffsets)
	slices.SortFunc(offs, func(a, b time.Duration) int { return int(b - a) }) // largest offset = earliest warning
	for _, off := range offs {
		if at := r.expiresAt.Add(-off); at.After(now) {
			r.dl.warn = append(r.dl.warn, at)
			r.dl.warnOff = append(r.dl.warnOff, off)
		}
	}
	r.dl.balance = r.startedAt.Add(r.d.Cfg.BalanceTickInterval)
}

// onWarning sends the T-60 s / T-10 s notice. The closing instruction of the prompt follows from
// the remaining time (the builder switches at 60 s), so no flag is kept here.
func (r *Runtime) onWarning(off time.Duration) {
	r.emit("session.ending", evMeta{}, protocol.SessionEnding{
		Stage: "WARNING", Reason: string(session.EndDurationElapsed), RemainingSeconds: int(off / time.Second),
	})
}

// onBalanceTick sends balance.tick every BALANCE_TICK_INTERVAL of elapsed time while a client is
// attached; the schedule keeps its alignment to multiples of the interval even when detached.
func (r *Runtime) onBalanceTick(now time.Time) {
	iv := r.d.Cfg.BalanceTickInterval
	for !r.dl.balance.After(now) {
		r.dl.balance = r.dl.balance.Add(iv)
	}
	if !r.connected {
		return
	}
	r.emit("balance.tick", evMeta{}, protocol.BalanceTick{
		RemainingSeconds: r.remainingSeconds(), ElapsedSeconds: int(now.Sub(r.startedAt) / time.Second),
		ExpiresAt: protocol.Timestamp{Time: r.expiresAt},
	})
}
