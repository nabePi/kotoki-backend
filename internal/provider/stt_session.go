package provider

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	rotationBufferBytes = 480000                   // 15 s of PCM16LE mono 16 kHz
	replayChunkBytes    = 3200                     // 100 ms
	sttBytesPerMs       = STTSampleRate * 2 / 1000 // 32
	finalTimeoutLimit   = 3                        // consecutive FINAL timeouts that fail the vendor
	vendorCodeFinalTO   = "final_timeout"
	sttEventBuffer      = 64
)

// rotationTriggers are the vendor closes that mean "reconnect to the same vendor": a
// duration or idle limit, not a failure (spec 8.5.8).
// "session_expired" is the OpenAI Realtime session-duration code (unverified, M9-T4).
var rotationTriggers = map[string]bool{"session_time_limit_exceeded": true, "insufficient_audio_activity": true, "session_expired": true}

// OpenSTT opens a stream on the single active STT vendor: breaker, then a semaphore try
// (no wait), then Open with a connect timeout. There is no other vendor to try.
func (r *Router) OpenSTT(ctx context.Context, cfg STTSessionConfig) (RoutedSTTSession, error) {
	p := r.stt.Name()
	tk, rel, aerr := r.admit(ctx, CapSTT)
	if aerr != nil {
		if aerr.VendorCode != "circuit_open" {
			r.metrics.ObserveCall(p, CapSTT, r.cfg.STTModel(), "", "error", aerr.Kind, aerr.VendorCode)
		}
		return nil, aerr
	}
	rt := &rotatingSTT{r: r, cfg: cfg, vendor: p, ring: newPCMRing(rotationBufferBytes, STTSampleRate), release: rel,
		q: newSTTQueue(), out: make(chan STTEvent, sttEventBuffer), done: make(chan struct{})}
	sctx, cancelSession := context.WithCancel(ctx)
	rt.ctx, rt.cancel = sctx, cancelSession

	start := r.clock.Now()
	conn, cancelConn, err := rt.openConn(sctx)
	if err != nil {
		rt.cancel()
		tk.Done(err)
		rel()
		r.metrics.ObserveCall(p, CapSTT, rt.model(), "", "error", err.Kind, err.VendorCode)
		r.health.Record(p, CapSTT, err, r.clock.Now().Sub(start))
		return nil, err
	}
	tk.Done(nil)
	r.metrics.ObserveCall(p, CapSTT, rt.model(), "", "success", "", "")
	r.metrics.ObserveLatency(p, CapSTT, rt.model(), "connect", r.clock.Now().Sub(start))
	r.health.Record(p, CapSTT, nil, r.clock.Now().Sub(start))
	rt.active, rt.cancelConn, rt.connGen = conn, cancelConn, 1
	rt.stats.OpenedAt = r.clock.Now()
	go rt.q.run(rt.out, rt.done)
	go rt.pump(1, 0, conn)
	go func() { // the session ends with its context
		select {
		case <-rt.ctx.Done():
			_ = rt.Close()
		case <-rt.done:
		}
	}()
	return rt, nil
}

// sttQueue hands events to the public channel; a new PARTIAL replaces queued ones, FINAL/
// ERROR/CLOSED are never dropped.
type sttQueue struct {
	mu     sync.Mutex
	q      []STTEvent
	wake   chan struct{}
	closed bool
}

func newSTTQueue() *sttQueue { return &sttQueue{wake: make(chan struct{}, 1)} }

func (s *sttQueue) push(ev STTEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if ev.Type == STTPartial {
		kept := s.q[:0]
		for _, e := range s.q {
			if e.Type != STTPartial {
				kept = append(kept, e)
			}
		}
		s.q = kept
	}
	s.q = append(s.q, ev)
	if ev.Type == STTClosed {
		s.closed = true
	}
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *sttQueue) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// run forwards until CLOSED was delivered, then closes out and done.
func (s *sttQueue) run(out chan<- STTEvent, done chan struct{}) {
	defer close(done)
	defer close(out)
	for {
		s.mu.Lock()
		if len(s.q) == 0 {
			s.mu.Unlock()
			<-s.wake
			continue
		}
		ev := s.q[0]
		s.q = s.q[1:]
		s.mu.Unlock()
		select {
		case out <- ev:
		case <-time.After(2 * time.Second):
			// A live session blocks for a slow reader; once CLOSED was queued the reader is
			// presumed gone and what cannot be delivered is dropped so the goroutine ends.
			if s.isClosed() {
				if ev.Type == STTClosed {
					return
				}
				continue
			}
			out <- ev
		}
		if ev.Type == STTClosed {
			return
		}
	}
}

// rotatingSTT wraps the active vendor's STTSession: breaker accounting, logical offsets, and
// reconnection to the SAME vendor with a 15 s rotation buffer (spec 8.5.8).
type rotatingSTT struct {
	r       *Router
	cfg     STTSessionConfig
	vendor  Name
	ctx     context.Context
	cancel  context.CancelFunc
	release func()
	out     chan STTEvent
	q       *sttQueue
	done    chan struct{}

	mu             sync.Mutex
	ring           *pcmRing
	active         STTSession
	cancelConn     context.CancelFunc
	connGen        int
	vendorOffsetMs int64
	lastFinalEndMs int64
	sentBytes      int64 // logical bytes already written to the active connection
	finalTimeouts  int
	rotating       bool
	pendingRotate  bool // another rotation trigger arrived while one was replaying
	pendingFinN    int  // Finalize calls to (re)send once the replay backlog is out
	finOutstanding int  // Finalize calls sent to the active connection and not answered yet
	ending         bool // fail or Close started: a vendor stream end is expected, not a failure
	terminated     bool
	closedAudioMs  int64 // AudioMsSent of connections that were rotated away
	stats          STTStats
	closeOnce      sync.Once
}

var _ RoutedSTTSession = (*rotatingSTT)(nil)

func (rt *rotatingSTT) model() string { return rt.r.cfg.STTModel() }

// Provider is the active vendor, constant for the session.
func (rt *rotatingSTT) Provider() Name { return rt.vendor }

// Events implements STTSession.
func (rt *rotatingSTT) Events() <-chan STTEvent { return rt.out }

// openConn opens a vendor connection with the connect timeout. The returned cancel ends the
// connection's context (call it when the connection is discarded).
func (rt *rotatingSTT) openConn(parent context.Context) (STTSession, context.CancelFunc, *Error) {
	type res struct {
		s   STTSession
		err error
	}
	cctx, cancel := context.WithCancel(parent)
	ch := make(chan res, 1)
	go func() {
		s, err := rt.r.stt.Open(cctx, rt.cfg)
		ch <- res{s, err}
	}()
	timer := time.NewTimer(rt.r.cfg.TimeoutSTTConnect)
	defer timer.Stop()
	select {
	case x := <-ch:
		if x.err != nil {
			cancel()
			return nil, nil, asError(rt.vendor, CapSTT, x.err)
		}
		return x.s, cancel, nil
	case <-timer.C:
		cancel()
		go func() {
			if x := <-ch; x.s != nil {
				_ = x.s.Close()
			}
		}()
		return nil, nil, &Error{Provider: rt.vendor, Capability: CapSTT, Kind: KindTimeout, VendorCode: "connect_timeout", Retryable: true,
			Message: "STT connect timed out"}
	case <-parent.Done():
		cancel()
		go func() {
			if x := <-ch; x.s != nil {
				_ = x.s.Close()
			}
		}()
		return nil, nil, ctxError(rt.vendor, CapSTT, parent.Err())
	}
}

// SendAudio writes to the rotation buffer and, unless a rotation is replaying, to the active
// connection (non-blocking).
func (rt *rotatingSTT) SendAudio(pcm []byte) error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.terminated {
		return ErrSessionClosed
	}
	rt.ring.Write(pcm)
	if rt.rotating {
		return nil // the replay loop sends it after the backlog
	}
	if err := rt.active.SendAudio(pcm); err != nil {
		return err
	}
	rt.sentBytes += int64(len(pcm))
	return nil
}

// Finalize asks the vendor to finalise; during a replay it is deferred until the backlog is out.
func (rt *rotatingSTT) Finalize() error {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	if rt.terminated {
		return ErrSessionClosed
	}
	if rt.rotating {
		rt.pendingFinN++
		return nil
	}
	if err := rt.active.Finalize(); err != nil {
		return err
	}
	rt.finOutstanding++
	return nil
}

// ReportFinalTimeout counts a missing FINAL{FromFinalize}; the third in a row fails the vendor.
func (rt *rotatingSTT) ReportFinalTimeout() {
	rt.mu.Lock()
	if rt.terminated || rt.ending {
		rt.mu.Unlock()
		return
	}
	rt.finalTimeouts++
	trip := rt.finalTimeouts == finalTimeoutLimit
	rt.mu.Unlock()
	if !trip {
		return
	}
	err := &Error{Provider: rt.vendor, Capability: CapSTT, Kind: KindTimeout, VendorCode: vendorCodeFinalTO, Retryable: true,
		Message: "no FINAL after three Finalize requests"}
	rt.r.breakers[key{rt.vendor, CapSTT}].Record(err)
	rt.r.metrics.ObserveCall(rt.vendor, CapSTT, rt.model(), "", "error", err.Kind, err.VendorCode)
	rt.fail(err)
}

// Stats merges the rotated connections' audio into the session total.
func (rt *rotatingSTT) Stats() STTStats {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	st := rt.stats
	st.AudioMsSent = rt.closedAudioMs
	if rt.active != nil {
		st.AudioMsSent += rt.active.Stats().AudioMsSent
	}
	return st
}

// Close ends the session; idempotent.
func (rt *rotatingSTT) Close() error {
	rt.closeOnce.Do(func() {
		rt.mu.Lock()
		rt.ending = true // before any connection is closed: its stream end is expected
		active, cancelConn := rt.active, rt.cancelConn
		rt.mu.Unlock()
		if active != nil {
			_ = active.Close()
		}
		if cancelConn != nil {
			cancelConn()
		}
		rt.cancel()
		rt.terminate()
	})
	return nil
}

// terminate queues CLOSED once, releases the slot and stops accepting audio.
func (rt *rotatingSTT) terminate() {
	rt.mu.Lock()
	if rt.terminated {
		rt.mu.Unlock()
		return
	}
	rt.terminated = true
	rt.mu.Unlock()
	rt.release()
	rt.q.push(STTEvent{Type: STTClosed, Provider: rt.vendor, ReceivedAt: rt.r.clock.Now()})
}

// emit queues a vendor-derived event for the runtime.
func (rt *rotatingSTT) emit(ev STTEvent) {
	rt.mu.Lock()
	if rt.stats.FirstEventAt.IsZero() {
		rt.stats.FirstEventAt = rt.r.clock.Now()
	}
	rt.mu.Unlock()
	ev.Provider = rt.vendor
	rt.q.push(ev)
}

// fail forwards a fatal STTError and ends the session (the runtime ends the session with
// PROVIDER_FAILURE; there is no other vendor).
func (rt *rotatingSTT) fail(err *Error) {
	rt.mu.Lock()
	if rt.ending || rt.terminated { // one fatal error per session
		rt.mu.Unlock()
		return
	}
	rt.ending = true
	rt.mu.Unlock()
	rt.emit(STTEvent{Type: STTError, Err: err, ReceivedAt: rt.r.clock.Now()})
	go func() { _ = rt.Close() }() // CLOSED follows the error; the vendor connection is released
}

func shift(ev STTEvent, off int64) STTEvent {
	ev.StartMs += off
	ev.EndMs += off
	if len(ev.Words) > 0 {
		w := make([]STTWord, len(ev.Words))
		for i, x := range ev.Words {
			x.StartMs += off
			x.EndMs += off
			w[i] = x
		}
		ev.Words = w
	}
	return ev
}

// pump forwards one connection's events; gen identifies the connection.
func (rt *rotatingSTT) pump(gen int, offset int64, conn STTSession) {
	for ev := range conn.Events() {
		rt.handle(gen, offset, ev)
	}
	rt.mu.Lock()
	unexpected := gen == rt.connGen && !rt.terminated && !rt.ending
	rt.mu.Unlock()
	if unexpected { // the vendor closed the stream without an error and we did not ask for it
		err := &Error{Provider: rt.vendor, Capability: CapSTT, Kind: KindUnavailable, VendorCode: "connection_closed", Retryable: true,
			Message: "STT connection closed unexpectedly"}
		rt.r.breakers[key{rt.vendor, CapSTT}].Record(err)
		rt.fail(err)
	}
}

func (rt *rotatingSTT) handle(gen int, offset int64, ev STTEvent) {
	rt.mu.Lock()
	current := gen == rt.connGen && !rt.ending
	rt.mu.Unlock()
	if !current {
		return // events of a rotated-away connection are stale: the replay re-transcribes its audio
	}
	switch ev.Type {
	case STTPartial:
		rt.emit(shift(ev, offset))
	case STTFinal:
		ev = shift(ev, offset)
		rt.mu.Lock()
		if ev.FromFinalize {
			rt.finalTimeouts = 0
			if rt.finOutstanding > 0 {
				rt.finOutstanding--
			}
		}
		if ev.EndMs > rt.lastFinalEndMs {
			rt.lastFinalEndMs = ev.EndMs
		}
		rt.mu.Unlock()
		rt.emit(ev)
	case STTError:
		if ev.Err == nil {
			return
		}
		if rotationTriggers[ev.Err.VendorCode] {
			rt.startRotation(gen)
			return
		}
		rt.r.breakers[key{rt.vendor, CapSTT}].Record(ev.Err)
		rt.r.metrics.StreamDisconnect(rt.vendor, CapSTT)
		rt.fail(ev.Err)
	case STTClosed:
		// the router emits its own CLOSED
	}
}

// beginRotationLocked switches the session to "rotating" under rt.mu: from this moment audio
// only goes to the buffer and the old connection's events are stale. It returns what the
// reconnect needs.
func (rt *rotatingSTT) beginRotationLocked() (gen int, replayFromMs int64, old STTSession, oldCancel context.CancelFunc) {
	rt.rotating = true
	rt.connGen++
	gen = rt.connGen
	old, oldCancel = rt.active, rt.cancelConn
	rt.active = nil
	if old != nil {
		rt.closedAudioMs += old.Stats().AudioMsSent
	}
	rt.pendingFinN += rt.finOutstanding // unanswered Finalize calls are repeated on the new connection
	rt.finOutstanding = 0
	// replay from the last FINAL, or the oldest audio still buffered; never beyond what was written
	replayFromMs = max(rt.lastFinalEndMs, rt.ring.oldestBytes()/sttBytesPerMs)
	replayFromMs = min(replayFromMs, rt.ring.total/sttBytesPerMs)
	return gen, replayFromMs, old, oldCancel
}

// startRotation handles a rotation trigger of connection fromGen.
func (rt *rotatingSTT) startRotation(fromGen int) {
	rt.mu.Lock()
	if rt.terminated || rt.ending || fromGen != rt.connGen {
		rt.mu.Unlock()
		return
	}
	if rt.rotating { // cut again while still replaying: rotate once more after this replay
		rt.pendingRotate = true
		rt.mu.Unlock()
		return
	}
	gen, from, old, oldCancel := rt.beginRotationLocked()
	rt.mu.Unlock()
	go rt.runRotation(gen, from, old, oldCancel)
}

func closeConn(old STTSession, cancel context.CancelFunc) {
	if old != nil {
		_ = old.Close()
	}
	if cancel != nil {
		cancel()
	}
}

// runRotation reconnects to the same vendor and replays; it repeats when another trigger
// arrived during the replay. Rotation is not a failure: the breaker is not involved.
func (rt *rotatingSTT) runRotation(gen int, replayFromMs int64, old STTSession, oldCancel context.CancelFunc) {
	for {
		go closeConn(old, oldCancel)
		conn, cancelConn, err := rt.openConn(rt.ctx)
		if err != nil {
			rt.fail(err)
			return
		}
		rt.mu.Lock()
		if rt.terminated || rt.ending { // closed while reconnecting
			rt.mu.Unlock()
			closeConn(conn, cancelConn)
			return
		}
		rt.active, rt.cancelConn = conn, cancelConn
		rt.vendorOffsetMs = replayFromMs
		rt.sentBytes = replayFromMs * sttBytesPerMs
		rt.stats.Reconnects++
		rt.mu.Unlock()
		rt.r.metrics.STTRotation(rt.vendor)
		go rt.pump(gen, replayFromMs, conn)

		replayed, again := rt.replay(conn)
		rt.r.metrics.STTReplay(rt.vendor, float64(replayed)/float64(STTSampleRate*2))
		if !again {
			return
		}
		rt.mu.Lock()
		if rt.terminated || rt.ending {
			rt.mu.Unlock()
			return
		}
		gen, replayFromMs, old, oldCancel = rt.beginRotationLocked()
		rt.mu.Unlock()
	}
}

// replay sends the backlog in 100 ms chunks, then the audio that arrived meanwhile, then the
// deferred Finalize calls. It returns the bytes replayed and whether another rotation was
// requested during the replay.
func (rt *rotatingSTT) replay(conn STTSession) (total int64, again bool) {
	for {
		rt.mu.Lock()
		if rt.terminated || rt.ending {
			rt.mu.Unlock()
			return total, false
		}
		if rt.sentBytes < rt.ring.oldestBytes() {
			rt.sentBytes = rt.ring.oldestBytes() // the buffer wrapped past us: skip what is gone
		}
		data := rt.ring.readFromBytes(rt.sentBytes)
		if len(data) == 0 {
			if rt.pendingRotate { // keep rotating: the caller reconnects again
				rt.pendingRotate = false
				rt.mu.Unlock()
				return total, true
			}
			rt.rotating = false
			for n := rt.pendingFinN; n > 0; n-- {
				if conn.Finalize() == nil {
					rt.finOutstanding++
				}
			}
			rt.pendingFinN = 0
			rt.mu.Unlock()
			return total, false
		}
		rt.mu.Unlock()
		waits := 0
		for off := 0; off < len(data); {
			end := min(off+replayChunkBytes, len(data))
			if err := conn.SendAudio(data[off:end]); err != nil {
				if errors.Is(err, ErrSessionClosed) {
					rt.mu.Lock()
					again = rt.pendingRotate
					rt.pendingRotate = false
					rt.mu.Unlock()
					return total, again
				}
				if KindOf(err) != KindUnavailable || waits > 2500 { // a full 2 s buffer drains; anything else is fatal
					rt.fail(asError(rt.vendor, CapSTT, err))
					return total, false
				}
				waits++
				if rt.r.sleep(rt.ctx, 2*time.Millisecond) != nil {
					return total, false
				}
				continue
			}
			waits = 0
			rt.mu.Lock()
			rt.sentBytes += int64(end - off)
			rt.mu.Unlock()
			total += int64(end - off)
			off = end
			if speed := rt.r.cfg.RotationReplaySpeed; speed > 0 {
				if rt.r.sleep(rt.ctx, 100*time.Millisecond/time.Duration(speed)) != nil {
					return total, false
				}
			}
		}
	}
}
