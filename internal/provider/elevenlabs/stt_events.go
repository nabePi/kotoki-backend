package elevenlabs

import (
	"sync"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
)

// Scribe realtime server message types read by the STT adapter (spec 8.4.1). Every message
// carries its type in message_type; error messages also carry `error`.
const (
	msgSessionStarted      = "session_started"
	msgPartialTranscript   = "partial_transcript"
	msgCommittedTranscript = "committed_transcript"
)

type rtMessage struct {
	MessageType string `json:"message_type"`
	Text        string `json:"text"`
	Error       string `json:"error"`
}

// errorTypes are the documented error message_type values; the type is the VendorCode.
var errorTypes = map[string]bool{
	"error": true, "auth_error": true, "quota_exceeded": true, "commit_throttled": true,
	"unaccepted_terms": true, "rate_limited": true, "queue_overflow": true, "resource_exhausted": true,
	"session_time_limit_exceeded": true, "input_error": true, "invalid_request": true,
	"chunk_size_exceeded": true, "insufficient_audio_activity": true, "transcriber_error": true,
}

// sttError normalises an error message; VendorCode is message_type verbatim.
func sttError(m rtMessage, scrub func(string) string) *provider.Error {
	e := provider.NewError(provider.ElevenLabs, provider.CapSTT, provider.KindUnavailable, httpx.Redact(scrub(m.Error)))
	e.VendorCode = httpx.Redact(scrub(m.MessageType))
	e.Retryable = true
	return e
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
