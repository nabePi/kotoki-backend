package kotokiclient

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/kotokiclient/audio"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
)

// Defaults of the waits a script can leave unset.
const (
	// DefaultAITimeout bounds WaitAIDone and the wait of BargeIn for the AI's first audio.
	DefaultAITimeout = 20 * time.Second
	// DefaultEndTimeout bounds how long End waits for session.completed.
	DefaultEndTimeout = 15 * time.Second
	// DefaultHandshakeTimeout bounds the wait for session.ready, session.started and the
	// resume snapshot. It is a real-time failsafe against a silent server, not part of the
	// measurements.
	DefaultHandshakeTimeout = 15 * time.Second
)

var (
	// ErrAITimeout is returned by a script whose AI did not finish within the step's timeout.
	ErrAITimeout = errors.New("kotokiclient: the AI did not answer in time")
	// ErrNoAIAudio is returned by BargeIn when the AI's turn ended without any audio to interrupt.
	ErrNoAIAudio = errors.New("kotokiclient: the AI turn ended without audio")
	// ErrEndTimeout is returned by End when the session was not completed in time.
	ErrEndTimeout = errors.New("kotokiclient: the session did not complete in time")

	errSessionEnded = errors.New("kotokiclient: session ended") // internal: a clean early finish
)

// ServerError is a fatal `error` event of the server (§7.4.8); it ends the conversation.
type ServerError struct {
	Code      string
	Message   string
	Retryable bool
}

// Error implements error.
func (e *ServerError) Error() string {
	return fmt.Sprintf("kotokiclient: server error %s: %s", e.Code, e.Message)
}

// SessionSpec says which session RunConversation talks to and how.
type SessionSpec struct {
	// SessionID is the RESERVED (or ACTIVE, which is resumed) session; required.
	SessionID uuid.UUID
	// WSURL is the realtime endpoint, e.g. ws://localhost:8081/api/v1/realtime; Origin the
	// Origin header of the handshake ("" = none).
	WSURL  string
	Origin string
	// Client is sent in session.start (optional).
	Client *protocol.ClientInfo

	// Clock stamps sends and receives, and times the script (default: the Client's clock).
	Clock clock.Clock
	// Sleep blocks for d or until ctx is done; the Pacer and Drop wait through it (default:
	// SleepContext). Tests pass one that advances a fake Clock.
	Sleep func(ctx context.Context, d time.Duration) error
	// IDs generates event ids (default: the Client's generator).
	IDs idgen.Generator
	// HandshakeTimeout: see DefaultHandshakeTimeout (zero = default).
	HandshakeTimeout time.Duration
}

var upstreamAudio = protocol.AudioParams{Codec: "opus", SampleRate: 48000, Channels: 1, FrameMS: 20}

// RunConversation plays steps against the realtime server for the session of s and returns what
// it measured (§14.11.1). It dials with a fresh ticket, sends session.start, and from
// session.started on sends an upstream frame every 20 ms (see Step): the server decides the
// turns (D-076), the script only supplies speech.
//
// The Report is returned even with an error, holding whatever was measured until then. The
// script ends early and cleanly (nil error) when the session completes or fails on its own, for
// example at its expiry while a long Think runs; Report.Status says which. A fatal server error
// event is returned as *ServerError; a lost connection that is not a Drop is an error.
//
// When the AI finishes speaking (ai.audio_completed) the client reports client.playback_finished
// at once, as a player that has just rendered the last frame would (§7.4.4), so the turn closes
// without waiting for the server's playback deadline.
func RunConversation(ctx context.Context, c *Client, s SessionSpec, steps []Step, sink Sink) (Report, error) {
	if c == nil {
		return Report{}, errors.New("kotokiclient: RunConversation needs a Client")
	}
	if s.SessionID == uuid.Nil {
		return Report{}, errors.New("kotokiclient: SessionSpec.SessionID is required")
	}
	if sink == nil {
		sink = NopSink{}
	}
	// Turn sequence numbers start at 0 (the AI_FIRST opening), so "no turn seen yet" is -1.
	r := &runner{c: c, spec: s, sink: sink, clk: s.Clock, sleep: s.Sleep, ids: s.IDs, sig: make(chan struct{}, 1), maxTurnSeq: -1, doneTurnSeq: -1, utt: utterance{floorSeq: -1}}
	if r.clk == nil {
		r.clk = c.clk
	}
	if r.sleep == nil {
		r.sleep = SleepContext
	}
	if r.ids == nil {
		r.ids = c.ids
	}
	r.hsTimeout = s.HandshakeTimeout
	if r.hsTimeout <= 0 {
		r.hsTimeout = DefaultHandshakeTimeout
	}
	r.silence = audio.SilencePacket()
	r.rep = Report{
		TurnLatencyMS: []int64{}, BargeInStopMS: []int64{}, ResumeMS: []int64{}, ServerLatencies: []ServerLatency{},
		Status: "ACTIVE",
	}

	err := r.run(ctx, steps)
	if errors.Is(err, errSessionEnded) {
		err = nil
	}
	r.shutdown(err == nil)
	return r.report(), err
}

// utterance tracks the answer to the last thing the client said.
type utterance struct {
	floorSeq  int       // highest turn seq seen when it began: a turn above it answers it
	armed     bool      // it ended: t0 and base are valid
	t0        time.Time // when its last speech frame was sent
	base      uint32    // highest generation seen then: audio above it is the answer
	audioSeen bool
	audioAt   time.Time // first frame of the answer
}

// bargeIn tracks one interruption until the server confirms it.
type bargeIn struct {
	armed     bool
	t0        time.Time // first frame of the interrupting speech sent
	oldGen    uint32    // generation being interrupted
	lastOldAt time.Time // last frame of oldGen received
}

type runner struct {
	c         *Client
	spec      SessionSpec
	sink      Sink
	clk       clock.Clock
	sleep     func(ctx context.Context, d time.Duration) error
	ids       idgen.Generator
	hsTimeout time.Duration
	silence   []byte

	// Owned by the sending goroutine (RunConversation's caller).
	conn         *RealtimeConn
	consumerDone chan struct{}
	pacer        *Pacer

	sig chan struct{} // poked on every state change; the sender waits on it

	mu           sync.Mutex // guards everything below
	rep          Report
	lastSeq      int64
	maxGen       uint32
	maxTurnSeq   int
	doneTurnSeq  int
	lastAudioGen uint32
	lastAudioAt  time.Time
	utt          utterance
	bi           bargeIn
	startSentAt  time.Time
	startRecord  bool // StartMS already taken
	resumeT0     time.Time
	ready        bool // per connection
	attached     bool // session.started or snapshot seen on this connection
	connLost     bool // the reader of this connection ended
	terminal     bool
	fatal        *ServerError
}

func (r *runner) wake() {
	select {
	case r.sig <- struct{}{}:
	default:
	}
}

func (r *runner) report() Report {
	r.mu.Lock()
	defer r.mu.Unlock()
	rep := r.rep
	rep.TurnLatencyMS = append([]int64{}, rep.TurnLatencyMS...)
	rep.BargeInStopMS = append([]int64{}, rep.BargeInStopMS...)
	rep.ResumeMS = append([]int64{}, rep.ResumeMS...)
	rep.ServerLatencies = append([]ServerLatency{}, rep.ServerLatencies...)
	return rep
}

func (r *runner) with(f func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	f()
}

func (r *runner) test(f func() bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return f()
}

// --- receiving ---------------------------------------------------------------------------

// consume handles everything one connection delivers until both channels close.
func (r *runner) consume(ctx context.Context, rc *RealtimeConn) {
	events, frames := rc.Events(), rc.Audio()
	for events != nil || frames != nil {
		select {
		case e, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			// The reader queues a frame before any later event, so every frame the server wrote
			// ahead of this event is waiting in the buffer: handle those first and the Report
			// sees the server's order (a barge-in is confirmed only after the last old frame).
			frames = r.drainFrames(frames)
			r.onEvent(ctx, rc, e)
		case f, ok := <-frames:
			if !ok {
				frames = nil
				continue
			}
			r.onAudio(f)
		}
	}
	r.with(func() { r.connLost = true })
	r.wake()
}

// drainFrames handles the queued frames without blocking; it returns nil once the channel is
// closed and empty.
func (r *runner) drainFrames(frames <-chan AudioFrame) <-chan AudioFrame {
	for frames != nil {
		select {
		case f, ok := <-frames:
			if !ok {
				return nil
			}
			r.onAudio(f)
		default:
			return frames
		}
	}
	return nil
}

func (r *runner) onAudio(f AudioFrame) {
	r.mu.Lock()
	r.lastAudioGen, r.lastAudioAt = f.Generation, f.At
	if f.Generation > r.maxGen {
		r.maxGen = f.Generation
	}
	if r.bi.armed && f.Generation == r.bi.oldGen {
		r.bi.lastOldAt = f.At
	}
	// turn_latency_ms: last speech frame sent → first frame of a newer generation.
	if u := &r.utt; u.armed && !u.audioSeen && f.Generation > u.base {
		u.audioSeen, u.audioAt = true, f.At
		r.rep.TurnLatencyMS = append(r.rep.TurnLatencyMS, f.At.Sub(u.t0).Milliseconds())
	}
	r.mu.Unlock()
	r.sink.OnAudio(f)
	r.wake()
}

func (r *runner) onEvent(ctx context.Context, rc *RealtimeConn, e Received[protocol.ServerEvent]) {
	ev := e.Value
	var playback *protocol.ClientPlaybackFinished

	r.mu.Lock()
	// A replayed event the client already processed (seq ≤ last_server_seq) is a duplicate: it
	// is shown to the sink but changes nothing. One it missed counts like a live one.
	fresh := !ev.Replay || ev.ServerSeq > r.lastSeq
	if ev.ServerSeq > r.lastSeq {
		r.lastSeq = ev.ServerSeq
	}
	if fresh {
		playback = r.apply(e)
	}
	r.mu.Unlock()

	r.sink.OnEvent(e)
	if playback != nil {
		// Best effort: if the connection is going down the turn closes by the server's deadline.
		_ = rc.Send(ctx, protocol.ClientEvent{Type: "client.playback_finished", SessionID: r.spec.SessionID, Payload: *playback})
	}
	r.wake()
}

// apply folds one fresh event into the state and the Report; r.mu is held. It returns the
// playback report to send, if the event asks for one.
func (r *runner) apply(e Received[protocol.ServerEvent]) *protocol.ClientPlaybackFinished {
	ev := e.Value
	if ev.TurnSeq != nil && *ev.TurnSeq > r.maxTurnSeq {
		r.maxTurnSeq = *ev.TurnSeq
	}
	if ev.Generation != nil && *ev.Generation > r.maxGen {
		r.maxGen = *ev.Generation
	}
	switch p := ev.Payload.(type) {
	case protocol.SessionReady:
		r.ready = true
	case protocol.SessionStarted:
		r.attached = true
		if !r.startRecord && !r.startSentAt.IsZero() {
			r.startRecord = true
			r.rep.StartMS = e.At.Sub(r.startSentAt).Milliseconds()
		}
	case protocol.SessionSnapshot:
		// §7.4.3: the baseline replaces last_server_seq, even when it is lower.
		r.lastSeq = p.ServerSeqBaseline
		if p.ActiveGeneration > r.maxGen {
			r.maxGen = p.ActiveGeneration
		}
		for _, t := range p.RecentTurns {
			r.maxTurnSeq = max(r.maxTurnSeq, t.TurnSeq)
			r.doneTurnSeq = max(r.doneTurnSeq, t.TurnSeq)
		}
		r.attached = true
		if !r.resumeT0.IsZero() {
			r.rep.ResumeMS = append(r.rep.ResumeMS, e.At.Sub(r.resumeT0).Milliseconds())
			r.resumeT0 = time.Time{}
		}
	case protocol.AIInterrupted:
		if p.ActiveGeneration > r.maxGen {
			r.maxGen = p.ActiveGeneration
		}
		// barge_in_stop_ms: first interrupting frame sent → last frame of the old generation.
		if b := &r.bi; b.armed && !ev.Replay && p.ActiveGeneration > b.oldGen {
			b.armed = false
			r.rep.BargeInStopMS = append(r.rep.BargeInStopMS, max(b.lastOldAt.Sub(b.t0), 0).Milliseconds())
		}
	case protocol.AIAudioCompleted:
		if !ev.Replay {
			gen := r.maxGen
			if ev.Generation != nil {
				gen = *ev.Generation
			}
			return &protocol.ClientPlaybackFinished{Generation: gen, PlayedMS: p.TotalAudioMS}
		}
	case protocol.TurnCompleted:
		seq := r.maxTurnSeq
		if ev.TurnSeq != nil {
			seq = *ev.TurnSeq
		}
		r.doneTurnSeq = max(r.doneTurnSeq, seq)
		if p.Latency != nil {
			r.rep.ServerLatencies = append(r.rep.ServerLatencies, ServerLatency{TurnSeq: seq, Latency: *p.Latency})
		}
	case protocol.SessionCompleted:
		r.terminal = true
		r.rep.Status, r.rep.ChargedSeconds = "COMPLETED", p.ChargedSeconds
	case protocol.SessionFailed:
		r.terminal = true
		r.rep.Status, r.rep.ChargedSeconds = "FAILED", p.ChargedSeconds
	case protocol.ErrorPayload:
		if p.Fatal {
			r.fatal = &ServerError{Code: p.Error.Code, Message: p.Error.Message, Retryable: p.Retryable}
		}
	}
	return nil
}

// --- connecting --------------------------------------------------------------------------

// waitFor blocks until pred (called with r.mu held) is true, the context ends or limit passes.
func (r *runner) waitFor(ctx context.Context, limit time.Duration, what string, pred func() bool) error {
	ctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	for {
		if r.test(pred) {
			return nil
		}
		select {
		case <-r.sig:
		case <-ctx.Done():
			return fmt.Errorf("kotokiclient: waiting for %s: %w", what, ctx.Err())
		}
	}
}

// connect opens a connection: ticket, dial, session.ready, then session.start (first) or
// session.resume (after a Drop), and waits until the server has attached the session.
func (r *runner) connect(ctx context.Context, resume bool) error {
	r.with(func() { r.ready, r.attached, r.connLost = false, false, false })
	ticket, err := r.c.RealtimeTicket(ctx, r.spec.SessionID)
	if err != nil {
		return fmt.Errorf("kotokiclient: realtime ticket: %w", err)
	}
	t0 := r.clk.Now()
	rc, err := Dial(ctx, r.spec.WSURL, ticket, r.spec.Origin, WithDialClock(r.clk), WithDialIDs(r.ids))
	if err != nil {
		return err
	}
	connectMS := r.clk.Now().Sub(t0).Milliseconds()
	r.conn = rc
	r.consumerDone = make(chan struct{})
	go func() {
		defer close(r.consumerDone)
		r.consume(ctx, rc)
	}()
	if !resume {
		r.with(func() { r.rep.ConnectMS = connectMS })
	}

	stopped := func() bool { return r.connLost || r.terminal || r.fatal != nil }
	if err := r.waitFor(ctx, r.hsTimeout, "session.ready", func() bool { return r.ready || stopped() }); err != nil {
		return err
	}
	if !r.test(func() bool { return r.ready }) {
		return r.lostErr(rc, "session.ready")
	}

	var cmd protocol.ClientEvent
	if resume {
		cmd = protocol.ClientEvent{Type: "session.resume", SessionID: r.spec.SessionID,
			Payload: protocol.SessionResume{LastServerSeq: r.lastServerSeq(), Audio: upstreamAudio}}
	} else {
		cmd = protocol.ClientEvent{Type: "session.start", SessionID: r.spec.SessionID,
			Payload: protocol.SessionStart{Client: r.spec.Client, Audio: upstreamAudio}}
		r.with(func() { r.startSentAt = r.clk.Now() })
	}
	if err := rc.Send(ctx, cmd); err != nil {
		return err
	}
	what := "session.started"
	if resume {
		what = "session.snapshot"
	}
	if err := r.waitFor(ctx, r.hsTimeout, what, func() bool { return r.attached || stopped() }); err != nil {
		return err
	}
	if !r.test(func() bool { return r.attached }) {
		return r.lostErr(rc, what)
	}
	r.pacer = NewPacer(r.clk.Now(), r.clk.Now, r.sleep)
	return nil
}

func (r *runner) lastServerSeq() int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastSeq
}

// lostErr explains why the connection ended where something else was expected.
func (r *runner) lostErr(rc *RealtimeConn, waiting string) error {
	r.mu.Lock()
	fatal, terminal := r.fatal, r.terminal
	r.mu.Unlock()
	switch {
	case fatal != nil:
		return fatal
	case terminal:
		return errSessionEnded
	}
	if err := rc.Err(); err != nil {
		return fmt.Errorf("kotokiclient: connection closed before %s: %w", waiting, err)
	}
	return fmt.Errorf("kotokiclient: connection closed before %s", waiting)
}

// shutdown closes the connection (gracefully after a clean run, abruptly otherwise) and waits
// for the receiving goroutine, so nothing touches the sink or the Report afterwards.
func (r *runner) shutdown(graceful bool) {
	if r.conn == nil {
		return
	}
	if graceful {
		_ = r.conn.Close(websocket.StatusNormalClosure, "script finished")
	} else {
		_ = r.conn.Abort()
	}
	<-r.consumerDone
}

// --- the script --------------------------------------------------------------------------

func (r *runner) run(ctx context.Context, steps []Step) error {
	if err := r.connect(ctx, false); err != nil {
		return err
	}
	for i, st := range steps {
		if err := r.stopped(); err != nil {
			return err
		}
		var err error
		switch st := st.(type) {
		case Speak:
			err = r.speak(ctx, st.Packets, nil)
		case Think:
			_, err = r.silent(ctx, r.clk.Now().Add(st.D), nil)
		case WaitAIDone:
			err = r.waitAIDone(ctx, st.Timeout)
		case BargeIn:
			err = r.bargeIn(ctx, st)
		case Drop:
			err = r.drop(ctx, st.For)
		case End:
			err = r.end(ctx, st.Timeout)
		default:
			err = fmt.Errorf("unknown step %T", st)
		}
		if err != nil {
			return fmt.Errorf("kotokiclient: step %d (%s): %w", i+1, stepName(steps[i]), err)
		}
	}
	return r.stopped()
}

// send writes one upstream frame; pace waits for the next 20 ms slot.
func (r *runner) send(ctx context.Context, pkt []byte) error {
	if err := r.conn.SendAudio(ctx, pkt); err != nil {
		if errors.Is(err, ErrBadPacket) || ctx.Err() != nil {
			return err
		}
		// The connection is gone. Let the receiving side finish first: the server may have
		// completed the session just before closing, and that is not a failure.
		_ = r.conn.Abort()
		<-r.consumerDone
		return r.lostErr(r.conn, "the next frame")
	}
	return nil
}

func (r *runner) pace(ctx context.Context) error {
	if err := r.pacer.Wait(ctx); err != nil {
		return err
	}
	return r.stopped()
}

// stopped reports what ended the session behind the script's back: a fatal server error, or the
// session completing or failing on its own (errSessionEnded). nil while it goes on.
func (r *runner) stopped() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.fatal != nil:
		return r.fatal
	case r.terminal:
		return errSessionEnded
	}
	return nil
}

func (r *runner) frame(ctx context.Context, pkt []byte) error {
	if err := r.send(ctx, pkt); err != nil {
		return err
	}
	return r.pace(ctx)
}

// silent sends silence until cond holds (true), deadline passes (false) or the session ends
// (errSessionEnded). cond may be nil.
func (r *runner) silent(ctx context.Context, deadline time.Time, cond func() bool) (bool, error) {
	for {
		if cond != nil && r.test(cond) {
			return true, nil
		}
		if err := r.stopped(); err != nil {
			return false, err
		}
		if !r.clk.Now().Before(deadline) {
			return false, nil
		}
		if err := r.frame(ctx, r.silence); err != nil {
			return false, err
		}
	}
}

// speak sends packets as one utterance. beforeFirst, when set, runs just before the first frame.
func (r *runner) speak(ctx context.Context, packets [][]byte, beforeFirst func()) error {
	if len(packets) == 0 {
		return nil
	}
	r.with(func() { r.utt = utterance{floorSeq: r.maxTurnSeq} })
	for i, p := range packets {
		if i == 0 && beforeFirst != nil {
			beforeFirst()
		}
		if i == len(packets)-1 {
			// Armed before the last frame goes out, so nothing the server answers can precede it.
			now := r.clk.Now()
			r.with(func() { r.utt.armed, r.utt.t0, r.utt.base = true, now, r.maxGen })
		}
		if err := r.frame(ctx, p); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) aiDone() bool { return r.doneTurnSeq > r.utt.floorSeq }

func (r *runner) waitAIDone(ctx context.Context, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = DefaultAITimeout
	}
	ok, err := r.silent(ctx, r.clk.Now().Add(timeout), r.aiDone)
	if err != nil {
		return err
	}
	if !ok {
		return ErrAITimeout
	}
	return nil
}

func (r *runner) bargeIn(ctx context.Context, st BargeIn) error {
	if len(st.Packets) == 0 {
		return errors.New("BargeIn needs Packets")
	}
	timeout := st.Timeout
	if timeout <= 0 {
		timeout = DefaultAITimeout
	}
	// Wait for the AI's first audio frame of the answer to the last utterance.
	ok, err := r.silent(ctx, r.clk.Now().Add(timeout), func() bool { return r.utt.audioSeen || r.aiDone() })
	if err != nil {
		return err
	}
	if !ok {
		return ErrAITimeout
	}
	var at time.Time
	if !r.test(func() bool { at = r.utt.audioAt; return r.utt.audioSeen }) {
		return ErrNoAIAudio
	}
	// Then After more, counted from that frame.
	if _, err := r.silent(ctx, at.Add(st.After), nil); err != nil {
		return err
	}
	return r.speak(ctx, st.Packets, func() {
		now := r.clk.Now()
		r.with(func() { r.bi = bargeIn{armed: true, t0: now, oldGen: r.lastAudioGen, lastOldAt: r.lastAudioAt} })
	})
}

func (r *runner) drop(ctx context.Context, away time.Duration) error {
	_ = r.conn.Abort()
	<-r.consumerDone // everything already received is accounted for before last_server_seq is read
	if r.test(func() bool { return r.terminal }) {
		return errSessionEnded
	}
	if away > 0 {
		if err := r.sleep(ctx, away); err != nil {
			return err
		}
	}
	now := r.clk.Now()
	r.with(func() { r.resumeT0 = now })
	return r.connect(ctx, true)
}

func (r *runner) end(ctx context.Context, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = DefaultEndTimeout
	}
	err := r.conn.Send(ctx, protocol.ClientEvent{Type: "session.end", SessionID: r.spec.SessionID, Payload: protocol.SessionEnd{Reason: "USER_ENDED"}})
	if err != nil {
		return err
	}
	// Keep the mic open until the server has completed the session (errSessionEnded).
	if _, err := r.silent(ctx, r.clk.Now().Add(timeout), nil); err != nil {
		return err
	}
	return ErrEndTimeout
}
