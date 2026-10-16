package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/conversation/rtstate"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/session"
)

// This file is the life of a session without, and between, connections (spec §7.10): the
// disconnect (§7.10.1), the resume on the pod that holds the runtime (§7.10.2) and the start of a
// runtime that takes the session over from another pod (§7.10.3). Manager.attachResume and
// Manager.takeover are the entries; replay.go holds the replay algorithm.

const (
	takeoverBudget   = 3 * time.Second // §7.10.3 step 7: the loads of a takeover
	maxSnapshotTurns = 8               // session.snapshot.recent_turns
	// maxTakeoverTurns bounds the turns a takeover reads back: the newest 12 are the recentTurns
	// ring, all of them give turn_count.
	maxTakeoverTurns = 500
	// replayReadBudget bounds the Redis calls the loop itself makes for a resume (Flush, then the
	// stream read; each is also bounded to 200 ms by the store, §7.9.4).
	replayReadBudget = time.Second
)

// Stages and error classes of the turns a disconnect or a takeover fails (§7.13.1 TURN_FAILED).
const (
	stageDisconnect = "DISCONNECT"
	stageTakeover   = "TAKEOVER"
	classDisconnect = "CLIENT_DISCONNECTED"
	classTakeover   = "TAKEOVER"
)

var (
	errNotResumable = errors.New("conversation: session not resumable")
	errTakenOver    = errors.New("conversation: a newer owner took the session over")
)

// ---- disconnect (§7.10.1) ----

// detachConn handles the end of the current connection: a read or write error, the heartbeat, a
// slow consumer. lastActivity of the connection is when the client was last heard.
func (r *Runtime) detachConn(cs *connState, who string) {
	if r.cs != cs {
		return
	}
	code := exitCloseCode(who)
	r.detach(cs, cs.act.Last(), code, closeReason(code), false)
}

// detach is §7.10.1. The connection cs is closed (after its queue drained when drain is set, so a
// last event still reaches a client that is being replaced), the runtime stays: the generation is
// cancelled, the turn in progress fails, the STT stream is closed, the timers of the session keep
// running and the reconnect window starts. from is when the client was last heard; the seconds
// since then are what the session will not charge. The events of the cancelled generation and
// the failed turn go to the replay stream only: there is no connection to send them to.
func (r *Runtime) detach(cs *connState, from time.Time, code int, reason string, drain bool) {
	spoken := 0
	if r.gen != nil { // the writer of the connection that goes away knows what was written
		spoken = cs.writer.SentencesWritten(r.gen.number)
	}
	if drain {
		cs.closeAfterDrain(code, reason)
	} else {
		cs.closeNow(code, reason) // idempotent: a violation or slow-consumer close may be under way
	}
	r.cs, r.cmdCh, r.notices, r.writerAcks, r.connExits = nil, nil, nil, nil, nil
	r.connected = false
	r.disconnectedAt, r.discCounted = from, false
	r.moveState(SessionDetached)
	if r.status != session.StatusActive || r.ending {
		r.pushRT() // STARTING (onStarted finishes the job) or an end in progress
		return
	}
	r.failTurnForDetach(spoken, stageDisconnect, classDisconnect)
	r.enterDetached(from)
}

// enterDetached is the part of §7.10.1 after the turn: the STT stream closes, the state is
// LISTENING again, the reconnect window starts and the disconnect is recorded.
func (r *Runtime) enterDetached(from time.Time) {
	r.dropSTT()
	r.resetListening()
	r.dl.reconnect = r.d.Clock.Now().Add(r.d.Cfg.ReconnectWindow)
	r.submit(SessionEvent{Type: string(session.EventDisconnect), Payload: map[string]protocol.Timestamp{"at": {Time: from}}})
	r.pushRT()
}

// failTurnForDetach cancels the active generation (ai.interrupted{DISCONNECTED}) and finalizes the
// turn in progress FAILED with what it has so far (turn.completed{FAILED}); stage and class go
// into TURN_FAILED. A turn that was already FAILED and waits for its APOLOGY keeps its own error.
// The turn does not count toward consecutiveFailedTurns: it is the connection that failed, not a
// provider.
func (r *Runtime) failTurnForDetach(spoken int, stage, class string) {
	t, g := r.turn, r.gen
	if g != nil {
		r.interruptGenerationWith("DISCONNECTED", spoken)
	}
	r.dl.sttFinal = time.Time{}
	if t == nil {
		return
	}
	now := r.d.Clock.Now()
	if !t.hasUser && t.initiator == initiatorUser { // the words so far are the user's side of the turn
		if text := strings.TrimSpace(r.sttSt.TurnText()); text != "" {
			started, ended := t.startedAt, now
			if !t.userEndedAt.IsZero() {
				ended = t.userEndedAt
			}
			r.submit(UpdateUser{TurnID: t.id, User: TurnUser{CreatedAt: t.createdAt, Text: text, StartedAt: &started, EndedAt: &ended,
				STTProvider: nonEmpty(r.sttProvider())}})
			t.hasUser, t.userText = true, text
		}
	}
	var fin session.TurnFinal
	failedBefore := t.failed != nil
	switch {
	case failedBefore:
		fin = *t.failed
	case g != nil:
		fin = r.finalOf(g, session.TurnFailed, g.spokenText(spoken))
		if g.g.Text() == "" {
			fin.AssistantText = nil
		}
		if spoken == 0 {
			fin.AssistantSpokenText = nil
		}
	default:
		fin = session.TurnFinal{CreatedAt: t.createdAt}
	}
	fin.Status, fin.CompletedAt = session.TurnFailed, now
	if fin.ErrorCode == nil {
		fin.ErrorCode = nonEmpty(class)
	}
	r.finalize(t.id, fin)
	r.d.Metrics.Turn(string(session.TurnFailed))
	if !failedBefore {
		r.submitTurnFailed(t, stage, class)
	}
	tc := protocol.TurnCompleted{Status: string(session.TurnFailed), AssistantText: fin.AssistantText, AssistantSpokenText: fin.AssistantSpokenText}
	if t.hasUser {
		u := t.userText
		tc.UserText = &u
	}
	r.emit("turn.completed", evMeta{turn: t}, tc)
	r.turn = nil
	r.sync.ClearTurn()
	r.postTurn(t, session.TurnFailed, "", "")
}

// resetListening returns the audio side to a clean LISTENING: a new detector and STT text state
// (the old ones belong to the stream and the connection that are gone), no timers of a turn.
func (r *Runtime) resetListening() {
	r.turnState = StateListening
	r.det = NewDetector(DetectorConfigFrom(r.d.Cfg))
	r.audioMs = 0
	r.sttSt = newSTTState(func() int64 { return r.audioMs })
	r.discardAt = map[CommitID]string{}
	r.lastFrameAt, r.lastPartialAt = time.Time{}, time.Time{}
	r.partialDirty = false
	r.hints = nil
	r.sttAudioBase = 0
	r.dl.sttFinal, r.dl.playback = time.Time{}, time.Time{}
}

// settleDisconnected is what a session that ends with the client away does before BeginEnding
// (§7.10.1): the seconds since disconnectedFrom, floor((now − disconnectedFrom) / 1 s), are written
// (once per episode) and the persister is flushed so they are in before the session leaves ACTIVE
// and is settled: the time away is not charged, whatever ends the session. It reports false when
// the fence was lost, which stops the runtime.
func (r *Runtime) settleDisconnected(ctx context.Context) bool {
	if r.connected || r.disconnectedAt.IsZero() || r.discCounted {
		return true
	}
	r.discCounted = true
	if r.submitDisconnected(r.disconnectedAt, r.d.Clock.Now()) {
		if err := r.persister.Flush(ctx); errors.Is(err, session.ErrFenceLost) {
			r.onFenceLost(err) //nolint:contextcheck // the terminal path reads the status under its own bound (ioCtx)
			return false
		}
	}
	return true
}

// submitDisconnected queues AddDisconnected for the whole seconds between from and now; false when
// there are none.
func (r *Runtime) submitDisconnected(from, now time.Time) bool {
	secs := int(now.Sub(from) / time.Second) // Duration division truncates: the floor
	if secs <= 0 {
		return false
	}
	r.submit(AddDisconnected{Seconds: secs})
	return true
}

// onReconnectTimeout is RECONNECT_WINDOW passed with no client (§7.10.1): the time away is added
// and the session ends RECONNECT_TIMEOUT. The client is not there to hear session.ending and
// session.completed; they go to the replay stream.
func (r *Runtime) onReconnectTimeout(now time.Time) {
	if r.connected || r.ending {
		return
	}
	r.d.Metrics.Reconnect("timeout", "", now.Sub(r.disconnectedAt))
	r.endWith(session.EndReconnectTimeout) // endWith adds the disconnected seconds first
}

// ---- resume on the pod that holds the runtime (§7.10.2) ----

// resumable: only an ACTIVE session that is not ending takes a new connection (§7.3.2).
func (r *Runtime) resumable() bool {
	return !r.stopped && !r.ending && !r.handoff && r.status == session.StatusActive && !r.takingOver
}

// attachReq is a resume on this pod: the new connection and the STT stream the Manager opened for
// it (outside the loop: connecting takes up to 3 s).
type attachReq struct {
	h       *handshake
	lastSeq int64
	stt     provider.RoutedSTTSession
	cancel  context.CancelFunc
	sttErr  error
	err     error // the result: errNotResumable, or nil
}

// onAttach runs on the loop: the connection of a resume replaces the current one (§7.10.2 steps
// 3–8). An old connection that is still attached (half-open, another tab) is closed 4011 and
// treated as a disconnect from its lastActivity.
func (r *Runtime) onAttach(a *attachReq) {
	if !r.resumable() {
		r.closeSTT(a.stt, a.cancel)
		a.err = errNotResumable
		return
	}
	now := r.d.Clock.Now()
	if old := r.cs; old != nil {
		r.emitError(errs.CodeSessionTakenOver, nil, nil) // the last thing the old connection hears
		r.detach(old, old.act.Last(), protocol.CloseCode(errs.CodeSessionTakenOver), string(errs.CodeSessionTakenOver), true)
	} else if r.disconnectedAt.IsZero() { // a start that never saw a client: nothing was missed
		r.disconnectedAt = now
	}
	from := r.disconnectedAt
	if !r.discCounted {
		r.submitDisconnected(from, now)
	}
	r.discCounted, r.dl.reconnect = false, time.Time{}
	known := r.serverSeq
	r.adoptConn(a.h.cs)
	if a.sttErr != nil { // D-017: no other vendor; the new client hears the end
		r.fatal(session.EndProviderFailure, errs.CodeProviderFailure, a.sttErr)
		return
	}
	r.resetListening()
	r.installSTT(a.stt, a.cancel)
	r.moveState(SessionActive)
	r.sendResume(a.lastSeq, known, r.readReplay(), false, true, now.Sub(from))
	r.pushRT()
	r.startPendingOpening()
}

// installSTT makes stt the stream of the session.
func (r *Runtime) installSTT(stt provider.RoutedSTTSession, cancel context.CancelFunc) {
	r.stt, r.sttCancel = stt, cancel
	r.sttPipe.Store(&sttBox{s: stt})
	r.startSTTReceiver(stt)
}

// readReplay flushes what the redisSync still holds and reads the replay stream. A stream that
// cannot be read is an empty one (§7.10.4): the resume then completes only when the client already
// has every event the runtime issued, and is UNAVAILABLE otherwise.
func (r *Runtime) readReplay() []rtstate.EventEntry {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.ctx), replayReadBudget)
	defer cancel()
	if err := r.sync.Flush(ctx); err != nil && !errors.Is(err, session.ErrFenceLost) {
		r.lg().Warn("redis sync flush before the replay failed", "error", err.Error())
	}
	entries, err := r.d.Store.ReadEvents(ctx, r.sid)
	if err != nil {
		r.lg().Warn("replay stream unavailable", "error", err.Error())
		return nil
	}
	return entries
}

// sendResume is §7.10.2 step 7 / §7.10.3 step 10: the events the client missed with replay=true,
// then session.snapshot, which ends the resume. known is the largest server_seq issued before
// this resume. down is a takeover that could not read Redis at all (§7.9.4): known is then not
// known, and the answer is the snapshot alone.
func (r *Runtime) sendResume(lastSeq, known int64, entries []rtstate.EventEntry, down, samePod bool, gap time.Duration) {
	mode := replayUnavailable
	var missed []rtstate.EventEntry
	if !down {
		missed, mode = replay(entries, lastSeq, known)
		if len(missed) > replayMaxEvents {
			missed, mode = nil, replayUnavailable
		}
	}
	for _, e := range missed {
		env, err := withReplayFlag(e.Env)
		if err != nil {
			r.lg().Error("replay entry skipped", "seq", e.Seq, "error", err.Error())
			continue
		}
		if err := r.cs.out.EnqueueControl(env, true); errors.Is(err, ErrSlowConsumer) {
			r.lg().Warn("slow consumer during the replay: closing the connection")
			r.cs.closeNow(protocol.CloseCode(errs.CodeSlowConsumer), string(errs.CodeSlowConsumer))
			return
		}
	}
	r.emit("session.snapshot", genMeta(nil, r.generation), r.snapshot(mode, len(missed)))
	r.submit(SessionEvent{Type: string(session.EventReconnect), Payload: map[string]any{
		"window_seconds": int(gap / time.Second), "replay": mode, "same_pod": samePod,
	}})
	result := "same_pod"
	if !samePod {
		result = "takeover"
	}
	r.d.Metrics.Reconnect(result, strings.ToLower(mode), gap)
}

// snapshot is the payload of session.snapshot: the state a client needs to carry on, and the
// baseline its last_server_seq becomes (the server_seq of the snapshot itself).
func (r *Runtime) snapshot(mode string, replayed int) protocol.SessionSnapshot {
	turns := r.recent
	if len(turns) > maxSnapshotTurns {
		turns = turns[len(turns)-maxSnapshotTurns:]
	}
	recent := make([]protocol.RecentTurn, 0, len(turns))
	for _, t := range turns {
		rt := protocol.RecentTurn{TurnID: t.id, TurnSeq: t.ctx.Seq, Status: t.ctx.Status, UserText: t.ctx.UserText}
		if t.spoken != "" {
			s := t.spoken
			rt.AssistantSpokenText = &s
		}
		recent = append(recent, rt)
	}
	return protocol.SessionSnapshot{
		Replay: mode, ReplayedCount: replayed, ServerSeqBaseline: r.serverSeq + 1,
		Session: protocol.SnapshotSession{
			Status: string(r.status), ConversationStartMode: string(r.rec.ConversationStartMode),
			StartedAt: protocol.Timestamp{Time: r.startedAt}, ExpiresAt: protocol.Timestamp{Time: r.expiresAt},
			RemainingSeconds: r.remainingSeconds(),
		},
		TurnState: string(r.turnState), ActiveGeneration: r.generation, NextTurnSeq: r.nextTurnSeq, RecentTurns: recent,
	}
}

// startPendingOpening plays the opening of an AI_FIRST session whose client was away when it
// started (startOpening had nobody to speak to).
func (r *Runtime) startPendingOpening() {
	if r.openingPending && r.turn == nil && r.connected && r.accepting {
		r.openingPending = false
		r.startOpening()
	}
}

// ---- takeover (§7.10.3) ----

// takeoverInput is what Manager.takeover learned before the new runtime exists (steps 2–6).
type takeoverInput struct {
	lastSeq int64
	// rt, turn, summary are what the previous owner left in Redis; have* says each was found. down:
	// Redis could not be read (§7.9.4): the takeover runs on Postgres alone.
	rt          rtstate.RT
	haveRT      bool
	turn        rtstate.TurnSnap
	haveTurn    bool
	summary     rtstate.Summary
	haveSummary bool
	entries     []rtstate.EventEntry
	down        bool
	from        time.Time // disconnectedFrom
	fromPod     string
	// known is the largest server_seq of the previous owner: max(rt.last_server_seq, stream tip);
	// lastDurable the largest durable one (the prev_seq chain of the new owner continues from it).
	known, lastDurable int64
}

// takeoverResult is what the loads of a takeover add to startResult.
type takeoverResult struct {
	maxSeq, maxGen int
	turns          []session.TurnRecord // the newest maxTakeoverTurns non-DISCARDED turns, oldest first
}

// beginTakeover starts the runtime of a takeover: like begin, but the session is ACTIVE already
// (the previous owner activated it) and what the startup does is §7.10.3 steps 6–8.
func (r *Runtime) beginTakeover(in *takeoverInput) {
	r.takeover, r.takingOver = in, true
	r.status = session.StatusActive
	if in.haveRT && in.rt.Adaptive.AlgorithmVersion != 0 { // §7.8.6: the runtime's adjustments win over the snapshot
		r.adaptive = AdaptiveState{Current: in.rt.Adaptive, TurnsSinceAdjust: in.rt.TurnsSinceAdjust}
	}
	r.startedAt, r.expiresAt = derefTime(r.rec.StartedAt), derefTime(r.rec.ExpiresAt)
	r.dl.takeBy = r.d.Clock.Now().Add(takeoverBudget)
	r.persister.Start(r.ctx)
	startCtx, startCancel := r.ctxWithDeadline(r.ctx, takeoverBudget)
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer startCancel(nil)
		r.startDone <- r.startupSafe(startCtx)
	}()
	go r.loop()
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// runTakeover is the goroutine side of a takeover: the owner key, FailInProgress, then the loads
// in parallel under the 3 s budget (config snapshot with the memory context and the STT stream,
// the learner block, the turns, the seq and generation maxima).
func (r *Runtime) runTakeover(ctx context.Context) startResult {
	in := r.takeover
	if err := r.sync.Acquire(ctx); err != nil {
		if errors.Is(err, session.ErrFenceLost) {
			return startResult{err: err, fenceLost: true}
		}
		r.log.Warn("owner key not set at takeover", "error", err.Error())
	}
	r.sync.Update(r.takeoverRT())
	r.sync.Start(r.ctx) //nolint:contextcheck // the sync loop lives as long as the runtime, not as long as the takeover
	if err := r.persister.Submit(SessionEvent{Type: string(session.EventOwner), Payload: ownerChanged{
		Reason: "TAKEOVER", FromPod: in.fromPod, ToPod: &r.pod, Fence: r.fence}}); err != nil {
		return startFailed(serverFailure(fmt.Errorf("conversation: submit OWNER_CHANGED: %w", err)))
	}
	// Step 6: the turn the previous owner left in progress ends FAILED.
	if err := failInProgressTurns(ctx, r.d, r.fence, r.sid, in, r.d.Clock.Now()); err != nil {
		if errors.Is(err, session.ErrFenceLost) {
			return startResult{err: err, fenceLost: true}
		}
		return takeoverFailed(ctx, err)
	}

	var res startResult
	var tr takeoverResult
	g, gctx := errgroup.WithContext(ctx)
	goSafe(g, func() error { res.learner = r.loadLearner(gctx); return nil })
	goSafe(g, func() error {
		snap, err := r.d.Configs.LoadSnapshot(gctx, r.rec)
		if err != nil {
			return serverFailure(fmt.Errorf("conversation: load session config: %w", err))
		}
		res.snap = snap
		g2, g2ctx := errgroup.WithContext(gctx)
		goSafe(g2, func() error { res.mem = r.loadTakeoverMemory(g2ctx, snap); return nil })
		goSafe(g2, func() error {
			stt, cancel, err := r.openSTT(g2ctx, snap)
			if err != nil {
				return providerFailure(err)
			}
			res.stt, res.sttCancel = stt, cancel
			return nil
		})
		return g2.Wait()
	})
	goSafe(g, func() error {
		turns, err := r.d.Turns.ListRecent(gctx, r.sid, maxTakeoverTurns)
		if err != nil {
			return serverFailure(fmt.Errorf("conversation: list recent turns: %w", err))
		}
		tr.turns = turns
		return nil
	})
	goSafe(g, func() error {
		maxSeq, maxGen, err := r.d.Turns.MaxSeqAndGeneration(gctx, r.sid)
		if err != nil {
			return serverFailure(fmt.Errorf("conversation: max seq and generation: %w", err))
		}
		tr.maxSeq, tr.maxGen = maxSeq, maxGen
		return nil
	})
	if err := g.Wait(); err != nil {
		r.closeSTT(res.stt, res.sttCancel)
		return takeoverFailed(ctx, err)
	}
	res.startedAt, res.expiresAt, res.takeover = r.startedAt, r.expiresAt, &tr
	return res
}

// takeoverFailed maps an error of the takeover loads: a deadline or cancellation is the budget
// running out, the rest is the failure of its step.
func takeoverFailed(ctx context.Context, err error) startResult {
	if isCancel(err) || errors.Is(context.Cause(ctx), context.DeadlineExceeded) {
		return startResult{err: err, timedOut: true}
	}
	return startFailed(err)
}

// takeoverRT is the rt hash a takeover writes first: the session is ACTIVE with the client attached.
func (r *Runtime) takeoverRT() rtstate.RT {
	in := r.takeover
	return rtstate.RT{
		Status: string(session.StatusActive), TurnState: string(StateListening), Connected: true, ConnID: r.firstConn,
		Adaptive: r.adaptive.Current, TurnsSinceAdjust: r.adaptive.TurnsSinceAdjust,
		Generation: int(r.generation), LastServerSeq: in.known, LastDurableSeq: in.known,
		ExpiresAtMS: r.expiresAt.UnixMilli(),
	}
}

// loadTakeoverMemory is §7.10.3 step 7 for the memory context: the one the previous owner left in
// Redis (written by memory at the start of the session), and only when that is missing or unusable
// a new BuildSessionContext.
func (r *Runtime) loadTakeoverMemory(ctx context.Context, snap ConfigSnapshot) memory.MemoryContext {
	if !r.takeover.down {
		if raw, ok, err := r.d.Store.ReadMemCtx(ctx, r.sid); err == nil && ok {
			var mc memory.MemoryContext
			if json.Unmarshal(raw, &mc) == nil && !mc.Degraded {
				return mc
			}
		}
	}
	return r.loadMemory(ctx, snap)
}

// onTakeoverDone receives the result of the takeover loads.
func (r *Runtime) onTakeoverDone(res startResult) {
	if r.startAborted || r.stopped || r.ending {
		r.closeSTT(res.stt, res.sttCancel)
		return
	}
	switch {
	case res.fenceLost:
		r.closeSTT(res.stt, res.sttCancel)
		r.onFenceLost(res.err)
	case res.timedOut:
		r.closeSTT(res.stt, res.sttCancel)
		r.onTakeoverTimeout()
	case res.err != nil:
		r.closeSTT(res.stt, res.sttCancel)
		r.takingOver = false
		r.fatal(res.reason, res.code, res.err)
	default:
		r.onTakenOver(res)
	}
}

// onTakeoverTimeout: the 3 s budget of the loads ran out (§7.10.3: a failure other than STT ends the
// session SERVER_ERROR; an STT that did not connect is PROVIDER_FAILURE).
func (r *Runtime) onTakeoverTimeout() {
	if !r.takingOver {
		return
	}
	r.startAborted, r.takingOver = true, false
	reason, code := session.EndServerError, errs.CodeInternalError
	if r.startSnapOK.Load() && !r.startSTTOpen.Load() {
		reason, code = session.EndProviderFailure, errs.CodeProviderFailure
	}
	r.fatal(reason, code, errStartTimeout)
}

// onTakenOver plays §7.10.3 steps 8–11 once the loads are done: the baselines of generation and turn
// sequence, the caches of the runtime, the timers, then the replay and session.snapshot.
func (r *Runtime) onTakenOver(res startResult) {
	in, tr := r.takeover, res.takeover
	r.takingOver = false
	r.snap, r.learnerCtx, r.mem = res.snap, res.learner, res.mem
	r.voice = voiceSelection(res.snap.Snapshot.Character)
	r.installSTT(res.stt, res.sttCancel)
	// Step 8: a generation beyond anything the previous owner can still be sending (the client
	// drops frames of a lower one), and the turn numbers after the ones that exist.
	gen := tr.maxGen
	if in.haveRT {
		gen = max(gen, in.rt.Generation)
	}
	r.generation = uint32(gen + 1) //nolint:gosec // generations count audio sentences of one session, far from 2^32
	r.cs.activeGen.Store(r.generation)
	r.nextTurnSeq = max(tr.maxSeq+1, 1) // turn 0 is the opening's, in both start modes
	r.turnCount = len(tr.turns)
	r.recent = recentOf(tr.turns)
	if in.haveSummary {
		r.summary, r.coveredUntil = in.summary.Text, int(in.summary.CoveredUntilSeq)
	}
	r.status, r.turnState, r.accepting = session.StatusActive, StateListening, true
	r.dl.takeBy = time.Time{}
	r.moveState(SessionActive)
	r.armTimers() // warnings that already passed are not sent again (armTimers keeps the future ones)
	// Step 9 and 10: the time away, then what the client missed and the snapshot.
	now := r.d.Clock.Now()
	r.submitDisconnected(in.from, now)
	r.sendResume(in.lastSeq, in.known, in.entries, in.down, false, now.Sub(in.from))
	r.pushRT()
	r.log.Info("session taken over", "from_pod", in.fromPod, "fence", r.fence, "generation", r.generation)
	if r.pendingEnd {
		r.userEnd()
	}
}

// recentOf turns the rows of the newest turns into the recentTurns ring (12 turns): what the prompt
// shows (the spoken text of an INTERRUPTED turn, D-078) and what the user heard.
func recentOf(turns []session.TurnRecord) []recentTurn {
	var out []recentTurn
	for _, t := range turns {
		if t.Status == nil || *t.Status == session.TurnDiscarded {
			continue
		}
		st := string(*t.Status)
		tc := TurnContext{Seq: t.Sequence, Status: st, UserText: t.UserText}
		spoken := ""
		if t.AssistantSpokenText != nil {
			spoken = *t.AssistantSpokenText
		}
		switch {
		case st == string(session.TurnInterrupted):
			tc.AssistantText = spoken
		case t.AssistantText != nil:
			tc.AssistantText = *t.AssistantText
		}
		out = append(out, recentTurn{id: t.ID, ctx: tc, spoken: spoken})
	}
	if len(out) > maxRecentTurns {
		out = out[len(out)-maxRecentTurns:]
	}
	return out
}
