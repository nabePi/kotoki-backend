package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/nabePi/kotoki-backend/internal/conversation/audio"
	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
)

// ErrSlowConsumer means a durable control message did not fit in the outbound queue (D-082):
// the caller sends SLOW_CONSUMER and closes 4101; the runtime itself stays alive.
var ErrSlowConsumer = errors.New("conversation: outbound queue full (slow consumer)")

// transientCoalesceAt: once len(control) exceeds it, a new transient message replaces a queued
// transient of the same type or is dropped (§7.12.3).
const transientCoalesceAt = 128

// OutItem is one message for the writer: a control envelope (Kind MessageText) or an audio item
// (Kind MessageBinary): a frame, or the end marker of generation Frame.Gen when GenEnd is set.
type OutItem struct {
	Kind   MessageKind
	Env    []byte
	Frame  audio.OutFrame
	GenEnd bool
}

type controlItem struct {
	env     []byte
	durable bool
	typ     string // transient only; resolved lazily, "" = not parsed yet
}

type audioItem struct {
	frame  audio.OutFrame
	genEnd bool
}

// Outbound is the per-connection outbound queue (§7.7.2, §7.12.3): a priority `control` queue
// of JSON envelopes and an `audio` queue of frames. Next hands out every control message before
// one audio item. The capacity bounds control + audio frames; durable control that does not fit
// is ErrSlowConsumer. Audio is bounded by the pacer (< 25 frames) and is never refused. Safe for
// concurrent use; the writer is its only consumer.
type Outbound struct {
	capacity int
	m        *Metrics
	notify   chan struct{}

	mu           sync.Mutex
	control      []controlItem
	audio        []audioItem
	frames       int // audio frames queued (end markers excluded)
	inflight     int // items handed out by Next and not yet marked done by the writer
	slowReported bool
}

// NewOutbound creates a queue holding at most capacity messages (WS_OUTBOUND_QUEUE, 256).
func NewOutbound(capacity int, m *Metrics) *Outbound {
	return &Outbound{capacity: capacity, m: m, notify: make(chan struct{}, 1)}
}

// EventDurable reports whether a server event is durable (§7.4.3): the durable types of
// protocol.IsDurable plus a non-fatal `error`, which protocol.IsDurable cannot decide.
func EventDurable(ev protocol.ServerEvent) bool {
	if protocol.IsDurable(ev.Type) {
		return true
	}
	if ev.Type != "error" {
		return false
	}
	switch p := ev.Payload.(type) {
	case protocol.ErrorPayload:
		return !p.Fatal
	case *protocol.ErrorPayload:
		return p != nil && !p.Fatal
	}
	return false
}

// EnqueueEvent marshals ev and enqueues it as control with its durability from EventDurable.
func (o *Outbound) EnqueueEvent(ev protocol.ServerEvent) error {
	b, err := protocol.MarshalServer(ev)
	if err != nil {
		return fmt.Errorf("conversation: marshal %s: %w", ev.Type, err)
	}
	return o.enqueueControl(controlItem{env: b, durable: EventDurable(ev), typ: ev.Type})
}

// EnqueueControl queues a JSON envelope. A durable message on a full queue → ErrSlowConsumer.
// A transient message is never an error: when len(control) > 128 it replaces the queued
// transient of the same type (the newest is kept, at the tail) or is dropped.
func (o *Outbound) EnqueueControl(env []byte, durable bool) error {
	return o.enqueueControl(controlItem{env: env, durable: durable})
}

func (o *Outbound) enqueueControl(it controlItem) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	full := len(o.control)+o.frames >= o.capacity
	if it.durable {
		if full {
			if !o.slowReported {
				o.slowReported = true
				o.m.slowConsumerInc()
			}
			return ErrSlowConsumer
		}
		o.pushControl(it)
		return nil
	}
	if len(o.control) > transientCoalesceAt {
		typ := o.typeOf(&it)
		for i := range o.control {
			c := &o.control[i]
			if !c.durable && o.typeOf(c) == typ {
				o.control = append(o.control[:i], o.control[i+1:]...)
				o.pushControl(it)
				o.m.transientDropped("coalesced")
				return nil
			}
		}
		o.m.transientDropped("dropped")
		return nil
	}
	if full {
		o.m.transientDropped("dropped")
		return nil
	}
	o.pushControl(it)
	return nil
}

// typeOf returns the envelope type of a transient item, parsing it once. Called with mu held.
func (o *Outbound) typeOf(c *controlItem) string {
	if c.typ == "" {
		var e struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(c.env, &e) != nil || e.Type == "" {
			e.Type = "?"
		}
		c.typ = e.Type
	}
	return c.typ
}

func (o *Outbound) pushControl(it controlItem) {
	o.control = append(o.control, it)
	o.signal()
}

// EnqueueAudio queues one downstream frame.
func (o *Outbound) EnqueueAudio(f audio.OutFrame) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.audio = append(o.audio, audioItem{frame: f})
	o.frames++
	o.signal()
}

// EnqueueAudioEnd queues the end marker of generation gen after its last frame: once the writer
// reaches it (gen still active) every frame of gen is written and it acks AUDIO_WRITTEN_ALL.
func (o *Outbound) EnqueueAudioEnd(gen uint32) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.audio = append(o.audio, audioItem{frame: audio.OutFrame{Gen: gen}, genEnd: true})
	o.signal()
}

// PurgeAudio drops every queued audio item of a generation other than activeGen (barge-in,
// §7.6.5); the dropped frames count as audio_frames_out_total{result="dropped_stale"}.
func (o *Outbound) PurgeAudio(activeGen uint32) {
	o.mu.Lock()
	defer o.mu.Unlock()
	kept := o.audio[:0]
	dropped := 0
	for _, a := range o.audio {
		if a.frame.Gen == activeGen {
			kept = append(kept, a)
			continue
		}
		if !a.genEnd {
			dropped++
		}
	}
	clear(o.audio[len(kept):])
	o.audio = kept
	o.frames -= dropped
	o.m.audioOut("dropped_stale", dropped)
}

// AudioLen returns the number of queued audio frames (end markers excluded).
func (o *Outbound) AudioLen() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.frames
}

// ControlLen returns the number of queued control messages.
func (o *Outbound) ControlLen() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.control)
}

// Next blocks until an item is available and returns it: control first, then one audio item.
// It returns false when ctx is done. The consumer calls done after handling the item.
func (o *Outbound) Next(ctx context.Context) (OutItem, bool) {
	for {
		o.mu.Lock()
		if it, ok := o.pop(); ok {
			o.inflight++
			o.mu.Unlock()
			return it, true
		}
		o.mu.Unlock()
		select {
		case <-o.notify:
		case <-ctx.Done():
			return OutItem{}, false
		}
	}
}

// pop removes the next item. Called with mu held.
func (o *Outbound) pop() (OutItem, bool) {
	if len(o.control) > 0 {
		c := o.control[0]
		o.control[0] = controlItem{}
		o.control = o.control[1:]
		return OutItem{Kind: MessageText, Env: c.env}, true
	}
	if len(o.audio) > 0 {
		a := o.audio[0]
		o.audio[0] = audioItem{}
		o.audio = o.audio[1:]
		if !a.genEnd {
			o.frames--
		}
		return OutItem{Kind: MessageBinary, Frame: a.frame, GenEnd: a.genEnd}, true
	}
	return OutItem{}, false
}

// done marks an item from Next as handled (written, dropped or failed).
func (o *Outbound) done() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.inflight--
}

// Drained reports that nothing is queued and the writer holds no item.
func (o *Outbound) Drained() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.control) == 0 && len(o.audio) == 0 && o.inflight == 0
}

// signal wakes a waiting Next. Called with mu held.
func (o *Outbound) signal() {
	select {
	case o.notify <- struct{}{}:
	default:
	}
}
