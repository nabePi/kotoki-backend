package cartesia

import (
	"math"
	"sync"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// STT server message types (spec 8.4.6; docs.cartesia.ai/api-reference/stt/stt, read
// 2026-10-16).
const (
	msgTranscript = "transcript"
	msgFlushDone  = "flush_done"
	msgDone       = "done"
	msgError      = "error"
)

type sttWord struct {
	Word  string  `json:"word"`
	Start float64 `json:"start"` // seconds
	End   float64 `json:"end"`
}

type sttMessage struct {
	Type    string    `json:"type"`
	IsFinal bool      `json:"is_final"`
	Text    string    `json:"text"`
	Words   []sttWord `json:"words"`
	vendorError
}

func toWords(ws []sttWord) []provider.STTWord {
	out := make([]provider.STTWord, 0, len(ws))
	for _, w := range ws {
		out = append(out, provider.STTWord{Text: w.Word, StartMs: int64(math.Round(w.Start * 1000)), EndMs: int64(math.Round(w.End * 1000))})
	}
	return out
}

// eventQueue feeds Events(): 64 buffered, the oldest PARTIAL is dropped when 64 events wait
// behind a slow consumer, FINAL/ERROR/CLOSED are never dropped.
type eventQueue struct {
	mu     sync.Mutex
	items  []provider.STTEvent
	closed bool
	wake   chan struct{}
	quit   chan struct{}
	qOnce  sync.Once
	out    chan provider.STTEvent
}

const sttEventBuffer = 64

func newEventQueue() *eventQueue {
	q := &eventQueue{wake: make(chan struct{}, 1), quit: make(chan struct{}), out: make(chan provider.STTEvent, sttEventBuffer)}
	go q.pump()
	return q
}

func (q *eventQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *eventQueue) push(ev provider.STTEvent) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	if ev.Type == provider.STTPartial && len(q.items) >= sttEventBuffer {
		dropped := false
		for i, it := range q.items {
			if it.Type == provider.STTPartial {
				q.items = append(q.items[:i], q.items[i+1:]...)
				dropped = true
				break
			}
		}
		if !dropped {
			q.mu.Unlock()
			return
		}
	}
	q.items = append(q.items, ev)
	q.mu.Unlock()
	q.signal()
}

// close ends the stream after the queued events were delivered.
func (q *eventQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.signal()
}

// abandon is for a consumer that left: whatever fits is handed over, the rest is dropped.
func (q *eventQueue) abandon() { q.qOnce.Do(func() { close(q.quit) }) }

func (q *eventQueue) next() (provider.STTEvent, bool) {
	for {
		q.mu.Lock()
		if len(q.items) > 0 {
			ev := q.items[0]
			q.items = q.items[1:]
			q.mu.Unlock()
			return ev, true
		}
		done := q.closed
		q.mu.Unlock()
		if done {
			return provider.STTEvent{}, false
		}
		select {
		case <-q.wake:
		case <-q.quit:
			return provider.STTEvent{}, false
		}
	}
}

func (q *eventQueue) flush(first *provider.STTEvent) {
	if first != nil {
		select {
		case q.out <- *first:
		default:
		}
	}
	for {
		q.mu.Lock()
		if len(q.items) == 0 {
			q.mu.Unlock()
			return
		}
		ev := q.items[0]
		q.items = q.items[1:]
		q.mu.Unlock()
		select {
		case q.out <- ev:
		default:
		}
	}
}

func (q *eventQueue) pump() {
	defer close(q.out)
	for {
		ev, ok := q.next()
		if !ok {
			q.flush(nil)
			return
		}
		select {
		case q.out <- ev:
		case <-q.quit:
			q.flush(&ev)
			return
		}
	}
}
