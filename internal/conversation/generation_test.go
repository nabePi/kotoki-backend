package conversation_test

import (
	"context"
	"encoding/json"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/conversation/audio"
	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

const (
	convPkg  = "github.com/nabePi/kotoki-backend/internal/conversation"
	fakesPkg = "github.com/nabePi/kotoki-backend/internal/testutil/fakes"
)

var (
	genT0   = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	testSID = uuid.MustParse("0190f5a0-0000-7000-8000-000000000001")
	testTID = uuid.MustParse("0190f5a0-0000-7000-8000-0000000000aa")

	// threeSentences splits 3 sentences across deltas; the last delta is the third sentence.
	threeSentences = []string{"こんにちは。", "今日はいい天気", "ですね。散歩に", "行きましょう！"}

	primaryOnly = provider.VoiceSelection{Primary: provider.VoiceRef{Provider: provider.Cartesia, ModelName: "sonic3.6", VoiceID: "v1"}}
	withBackup  = provider.VoiceSelection{
		Primary:  provider.VoiceRef{Provider: provider.Cartesia, ModelName: "sonic3.6", VoiceID: "v1"},
		Fallback: &provider.VoiceRef{Provider: provider.ElevenLabs, ModelName: "Eleven v3 Conversational", VoiceID: "v2"},
	}
)

func noLeak(t *testing.T) {
	t.Helper()
	testutil.NoGoroutineLeak(t, convPkg)
	testutil.NoGoroutineLeak(t, fakesPkg)
}

// recConn is a Conn that records the binary frames the writer sends.
type recConn struct {
	mu     sync.Mutex
	frames [][]byte
}

func (c *recConn) Read(ctx context.Context) (conversation.MessageKind, []byte, error) {
	<-ctx.Done()
	return 0, nil, ctx.Err()
}

func (c *recConn) Write(_ context.Context, kind conversation.MessageKind, p []byte) error {
	if kind == conversation.MessageBinary {
		c.mu.Lock()
		c.frames = append(c.frames, append([]byte(nil), p...))
		c.mu.Unlock()
	}
	return nil
}

func (c *recConn) Ping(context.Context) error { return nil }
func (c *recConn) Close(int, string) error    { return nil }

func (c *recConn) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.frames)
}

type sentFrame struct{ gen, seq uint32 }

func (c *recConn) sent(t *testing.T) []sentFrame {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]sentFrame, 0, len(c.frames))
	for _, b := range c.frames {
		gen, seq, _, err := protocol.DecodeDownstream(b)
		require.NoError(t, err)
		out = append(out, sentFrame{gen, seq})
	}
	return out
}

// genHarness wires one generation to a real Outbound and Writer on a fake clock.
type genHarness struct {
	t      *testing.T
	clk    *clock.Fake
	router *fakes.ScriptedRouter
	out    *conversation.Outbound
	conn   *recConn
	m      *conversation.Metrics
	events chan conversation.GenEvent
	acks   chan conversation.WriterAck
	active atomic.Uint32
	ctx    context.Context
	cancel context.CancelFunc
	sleep  func(context.Context, time.Duration) error

	writerOnce sync.Once
	writerDone chan struct{}
	gens       []*conversation.Generation
}

func newGenHarness(t *testing.T, s fakes.Script) *genHarness {
	t.Helper()
	h := &genHarness{
		t: t, clk: clock.NewFake(genT0), router: fakes.NewScriptedRouter(s),
		out: conversation.NewOutbound(256, nil), conn: &recConn{}, m: conversation.NewMetrics(nil),
		events: make(chan conversation.GenEvent, 128), acks: make(chan conversation.WriterAck, 64),
		writerDone: make(chan struct{}),
	}
	h.ctx, h.cancel = context.WithCancel(context.Background())
	h.sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		h.clk.Advance(d)
		runtime.Gosched() // let the writer drain
		return nil
	}
	t.Cleanup(func() {
		h.cancel()
		for _, g := range h.gens {
			waitGen(t, g)
		}
		h.writerOnce.Do(func() { close(h.writerDone) })
		<-h.writerDone
	})
	return h
}

func (h *genHarness) params(kind conversation.GenKind, n uint32) conversation.GenParams {
	return conversation.GenParams{
		Number: n, Kind: kind, TurnID: testTID, SessionID: testSID, Parent: h.ctx,
		Router: h.router, Voice: primaryOnly, VoiceSpeakingRate: 1.0, AdaptiveRate: 1.0, MaxSentences: 10,
		LLM: provider.LLMRequest{Messages: []provider.Message{{Role: provider.RoleUser, Content: "やあ"}}, MaxTokens: 320, Temperature: 0.8},
		Out: h.out, Events: h.events, Clock: h.clk, Sleep: h.sleep, MaxLeadMS: 400, Metrics: h.m,
	}
}

func (h *genHarness) newGen(p conversation.GenParams) *conversation.Generation {
	g := conversation.NewGenerationForTest(p)
	h.active.Store(p.Number)
	h.gens = append(h.gens, g)
	return g
}

// startWriter runs the real Writer (activeGen = the last generation created). The writer has no
// metrics so h.m only counts what the generation does.
func (h *genHarness) startWriter() {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = conversation.NewWriter(time.Second, nil).Run(h.ctx, h.conn, h.out, &h.active, h.acks)
	}()
	h.writerOnce.Do(func() {
		go func() { <-done; close(h.writerDone) }()
	})
}

// collect gathers g's events (and the EvAudioDone made from writer acks) until a terminal
// event: EvAudioDone or EvGenFailed.
func (h *genHarness) collect(g *conversation.Generation) []conversation.GenEvent {
	h.t.Helper()
	var evs []conversation.GenEvent
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev := <-h.events:
			evs = append(evs, ev)
			if _, ok := ev.(conversation.EvGenFailed); ok {
				return evs
			}
		case a := <-h.acks:
			if ev, ok := g.OnWriterAck(a); ok {
				return append(evs, ev)
			}
		case <-timeout:
			h.t.Fatalf("no terminal generation event; got %#v", evs)
			return nil
		}
	}
}

func waitGen(t *testing.T, g *conversation.Generation) {
	t.Helper()
	done := make(chan struct{})
	go func() { g.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatalf("generation %d goroutines did not exit within 1 s", g.Number)
	}
}

func eventsOf[T conversation.GenEvent](evs []conversation.GenEvent) []T {
	var out []T
	for _, ev := range evs {
		if v, ok := ev.(T); ok {
			out = append(out, v)
		}
	}
	return out
}

func last(evs []conversation.GenEvent) conversation.GenEvent { return evs[len(evs)-1] }

func sentenceTexts(evs []conversation.GenEvent) []string {
	var out []string
	for _, s := range eventsOf[conversation.EvSentence](evs) {
		out = append(out, s.Sentence.Text)
	}
	return out
}

func cleanText(evs []conversation.GenEvent) string {
	var b strings.Builder
	for _, d := range eventsOf[conversation.EvTextDelta](evs) {
		b.WriteString(d.Clean)
	}
	return b.String()
}

func ttsIndexes(sg *fakes.ScriptedGeneration) []int {
	var out []int
	for _, c := range sg.TTSCalls() {
		out = append(out, c.Index)
	}
	return out
}

func requireContiguous(t *testing.T, frames []sentFrame, gen uint32, n int) {
	t.Helper()
	require.Len(t, frames, n)
	for i, f := range frames {
		require.Equal(t, gen, f.gen, "frame %d", i)
		require.Equal(t, uint32(i), f.seq, "frame %d", i)
	}
}

func eventually(t *testing.T, cond func() bool, what string) {
	t.Helper()
	require.Eventually(t, cond, 2*time.Second, time.Millisecond, what)
}

func TestPipeliningOrder(t *testing.T) {
	gate := make(chan struct{})
	h := newGenHarness(t, fakes.Script{Deltas: threeSentences, Gate: gate})
	h.startWriter()
	g := h.newGen(h.params(conversation.GenReply, 1))
	g.StartLLM()

	eventually(t, func() bool {
		gs := h.router.Generations()
		return len(gs) == 1 && slices.Contains(gs[0].Log(), "tts:1:e0")
	}, "sentence 1 synthesised")
	sg := h.router.Generations()[0]
	log := sg.Log()
	require.NotContains(t, log, "llm:delta:3", "the last delta is still gated")
	require.NotContains(t, log, "llm:done:STOP")

	close(gate)
	evs := h.collect(g)
	done, ok := last(evs).(conversation.EvAudioDone)
	require.True(t, ok, "%#v", last(evs))
	require.True(t, done.LLMFinished)
	require.Equal(t, uint32(1), done.Gen)
	require.Equal(t, 15, done.FrameCount)
	require.Equal(t, int64(300), done.TotalMS)

	require.Equal(t, []int{0, 1, 2}, ttsIndexes(sg))
	log = sg.Log()
	require.Less(t, slices.Index(log, "tts:1:e0"), slices.Index(log, "llm:delta:3"), "sentence 1 starts before the LLM finished")
	require.Equal(t, []string{"こんにちは。", "今日はいい天気ですね。", "散歩に行きましょう！"}, sentenceTexts(evs))

	var idx []int
	var seqs []uint32
	for _, s := range eventsOf[conversation.EvSentenceAudio](evs) {
		idx = append(idx, s.Index)
		seqs = append(seqs, s.FirstFrameSeq)
	}
	require.Equal(t, []int{0, 1, 2}, idx)
	require.Equal(t, []uint32{0, 5, 10}, seqs)
	require.Len(t, eventsOf[conversation.EvFirstAudio](evs), 1)
	requireContiguous(t, h.conn.sent(t), 1, 15)

	req := sg.LLMRequests()
	require.Len(t, req, 1)
	require.Equal(t, testSID.String()+":1", req[0].RequestID)
	require.Equal(t, 320, req[0].MaxTokens)
	calls := sg.TTSCalls()
	require.Equal(t, testSID.String()+":1:2", calls[2].Req.RequestID)
	require.Equal(t, "ja", calls[2].Req.Language)
	require.Equal(t, "今日はいい天気ですね。", calls[2].Req.PreviousText)
	require.Empty(t, calls[0].Req.PreviousText)
}

func TestSentenceLimit(t *testing.T) {
	deltas := []string{"一つ目です。", "二つ目です。", "三つ目です。", "四つ目です。", "五つ目です。", "六つ目です。"}
	h := newGenHarness(t, fakes.Script{Deltas: deltas})
	h.startWriter()
	p := h.params(conversation.GenReply, 1)
	p.MaxSentences = 2
	g := h.newGen(p)
	g.StartLLM()
	evs := h.collect(g)

	done, ok := last(evs).(conversation.EvAudioDone)
	require.True(t, ok, "%#v", last(evs))
	require.True(t, done.LLMFinished, "R-17: the limit counts as a normal LLM end")
	require.Equal(t, []string{"一つ目です。", "二つ目です。"}, sentenceTexts(evs))
	require.Equal(t, "一つ目です。二つ目です。", cleanText(evs), "text after the limit never reaches ai.text_delta")
	sg := h.router.Generations()[0]
	require.True(t, sg.LLMCancelled(), "llmCancel cuts the LLM stream")
	require.Equal(t, []int{0, 1}, ttsIndexes(sg))
	require.InDelta(t, 1.0, h.m.SentenceLimitTotal("reply"), 0)
	require.Zero(t, h.m.SentenceLimitTotal("opening"))
}

func TestTextDeltaSanitized(t *testing.T) {
	h := newGenHarness(t, fakes.Script{Deltas: []string{"**は", "い**"}})
	h.startWriter()
	g := h.newGen(h.params(conversation.GenReply, 1))
	g.StartLLM()
	evs := h.collect(g)
	_, ok := last(evs).(conversation.EvAudioDone)
	require.True(t, ok, "%#v", last(evs))
	require.Equal(t, "はい", cleanText(evs))
	for _, d := range eventsOf[conversation.EvTextDelta](evs) {
		require.NotContains(t, d.Clean, "*")
	}
	calls := h.router.Generations()[0].TTSCalls()
	require.Len(t, calls, 1)
	require.Equal(t, "はい", calls[0].Req.Text)
	for _, c := range calls {
		require.False(t, strings.ContainsAny(c.Req.Text, "*#`_"), c.Req.Text)
	}
	// D-014: no expression anywhere in the sentence path
	for _, typ := range []reflect.Type{reflect.TypeFor[conversation.EvSentence](), reflect.TypeFor[prompt.Sentence](),
		reflect.TypeFor[provider.TTSRequest](), reflect.TypeFor[conversation.EvSentenceAudio]()} {
		for i := range typ.NumField() {
			require.NotContains(t, strings.ToLower(typ.Field(i).Name), "expression", typ.String())
		}
	}
}

func TestSpeakingRateFormula(t *testing.T) {
	cases := []struct{ voice, adaptive, want float64 }{
		{1.0, 1.05, 1.05},
		{1.2, 1.05, 1.05}, // clamp up
		{0.9, 0.80, 0.80}, // clamp down
		{1.0, 0, 1.0},     // adaptive 0 → 1.0
		{1.0, 0.95, 0.95},
	}
	for _, c := range cases {
		h := newGenHarness(t, fakes.Script{Deltas: []string{"はい。"}})
		p := h.params(conversation.GenReply, 1)
		p.VoiceSpeakingRate, p.AdaptiveRate = c.voice, c.adaptive
		g := h.newGen(p)
		require.InDelta(t, c.want, g.SpeakingRateForTest(), 1e-9, "voice %v × adaptive %v", c.voice, c.adaptive)
	}

	// computed once per generation: every sentence carries the same rate
	h := newGenHarness(t, fakes.Script{Deltas: threeSentences})
	h.startWriter()
	p := h.params(conversation.GenReply, 1)
	p.VoiceSpeakingRate, p.AdaptiveRate = 1.1, 0.9
	g := h.newGen(p)
	g.StartLLM()
	_, ok := last(h.collect(g)).(conversation.EvAudioDone)
	require.True(t, ok)
	calls := h.router.Generations()[0].TTSCalls()
	require.Len(t, calls, 3)
	for _, c := range calls {
		require.InDelta(t, 0.99, c.Req.SpeakingRate, 1e-9)
	}
}

func TestVoiceSelectionFromSnapshot(t *testing.T) {
	snap := content.CharacterSnapshot{
		ProviderName: "CARTESIA", ModelName: "sonic3.6", VoiceID: "db6b0ed5-1d4b-4a37-9a8b-0f3c2a4f9e11",
		Voice: content.Voice{SpeakingRate: 1.0, Fallback: &content.TTSBinding{
			ProviderName: "ELEVENLABS", ModelName: "Eleven v3 Conversational", VoiceID: "21m00Tcm4TlvDq8ikWAM"}},
	}
	sel := conversation.VoiceSelectionFor(snap)
	require.Equal(t, provider.VoiceRef{Provider: "CARTESIA", ModelName: "sonic3.6", VoiceID: "db6b0ed5-1d4b-4a37-9a8b-0f3c2a4f9e11"}, sel.Primary)
	require.NotNil(t, sel.Fallback)
	require.Equal(t, provider.VoiceRef{Provider: "ELEVENLABS", ModelName: "Eleven v3 Conversational", VoiceID: "21m00Tcm4TlvDq8ikWAM"}, *sel.Fallback)

	noFallback := snap
	noFallback.Voice.Fallback = nil
	require.Nil(t, conversation.VoiceSelectionFor(noFallback).Fallback)

	// the generation passes the selection as is; VoiceID/Model are the router's business
	h := newGenHarness(t, fakes.Script{Deltas: []string{"はい、", "そうです。"}})
	h.startWriter()
	p := h.params(conversation.GenReply, 4)
	p.Voice = sel
	g := h.newGen(p)
	g.StartLLM()
	_, ok := last(h.collect(g)).(conversation.EvAudioDone)
	require.True(t, ok)
	gs := h.router.Generations()
	require.Len(t, gs, 1)
	require.Equal(t, sel, gs[0].Voice)
	require.Equal(t, int64(4), gs[0].Gen)
	require.Equal(t, testSID, gs[0].SessionID)
	for _, c := range gs[0].TTSCalls() {
		require.Equal(t, sel, c.Req.Voice)
		require.Empty(t, c.Req.VoiceID)
		require.Empty(t, c.Req.Model)
	}
}

func TestNoTTSFallbackFailsToApology(t *testing.T) {
	h := newGenHarness(t, fakes.Script{Deltas: threeSentences,
		TTSErr: provider.NewError(provider.Cartesia, provider.CapTTS, provider.KindUnavailable, "boom"), TTSErrSentence: 0})
	h.startWriter()
	g := h.newGen(h.params(conversation.GenReply, 1)) // primaryOnly: Fallback == nil
	g.StartLLM()
	evs := h.collect(g)
	require.Equal(t, conversation.EvGenFailed{Gen: 1, Cause: conversation.CauseProvider}, last(evs))
	require.Empty(t, eventsOf[conversation.EvFirstAudio](evs))
	waitGen(t, g)
	sg := h.router.Generations()[0]
	require.Zero(t, sg.Epoch(), "no epoch switch without voice.fallback")
	require.Equal(t, []int{0}, ttsIndexes(sg), "no retry, no other vendor")
	require.Zero(t, h.conn.count())
}

func TestLLMErrorFailsGeneration(t *testing.T) {
	h := newGenHarness(t, fakes.Script{Deltas: threeSentences, LLMErrAfter: 2})
	h.startWriter()
	g := h.newGen(h.params(conversation.GenReply, 1))
	g.StartLLM()
	evs := h.collect(g)
	require.Equal(t, conversation.EvGenFailed{Gen: 1, Cause: conversation.CauseProvider}, last(evs))
	waitGen(t, g) // the pipeline winds down by itself
	for _, ev := range evs {
		switch ev.(type) {
		case conversation.EvTextDelta, conversation.EvSentence, conversation.EvFirstAudio, conversation.EvSentenceAudio, conversation.EvGenFailed:
		default:
			t.Fatalf("unexpected event %#v (no reset / vendor switch, D-017)", ev)
		}
	}
	require.Len(t, h.router.Generations(), 1)
	require.Len(t, h.router.Generations()[0].LLMRequests(), 1)
	// nothing after the failure: no second failure, no AUDIO_DONE
	drainUntilQuiet(t, h, g)
}

// drainUntilQuiet reads events and acks for a short while and fails on a terminal event.
func drainUntilQuiet(t *testing.T, h *genHarness, g *conversation.Generation) {
	t.Helper()
	quiet := time.After(100 * time.Millisecond)
	for {
		select {
		case ev := <-h.events:
			switch ev.(type) {
			case conversation.EvGenFailed, conversation.EvAudioDone:
				t.Fatalf("terminal event after the end: %#v", ev)
			}
		case a := <-h.acks:
			if ev, ok := g.OnWriterAck(a); ok {
				t.Fatalf("AUDIO_DONE after the end: %#v", ev)
			}
		case <-quiet:
			return
		}
	}
}

func TestTTSEpochRestart(t *testing.T) {
	h := newGenHarness(t, fakes.Script{Deltas: threeSentences, TTSEpochAfterSentence: 1})
	const gen = 3
	// hold the pacer: 25 queued frames of another generation (network backpressure) and a
	// sleep that waits for release; it then drops them so the pacer can go on
	for i := range 25 {
		h.out.EnqueueAudio(audio.OutFrame{Gen: 99, Seq: uint32(i)})
	}
	release := make(chan struct{})
	held := make(chan struct{}, 1)
	p := h.params(conversation.GenReply, gen)
	p.Voice = withBackup
	p.Sleep = func(ctx context.Context, d time.Duration) error {
		select {
		case held <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		h.out.PurgeAudio(gen)
		return h.sleep(ctx, d)
	}
	g := h.newGen(p)
	g.StartLLM()

	<-held
	eventually(t, func() bool {
		gs := h.router.Generations()
		return len(gs) == 1 && slices.Contains(gs[0].Log(), "tts:2:e1")
	}, "all sentences resynthesised in the new epoch")
	close(release)
	h.startWriter()
	evs := h.collect(g)
	done, ok := last(evs).(conversation.EvAudioDone)
	require.True(t, ok, "%#v", last(evs))
	require.True(t, done.LLMFinished)

	sg := h.router.Generations()[0]
	var calls [][2]int
	for _, c := range sg.TTSCalls() {
		calls = append(calls, [2]int{c.Index, c.Epoch})
	}
	require.Equal(t, [][2]int{{0, 0}, {1, 0}, {2, 0}, {0, 1}, {1, 1}, {2, 1}}, calls, "every sentence again from 0")
	// only new-epoch audio, numbered from 0; the stale chunk at the start of the new epoch is ignored
	requireContiguous(t, h.conn.sent(t), gen, 15)
	require.InDelta(t, 10, h.m.AudioFramesOutTotal("dropped_stale"), 0, "old-epoch frames of sentences 0 and 1 dropped")
	var idx []int
	for _, s := range eventsOf[conversation.EvSentenceAudio](evs) {
		idx = append(idx, s.Index)
	}
	require.Equal(t, []int{0, 1, 2}, idx)
	require.Len(t, eventsOf[conversation.EvSentence](evs), 3, "sentences are parsed once")
	require.Equal(t, 1, sg.MarkAudioSentCalls())
}

func TestTruncated(t *testing.T) {
	h := newGenHarness(t, fakes.Script{Deltas: []string{"はい。", "そうです。", "それから"}, Finish: provider.FinishTruncated})
	h.startWriter()
	g := h.newGen(h.params(conversation.GenReply, 1))
	g.StartLLM()
	evs := h.collect(g)
	done, ok := last(evs).(conversation.EvAudioDone)
	require.True(t, ok, "%#v", last(evs))
	require.False(t, done.LLMFinished)
	require.Equal(t, []string{"はい。", "そうです。"}, sentenceTexts(evs), "the parser is not closed: the fragment is not spoken")
	require.Equal(t, "はい。そうです。それから", cleanText(evs), "the fragment stays in the text")
	require.Equal(t, []int{0, 1}, ttsIndexes(h.router.Generations()[0]))
	require.InDelta(t, 1.0, h.m.LLMTruncatedTotal(), 0)
}

func TestContentFiltered(t *testing.T) {
	for name, s := range map[string]fakes.Script{
		"done":  {Deltas: []string{"それは", "ちょっと"}, Finish: provider.FinishContentFiltered},
		"error": {Deltas: []string{"それは", "ちょっと"}, LLMErrAfter: 1, LLMErr: provider.NewError(provider.OpenAI, provider.CapLLM, provider.KindContentFiltered, "blocked")},
	} {
		t.Run(name, func(t *testing.T) {
			h := newGenHarness(t, s)
			h.startWriter()
			g := h.newGen(h.params(conversation.GenReply, 2))
			g.StartLLM()
			evs := h.collect(g)
			require.Equal(t, conversation.EvGenFailed{Gen: 2, Cause: conversation.CauseContentFiltered}, last(evs))
			waitGen(t, g)
		})
	}
}

func TestStaticOpening(t *testing.T) {
	h := newGenHarness(t, fakes.Script{})
	h.startWriter()
	p := h.params(conversation.GenOpening, 1)
	p.MaxSentences = 1 // the static opening is not limited (S-2)
	g := h.newGen(p)
	g.StartStatic("こんにちは。今日は何をしますか？")
	evs := h.collect(g)
	done, ok := last(evs).(conversation.EvAudioDone)
	require.True(t, ok, "%#v", last(evs))
	require.True(t, done.LLMFinished)
	gs := h.router.Generations()
	require.Len(t, gs, 1)
	require.Empty(t, gs[0].LLMRequests(), "no StreamLLM call")
	require.Equal(t, []int{0, 1}, ttsIndexes(gs[0]))
	require.Equal(t, []string{"こんにちは。", "今日は何をしますか？"}, sentenceTexts(evs))
	require.Equal(t, "こんにちは。今日は何をしますか？", cleanText(evs))
	require.Zero(t, h.m.SentenceLimitTotal("opening"))

	// ai.sentence built from the events carries no expression (D-014)
	sa := eventsOf[conversation.EvSentenceAudio](evs)[1]
	b, err := json.Marshal(protocol.AISentence{SentenceIndex: sa.Index, Text: sentenceTexts(evs)[1], FirstFrameSeq: sa.FirstFrameSeq, StartMS: int(sa.FirstFrameSeq) * 20})
	require.NoError(t, err)
	require.NotContains(t, string(b), "expression")
}

func TestApologyAsset(t *testing.T) {
	h := newGenHarness(t, fakes.Script{})
	p := h.params(conversation.GenApology, 7)
	p.Router = nil // APOLOGY never touches LLM/TTS
	g := h.newGen(p)
	// stand-in for the writer: take every item up to the end marker of generation 7
	got := make(chan []audio.OutFrame, 1)
	go func() {
		var frames []audio.OutFrame
		for {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			it, ok := h.out.Next(ctx)
			cancel()
			switch {
			case !ok:
				got <- nil
				return
			case it.GenEnd && it.Frame.Gen == 7:
				got <- frames
				return
			}
			frames = append(frames, it.Frame)
		}
	}()
	g.StartAsset()
	frames := <-got
	waitGen(t, g)

	want, err := audio.ApologyFrames()
	require.NoError(t, err)
	require.Len(t, frames, len(want))
	for i, f := range frames {
		require.Equal(t, uint32(7), f.Gen)
		require.Equal(t, uint32(i), f.Seq)
		require.Zero(t, f.SentenceIdx)
		require.Equal(t, want[i], f.Opus)
		require.Equal(t, i == len(frames)-1, f.LastOfSentence, "frame %d", i)
	}
	evs := drainEvents(h)
	require.Equal(t, []string{audio.ApologyText}, sentenceTexts(evs))
	require.Len(t, eventsOf[conversation.EvFirstAudio](evs), 1)
	ev, ok := g.OnWriterAck(conversation.WriterAck{Kind: conversation.AckAudioWrittenAll, Gen: 7, Sentences: 1})
	require.True(t, ok)
	require.Equal(t, conversation.EvAudioDone{Gen: 7, LLMFinished: true, FrameCount: len(want), TotalMS: int64(len(want)) * 20, SentenceCount: 1}, ev)
}

func drainEvents(h *genHarness) []conversation.GenEvent {
	var evs []conversation.GenEvent
	for {
		select {
		case ev := <-h.events:
			evs = append(evs, ev)
		default:
			return evs
		}
	}
}

func TestMarkAudioSentBeforeFirstFrame(t *testing.T) {
	h := newGenHarness(t, fakes.Script{Deltas: threeSentences})
	var queuedAtMark, writtenAtMark atomic.Int64
	queuedAtMark.Store(-1)
	h.router.SetOnMarkAudioSent(func() {
		queuedAtMark.Store(int64(h.out.AudioLen()))
		writtenAtMark.Store(int64(h.conn.count()))
	})
	h.startWriter()
	g := h.newGen(h.params(conversation.GenReply, 1))
	g.StartLLM()
	evs := h.collect(g)
	_, ok := last(evs).(conversation.EvAudioDone)
	require.True(t, ok)
	require.Equal(t, 1, h.router.Generations()[0].MarkAudioSentCalls())
	require.Zero(t, queuedAtMark.Load(), "MarkAudioSent runs before the first frame is queued (S-12)")
	require.Zero(t, writtenAtMark.Load())
	require.Positive(t, h.conn.count())
	first := slices.IndexFunc(evs, func(ev conversation.GenEvent) bool { _, ok := ev.(conversation.EvFirstAudio); return ok })
	sa := slices.IndexFunc(evs, func(ev conversation.GenEvent) bool { _, ok := ev.(conversation.EvSentenceAudio); return ok })
	require.GreaterOrEqual(t, first, 0)
	require.Less(t, first, sa, "FIRST_AUDIO precedes the first ai.sentence")
}

func TestCancelNoLeak(t *testing.T) {
	noLeak(t)
	gate := make(chan struct{}) // never released: the LLM stays mid-stream
	h := newGenHarness(t, fakes.Script{Deltas: threeSentences, Gate: gate})
	h.startWriter()
	g := h.newGen(h.params(conversation.GenReply, 1))
	g.StartLLM()
	eventually(t, func() bool { return h.conn.count() > 0 }, "audio flowing")
	start := time.Now()
	g.Cancel()
	waitGen(t, g)
	require.Less(t, time.Since(start), time.Second)
	require.True(t, h.router.Generations()[0].LLMCancelled())
	drainUntilQuiet(t, h, g)

	// cancelled before anything started: every stage still exits
	h2 := newGenHarness(t, fakes.Script{Deltas: threeSentences})
	g2 := h2.newGen(h2.params(conversation.GenReply, 1))
	g2.Cancel()
	g2.StartLLM()
	waitGen(t, g2)
	g3 := h2.newGen(h2.params(conversation.GenApology, 2))
	g3.Cancel()
	g3.StartAsset()
	waitGen(t, g3)
}

func TestSilentGenerationFails(t *testing.T) {
	cases := map[string]struct {
		kind  conversation.GenKind
		start func(g *conversation.Generation)
		s     fakes.Script
		asset func() ([][]byte, error)
	}{
		"punctuation-only reply": {kind: conversation.GenReply, s: fakes.Script{Deltas: []string{"。", "……！"}},
			start: func(g *conversation.Generation) { g.StartLLM() }},
		"empty reply": {kind: conversation.GenReply, s: fakes.Script{},
			start: func(g *conversation.Generation) { g.StartLLM() }},
		"empty static opening": {kind: conversation.GenOpening,
			start: func(g *conversation.Generation) { g.StartStatic("") }},
		"punctuation static opening": {kind: conversation.GenOpening,
			start: func(g *conversation.Generation) { g.StartStatic("。。") }},
		"empty apology asset": {kind: conversation.GenApology, asset: func() ([][]byte, error) { return nil, nil },
			start: func(g *conversation.Generation) { g.StartAsset() }},
		"broken apology asset": {kind: conversation.GenApology, asset: func() ([][]byte, error) { return nil, audio.ErrBadApologyAsset },
			start: func(g *conversation.Generation) { g.StartAsset() }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			h := newGenHarness(t, c.s)
			h.startWriter()
			p := h.params(c.kind, 5)
			p.Asset = c.asset
			g := h.newGen(p)
			c.start(g)
			evs := h.collect(g)
			require.Equal(t, conversation.EvGenFailed{Gen: 5, Cause: conversation.CauseProvider}, last(evs))
			require.Len(t, eventsOf[conversation.EvGenFailed](evs), 1)
			waitGen(t, g)
			drainUntilQuiet(t, h, g)
			require.Zero(t, h.conn.count())
		})
	}
}

func TestAudioDoneFromWriterAck(t *testing.T) {
	h := newGenHarness(t, fakes.Script{Deltas: []string{"はい、", "そうです。"}})
	g := h.newGen(h.params(conversation.GenReply, 6))
	g.StartLLM()
	waitGen(t, g) // the pacer queued every frame and the end marker: not yet AUDIO_DONE
	evs := drainEvents(h)
	require.Empty(t, eventsOf[conversation.EvAudioDone](evs))
	require.Empty(t, eventsOf[conversation.EvGenFailed](evs))

	_, ok := g.OnWriterAck(conversation.WriterAck{Kind: conversation.AckAudioWrittenAll, Gen: 5})
	require.False(t, ok, "another generation")
	_, ok = g.OnWriterAck(conversation.WriterAck{Kind: conversation.AckSentenceWritten, Gen: 6, Sentences: 1})
	require.False(t, ok, "a sentence ack is not AUDIO_DONE")

	h.startWriter()
	var ev conversation.EvAudioDone
	timeout := time.After(2 * time.Second)
	for !ok {
		select {
		case a := <-h.acks:
			ev, ok = g.OnWriterAck(a)
		case <-timeout:
			t.Fatal("no AUDIO_WRITTEN_ALL")
		}
	}
	require.Equal(t, conversation.EvAudioDone{Gen: 6, LLMFinished: true, FrameCount: 5, TotalMS: 100, SentenceCount: 1}, ev)
	require.Equal(t, 5, h.conn.count())
	_, ok = g.OnWriterAck(conversation.WriterAck{Kind: conversation.AckAudioWrittenAll, Gen: 6, Sentences: 1})
	require.False(t, ok, "AUDIO_DONE once")
}

func TestSleepCtx(t *testing.T) {
	require.NoError(t, conversation.SleepCtx(context.Background(), 0))
	require.NoError(t, conversation.SleepCtx(context.Background(), time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, conversation.SleepCtx(ctx, time.Hour), context.Canceled)
	require.ErrorIs(t, conversation.SleepCtx(ctx, 0), context.Canceled)
}
