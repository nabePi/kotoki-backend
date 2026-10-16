package openai

import (
	"strings"
	"sync"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
)

// Realtime server event types read by the STT adapter (spec 8.4.2).
const (
	evtSessionUpdated = "session.updated"
	evtDelta          = "conversation.item.input_audio_transcription.delta"
	evtCompleted      = "conversation.item.input_audio_transcription.completed"
	evtCommitted      = "input_audio_buffer.committed"
	evtError          = "error"
)

// codeSessionExpired is the vendor code of the session duration limit. UNVERIFIED (no live
// access, deviations M9-T4): it is passed through as VendorCode so rotatingSTT can recognise it.
const codeSessionExpired = "session_expired"

type rtEvent struct {
	Type       string   `json:"type"`
	ItemID     string   `json:"item_id"`
	Delta      string   `json:"delta"`
	Transcript string   `json:"transcript"`
	Error      *rtError `json:"error"`
}

type rtError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// sttError normalises a Realtime `error` event; VendorCode is the vendor code verbatim (the
// type when the code is empty).
func sttError(re *rtError, scrub func(string) string) *provider.Error {
	if re == nil {
		re = &rtError{}
	}
	code := re.Code
	if code == "" {
		code = re.Type
	}
	e := provider.NewError(provider.OpenAI, provider.CapSTT, provider.KindUnavailable, httpx.Redact(scrub(re.Message)))
	e.VendorCode = httpx.Redact(scrub(code))
	e.Retryable = true
	has := func(sub string) bool {
		return strings.Contains(strings.ToLower(re.Code), sub) || strings.Contains(strings.ToLower(re.Type), sub)
	}
	switch {
	case re.Code == codeSessionExpired:
		// rotation signal: stays UNAVAILABLE with the exact vendor code
	case has("insufficient_quota"):
		e.Kind, e.Retryable = provider.KindRateLimited, false
	case has("rate_limit"):
		e.Kind = provider.KindRateLimited
	case has("api_key"), has("authentication"), has("invalid_authentication"):
		e.Kind, e.Retryable = provider.KindAuth, false
	case re.Type == "invalid_request_error":
		e.Kind, e.Retryable = provider.KindInvalidRequest, false
	}
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
