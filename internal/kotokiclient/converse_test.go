package kotokiclient

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/kotokiclient/audio"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
)

// The conversation tests run a client against an in-process fake server on a fake clock, in
// lock step: after every upstream frame the server answers with whatever the script says and then
// with a `pong` (seq 0) marker. The Pacer's sleep waits for that marker before it advances the
// clock by the 20 ms slot, so everything the server wrote in reaction to frame i has been read and
// stamped (the reader stamps in stream order) before the clock moves. Receive times, and with them
// every latency in the Report, are therefore exact and there is no real sleeping.

const (
	tHandshakeAdvance = 7 * time.Millisecond // the first upgrade takes this long: connect_ms
	speechA           = 0xAA
	speechB           = 0xBB
)

type frameRec struct {
	seq     uint32
	payload []byte
	at      time.Time
}

type harness struct {
	t    *testing.T
	clk  *clock.Fake
	srv  *httptest.Server
	acks chan struct{}

	over     chan struct{} // closed when the session ended or failed fatally: no more markers
	overOnce sync.Once

	script func(s *sconn)
	wg     sync.WaitGroup
	conns  atomic.Int32

	mu      sync.Mutex
	events  []Received[protocol.ServerEvent]
	audioIn []AudioFrame
	frames  map[int][]frameRec
	texts   map[int][]protocol.ClientEvent
	tickets map[int]string
	issued  int
	closeAt map[int]error // read error that ended each connection's server loop

	clientIP string         // when set, the client is built WithClientIP
	xff      map[int]string // X-Forwarded-For of each handshake
}

func newHarness(t *testing.T, script func(s *sconn)) *harness {
	t.Helper()
	h := &harness{
		t: t, clk: clock.NewFake(time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)),
		acks: make(chan struct{}, 8192), over: make(chan struct{}), script: script,
		frames: map[int][]frameRec{}, texts: map[int][]protocol.ClientEvent{}, tickets: map[int]string{}, closeAt: map[int]error{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/sessions/{id}/realtime-ticket", func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, testSession.String(), r.PathValue("id"))
		h.mu.Lock()
		h.issued++
		n := h.issued
		h.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ticket": "tkt-" + string(rune('0'+n)), "expires_in": 30})
	})
	mux.HandleFunc("GET /api/v1/realtime", h.serveWS)
	h.srv = httptest.NewServer(mux)
	t.Cleanup(func() {
		h.srv.Close()
		h.wg.Wait()
	})
	return h
}

func (h *harness) serveWS(w http.ResponseWriter, r *http.Request) {
	h.wg.Add(1)
	defer h.wg.Done()
	idx := int(h.conns.Add(1) - 1)
	if idx == 0 {
		h.clk.Advance(tHandshakeAdvance)
	}
	h.mu.Lock()
	h.tickets[idx] = r.URL.Query().Get("ticket")
	if h.xff == nil {
		h.xff = map[int]string{}
	}
	h.xff[idx] = r.Header.Get("X-Forwarded-For")
	h.mu.Unlock()
	assert.Equal(h.t, "http://localhost:3000", r.Header.Get("Origin"))
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{Subprotocol}, InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	h.script(&sconn{h: h, ctx: r.Context(), c: c, idx: idx})
}

// OnEvent and OnAudio make the harness the Sink of the run.
func (h *harness) OnEvent(e Received[protocol.ServerEvent]) {
	h.mu.Lock()
	h.events = append(h.events, e)
	h.mu.Unlock()
	switch p := e.Value.Payload.(type) {
	case protocol.Pong:
		h.acks <- struct{}{}
	case protocol.SessionCompleted, protocol.SessionFailed:
		h.overOnce.Do(func() { close(h.over) })
	case protocol.ErrorPayload:
		if p.Fatal {
			h.overOnce.Do(func() { close(h.over) })
		}
	}
}

func (h *harness) OnAudio(f AudioFrame) {
	h.mu.Lock()
	h.audioIn = append(h.audioIn, f)
	h.mu.Unlock()
}

// sleep is the Pacer's (and Drop's) sleep: a frame slot waits for the server's marker, then the
// fake clock moves.
func (h *harness) sleep(ctx context.Context, d time.Duration) error {
	if d == FrameInterval {
		select {
		case <-h.acks:
		case <-h.over:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	h.clk.Advance(d)
	return nil
}

func (h *harness) run(steps []Step) (Report, error) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var opts []Option
	if h.clientIP != "" {
		opts = append(opts, WithClientIP(h.clientIP))
	}
	c := New(h.srv.URL, opts...)
	rep, err := RunConversation(ctx, c, SessionSpec{
		SessionID: testSession, WSURL: "ws" + strings.TrimPrefix(h.srv.URL, "http") + "/api/v1/realtime",
		Origin: "http://localhost:3000", Clock: h.clk, Sleep: h.sleep, IDs: idgen.NewSequence(h.t),
	}, steps, h)
	h.wg.Wait() // the server side is finished: its records are complete
	return rep, err
}

func (h *harness) framesOf(idx int) []frameRec {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]frameRec(nil), h.frames[idx]...)
}

func (h *harness) textsOf(idx int) []protocol.ClientEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]protocol.ClientEvent(nil), h.texts[idx]...)
}

func (h *harness) eventsSnapshot() []Received[protocol.ServerEvent] {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]Received[protocol.ServerEvent](nil), h.events...)
}

// sconn is the server side of one connection.
type sconn struct {
	h   *harness
	ctx context.Context
	c   *websocket.Conn
	idx int
	seq int64
}

type evo struct {
	seq      int64 // 0: the next sequence number (unless fixed)
	fixed    bool  // use seq as given, even 0, and leave the counter alone
	turn     int
	gen      *uint32
	replay   bool
	zeroTurn bool // the event carries turn_seq 0 (turn 0 is the AI_FIRST opening)
}

func (s *sconn) send(typ string, o evo, payload any) {
	switch {
	case o.fixed:
	case o.seq == 0:
		s.seq++
		o.seq = s.seq
	default:
		s.seq = o.seq
	}
	ev := protocol.ServerEvent{
		Type: typ, EventID: uuid.New(), ServerSeq: o.seq, SessionID: testSession, Generation: o.gen,
		TS: time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC), Replay: o.replay, Payload: payload,
	}
	if o.turn > 0 || o.zeroTurn {
		ev.TurnSeq = ptr(o.turn)
	}
	b, err := protocol.MarshalServer(ev)
	if err != nil {
		panic(err)
	}
	_ = s.c.Write(s.ctx, websocket.MessageText, b) // a closed connection is the script's business
}

func (s *sconn) audio(gen, seq uint32, payload byte) {
	_ = s.c.Write(s.ctx, websocket.MessageBinary, protocol.EncodeDownstream(gen, seq, []byte{payload}))
}

func (s *sconn) ready(expected string) {
	// Not part of the numbered stream the scripts count.
	s.send("session.ready", evo{fixed: true}, protocol.SessionReady{Status: "RESERVED", ExpectedCommand: expected, ProtocolVersion: 1})
}

// command reads the first text message: the client's session.start / session.resume.
func (s *sconn) command() protocol.ClientEvent {
	for {
		typ, b, err := s.c.Read(s.ctx)
		if err != nil {
			s.h.t.Errorf("conn %d: waiting for the first command: %v", s.idx, err)
			return protocol.ClientEvent{}
		}
		if typ != websocket.MessageText {
			continue
		}
		ev, err := protocol.DecodeClientEvent(b)
		assert.NoError(s.h.t, err)
		assert.Equal(s.h.t, testSession, ev.SessionID)
		return ev
	}
}

// serve reads until the connection ends: every frame is recorded, handed to onFrame (with its
// 0-based index) and answered with a marker; every client event is recorded and handed to onText.
func (s *sconn) serve(onFrame func(i int, f frameRec), onText func(ev protocol.ClientEvent)) error {
	for {
		typ, b, err := s.c.Read(s.ctx)
		if err != nil {
			s.h.mu.Lock()
			s.h.closeAt[s.idx] = err
			s.h.mu.Unlock()
			return err
		}
		if typ == websocket.MessageText {
			ev, derr := protocol.DecodeClientEvent(b)
			assert.NoError(s.h.t, derr)
			s.h.mu.Lock()
			s.h.texts[s.idx] = append(s.h.texts[s.idx], ev)
			s.h.mu.Unlock()
			if onText != nil {
				onText(ev)
			}
			continue
		}
		seq, opus, derr := protocol.DecodeUpstream(b)
		assert.NoError(s.h.t, derr)
		rec := frameRec{seq: seq, payload: append([]byte(nil), opus...), at: s.h.clk.Now()}
		s.h.mu.Lock()
		s.h.frames[s.idx] = append(s.h.frames[s.idx], rec)
		i := len(s.h.frames[s.idx]) - 1
		s.h.mu.Unlock()
		if onFrame != nil {
			onFrame(i, rec)
		}
		// The marker: seq 0 keeps it out of the numbered stream.
		mb, _ := protocol.MarshalServer(protocol.ServerEvent{
			Type: "pong", EventID: uuid.New(), SessionID: testSession, TS: time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC),
			Payload: protocol.Pong{ClientTS: int64(i)},
		})
		_ = s.c.Write(s.ctx, websocket.MessageText, mb)
	}
}

func packets(b byte, n int) [][]byte {
	out := make([][]byte, n)
	for i := range out {
		out[i] = []byte{b, byte(i)}
	}
	return out
}

var silencePkt = audio.SilencePacket()

func isSilence(p []byte) bool { return bytes.Equal(p, silencePkt) }

// turnDetector watches the frames the way the server's detector would, in frame counts: a turn
// ends after `quiet` silence frames that follow speech.
type turnDetector struct {
	quiet, silent int
	speaking      bool
}

// feed returns true when the frame completes the turn.
func (d *turnDetector) feed(p []byte) bool {
	if !isSilence(p) {
		d.speaking, d.silent = true, 0
		return false
	}
	if !d.speaking {
		return false
	}
	d.silent++
	if d.silent == d.quiet {
		d.speaking, d.silent = false, 0
		return true
	}
	return false
}

var lat = protocol.Latency{TurnEndToFirstAudioMS: 800, STTFinalMS: 120, LLMFirstTokenMS: 400, TTSFirstAudioMS: 250}

// answer plays the server's reaction to a finished utterance: the whole turn in one go, with one
// audio frame of the new generation.
func (s *sconn) answer(turn int, gen uint32, latency protocol.Latency) {
	s.send("turn.started", evo{turn: turn}, protocol.TurnStarted{Initiator: "USER"})
	s.send("stt.final", evo{turn: turn}, protocol.STTFinal{Text: "こんにちは", EndReason: "SILENCE"})
	s.send("ai.generation_started", evo{turn: turn, gen: ptr(gen)}, protocol.AIGenerationStarted{Kind: "REPLY"})
	s.audio(gen, 0, 0xC0+byte(gen))
	s.send("ai.audio_completed", evo{turn: turn, gen: ptr(gen)}, protocol.AIAudioCompleted{FrameCount: 1, TotalAudioMS: 20, SentenceCount: 1, LLMFinished: true})
	s.send("turn.completed", evo{turn: turn}, protocol.TurnCompleted{Status: "COMPLETED", Latency: &latency})
}

func TestRunConversationHappy(t *testing.T) {
	h := newHarness(t, func(s *sconn) {
		s.ready("session.start")
		cmd := s.command()
		assert.Equal(t, "session.start", cmd.Type)
		start, ok := cmd.Payload.(protocol.SessionStart)
		assert.True(t, ok)
		assert.True(t, start.Audio.Valid(), "audio parameters are the only valid ones")
		s.h.clk.Advance(30 * time.Millisecond) // the server takes 30 ms to start the session
		s.send("session.started", evo{}, protocol.SessionStarted{Status: "ACTIVE", TurnState: "LISTENING"})
		det := turnDetector{quiet: 5}
		_ = s.serve(func(_ int, f frameRec) {
			if det.feed(f.payload) {
				s.answer(1, 1, lat)
			}
		}, func(ev protocol.ClientEvent) {
			if ev.Type == "session.end" {
				assert.Equal(t, protocol.SessionEnd{Reason: "USER_ENDED"}, ev.Payload)
				s.send("session.completed", evo{}, protocol.SessionCompleted{EndReason: "USER_ENDED", ChargedSeconds: 42})
			}
		})
	})
	rep, err := h.run([]Step{Speak{Packets: packets(speechA, 3), Label: "u1"}, WaitAIDone{Timeout: 5 * time.Second}, End{}})
	require.NoError(t, err)

	require.EqualValues(t, tHandshakeAdvance.Milliseconds(), rep.ConnectMS, "dial → handshake")
	require.EqualValues(t, 30, rep.StartMS, "session.start sent → session.started received")
	// The last speech frame is frame 2; the turn closes on the 5th silence frame, frame 7:
	// 5 slots of 20 ms later (not 7 from the first speech frame, not 1 from the last silence).
	require.Equal(t, []int64{100}, rep.TurnLatencyMS)
	require.Equal(t, []ServerLatency{{TurnSeq: 1, Latency: lat}}, rep.ServerLatencies)
	require.Equal(t, "COMPLETED", rep.Status)
	require.Equal(t, 42, rep.ChargedSeconds)
	require.Empty(t, rep.BargeInStopMS)
	require.Empty(t, rep.ResumeMS)

	// Speech goes out verbatim, in order; every other slot carries the silence packet.
	fr := h.framesOf(0)
	require.Greater(t, len(fr), 8)
	for i, f := range fr {
		require.Equal(t, uint32(i), f.seq)
		if i < 3 {
			require.Equal(t, []byte{speechA, byte(i)}, f.payload, "frame %d", i)
		} else {
			require.True(t, isSilence(f.payload), "frame %d is silence", i)
		}
	}

	// The AI finished speaking: the client reports the playback, as a player would.
	var pf []protocol.ClientPlaybackFinished
	for _, ev := range h.textsOf(0) {
		if p, ok := ev.Payload.(protocol.ClientPlaybackFinished); ok {
			pf = append(pf, p)
		}
	}
	require.Equal(t, []protocol.ClientPlaybackFinished{{Generation: 1, PlayedMS: 20}}, pf)

	// The Sink saw the events in the server's order and the frame of generation 1.
	var types []string
	for _, e := range h.eventsSnapshot() {
		if e.Value.Type != "pong" {
			types = append(types, e.Value.Type)
		}
	}
	require.Equal(t, []string{"session.ready", "session.started", "turn.started", "stt.final", "ai.generation_started", "ai.audio_completed", "turn.completed", "session.completed"}, types)
	require.Len(t, h.audioIn, 1)
	require.EqualValues(t, 1, h.audioIn[0].Generation)
}

func TestBargeInStepMeasured(t *testing.T) {
	const after = 100 * time.Millisecond
	var firstAudio, bargeAt time.Time
	var turn2Sent atomic.Bool
	h := newHarness(t, func(s *sconn) {
		s.ready("session.start")
		_ = s.command()
		s.send("session.started", evo{}, protocol.SessionStarted{Status: "ACTIVE"})
		det := turnDetector{quiet: 5}
		answered, interrupted, streaming := 0, false, false
		var gen1Seq uint32
		_ = s.serve(func(_ int, f frameRec) {
			if det.feed(f.payload) {
				answered++
				if answered == 1 { // the AI starts to speak and keeps streaming, one frame per client frame
					s.send("turn.started", evo{turn: 1}, protocol.TurnStarted{Initiator: "USER"})
					s.send("ai.generation_started", evo{turn: 1, gen: ptr(uint32(1))}, protocol.AIGenerationStarted{Kind: "REPLY"})
					firstAudio, streaming = f.at, true
				} else {
					s.answer(2, 3, lat)
					turn2Sent.Store(true)
					return
				}
			}
			if f.payload[0] == speechB && bargeAt.IsZero() {
				bargeAt = f.at
			}
			if streaming && !interrupted {
				s.audio(1, gen1Seq, 0x10)
				gen1Seq++
				// Two client frames after the first interrupting one, the server stops the AI.
				if !bargeAt.IsZero() && f.at.Sub(bargeAt) == 2*FrameInterval {
					interrupted, streaming = true, false
					s.send("ai.interrupted", evo{turn: 1}, protocol.AIInterrupted{Reason: "BARGE_IN", ActiveGeneration: 2, SpokenSentenceCount: 0})
					s.send("turn.completed", evo{turn: 1}, protocol.TurnCompleted{Status: "INTERRUPTED"})
					// Two frames of the cut generation still in flight: the client discards and counts them.
					s.audio(1, gen1Seq, 0x10)
					s.audio(1, gen1Seq+1, 0x10)
				}
			}
		}, func(ev protocol.ClientEvent) {
			if ev.Type == "session.end" {
				assert.True(t, turn2Sent.Load(), "the script must wait for the answer to the interruption, not for the interrupted turn")
				s.send("session.completed", evo{}, protocol.SessionCompleted{EndReason: "USER_ENDED", ChargedSeconds: 9})
			}
		})
	})
	rep, err := h.run([]Step{
		Speak{Packets: packets(speechA, 3)},
		BargeIn{After: after, Packets: packets(speechB, 3)},
		WaitAIDone{},
		End{},
	})
	require.NoError(t, err)

	// The interrupting speech starts `after` the AI's first audio frame, not before.
	require.Equal(t, after, bargeAt.Sub(firstAudio))
	// First interrupting frame sent at T; the last generation-1 frame arrived with the third
	// interrupting frame, T+40 ms.
	require.Equal(t, []int64{40}, rep.BargeInStopMS)
	require.Equal(t, 2, rep.StaleFramesDropped, "frames of the old generation after ai.interrupted are counted, not delivered")
	// Both utterances were answered: the second one 5 silence slots after its last frame.
	require.Equal(t, []int64{100, 100}, rep.TurnLatencyMS)
	require.Equal(t, "COMPLETED", rep.Status)
	// No frame of the interrupted generation is delivered after the interruption.
	var gens []uint32
	for _, f := range h.audioIn {
		gens = append(gens, f.Generation)
	}
	// Generation 1 streamed on frames 7..14 (eight frames, the last one with the third
	// interrupting frame), then generation 3 answers the interruption.
	require.Equal(t, []uint32{1, 1, 1, 1, 1, 1, 1, 1, 3}, gens)
}

func TestBargeInWithoutAudio(t *testing.T) {
	h := newHarness(t, func(s *sconn) {
		s.ready("session.start")
		_ = s.command()
		s.send("session.started", evo{}, protocol.SessionStarted{Status: "ACTIVE"})
		det := turnDetector{quiet: 5}
		_ = s.serve(func(_ int, f frameRec) {
			if det.feed(f.payload) { // the turn ends without the AI saying anything
				s.send("turn.started", evo{turn: 1}, protocol.TurnStarted{Initiator: "USER"})
				s.send("turn.completed", evo{turn: 1}, protocol.TurnCompleted{Status: "DISCARDED"})
			}
		}, nil)
	})
	_, err := h.run([]Step{Speak{Packets: packets(speechA, 2)}, BargeIn{After: 0, Packets: packets(speechB, 2)}})
	require.ErrorIs(t, err, ErrNoAIAudio)
}

func TestTurnLatencyUsesTheNewGenerationOnly(t *testing.T) {
	// The AI is still speaking (generation 1) when the user stops; frames of generation 1 keep
	// coming for a while. The latency is up to the first frame of generation 2.
	h := newHarness(t, func(s *sconn) {
		s.ready("session.start")
		_ = s.command()
		s.send("session.started", evo{}, protocol.SessionStarted{Status: "ACTIVE"})
		s.send("turn.started", evo{turn: 1}, protocol.TurnStarted{Initiator: "AI"})
		s.send("ai.generation_started", evo{turn: 1, gen: ptr(uint32(1))}, protocol.AIGenerationStarted{Kind: "GREETING"})
		det := turnDetector{quiet: 5}
		var n uint32
		_ = s.serve(func(i int, f frameRec) {
			if i == 4 {
				s.send("turn.completed", evo{turn: 1}, protocol.TurnCompleted{Status: "COMPLETED"})
			}
			if det.feed(f.payload) {
				s.answer(2, 2, lat)
				return
			}
			if i < 12 {
				s.audio(1, n, 0x10) // old generation, still streaming after the user stopped
				n++
			}
		}, func(ev protocol.ClientEvent) {
			if ev.Type == "session.end" {
				s.send("session.completed", evo{}, protocol.SessionCompleted{EndReason: "USER_ENDED"})
			}
		})
	})
	// Frame 0 is idle (it lets the greeting's events land, so the utterance knows which turn was
	// already open); the user speaks over frames 1..3 while generation 1 plays; the turn closes
	// at frame 8, five silent slots after the last speech frame.
	rep, err := h.run([]Step{Think{D: FrameInterval}, Speak{Packets: packets(speechA, 3)}, WaitAIDone{}, End{}})
	require.NoError(t, err)
	require.Equal(t, []int64{100}, rep.TurnLatencyMS)
	// The greeting turn finished at frame 4 (turn 1): it did not answer the utterance (turn 2).
	require.Len(t, rep.ServerLatencies, 1)
	require.Equal(t, 2, rep.ServerLatencies[0].TurnSeq)
}

func TestDropAndResume(t *testing.T) {
	lat2 := protocol.Latency{TurnEndToFirstAudioMS: 1500}
	lat3 := protocol.Latency{TurnEndToFirstAudioMS: 700}
	var resumeSeq atomic.Int64
	resumeSeq.Store(-1)
	h := newHarness(t, func(s *sconn) {
		switch s.idx {
		case 0:
			s.ready("session.start")
			_ = s.command()
			s.send("session.started", evo{}, protocol.SessionStarted{Status: "ACTIVE"}) // seq 1
			det := turnDetector{quiet: 5}
			_ = s.serve(func(_ int, f frameRec) {
				if det.feed(f.payload) {
					s.answer(1, 1, lat) // seq 2..6
				}
			}, nil)
		case 1:
			s.ready("session.resume")
			cmd := s.command()
			assert.Equal(t, "session.resume", cmd.Type)
			rs, _ := cmd.Payload.(protocol.SessionResume)
			resumeSeq.Store(rs.LastServerSeq)
			assert.True(t, rs.Audio.Valid())
			s.h.clk.Advance(45 * time.Millisecond)
			// Two events the client already has (duplicates) and one it missed, then the snapshot.
			s.send("ai.audio_completed", evo{seq: 5, turn: 1, gen: ptr(uint32(1)), replay: true}, protocol.AIAudioCompleted{FrameCount: 1, TotalAudioMS: 20})
			s.send("turn.completed", evo{seq: 6, turn: 1, replay: true}, protocol.TurnCompleted{Status: "COMPLETED", Latency: &lat})
			s.send("turn.completed", evo{seq: 7, turn: 2, replay: true}, protocol.TurnCompleted{Status: "FAILED", Latency: &lat2})
			s.send("session.snapshot", evo{seq: 8, gen: ptr(uint32(3))}, protocol.SessionSnapshot{
				Replay: "COMPLETE", ReplayedCount: 3, ServerSeqBaseline: 8, ActiveGeneration: 3, NextTurnSeq: 3, TurnState: "LISTENING",
			})
			det := turnDetector{quiet: 5}
			_ = s.serve(func(_ int, f frameRec) {
				if det.feed(f.payload) {
					s.answer(3, 4, lat3)
				}
			}, func(ev protocol.ClientEvent) {
				if ev.Type == "session.end" {
					s.send("session.completed", evo{}, protocol.SessionCompleted{EndReason: "USER_ENDED", ChargedSeconds: 17})
				}
			})
		default:
			t.Errorf("unexpected connection %d", s.idx)
		}
	})
	rep, err := h.run([]Step{
		Speak{Packets: packets(speechA, 3)}, WaitAIDone{},
		Drop{For: 100 * time.Millisecond},
		Speak{Packets: packets(speechB, 3)}, WaitAIDone{},
		End{},
	})
	require.NoError(t, err)

	// A fresh ticket for the second connection, and resume with the largest server_seq seen.
	require.Equal(t, "tkt-1", h.tickets[0])
	require.Equal(t, "tkt-2", h.tickets[1])
	require.EqualValues(t, 6, resumeSeq.Load())
	// The first connection ended by the transport going away, not by a close handshake.
	require.Equal(t, websocket.StatusCode(-1), websocket.CloseStatus(h.closeAt[0]), "Drop is a TCP drop: %v", h.closeAt[0])
	// Upstream numbering restarts with the connection.
	fr := h.framesOf(1)
	require.NotEmpty(t, fr)
	require.Equal(t, uint32(0), fr[0].seq)

	// resume_ms: reconnect started (after the 100 ms away) → snapshot received: 45 ms, once.
	require.Equal(t, []int64{45}, rep.ResumeMS)
	// connect_ms is the first dial only.
	require.EqualValues(t, tHandshakeAdvance.Milliseconds(), rep.ConnectMS)
	// Replay duplicates do not multiply the Report; the event it missed counts once.
	require.Equal(t, []ServerLatency{{TurnSeq: 1, Latency: lat}, {TurnSeq: 2, Latency: lat2}, {TurnSeq: 3, Latency: lat3}}, rep.ServerLatencies)
	require.Equal(t, []int64{100, 100}, rep.TurnLatencyMS)
	require.Equal(t, "COMPLETED", rep.Status)
	require.Equal(t, 17, rep.ChargedSeconds)

	// The Sink still sees the replayed events, flagged.
	replayed := 0
	for _, e := range h.eventsSnapshot() {
		if e.Value.Replay {
			replayed++
		}
	}
	require.Equal(t, 3, replayed)
}

// A session.start sent to a session that is already ACTIVE is a resume at the server: it replays
// from seq 0, session.started included. The client must not take that replayed event for the end of
// the handshake: it would start the script (and read "the last turn" from a half-processed replay)
// before the snapshot (found by e2e E5).
func TestReplayedSessionStartedDoesNotAttach(t *testing.T) {
	var snapshotSent atomic.Bool
	var framesBeforeSnapshot atomic.Int32
	h := newHarness(t, func(s *sconn) {
		s.ready("session.start")
		_ = s.command()
		s.send("session.started", evo{seq: 2, replay: true}, protocol.SessionStarted{Status: "ACTIVE"})
		s.send("turn.started", evo{seq: 4, turn: 1, replay: true}, protocol.TurnStarted{Initiator: "USER"})
		go func() {
			time.Sleep(150 * time.Millisecond) // the rest of a long replay
			s.send("turn.completed", evo{seq: 6, turn: 1, replay: true}, protocol.TurnCompleted{Status: "FAILED"})
			snapshotSent.Store(true)
			s.send("session.snapshot", evo{seq: 8}, protocol.SessionSnapshot{
				Replay: "COMPLETE", ReplayedCount: 3, ServerSeqBaseline: 8, NextTurnSeq: 2, TurnState: "LISTENING",
				RecentTurns: []protocol.RecentTurn{{TurnSeq: 1, Status: "FAILED"}},
			})
		}()
		det := turnDetector{quiet: 5}
		_ = s.serve(func(_ int, f frameRec) {
			if !snapshotSent.Load() {
				framesBeforeSnapshot.Add(1)
			}
			if det.feed(f.payload) {
				s.answer(2, 4, lat)
			}
		}, func(ev protocol.ClientEvent) {
			if ev.Type == "session.end" {
				s.send("session.completed", evo{}, protocol.SessionCompleted{EndReason: "USER_ENDED", ChargedSeconds: 3})
			}
		})
	})
	rep, err := h.run([]Step{Speak{Packets: packets(speechA, 3)}, WaitAIDone{}, End{}})
	require.NoError(t, err)
	require.Zero(t, framesBeforeSnapshot.Load(), "no upstream frame before the snapshot that ends the replay")
	require.Equal(t, "COMPLETED", rep.Status)
	require.Equal(t, []int64{100}, rep.TurnLatencyMS, "the answer to the utterance, not the replayed turn, ended WaitAIDone")
}

func TestSnapshotResetsBaseline(t *testing.T) {
	var resumes []int64
	h := newHarness(t, func(s *sconn) {
		switch s.idx {
		case 0:
			s.ready("session.start")
			_ = s.command()
			s.send("session.started", evo{}, protocol.SessionStarted{Status: "ACTIVE"})
			_ = s.serve(func(_ int, _ frameRec) {
				// Numbers 2..20, the highest the client will have seen.
				for q := 2; q <= 20; q++ {
					s.send("balance.tick", evo{}, protocol.BalanceTick{RemainingSeconds: 300 - q})
				}
			}, nil)
		default:
			s.ready("session.resume")
			cmd := s.command()
			rs, _ := cmd.Payload.(protocol.SessionResume)
			resumes = append(resumes, rs.LastServerSeq)
			base := int64(12) // a takeover pod numbers from what Redis had: lower than the client saw
			if s.idx == 2 {
				base = 30
			}
			s.send("session.snapshot", evo{seq: base}, protocol.SessionSnapshot{Replay: "UNAVAILABLE", ServerSeqBaseline: base, ActiveGeneration: 1})
			_ = s.serve(nil, func(ev protocol.ClientEvent) {
				if ev.Type == "session.end" {
					s.send("session.completed", evo{}, protocol.SessionCompleted{EndReason: "USER_ENDED"})
				}
			})
		}
	})
	rep, err := h.run([]Step{Think{D: FrameInterval}, Drop{For: 100 * time.Millisecond}, Drop{For: 100 * time.Millisecond}, Drop{For: 100 * time.Millisecond}, End{}})
	require.NoError(t, err)
	// 20 before the first drop; the snapshot said 12, so that is what the next resume reports
	// (a client that kept its own maximum would say 20).
	require.Equal(t, []int64{20, 12, 30}, resumes)
	require.Len(t, rep.ResumeMS, 3)
}

func TestAlwaysSendsFramesEvery20ms(t *testing.T) {
	h := newHarness(t, func(s *sconn) {
		s.ready("session.start")
		_ = s.command()
		s.send("session.started", evo{}, protocol.SessionStarted{Status: "ACTIVE"})
		_ = s.serve(nil, func(ev protocol.ClientEvent) {
			if ev.Type == "session.end" {
				s.send("session.completed", evo{}, protocol.SessionCompleted{EndReason: "USER_ENDED"})
			}
		})
	})
	rep, err := h.run([]Step{Think{D: time.Second}, End{}})
	require.NoError(t, err)
	require.Equal(t, "COMPLETED", rep.Status)

	fr := h.framesOf(0)
	require.Len(t, h.textsOf(0), 1, "session.end is the only text event")
	var before int
	for _, f := range fr {
		if f.at.Sub(fr[0].at) < time.Second {
			before++
		}
	}
	require.Equal(t, 50, before, "1 s of Think is 50 frames")
	for i, f := range fr {
		require.Equal(t, uint32(i), f.seq, "sequence numbers are consecutive")
		require.Equal(t, fr[0].at.Add(time.Duration(i)*FrameInterval), f.at, "frame %d goes out exactly 20 ms after the previous one", i)
		require.True(t, isSilence(f.payload), "an idle mic still sends silence packets")
	}
}

func TestWaitAIDoneTimesOut(t *testing.T) {
	h := newHarness(t, func(s *sconn) {
		s.ready("session.start")
		_ = s.command()
		s.send("session.started", evo{}, protocol.SessionStarted{Status: "ACTIVE"})
		_ = s.serve(nil, nil) // the AI never answers
	})
	rep, err := h.run([]Step{Speak{Packets: packets(speechA, 3)}, WaitAIDone{Timeout: 200 * time.Millisecond}, End{}})
	require.ErrorIs(t, err, ErrAITimeout)
	require.EqualValues(t, tHandshakeAdvance.Milliseconds(), rep.ConnectMS, "the Report comes back with the error")
	require.Equal(t, "ACTIVE", rep.Status)
	// 3 speech frames, then silence for 200 ms = 10 slots; nothing was sent after the timeout.
	require.Len(t, h.framesOf(0), 13)
	require.Empty(t, rep.TurnLatencyMS)
}

func TestSessionEndingOnItsOwnEndsTheScript(t *testing.T) {
	// wait-expiry: a long Think while the server's timer expires the session.
	h := newHarness(t, func(s *sconn) {
		s.ready("session.start")
		_ = s.command()
		s.send("session.started", evo{}, protocol.SessionStarted{Status: "ACTIVE"})
		_ = s.serve(func(i int, _ frameRec) {
			if i == 5 {
				s.send("session.completed", evo{}, protocol.SessionCompleted{EndReason: "DURATION_ELAPSED", ChargedSeconds: 300})
				_ = s.c.Close(websocket.StatusNormalClosure, "")
			}
		}, nil)
	})
	rep, err := h.run([]Step{Think{D: 10 * time.Minute}, End{}})
	require.NoError(t, err, "the session ending by itself is the expected outcome of a wait-expiry script")
	require.Equal(t, "COMPLETED", rep.Status)
	require.Equal(t, 300, rep.ChargedSeconds)
	require.Less(t, len(h.framesOf(0)), 12, "the script stopped sending once the session was over")
	for _, ev := range h.textsOf(0) {
		require.NotEqual(t, "session.end", ev.Type, "End is not sent into a finished session")
	}
}

func TestFatalServerErrorIsReturned(t *testing.T) {
	h := newHarness(t, func(s *sconn) {
		s.ready("session.start")
		_ = s.command()
		s.send("session.started", evo{}, protocol.SessionStarted{Status: "ACTIVE"})
		_ = s.serve(func(i int, _ frameRec) {
			if i == 2 {
				s.send("error", evo{}, protocol.ErrorPayload{
					Error: protocol.ErrorBody{Code: "PROTOCOL_VIOLATION", Message: "no"}, Fatal: true,
				})
				_ = s.c.Close(websocket.StatusPolicyViolation, "violation")
			}
		}, nil)
	})
	rep, err := h.run([]Step{Think{D: time.Second}})
	var se *ServerError
	require.ErrorAs(t, err, &se)
	require.Equal(t, "PROTOCOL_VIOLATION", se.Code)
	require.Equal(t, "ACTIVE", rep.Status)
	require.Equal(t, int(websocket.StatusPolicyViolation), rep.CloseCode, "the close code the server ended the connection with")
}

func TestResumeRefusedFails(t *testing.T) {
	h := newHarness(t, func(s *sconn) {
		s.ready("session.start")
		if s.idx == 0 {
			_ = s.command()
			s.send("session.started", evo{}, protocol.SessionStarted{Status: "ACTIVE"})
			_ = s.serve(nil, nil)
			return
		}
		_ = s.command()
		s.send("error", evo{}, protocol.ErrorPayload{Error: protocol.ErrorBody{Code: "SESSION_NOT_RESUMABLE", Message: "gone"}, Fatal: true})
		_ = s.c.Close(4010, "SESSION_NOT_RESUMABLE")
	})
	_, err := h.run([]Step{Drop{For: FrameInterval * 5}, Think{D: time.Second}})
	var se *ServerError
	require.ErrorAs(t, err, &se)
	require.Equal(t, "SESSION_NOT_RESUMABLE", se.Code)
}

func TestRunConversationRejectsBadInput(t *testing.T) {
	_, err := RunConversation(context.Background(), nil, SessionSpec{SessionID: testSession}, nil, nil)
	require.Error(t, err)
	_, err = RunConversation(context.Background(), New("http://x"), SessionSpec{}, nil, nil)
	require.Error(t, err)

	// A refused ticket is the REST error, and the Report is still returned.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, 409, map[string]any{"error": map[string]any{"code": "SESSION_NOT_ACTIVE", "message": "no", "request_id": "r", "details": nil}})
	}))
	defer srv.Close()
	rep, err := RunConversation(context.Background(), New(srv.URL), SessionSpec{SessionID: testSession, WSURL: "ws://127.0.0.1:1/x"}, nil, nil)
	var ae *APIError
	require.ErrorAs(t, err, &ae)
	require.Equal(t, "SESSION_NOT_ACTIVE", ae.Code)
	require.Equal(t, "ACTIVE", rep.Status)
	require.NotNil(t, rep.TurnLatencyMS, "slices are never null in summary.json")
	require.False(t, errors.Is(err, errSessionEnded))
}

func TestAudioCollectorGroupsByGeneration(t *testing.T) {
	ac := NewAudioCollector()
	ev := func(typ string, turn int, gen uint32) Received[protocol.ServerEvent] {
		return Received[protocol.ServerEvent]{Value: protocol.ServerEvent{Type: typ, TurnSeq: ptr(turn), Generation: ptr(gen)}}
	}
	ac.OnAudio(AudioFrame{Generation: 4, Seq: 0, Payload: []byte{1}}) // the event may come after the first frame
	ac.OnEvent(ev("ai.generation_started", 2, 4))
	ac.OnEvent(ev("ai.generation_started", 1, 1))
	ac.OnEvent(ev("turn.completed", 9, 9)) // not a generation start
	ac.OnAudio(AudioFrame{Generation: 1, Seq: 0, Payload: []byte{2}})
	ac.OnAudio(AudioFrame{Generation: 4, Seq: 1, Payload: []byte{3}})
	ac.OnAudio(AudioFrame{Generation: 7, Seq: 0, Payload: []byte{4}}) // generation without its event

	got := ac.Generations()
	require.Len(t, got, 3)
	require.Equal(t, []uint32{1, 4, 7}, []uint32{got[0].Generation, got[1].Generation, got[2].Generation})
	require.Equal(t, []int{1, 2, 0}, []int{got[0].TurnSeq, got[1].TurnSeq, got[2].TurnSeq})
	require.Len(t, got[1].Frames, 2)
	require.EqualValues(t, 1, got[1].Frames[1].Seq)
}

func TestSinkHelpers(t *testing.T) {
	var order []string
	a := SinkFuncs{Event: func(Received[protocol.ServerEvent]) { order = append(order, "a-event") }}
	b := SinkFuncs{Audio: func(AudioFrame) { order = append(order, "b-audio") }}
	m := MultiSink(a, b, NopSink{})
	m.OnEvent(Received[protocol.ServerEvent]{})
	m.OnAudio(AudioFrame{})
	require.Equal(t, []string{"a-event", "b-audio"}, order, "nil functions and NopSink discard; the rest run in order")
}

func TestConsumeHandlesFramesBeforeALaterEvent(t *testing.T) {
	// The reader queues frames and events in different channels; when the consumer is behind
	// and both are ready, a frame the server wrote before ai.interrupted must still be counted
	// as the last frame of the old generation. Repeated: a select alone picks at random.
	t0 := time.Date(2026, 10, 16, 9, 0, 0, 0, time.UTC)
	for i := 0; i < 200; i++ {
		r := &runner{sink: NopSink{}, sig: make(chan struct{}, 1), clk: clock.NewFake(t0), spec: SessionSpec{SessionID: testSession}}
		r.rep = Report{BargeInStopMS: []int64{}}
		r.bi = bargeIn{armed: true, t0: t0, oldGen: 1, lastOldAt: t0}
		rc := &RealtimeConn{events: make(chan Received[protocol.ServerEvent], 4), audio: make(chan AudioFrame, 4)}
		rc.audio <- AudioFrame{Generation: 1, Seq: 9, At: t0.Add(40 * time.Millisecond)}
		rc.events <- Received[protocol.ServerEvent]{
			Value: protocol.ServerEvent{Type: "ai.interrupted", ServerSeq: 3, Payload: protocol.AIInterrupted{Reason: "BARGE_IN", ActiveGeneration: 2}},
			At:    t0.Add(41 * time.Millisecond),
		}
		close(rc.audio)
		close(rc.events)
		r.consume(context.Background(), rc)
		require.Equal(t, []int64{40}, r.rep.BargeInStopMS, "iteration %d", i)
	}
}

// TestWaitAIDoneSeesTheAIFirstOpening: the opening of an AI_FIRST session is turn 0, and the
// script's WaitAIDone must end when that turn completes (found by e2e E2: it waited for a turn
// above 0 and timed out).
func TestWaitAIDoneSeesTheAIFirstOpening(t *testing.T) {
	h := newHarness(t, func(s *sconn) {
		s.ready("session.start")
		_ = s.command()
		s.send("session.started", evo{}, protocol.SessionStarted{Status: "ACTIVE", ConversationStartMode: "AI_FIRST"})
		open := evo{zeroTurn: true}
		s.send("turn.started", open, protocol.TurnStarted{Initiator: "AI"})
		s.send("ai.generation_started", evo{zeroTurn: true, gen: ptr(uint32(1))}, protocol.AIGenerationStarted{Kind: "OPENING"})
		s.audio(1, 0, 0xC1)
		s.send("ai.audio_completed", evo{zeroTurn: true, gen: ptr(uint32(1))}, protocol.AIAudioCompleted{FrameCount: 1, TotalAudioMS: 20, SentenceCount: 1, LLMFinished: true})
		s.send("turn.completed", open, protocol.TurnCompleted{Status: "COMPLETED"})
		_ = s.serve(nil, func(ev protocol.ClientEvent) {
			if ev.Type == "session.end" {
				s.send("session.completed", evo{}, protocol.SessionCompleted{EndReason: "USER_ENDED", ChargedSeconds: 3})
			}
		})
	})
	rep, err := h.run([]Step{WaitAIDone{Timeout: 5 * time.Second}, End{}})
	require.NoError(t, err)
	require.Equal(t, "COMPLETED", rep.Status)
}

func TestRunConversationHandshakeCarriesTheClientIP(t *testing.T) {
	h := newHarness(t, func(s *sconn) {
		s.ready("session.start")
		_ = s.command()
		s.send("session.started", evo{}, protocol.SessionStarted{Status: "ACTIVE", TurnState: "LISTENING"})
		_ = s.serve(func(int, frameRec) {}, func(ev protocol.ClientEvent) {
			if ev.Type == "session.end" {
				s.send("session.completed", evo{}, protocol.SessionCompleted{EndReason: "USER_ENDED", ChargedSeconds: 1})
			}
		})
	})
	h.clientIP = "10.7.0.9"
	_, err := h.run([]Step{End{}})
	require.NoError(t, err)
	h.mu.Lock()
	defer h.mu.Unlock()
	require.Equal(t, map[int]string{0: "10.7.0.9"}, h.xff)
}
