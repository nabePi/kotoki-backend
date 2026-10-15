package kotokiclient

import (
	"sort"
	"sync"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
)

// Sink receives everything a conversation delivers: every server event (replayed ones carry
// Value.Replay) and every downstream frame of the active generation, each with its receive time.
// The CLI prints and records through it; a load harness aggregates.
//
// RunConversation calls a Sink from one goroutine at a time, in the order the client processes
// the stream; it must not block for long (the connection's buffers fill up behind it). It is
// called after the Report took the item into account.
type Sink interface {
	OnEvent(Received[protocol.ServerEvent])
	OnAudio(AudioFrame)
}

// NopSink discards everything.
type NopSink struct{}

// OnEvent implements Sink.
func (NopSink) OnEvent(Received[protocol.ServerEvent]) {}

// OnAudio implements Sink.
func (NopSink) OnAudio(AudioFrame) {}

// SinkFuncs adapts functions to a Sink; a nil function discards.
type SinkFuncs struct {
	Event func(Received[protocol.ServerEvent])
	Audio func(AudioFrame)
}

// OnEvent implements Sink.
func (s SinkFuncs) OnEvent(e Received[protocol.ServerEvent]) {
	if s.Event != nil {
		s.Event(e)
	}
}

// OnAudio implements Sink.
func (s SinkFuncs) OnAudio(f AudioFrame) {
	if s.Audio != nil {
		s.Audio(f)
	}
}

// MultiSink fans every item out to all sinks, in order.
func MultiSink(sinks ...Sink) Sink { return multiSink(sinks) }

type multiSink []Sink

func (m multiSink) OnEvent(e Received[protocol.ServerEvent]) {
	for _, s := range m {
		s.OnEvent(e)
	}
}

func (m multiSink) OnAudio(f AudioFrame) {
	for _, s := range m {
		s.OnAudio(f)
	}
}

// GenerationAudio is the downstream Opus audio of one generation, in arrival order.
type GenerationAudio struct {
	Generation uint32
	// TurnSeq is the turn the generation answers, from ai.generation_started; 0 when that event
	// was not seen.
	TurnSeq int
	Frames  []AudioFrame
}

// AudioCollector is a Sink that keeps the downstream audio per generation, labelled with the turn
// it belongs to, so a caller can decode and save one file per turn and generation
// (turn-<seq>-gen-<generation>.wav). Safe for concurrent use.
type AudioCollector struct {
	mu    sync.Mutex
	gens  map[uint32]*GenerationAudio
	turns map[uint32]int
}

// NewAudioCollector returns an empty collector.
func NewAudioCollector() *AudioCollector {
	return &AudioCollector{gens: map[uint32]*GenerationAudio{}, turns: map[uint32]int{}}
}

// OnEvent implements Sink: ai.generation_started tells which turn a generation belongs to.
func (a *AudioCollector) OnEvent(e Received[protocol.ServerEvent]) {
	if e.Value.Type != "ai.generation_started" || e.Value.Generation == nil || e.Value.TurnSeq == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	g := *e.Value.Generation
	a.turns[g] = *e.Value.TurnSeq
	if ga, ok := a.gens[g]; ok {
		ga.TurnSeq = *e.Value.TurnSeq
	}
}

// OnAudio implements Sink.
func (a *AudioCollector) OnAudio(f AudioFrame) {
	a.mu.Lock()
	defer a.mu.Unlock()
	ga, ok := a.gens[f.Generation]
	if !ok {
		ga = &GenerationAudio{Generation: f.Generation, TurnSeq: a.turns[f.Generation]}
		a.gens[f.Generation] = ga
	}
	ga.Frames = append(ga.Frames, f)
}

// Generations returns what was collected, ordered by generation. The frames are copies of the
// slice headers; the payloads are shared and must not be modified.
func (a *AudioCollector) Generations() []GenerationAudio {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]GenerationAudio, 0, len(a.gens))
	for _, ga := range a.gens {
		cp := *ga
		cp.Frames = append([]AudioFrame(nil), ga.Frames...)
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Generation < out[j].Generation })
	return out
}
