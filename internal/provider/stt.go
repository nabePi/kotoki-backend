package provider

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// STTSessionConfig is sent when opening one STT stream per session (D-080).
type STTSessionConfig struct {
	SessionID  uuid.UUID // log/metric/trace only
	Language   string    // always "ja"
	SampleRate int       // always STTSampleRate
	Keyterms   []string  // optional scenario vocabulary, at most 50
}

// STTProvider opens streaming recognition sessions.
type STTProvider interface {
	Name() Name
	Model() string
	// Open returns after the vendor handshake succeeded or ctx ended.
	Open(ctx context.Context, cfg STTSessionConfig) (STTSession, error)
}

// STTSession is one stream. SendAudio and Finalize are called from one goroutine,
// Events from one goroutine; Close is idempotent and may be called from anywhere.
type STTSession interface {
	// SendAudio takes PCM16LE mono 16 kHz (even length, usually 640 bytes). Non-blocking.
	SendAudio(pcm []byte) error
	// Finalize makes the vendor finalise everything sent; exactly one FINAL with
	// FromFinalize=true follows (text may be empty).
	Finalize() error
	// Events is buffered (64) and closed after CLOSED. When full the oldest PARTIAL is
	// dropped; FINAL/ERROR/CLOSED never are.
	Events() <-chan STTEvent
	Close() error
	Stats() STTStats
}

// STTEventType is the kind of STTEvent.
type STTEventType string

// STT event types.
const (
	STTPartial STTEventType = "PARTIAL"
	STTFinal   STTEventType = "FINAL"
	STTError   STTEventType = "ERROR"
	STTClosed  STTEventType = "CLOSED"
)

// STTEvent is one recognition event: PARTIAL.Text is the whole running segment (not a delta); FINAL.Text the
// stable text of that segment.
type STTEvent struct {
	Type         STTEventType
	Text         string
	Confidence   *float64
	StartMs      int64 // relative to the first sample of the LOGICAL stream
	EndMs        int64
	FromFinalize bool
	Words        []STTWord
	Provider     Name
	ReceivedAt   time.Time
	Err          *Error // STTError only
}

// STTWord is a word-level timing.
type STTWord struct {
	Text       string
	StartMs    int64
	EndMs      int64
	Confidence *float64
}

// STTStats feeds metering.
type STTStats struct {
	AudioMsSent  int64 // includes audio re-sent after a connection rotation
	OpenedAt     time.Time
	FirstEventAt time.Time
	Reconnects   int
}

// ErrSessionClosed is returned by SendAudio/Finalize after Close.
var ErrSessionClosed = errors.New("provider: stt session closed")
