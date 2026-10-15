package replay

import (
	"fmt"
	"slices"
	"strconv"
	"sync"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// bytesPerMs is PCM16LE mono 16 kHz: 640 bytes = 20 ms.
const bytesPerMs = 2 * provider.STTSampleRate / 1000

// DefaultEventBuffer is the Events() capacity of the port contract (provider.STTSession).
const DefaultEventBuffer = 64

// EventBarrier is the type of the sentinel Barrier puts on Events(); it is no vendor event,
// so consumers that only know PARTIAL/FINAL/ERROR/CLOSED must skip or recognise it.
const EventBarrier provider.STTEventType = "BARRIER"

// Option configures a ScriptedSTT.
type Option func(*ScriptedSTT)

// WithProvider sets the name Provider() and every event report (default FAKE_PRIMARY).
func WithProvider(n provider.Name) Option { return func(s *ScriptedSTT) { s.name = n } }

// WithEventBuffer sets the Events() capacity (default DefaultEventBuffer).
func WithEventBuffer(n int) Option { return func(s *ScriptedSTT) { s.bufSize = max(1, n) } }

// ScriptedSTT is a deterministic provider.RoutedSTTSession. Time is the audio sent: every
// SendAudio advances it by len/32 ms (640 B = 20 ms), and each timeline step whose AtMs is
// reached is emitted synchronously before SendAudio returns: a PARTIAL, or for a final step a
// FINAL that closes the segment. Finalize always yields exactly one FINAL{FromFinalize:true},
// in call order. Events are queued on a buffered channel without a goroutine, so after a call
// returns its events can be drained. Safe for concurrent use.
//
// A full buffer follows the port contract: the oldest PARTIAL is dropped (DroppedPartials);
// FINAL, CLOSED and BARRIER never are. When the buffer holds no PARTIAL to drop, the event
// waits in an overflow list that later calls move into the channel, and Close hands that
// list plus CLOSED to a goroutine that delivers them and then closes Events (so Close never
// blocks or panics; the goroutine only exists, and only lingers, if more than the buffer of
// FINAL/BARRIER events is left undrained). Making room drains and refills the channel, which
// is not atomic against a concurrent receiver; a consumer that keeps up never triggers it.
type ScriptedSTT struct {
	mu            sync.Mutex
	tl            Timeline
	next          int // first step not emitted yet
	audioBytes    int64
	audioMs       int64  // audioBytes / 32
	partial       string // running segment text
	segStartMs    int64
	finalizeText  *string
	hold          bool
	held          []provider.STTEvent
	finalizeCalls int
	timeouts      int
	barriers      uint64
	closed        bool
	name          provider.Name
	bufSize       int
	events        chan provider.STTEvent
	overflow      []provider.STTEvent // non-droppable events waiting for room, in order
	dropped       int                 // PARTIALs dropped on a full buffer
}

var _ provider.RoutedSTTSession = (*ScriptedSTT)(nil)

// NewScriptedSTT returns a session that plays tl; steps are played in slice order.
func NewScriptedSTT(tl Timeline, opts ...Option) *ScriptedSTT {
	s := &ScriptedSTT{tl: append(Timeline(nil), tl...), name: provider.FakePrimary, bufSize: DefaultEventBuffer}
	for _, o := range opts {
		o(s)
	}
	s.events = make(chan provider.STTEvent, s.bufSize)
	return s
}

// emit queues ev; the caller holds mu.
func (s *ScriptedSTT) emit(ev provider.STTEvent) {
	ev.Provider = s.name
	s.flush()
	if len(s.overflow) == 0 {
		select {
		case s.events <- ev:
			return
		default:
		}
	}
	// Full: drop the oldest PARTIAL among the buffered, overflowed and new events.
	q := make([]provider.STTEvent, 0, s.bufSize+len(s.overflow)+1)
	for drained := false; !drained; {
		select {
		case e := <-s.events:
			q = append(q, e)
		default:
			drained = true
		}
	}
	q = append(append(q, s.overflow...), ev)
	s.overflow = nil
	if i := slices.IndexFunc(q, func(e provider.STTEvent) bool { return e.Type == provider.STTPartial }); i >= 0 {
		q = slices.Delete(q, i, i+1)
		s.dropped++
	}
	for i, e := range q {
		select {
		case s.events <- e:
		default:
			s.overflow = append(s.overflow, q[i:]...)
			return
		}
	}
}

// flush moves overflowed events into the channel while there is room; the caller holds mu.
func (s *ScriptedSTT) flush() {
	for len(s.overflow) > 0 {
		select {
		case s.events <- s.overflow[0]:
			s.overflow = s.overflow[1:]
		default:
			return
		}
	}
}

// SendAudio advances audio time by len(pcm)/32 ms and emits the steps reached.
func (s *ScriptedSTT) SendAudio(pcm []byte) error {
	if len(pcm)%2 != 0 {
		return fmt.Errorf("replay: odd PCM16 length %d", len(pcm))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return provider.ErrSessionClosed
	}
	s.flush()
	s.audioBytes += int64(len(pcm))
	s.audioMs = s.audioBytes / bytesPerMs
	for s.next < len(s.tl) && s.tl[s.next].AtMs <= s.audioMs {
		st := s.tl[s.next]
		s.next++
		if !st.Final {
			s.emit(provider.STTEvent{Type: provider.STTPartial, Text: st.Text, StartMs: s.segStartMs, EndMs: st.AtMs})
			s.partial = st.Text
			continue
		}
		s.emit(provider.STTEvent{Type: provider.STTFinal, Text: st.Text, StartMs: s.segStartMs, EndMs: st.AtMs})
		s.partial, s.segStartMs = "", st.AtMs
	}
	return nil
}

// Finalize closes the running segment: one FINAL{FromFinalize:true} whose text is the
// SetFinalizeText override or else the running partial (may be empty). While HoldFinalize is
// on, the FINAL is computed now but emitted on release.
func (s *ScriptedSTT) Finalize() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return provider.ErrSessionClosed
	}
	s.finalizeCalls++
	text := s.partial
	if s.finalizeText != nil {
		text, s.finalizeText = *s.finalizeText, nil
	}
	ev := provider.STTEvent{Type: provider.STTFinal, Text: text, FromFinalize: true, StartMs: s.segStartMs, EndMs: s.audioMs}
	s.partial, s.segStartMs = "", s.audioMs
	if s.hold {
		s.held = append(s.held, ev)
		return nil
	}
	s.emit(ev)
	return nil
}

// EmitPartial emits a PARTIAL with text now, outside the timeline (the running segment becomes
// text): the STT hearing something at a moment the test chooses, e.g. the user's words during
// the AI's speech.
func (s *ScriptedSTT) EmitPartial(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.partial = text
	s.emit(provider.STTEvent{Type: provider.STTPartial, Text: text, StartMs: s.segStartMs, EndMs: s.audioMs})
}

// EmitFinal emits a FINAL{FromFinalize:false} with text now, outside the timeline: the STT closing
// a segment of its own accord (a pause in the user's speech).
func (s *ScriptedSTT) EmitFinal(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.emit(provider.STTEvent{Type: provider.STTFinal, Text: text, StartMs: s.segStartMs, EndMs: s.audioMs})
	s.partial, s.segStartMs = "", s.audioMs
}

// Fail emits the ERROR event the router sends when the vendor stream is lost for good.
func (s *ScriptedSTT) Fail(err *provider.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.emit(provider.STTEvent{Type: provider.STTError, Err: err})
}

// SetFinalizeText overrides the text of the next Finalize FINAL only (e.g. an echo "gema").
func (s *ScriptedSTT) SetFinalizeText(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finalizeText = &text
}

// HoldFinalize withholds the FINALs of later Finalize calls; turning it off emits the held
// ones in call order. Used to keep a commit pending (S-8) or to provoke STT_FINAL_TIMEOUT.
func (s *ScriptedSTT) HoldFinalize(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hold = on
	if on || s.closed {
		return
	}
	for _, ev := range s.held {
		s.emit(ev)
	}
	s.held = nil
}

// Barrier queues a sentinel event (Type EventBarrier) behind everything emitted so far and
// returns its id (from 1); once a consumer sees it, every earlier event has been delivered.
// After Close it queues nothing and returns 0: there is nothing to wait for.
func (s *ScriptedSTT) Barrier() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0
	}
	s.barriers++
	s.emit(provider.STTEvent{Type: EventBarrier, Text: strconv.FormatUint(s.barriers, 10)})
	return s.barriers
}

// BarrierID reports whether ev is a Barrier sentinel and returns its id.
func BarrierID(ev provider.STTEvent) (uint64, bool) {
	if ev.Type != EventBarrier {
		return 0, false
	}
	id, err := strconv.ParseUint(ev.Text, 10, 64)
	return id, err == nil
}

// FinalizeCalls is the number of Finalize calls so far.
func (s *ScriptedSTT) FinalizeCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finalizeCalls
}

// DroppedPartials is the number of PARTIALs dropped because Events was full.
func (s *ScriptedSTT) DroppedPartials() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// FinalTimeoutReports is the number of ReportFinalTimeout calls so far.
func (s *ScriptedSTT) FinalTimeoutReports() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.timeouts
}

// AudioMs is the audio time sent so far.
func (s *ScriptedSTT) AudioMs() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.audioMs
}

// Events delivers PARTIAL/FINAL/BARRIER events and a last CLOSED; it is closed after Close.
func (s *ScriptedSTT) Events() <-chan provider.STTEvent { return s.events }

// Close emits CLOSED (never dropped) and closes Events; idempotent and never blocks.
func (s *ScriptedSTT) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	s.emit(provider.STTEvent{Type: provider.STTClosed})
	if len(s.overflow) == 0 {
		close(s.events)
		return nil
	}
	rest := s.overflow
	s.overflow = nil
	go func() {
		for _, ev := range rest {
			s.events <- ev
		}
		close(s.events)
	}()
	return nil
}

// Stats reports the audio sent.
func (s *ScriptedSTT) Stats() provider.STTStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return provider.STTStats{AudioMsSent: s.audioMs}
}

// Provider is the configured vendor name.
func (s *ScriptedSTT) Provider() provider.Name { return s.name }

// ReportFinalTimeout counts the call (see FinalTimeoutReports).
func (s *ScriptedSTT) ReportFinalTimeout() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.timeouts++
}
