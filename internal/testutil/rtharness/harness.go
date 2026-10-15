// Package rtharness runs a conversation.Manager end to end in a test: a fake clock, manual
// tickers, an in-memory WebSocket (PipeConn), the scripted STT and LLM/TTS providers and the
// in-memory ports of testutil/fakes. It is the harness of the runtime tests of M5 (Tasks 18–23)
// and of the e2e of M5.
//
// The harness imports the conversation package and so conversation/audio (cgo); the depguard
// exception for it sits next to the one of testutil/audiofx (.golangci.yml, rule 5).
//
// Extension: a later task that needs a new seam adds an Option, a field on H or a helper here;
// the Runtime-side seams are Runtime.View and Runtime.Barrier (H.Sync), Deps.NewTicker and
// Deps.Sleep (fake time), and the fakes' FailNext hooks. Nothing here sleeps on the wall clock
// except the bounded polling of WaitFor and Sync.
package rtharness

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/conversation/audio"
	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/session"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/audiofx"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

const (
	convPkg  = "github.com/nabePi/kotoki-backend/internal/conversation"
	fakesPkg = "github.com/nabePi/kotoki-backend/internal/testutil/fakes"

	// waitTimeout bounds every real-time wait of the harness.
	waitTimeout = 5 * time.Second
	podID       = "pod-a"
)

// T0 is the fake clock's start.
var T0 = time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)

// Fixed ids of the seeded session and its content.
var (
	SessionID         = uuid.MustParse("0190f5a0-0000-7000-8000-000000000001")
	UserID            = uuid.MustParse("0190f5a0-0000-7000-8000-000000000002")
	topicID           = uuid.MustParse("0190f5a0-0000-7000-8000-0000000000a1")
	scenarioID        = uuid.MustParse("0190f5a0-0000-7000-8000-0000000000a2")
	scenarioVersionID = uuid.MustParse("0190f5a0-0000-7000-8000-0000000000a3")
	characterID       = uuid.MustParse("0190f5a0-0000-7000-8000-0000000000a4")
	promptVersionID   = uuid.MustParse("0190f5a0-0000-7000-8000-0000000000a5")
	openingVersionID  = uuid.MustParse("0190f5a0-0000-7000-8000-0000000000a6")
)

// Voices of the seeded character (D-015): the primary and the optional voice.fallback.
var (
	PrimaryVoice  = content.TTSBinding{ProviderName: "CARTESIA", ModelName: "sonic3.6", VoiceID: "voice-primary"}
	FallbackVoice = content.TTSBinding{ProviderName: "ELEVENLABS", ModelName: "Eleven v3 Conversational", VoiceID: "voice-fallback"}
)

// OpeningLine is the scenario_versions.opening_line of the seeded scenario.
const OpeningLine = "いらっしゃいませ。ご注文はお決まりですか。"

// options is what an Option changes before New builds the harness.
type options struct {
	mode          session.StartMode
	status        session.Status
	duration      int
	openingPrompt bool
	voiceFallback bool
	script        fakes.Script
	timeline      replay.Timeline
	sttOpenErr    error
	tune          func(*config.RealtimeTuning)
	limits        conversation.Limits
	wrapGen       func(provider.Generation) provider.Generation
	sleep         func(ctx context.Context, d time.Duration) error
	apology       func() ([][]byte, error)
}

// Option configures New.
type Option func(*options)

// WithStartMode sets conversation_start_mode (default USER_FIRST). AI_FIRST also gives the session
// an OPENING prompt version unless WithoutOpeningPrompt is used.
func WithStartMode(m session.StartMode) Option { return func(o *options) { o.mode = m } }

// WithStatus seeds the session in another status (default RESERVED).
func WithStatus(s session.Status) Option { return func(o *options) { o.status = s } }

// WithDuration sets duration_seconds (default 600).
func WithDuration(seconds int) Option { return func(o *options) { o.duration = seconds } }

// WithoutOpeningPrompt leaves opening_prompt_version_id NULL: an AI_FIRST session plays the
// static opening line (S-2).
func WithoutOpeningPrompt() Option { return func(o *options) { o.openingPrompt = false } }

// WithVoiceFallback gives the character a voice.fallback (default none).
func WithVoiceFallback() Option { return func(o *options) { o.voiceFallback = true } }

// WithScript sets the script every generation plays (see fakes.Script).
func WithScript(s fakes.Script) Option { return func(o *options) { o.script = s } }

// WithTimeline sets the STT timeline the opened STT stream plays (default: empty).
func WithTimeline(tl replay.Timeline) Option { return func(o *options) { o.timeline = tl } }

// WithSTTOpenError makes OpenSTT fail with err.
func WithSTTOpenError(err error) Option { return func(o *options) { o.sttOpenErr = err } }

// WithTuning edits the RealtimeTuning (defaults of spec §7.2) before the Manager is built.
func WithTuning(f func(*config.RealtimeTuning)) Option { return func(o *options) { o.tune = f } }

// WithGenerationWrapper wraps every provider.Generation the router hands out (to inject a panic or
// a delay into one call).
func WithGenerationWrapper(f func(provider.Generation) provider.Generation) Option {
	return func(o *options) { o.wrapGen = f }
}

// WithSleep replaces the pacer's wait (default: the fake clock moves by the wait and it returns at
// once). A test that blocks it holds the pacer mid-generation, so audio is parsed and queued but
// not yet written. The clock is the test's to move then.
func WithSleep(f func(ctx context.Context, d time.Duration) error) Option {
	return func(o *options) { o.sleep = f }
}

// WithApology replaces the source of the APOLOGY packets (default: the embedded clip), to hold the
// asset load or make it fail.
func WithApology(f func() ([][]byte, error)) Option { return func(o *options) { o.apology = f } }

// WithLimits sets the pod capacity limits (default 95/100/120).
func WithLimits(l conversation.Limits) Option { return func(o *options) { o.limits = l } }

// Gates make a load of the start flow hang until the test closes the channel (or the context of
// the load ends). Set the fields before Connect.
type Gates struct {
	Config  chan struct{} // content.SessionConfigLoader.Load
	Learner chan struct{} // LearnerContextProvider.PromptContext
	Memory  chan struct{} // memory.RetrievalService.BuildSessionContext
	STT     chan struct{} // RealtimeRouter.OpenSTT
	Refresh chan struct{} // memory.RetrievalService.RefreshEpisodic (the post-turn memory job)
}

// H is one running harness.
type H struct {
	T       *testing.T
	Clock   *clock.Fake
	Tickers *Tickers
	Conn    *PipeConn
	Mgr     *conversation.Manager
	Deps    conversation.Deps
	Metrics *conversation.Metrics
	Reg     *prometheus.Registry

	Session *fakes.SessionRuntime
	Calls   *fakes.CallRecorder
	Turns   *fakes.TurnRepo
	Written *fakes.EventWriter // session_events appended by the runtime
	Store   *fakes.RuntimeStore
	Learner *fakes.LearnerContext
	Memory  *fakes.MemoryRetrieval
	Router  *Router
	Content *ContentLoader
	Gates   Gates
	// OnLearner, when set, runs at the start of every LearnerContextProvider.PromptContext (a test
	// panics or counts in it). Set it before Connect.
	OnLearner func()

	SID    uuid.UUID
	Ticket conversation.AttachTicket

	// Prev is the connection a Reconnect replaced (nil before the first one).
	Prev *PipeConn

	ids     idgen.Generator
	ctx     context.Context
	cancel  context.CancelFunc
	enc     *audio.Encoder
	upSeq   uint32
	cur     *attachRun   // the Attach of the current connection (nil before Attach)
	runs    []*attachRun // every Attach this harness started, for Close
	once    sync.Once
	blocked atomic.Int32
}

// attachRun is one Manager.Attach call: done is closed when it returned, err is its result.
type attachRun struct {
	done chan struct{}
	err  error
}

// New builds the harness: the fakes are seeded with one session (RESERVED unless WithStatus) and
// the Manager is ready. Close runs at the end of the test (t.Cleanup) and the test fails on
// leaked goroutines of the conversation and fakes packages.
func New(t *testing.T, opts ...Option) *H {
	t.Helper()
	o := options{
		mode: session.StartUserFirst, status: session.StatusReserved, duration: 600, openingPrompt: true,
		script: fakes.Script{Deltas: []string{"こんにちは。", "今日はいい天気ですね。"}, PCMPerSentenceMS: 100},
		limits: conversation.Limits{ReadyMax: 95, Max: 100, HardMax: 120},
	}
	for _, f := range opts {
		f(&o)
	}
	testutil.NoGoroutineLeak(t, convPkg)
	testutil.NoGoroutineLeak(t, fakesPkg)

	h := &H{T: t, SID: SessionID, ids: idgen.NewSequence(t)}
	h.Clock = clock.NewFake(T0)
	h.Tickers = &Tickers{}
	h.Conn = NewPipeConn()
	h.Calls = &fakes.CallRecorder{}
	h.Session = fakes.NewSessionRuntime(h.Clock)
	h.Session.Recorder = h.Calls
	h.Turns = fakes.NewTurnRepo()
	h.Written = fakes.NewEventWriter()
	h.Turns.FenceOf, h.Written.FenceOf = h.Session.Fence, h.Session.Fence // a stale owner's writes are refused, as the fenced SQL does
	h.Store = fakes.NewRuntimeStore()
	h.Learner = &fakes.LearnerContext{Ctx: prompt.LearnerContext{DisplayName: "タロウ"}}
	h.Memory = &fakes.MemoryRetrieval{}
	h.Router = &Router{ScriptedRouter: fakes.NewScriptedRouter(o.script), timeline: o.timeline, openErr: o.sttOpenErr, h: h, wrap: o.wrapGen}
	h.Content = newContentLoader(h)
	h.Reg = prometheus.NewRegistry()
	h.Metrics = conversation.NewMetrics(h.Reg)
	h.enc, _ = audio.NewEncoder(48000, 32000, 5)

	tuning := DefaultTuning(t)
	if o.tune != nil {
		o.tune(&tuning)
	}
	h.Session.Seed(sessionRecord(t, o))
	h.Ticket = conversation.AttachTicket{UserID: UserID, SessionID: SessionID}
	h.Deps = conversation.Deps{
		Cfg: tuning, Limits: o.limits, Clock: h.Clock, IDs: h.ids, Metrics: h.Metrics,
		Session: h.Session, Turns: h.Turns, Events: h.Written, Store: h.Store,
		Configs: conversation.NewConfigLoader(h.Content, h.Content, 8),
		Learner: learnerGate{h.Learner, h}, Memory: memoryGate{h.Memory, h},
		Router: h.Router, Builder: conversation.NewPromptBuilder(),
		NewTicker: func(d time.Duration) conversation.Ticker { return h.Tickers.New(d) },
		// The pacer waits on the fake clock: a sleep is the passing of that much time.
		Sleep: func(ctx context.Context, d time.Duration) error { h.Clock.Advance(d); return ctx.Err() },
	}
	if o.sleep != nil {
		h.Deps.Sleep = o.sleep
	}
	h.Deps.Apology = o.apology
	h.Mgr = conversation.NewManager(h.Deps)
	h.ctx, h.cancel = context.WithCancel(context.Background())
	h.Mgr.StartSubscriber(h.ctx)
	t.Cleanup(h.Close)
	return h
}

// DefaultTuning returns the RealtimeTuning of spec §7.2 (the envDefault tags), with the pod id of
// the harness.
func DefaultTuning(t testing.TB) config.RealtimeTuning {
	t.Helper()
	var c config.RealtimeTuning
	if err := env.ParseWithOptions(&c, env.Options{Environment: map[string]string{}}); err != nil {
		t.Fatalf("rtharness: default tuning: %v", err)
	}
	c.PodID = podID
	return c
}

// sessionRecord is the seeded sessions row as the runtime reads it.
func sessionRecord(t testing.TB, o options) session.RuntimeRecord {
	t.Helper()
	snap := content.SessionSnapshot{
		Character: content.CharacterSnapshot{
			ID: characterID, Code: "HARUKA", Name: "春香", Persona: "明るくて親切な会話パートナー。",
			ProviderName: PrimaryVoice.ProviderName, ModelName: PrimaryVoice.ModelName, VoiceID: PrimaryVoice.VoiceID,
			Voice: content.Voice{Gender: content.GenderFemale, Accent: content.AccentKanto, SpeakingRate: 1.0},
		},
		Formality: content.FormalityPolite,
	}
	if o.voiceFallback {
		fb := FallbackVoice
		snap.Character.Voice.Fallback = &fb
	}
	rawSnap, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("rtharness: snapshot: %v", err)
	}
	ad, err := json.Marshal(learner.AdaptiveDifficulty{
		AlgorithmVersion: 1, ScenarioDifficulty: 3, VocabularyComplexity: 3, GrammarComplexity: 3, SpeechSpeed: 3,
		TTSSpeakingRate: 1.0, MaxAISentencesPerTurn: 3, TopicNovelty: 3, TopicAbstraction: 3, QuestionComplexity: 3,
		FollowUpDepth: 2, MaxFollowUps: 1, ExpectedResponseLength: 3, Spontaneity: 3, Scaffolding: 1,
		Formality: learner.Formality("POLITE"), TurnTakingComplexity: 3,
	})
	if err != nil {
		t.Fatalf("rtharness: adaptive: %v", err)
	}
	rec := session.RuntimeRecord{
		ID: SessionID, UserID: UserID, Status: o.status, ConversationStartMode: o.mode, DurationSeconds: o.duration,
		TopicID: topicID, ScenarioID: scenarioID, ScenarioVersionID: scenarioVersionID, CharacterID: characterID,
		PromptVersionID: promptVersionID, ConfigSnapshot: rawSnap, AdaptiveDifficulty: ad,
	}
	if o.mode == session.StartAIFirst && o.openingPrompt {
		id := openingVersionID
		rec.OpeningPromptVersionID = &id
	}
	if o.status == session.StatusActive {
		start := T0
		exp := T0.Add(time.Duration(o.duration) * time.Second)
		rec.StartedAt, rec.ExpiresAt, rec.RuntimeFence = &start, &exp, 1
	}
	return rec
}

// ---- connecting ----

// Cmd builds a client command of the session.
func (h *H) Cmd(typ string, payload any) protocol.ClientEvent {
	return protocol.ClientEvent{Type: typ, Version: protocol.Version, EventID: h.ids.New(), SessionID: h.SID, Payload: payload}
}

// ValidAudio is the only audio format the protocol accepts (D-071).
var ValidAudio = protocol.AudioParams{Codec: "opus", SampleRate: 48000, Channels: 1, FrameMS: 20}

// StartCmd is a valid session.start.
func (h *H) StartCmd() protocol.ClientEvent {
	return h.Cmd("session.start", protocol.SessionStart{Audio: ValidAudio})
}

// ResumeCmd is a valid session.resume.
func (h *H) ResumeCmd(lastServerSeq int64) protocol.ClientEvent {
	return h.Cmd("session.resume", protocol.SessionResume{LastServerSeq: lastServerSeq, Audio: ValidAudio})
}

// EndCmd is session.end{USER_ENDED}.
func (h *H) EndCmd() protocol.ClientEvent {
	return h.Cmd("session.end", protocol.SessionEnd{Reason: "USER_ENDED"})
}

// PlaybackFinishedCmd is client.playback_finished for generation gen.
func (h *H) PlaybackFinishedCmd(gen uint32) protocol.ClientEvent {
	return h.Cmd("client.playback_finished", protocol.ClientPlaybackFinished{Generation: gen, PlayedMS: 1000})
}

// Connect sends first as the first command and attaches the connection to the Manager.
func (h *H) Connect(first protocol.ClientEvent) {
	h.T.Helper()
	h.Conn.SendEvent(h.T, first)
	h.Attach()
}

// Attach attaches the connection without sending a command (the handshake waits for one).
func (h *H) Attach() {
	h.T.Helper()
	if h.cur != nil {
		h.T.Fatal("rtharness: Attach called twice on one connection (use Reconnect for another)")
	}
	run := &attachRun{done: make(chan struct{})}
	h.cur = run
	h.runs = append(h.runs, run)
	mgr, conn, ticket, ctx := h.Mgr, h.Conn, h.Ticket, h.ctx
	go func() {
		run.err = mgr.Attach(ctx, conn, ticket)
		close(run.done)
	}()
}

// Reconnect gives the harness a new connection, as a client that dropped and came back: Conn is a
// fresh PipeConn (the old one stays in Prev, still attached unless it ended), the upstream sequence
// starts at 0 again, and the next Connect/Attach attaches it. Events, Frames and the like read the
// new connection.
func (h *H) Reconnect() {
	h.T.Helper()
	h.Prev = h.Conn
	h.Conn = NewPipeConn()
	h.cur = nil
	h.upSeq = 0
}

// Peer builds a second pod over the same session: its own Manager and connection, the same clock,
// tickers, fakes and session (so the fence of Postgres and the state of Redis are shared). Use it
// for a takeover: Connect a resume on the peer.
func (h *H) Peer(pod string) *H {
	h.T.Helper()
	p := &H{
		T: h.T, SID: h.SID, Clock: h.Clock, Tickers: h.Tickers, Session: h.Session, Calls: h.Calls, Turns: h.Turns,
		Written: h.Written, Store: h.Store, Learner: h.Learner, Memory: h.Memory, Router: h.Router, Content: h.Content,
		Ticket: h.Ticket, ids: h.ids, Conn: NewPipeConn(), Reg: prometheus.NewRegistry(),
	}
	p.enc, _ = audio.NewEncoder(48000, 32000, 5)
	p.Metrics = conversation.NewMetrics(p.Reg)
	p.Deps = h.Deps
	p.Deps.Metrics = p.Metrics
	p.Deps.Cfg.PodID = pod
	p.Mgr = conversation.NewManager(p.Deps)
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.Mgr.StartSubscriber(p.ctx)
	h.T.Cleanup(p.Close)
	return p
}

// Send sends a client command on the open connection.
func (h *H) Send(ev protocol.ClientEvent) {
	h.T.Helper()
	h.Conn.SendEvent(h.T, ev)
}

// AttachResult waits for the Attach of the current connection to return and returns its error.
func (h *H) AttachResult() error {
	h.T.Helper()
	if h.cur == nil {
		h.T.Fatal("rtharness: no Attach to wait for")
	}
	run := h.cur
	h.WaitFor("Attach to return", func() bool {
		select {
		case <-run.done:
			return true
		default:
			return false
		}
	})
	return run.err
}

// ---- audio ----

// Speak sends the audio of a fixture (testdata/audio) as real Opus frames, one per 20 ms of fake
// time (the clock advances 20 ms per frame), in order with the upstream sequence of the
// connection. The runtime handles them as it would a client's microphone.
func (h *H) Speak(fixture string) {
	h.T.Helper()
	pcm, _ := audiofx.LoadFixture(h.T, fixture)
	if pcm.Rate != 48000 {
		h.T.Fatalf("rtharness: fixture %s is %d Hz, want 48000", fixture, pcm.Rate)
	}
	const block = 960 // 20 ms at 48 kHz
	for off := 0; off+block <= len(pcm.Samples); off += block {
		h.sendFrame(pcm.Samples[off : off+block])
	}
}

// SpeakSlice sends the part [fromMs, toMs) of a fixture, for a test that needs a short or partial
// utterance (a 300 ms "ん" of a longer recording).
func (h *H) SpeakSlice(fixture string, fromMs, toMs int) {
	h.T.Helper()
	pcm, _ := audiofx.LoadFixture(h.T, fixture)
	if pcm.Rate != 48000 {
		h.T.Fatalf("rtharness: fixture %s is %d Hz, want 48000", fixture, pcm.Rate)
	}
	const block = 960 // 20 ms at 48 kHz
	from, to := fromMs*48, min(toMs*48, len(pcm.Samples))
	for off := from; off+block <= to; off += block {
		h.sendFrame(pcm.Samples[off : off+block])
	}
}

// SendRawUpstream sends opus as the next upstream packet (the sequence advances), whatever it
// holds: an undecodable packet for the AUDIO_FRAME_INVALID tests.
func (h *H) SendRawUpstream(opus []byte) {
	h.T.Helper()
	h.Conn.Send(conversation.MessageBinary, protocol.EncodeUpstream(h.upSeq, opus))
	h.upSeq++
	h.Clock.Advance(20 * time.Millisecond)
}

// SkipUpstream advances the upstream sequence by n without sending: the next frame shows a gap of
// n frames (a client that dropped them, §7.5.2).
func (h *H) SkipUpstream(n int) { h.upSeq += uint32(n) } //nolint:gosec // a test passes a small count

// Silence sends ms of digital silence as Opus frames (a muted microphone, D-071).
func (h *H) Silence(ms int) {
	h.T.Helper()
	zero := make([]int16, 960)
	for range ms / 20 {
		h.sendFrame(zero)
	}
}

func (h *H) sendFrame(block []int16) {
	h.T.Helper()
	pkt, err := h.enc.Encode(block)
	if err != nil {
		h.T.Fatalf("rtharness: encode frame: %v", err)
	}
	h.Conn.Send(conversation.MessageBinary, protocol.EncodeUpstream(h.upSeq, pkt))
	h.upSeq++
	h.Clock.Advance(20 * time.Millisecond)
	if h.upSeq%25 == 0 { // never let audioIn overflow (the reader drops the newest frame when it is full)
		h.WaitFor("pipe idle", h.Conn.Idle)
		// ...and let the loop catch up with the audio and STT events every 500 ms of audio, so a
		// timeline step (a FINAL) is handled before the frames that follow it, as it would be
		// with a real-time client.
		if r, ok := h.Runtime(); ok {
			ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
			_ = r.Barrier(ctx) // a stopped runtime has nothing to catch up with
			cancel()
		}
	}
}

// ---- time ----

// AdvanceClock moves the fake clock forward.
func (h *H) AdvanceClock(d time.Duration) { h.Clock.Advance(d) }

// Tick fires the runtime's tickers (the 100 ms loop and handshake tickers and the 50 ms redisSync
// ticker), each once, with the current fake time. Deadlines of the loop are checked on the tick;
// call Sync afterwards to wait until it has been handled.
func (h *H) Tick() {
	h.Tickers.Fire(h.Clock.Now(), func(d time.Duration) bool { return d < time.Second })
}

// TickPing fires the heartbeat tickers (WS_PING_INTERVAL): the idle check and the ping.
func (h *H) TickPing() {
	h.Tickers.Fire(h.Clock.Now(), func(d time.Duration) bool { return d >= time.Second && d <= time.Minute })
}

// ---- observing ----

// Runtime returns the session's runtime once it exists.
func (h *H) Runtime() (*conversation.Runtime, bool) { return h.Mgr.Runtime(h.SID) }

// View returns the loop state of the session's runtime.
func (h *H) View() conversation.View {
	h.T.Helper()
	r, ok := h.Runtime()
	if !ok {
		h.T.Fatal("rtharness: the session has no runtime")
	}
	ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
	defer cancel()
	v, err := r.View(ctx)
	if err != nil {
		h.T.Fatalf("rtharness: view: %v", err)
	}
	return v
}

// Sync waits until the runtime has settled: the connection's reader is idle, a marker has passed
// through the audio pipeline and the STT receiver, the loop has drained its inputs and the
// outbound queue is empty, twice in a row with no new input handled in between. It needs a
// command already sent (Connect) or an Attach that has returned. Events a generation goroutine
// has not produced yet are not covered: wait for them with WaitEvent.
func (h *H) Sync() {
	h.T.Helper()
	deadline := time.Now().Add(waitTimeout)
	stable, last := 0, ^uint64(0)
	for {
		if time.Now().After(deadline) {
			h.T.Fatal("rtharness: Sync timed out")
		}
		r, ok := h.Runtime()
		if !ok {
			if h.attachDone() { // the runtime is gone and so is its connection
				return
			}
			time.Sleep(200 * time.Microsecond)
			continue
		}
		if !h.Conn.Idle() {
			time.Sleep(200 * time.Microsecond)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), waitTimeout)
		berr := r.Barrier(ctx)
		var v conversation.View
		var verr error
		if berr == nil {
			v, verr = r.View(ctx)
		}
		cancel()
		if berr != nil || verr != nil { // the runtime stopped: nothing more will be handled
			h.drainRuntime()
			return
		}
		if v.Handled == last && v.OutboundDrained {
			stable++
		} else {
			stable = 0
		}
		last = v.Handled
		if stable >= 2 {
			return
		}
		time.Sleep(200 * time.Microsecond)
	}
}

// attachDone reports that the Attach of the current connection has returned.
func (h *H) attachDone() bool {
	if h.cur == nil {
		return false
	}
	select {
	case <-h.cur.done:
		return true
	default:
		return false
	}
}

// drainRuntime waits for a stopped runtime to finish its exit.
func (h *H) drainRuntime() {
	if r, ok := h.Runtime(); ok {
		select {
		case <-r.Done():
		case <-time.After(waitTimeout):
			h.T.Fatal("rtharness: runtime did not stop")
		}
	}
}

// WaitFor polls cond (real time, 1 ms) until it holds; the test fails after 5 s.
func (h *H) WaitFor(what string, cond func() bool) {
	h.T.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			h.T.Fatalf("rtharness: timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// Events returns the server events written to the connection, in order, of the given types (all
// when none is given).
func (h *H) Events(types ...string) []protocol.ServerEvent {
	h.T.Helper()
	var out []protocol.ServerEvent
	for _, w := range h.Conn.Written() {
		if w.Kind != conversation.MessageText {
			continue
		}
		ev, err := protocol.DecodeServerEvent(w.Data)
		if err != nil {
			h.T.Fatalf("rtharness: decode server event: %v (%s)", err, w.Data)
		}
		if len(types) == 0 || contains(types, ev.Type) {
			out = append(out, ev)
		}
	}
	return out
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// WaitEvents waits until at least n events of typ were written and returns them.
func (h *H) WaitEvents(typ string, n int) []protocol.ServerEvent {
	h.T.Helper()
	var got []protocol.ServerEvent
	h.WaitFor(typ+" events", func() bool {
		got = h.Events(typ)
		return len(got) >= n
	})
	return got
}

// Types returns the types of the events written so far, in order.
func (h *H) Types() []string {
	h.T.Helper()
	var out []string
	for _, ev := range h.Events() {
		out = append(out, ev.Type)
	}
	return out
}

// Frame is one downstream audio frame the server wrote.
type Frame struct {
	Gen, Seq uint32
	Opus     []byte
}

// Frames returns the binary frames written so far, in order.
func (h *H) Frames() []Frame {
	h.T.Helper()
	var out []Frame
	for _, w := range h.Conn.Written() {
		if w.Kind != conversation.MessageBinary {
			continue
		}
		gen, seq, opus, err := protocol.DecodeDownstream(w.Data)
		if err != nil {
			h.T.Fatalf("rtharness: decode downstream frame: %v", err)
		}
		out = append(out, Frame{Gen: gen, Seq: seq, Opus: opus})
	}
	return out
}

// WaitClosed waits until the server closed the connection and returns the code.
func (h *H) WaitClosed() (int, string) {
	h.T.Helper()
	select {
	case <-h.Conn.Closed():
	case <-time.After(waitTimeout):
		h.T.Fatal("rtharness: the server did not close the connection")
	}
	code, reason, _ := h.Conn.CloseInfo()
	return code, reason
}

// Close stops the Manager and every runtime and waits for the attach to return. It runs at the
// end of the test; calling it earlier is fine.
func (h *H) Close() {
	h.once.Do(func() {
		h.Mgr.Close()
		h.cancel()
		for _, run := range h.runs {
			select {
			case <-run.done:
			case <-time.After(waitTimeout):
				h.T.Error("rtharness: Attach did not return after Close")
			}
		}
	})
}

// Blocked is how many loads are waiting on a gate right now.
func (h *H) Blocked() int { return int(h.blocked.Load()) }

// ---- fakes with gates ----

// wait blocks on gate (nil: no gate) until it is closed or ctx ends.
func (h *H) wait(ctx context.Context, gate chan struct{}) error {
	if gate == nil {
		return nil
	}
	h.blocked.Add(1)
	defer h.blocked.Add(-1)
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

type learnerGate struct {
	*fakes.LearnerContext
	h *H
}

func (l learnerGate) PromptContext(ctx context.Context, userID uuid.UUID, ad learner.AdaptiveDifficulty) (prompt.LearnerContext, error) {
	if l.h.OnLearner != nil {
		l.h.OnLearner()
	}
	if err := l.h.wait(ctx, l.h.Gates.Learner); err != nil {
		return prompt.LearnerContext{}, err
	}
	return l.LearnerContext.PromptContext(ctx, userID, ad)
}

type memoryGate struct {
	*fakes.MemoryRetrieval
	h *H
}

func (m memoryGate) BuildSessionContext(ctx context.Context, in memory.SessionContextInput) (memory.MemoryContext, error) {
	if err := m.h.wait(ctx, m.h.Gates.Memory); err != nil {
		return memory.MemoryContext{}, err
	}
	return m.MemoryRetrieval.BuildSessionContext(ctx, in)
}

func (m memoryGate) RefreshEpisodic(ctx context.Context, userID, sessionID uuid.UUID, recent []memory.TurnText) ([]memory.ContextItem, error) {
	if err := m.h.wait(ctx, m.h.Gates.Refresh); err != nil {
		return nil, err
	}
	return m.MemoryRetrieval.RefreshEpisodic(ctx, userID, sessionID, recent)
}

// ContentLoader is the content.SessionConfigLoader and prompt-version source of the harness: the
// seeded topic, scenario and prompt versions, never a PUBLISHED lookup and never a characters row
// (the character comes from the session snapshot, D-014).
type ContentLoader struct {
	h *H

	mu       sync.Mutex
	loads    int
	openings []uuid.UUID
}

func newContentLoader(h *H) *ContentLoader { return &ContentLoader{h: h} }

// Loads is how many times the scenario/prompt versions were loaded (the ConfigLoader caches).
func (c *ContentLoader) Loads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loads
}

// OpeningLoads returns the ids GetPromptVersion was asked for.
func (c *ContentLoader) OpeningLoads() []uuid.UUID {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]uuid.UUID(nil), c.openings...)
}

// Load implements content.SessionConfigLoader.
func (c *ContentLoader) Load(ctx context.Context, refs content.SessionRefs) (content.SessionConfig, error) {
	if err := c.h.wait(ctx, c.h.Gates.Config); err != nil {
		return content.SessionConfig{}, err
	}
	c.mu.Lock()
	c.loads++
	c.mu.Unlock()
	snap, err := content.ParseSnapshot(refs.ConfigSnapshot)
	if err != nil {
		return content.SessionConfig{}, err
	}
	return content.SessionConfig{
		Topic: content.Topic{ID: refs.TopicID, Code: "TRAVEL", Name: "Travel and holidays", NameJA: "旅行", Description: "旅行の会話"},
		ScenarioVersion: content.ScenarioVersion{
			ID: refs.ScenarioVersionID, ScenarioID: scenarioID, Version: 1, DifficultyLevel: 3,
			Situation: "駅で切符を買います。", AIRole: "駅員", UserRole: "旅行者", Goal: "切符を買う",
			Formality: content.FormalityPolite, OpeningLine: OpeningLine,
		},
		Snapshot: snap,
		PromptVersion: content.PromptVersion{
			ID: refs.PromptVersionID, Key: content.PromptConversation, Version: 1, SystemTemplate: prompt.ConversationV1.System,
		},
	}, nil
}

// GetPromptVersion implements the prompt source of conversation.NewConfigLoader: the OPENING
// version by id.
func (c *ContentLoader) GetPromptVersion(_ context.Context, id uuid.UUID) (content.PromptVersion, error) {
	c.mu.Lock()
	c.openings = append(c.openings, id)
	c.mu.Unlock()
	return content.PromptVersion{ID: id, Key: content.PromptOpening, Version: 1, SystemTemplate: prompt.OpeningV1.System}, nil
}

// Router is the provider.RealtimeRouter of the harness: the ScriptedRouter for LLM and TTS plus an
// OpenSTT that plays the timeline of ScriptedSTT.
type Router struct {
	*fakes.ScriptedRouter
	h        *H
	timeline replay.Timeline
	openErr  error
	wrap     func(provider.Generation) provider.Generation

	mu       sync.Mutex
	sessions []*replay.ScriptedSTT
	configs  []provider.STTSessionConfig
	summary  string
	texts    []provider.TextRequest
}

var _ provider.RealtimeRouter = (*Router)(nil)

// OpenSTT implements provider.RealtimeRouter.
func (r *Router) OpenSTT(ctx context.Context, cfg provider.STTSessionConfig) (provider.RoutedSTTSession, error) {
	if err := r.h.wait(ctx, r.h.Gates.STT); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.configs = append(r.configs, cfg)
	if r.openErr != nil {
		return nil, r.openErr
	}
	s := replay.NewScriptedSTT(r.timeline)
	r.sessions = append(r.sessions, s)
	return s, nil
}

// NewGeneration implements provider.RealtimeRouter (wrapped by WithGenerationWrapper).
func (r *Router) NewGeneration(sid uuid.UUID, gen int64, voice provider.VoiceSelection) provider.Generation {
	g := r.ScriptedRouter.NewGeneration(sid, gen, voice)
	if r.wrap != nil {
		return r.wrap(g)
	}
	return g
}

// CompleteText implements provider.RealtimeRouter for the rolling summary: it answers the text set
// by SetSummary (an error while none is set) and records the request.
func (r *Router) CompleteText(_ context.Context, req provider.TextRequest) (provider.TextResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.texts = append(r.texts, req)
	if r.summary == "" {
		return provider.TextResult{}, errors.New("rtharness: no summary scripted")
	}
	return provider.TextResult{Text: r.summary, Provider: provider.FakePrimary}, nil
}

// SetSummary sets the text CompleteText returns.
func (r *Router) SetSummary(text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.summary = text
}

// TextRequests returns the CompleteText requests so far.
func (r *Router) TextRequests() []provider.TextRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]provider.TextRequest(nil), r.texts...)
}

// SetOpenError changes the error OpenSTT returns (nil: succeed).
func (r *Router) SetOpenError(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.openErr = err
}

// STT returns the most recently opened STT stream; the test fails when none was opened.
func (r *Router) STT() *replay.ScriptedSTT {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.sessions) == 0 {
		r.h.T.Helper()
		r.h.T.Fatal("rtharness: no STT stream was opened")
	}
	return r.sessions[len(r.sessions)-1]
}

// STTOpens is how many STT streams were opened (failed attempts excluded).
func (r *Router) STTOpens() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.sessions)
}

// STTConfigs returns the configs OpenSTT was called with.
func (r *Router) STTConfigs() []provider.STTSessionConfig {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]provider.STTSessionConfig(nil), r.configs...)
}
