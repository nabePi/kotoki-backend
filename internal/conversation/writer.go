package conversation

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

// ErrWriteTimeout means one message was not written within WS_WRITE_TIMEOUT (§7.12.3): the
// caller treats the connection as lost (disconnect flow §7.10.1).
var ErrWriteTimeout = errors.New("conversation: websocket write timeout")

// defaultWriteTimeout is WS_WRITE_TIMEOUT (5 s per message).
const defaultWriteTimeout = 5 * time.Second

// WriterAckKind says what a WriterAck reports.
type WriterAckKind uint8

const (
	// AckSentenceWritten means the LastOfSentence frame of sentence SentenceIdx was written.
	AckSentenceWritten WriterAckKind = iota + 1
	// AckAudioWrittenAll means every frame of generation Gen was written (AUDIO_WRITTEN_ALL).
	AckAudioWrittenAll
)

// String returns the spec name of the ack.
func (k WriterAckKind) String() string {
	switch k {
	case AckSentenceWritten:
		return "SENTENCE_WRITTEN"
	case AckAudioWrittenAll:
		return "AUDIO_WRITTEN_ALL"
	}
	return "UNKNOWN"
}

// WriterAck is what the writer reports to Runtime.loop (writerAcks, cap 64). Sentences is the
// count of sentences of Gen fully written so far.
type WriterAck struct {
	Kind        WriterAckKind
	Gen         uint32
	SentenceIdx int
	Sentences   int
}

// Writer is the single goroutine that writes to a Conn (§7.7.2). It drains Outbound (control
// first, then one audio frame), drops audio of a generation that is no longer active, and acks
// written sentences and the end of a generation.
type Writer struct {
	// Timeout bounds each Write (WS_WRITE_TIMEOUT).
	Timeout time.Duration
	// WithTimeout derives the per-write context; context.WithTimeout by default.
	WithTimeout TimeoutFunc

	// Now is the clock that stamps lastAudio (the system clock by default).
	Now func() time.Time

	m *Metrics
	// mu guards the sentence counter of the generation being played (sentGen, sentN) and lastAudio.
	mu        sync.Mutex
	sentGen   uint32
	sentN     int
	lastAudio time.Time
}

// NewWriter creates a writer; timeout <= 0 means 5 s.
func NewWriter(timeout time.Duration, m *Metrics) *Writer {
	if timeout <= 0 {
		timeout = defaultWriteTimeout
	}
	return &Writer{Timeout: timeout, WithTimeout: context.WithTimeout, Now: clock.System{}.Now, m: m}
}

// SentencesWritten returns how many sentences of gen had their last frame written
// (gen.sentencesWritten of §7.6.5); 0 for any other generation.
func (w *Writer) SentencesWritten(gen uint32) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sentGen != gen {
		return 0
	}
	return w.sentN
}

// LastAudioWrite is when the last audio frame was written to the socket (zero before the first):
// the end of the barge-in stop time (D-101, bargein_stop_seconds).
func (w *Writer) LastAudioWrite() time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.lastAudio
}

func (w *Writer) bumpSentences(gen uint32) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sentGen != gen {
		w.sentGen, w.sentN = gen, 0
	}
	w.sentN++
	return w.sentN
}

// Run writes until ctx is done (nil) or a write fails (ErrWriteTimeout or the Conn error).
// activeGen is read before every audio write; acks may be nil.
func (w *Writer) Run(ctx context.Context, conn Conn, out *Outbound, activeGen *atomic.Uint32, acks chan<- WriterAck) error {
	for {
		it, ok := out.Next(ctx)
		if !ok {
			return nil
		}
		err := w.handle(ctx, conn, it, activeGen, acks)
		out.done()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

func (w *Writer) handle(ctx context.Context, conn Conn, it OutItem, activeGen *atomic.Uint32, acks chan<- WriterAck) error {
	if it.Kind == MessageText {
		return w.write(ctx, conn, MessageText, it.Env)
	}
	f := it.Frame
	if f.Gen != activeGen.Load() { // Review Focus 2: a stale frame is never written
		if !it.GenEnd {
			w.m.audioOut("dropped_stale", 1)
		}
		return nil
	}
	if it.GenEnd {
		return w.ack(ctx, acks, WriterAck{Kind: AckAudioWrittenAll, Gen: f.Gen, Sentences: w.SentencesWritten(f.Gen)})
	}
	if err := w.write(ctx, conn, MessageBinary, protocol.EncodeDownstream(f.Gen, f.Seq, f.Opus)); err != nil {
		return err
	}
	w.m.audioOut("written", 1)
	w.mu.Lock()
	w.lastAudio = w.Now()
	w.mu.Unlock()
	if f.LastOfSentence {
		n := w.bumpSentences(f.Gen)
		return w.ack(ctx, acks, WriterAck{Kind: AckSentenceWritten, Gen: f.Gen, SentenceIdx: f.SentenceIdx, Sentences: n})
	}
	return nil
}

func (w *Writer) write(ctx context.Context, conn Conn, kind MessageKind, p []byte) error {
	wctx, cancel := w.WithTimeout(ctx, w.Timeout)
	defer cancel()
	err := conn.Write(wctx, kind, p)
	if err == nil {
		return nil
	}
	if ctx.Err() == nil && (errors.Is(err, context.DeadlineExceeded) || errors.Is(context.Cause(wctx), context.DeadlineExceeded)) {
		return ErrWriteTimeout
	}
	return fmt.Errorf("conversation: websocket write: %w", err)
}

func (w *Writer) ack(ctx context.Context, acks chan<- WriterAck, a WriterAck) error {
	if acks == nil {
		return nil
	}
	select {
	case acks <- a:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
