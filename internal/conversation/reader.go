package conversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// Kinds of protocol_violations_total (§7.14).
const (
	ViolationFrameSize    = "frame_size"
	ViolationHeader       = "header"
	ViolationRate         = "rate"
	ViolationVersion      = "version"
	ViolationSessionID    = "session_id"
	ViolationInvalidBurst = "invalid_burst"
	ViolationAudioFormat  = "audio_format"
)

// eventIDCacheSize is the per-session LRU of client event_ids (§7.4.2).
const eventIDCacheSize = 64

// ProtocolError is a fatal violation found by the reader: the caller emits the `error` event
// (Code), closes with CloseCode and records PROTOCOL_VIOLATION in session_events. Kind is the
// protocol_violations_total label (already counted by the reader). Detail never holds frame data.
type ProtocolError struct {
	Code   errs.Code
	Kind   string
	Detail string
}

func (e *ProtocolError) Error() string {
	return fmt.Sprintf("conversation: %s (%s): %s", e.Code, e.Kind, e.Detail)
}

// CloseCode is the WebSocket close code of the error (1008).
func (e *ProtocolError) CloseCode() int { return protocol.CloseCode(e.Code) }

// AudioIn is one validated upstream frame for the audio pipeline (audioIn, cap 50).
type AudioIn struct {
	Seq  uint32
	Opus []byte
	At   time.Time
}

// ReaderNotice is a non-fatal problem the loop answers with an `error` event (INVALID_MESSAGE).
type ReaderNotice struct {
	Code       errs.Code
	RefEventID *uuid.UUID
	Detail     string
}

// EventIDCache remembers the last 64 client event_ids of a session (§7.4.2): a duplicate is
// ignored silently. The runtime keeps one per session so it survives reconnects. Safe for
// concurrent use.
type EventIDCache struct {
	mu   sync.Mutex
	ring [eventIDCacheSize]uuid.UUID
	next int
	set  map[uuid.UUID]struct{}
}

// NewEventIDCache creates an empty cache.
func NewEventIDCache() *EventIDCache {
	return &EventIDCache{set: make(map[uuid.UUID]struct{}, eventIDCacheSize)}
}

// Seen records id and reports whether it was already among the last 64.
func (c *EventIDCache) Seen(id uuid.UUID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.set[id]; ok {
		return true
	}
	if old := c.ring[c.next]; old != uuid.Nil {
		delete(c.set, old)
	}
	c.ring[c.next] = id
	c.next = (c.next + 1) % eventIDCacheSize
	c.set[id] = struct{}{}
	return false
}

// ReaderConfig configures a Reader; zero limits take the §7.4.10 defaults.
type ReaderConfig struct {
	SessionID      uuid.UUID
	Clock          clock.Clock
	MaxJSONBytes   int           // 16384
	MaxBinaryBytes int           // 4096
	BinaryRate     int           // 100/s
	BinaryBurst    int           // 200
	JSONRate       int           // 20/s
	JSONBurst      int           // 40
	Activity       *Activity     // touched for every frame; nil = not tracked
	Seen           *EventIDCache // per-session dedupe; nil = a new cache per reader
	Metrics        *Metrics
}

// ReaderConfigFromTuning fills the limits from RealtimeTuning (WS_* env).
func ReaderConfigFromTuning(t config.RealtimeTuning, sessionID uuid.UUID, clk clock.Clock) ReaderConfig {
	return ReaderConfig{
		SessionID: sessionID, Clock: clk,
		MaxJSONBytes: int(t.WSMaxJSONBytes), MaxBinaryBytes: int(t.WSMaxBinaryBytes),
		BinaryRate: t.WSBinaryRate, BinaryBurst: t.WSBinaryBurst, JSONRate: t.WSJSONRate, JSONBurst: t.WSJSONBurst,
	}
}

// Reader is the connection's reader goroutine (§7.7.2): it enforces the limits of §7.4.10,
// validates frames and envelopes, drops duplicate commands and feeds cmdCh / audioIn.
type Reader struct {
	cfg     ReaderConfig
	binary  *bucket
	json    *bucket
	invalid *burstWindow
}

// NewReader creates a reader for one connection.
func NewReader(cfg ReaderConfig) *Reader {
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&cfg.MaxJSONBytes, 16384)
	def(&cfg.MaxBinaryBytes, protocol.MaxBinary)
	def(&cfg.BinaryRate, 100)
	def(&cfg.BinaryBurst, 200)
	def(&cfg.JSONRate, 20)
	def(&cfg.JSONBurst, 40)
	if cfg.Clock == nil {
		cfg.Clock = clock.System{}
	}
	if cfg.Seen == nil {
		cfg.Seen = NewEventIDCache()
	}
	return &Reader{
		cfg:     cfg,
		binary:  newBucket(cfg.Clock, cfg.BinaryRate, cfg.BinaryBurst),
		json:    newBucket(cfg.Clock, cfg.JSONRate, cfg.JSONBurst),
		invalid: newBurstWindow(cfg.Clock, invalidMessageLimit, invalidMessageWindow),
	}
}

// Run reads until ctx is done (nil), the Conn fails (its error) or a fatal violation
// (*ProtocolError). Commands go to cmdCh (the reader waits; JSON is rate limited), frames to
// audioIn (never blocks: when full the newest frame is dropped and counted as overflow), and
// INVALID_MESSAGE notices to notices (may be nil). The reader never writes to the Conn.
func (r *Reader) Run(ctx context.Context, conn Conn, cmdCh chan<- protocol.ClientEvent, audioIn chan<- AudioIn, notices chan<- ReaderNotice) error {
	for {
		kind, data, err := conn.Read(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("conversation: websocket read: %w", err)
		}
		now := r.cfg.Clock.Now()
		if r.cfg.Activity != nil {
			r.cfg.Activity.Touch(now)
		}
		switch kind {
		case MessageBinary:
			err = r.onBinary(data, now, audioIn)
		case MessageText:
			err = r.onText(ctx, data, cmdCh, notices)
		default:
			err = r.violation(errs.CodeProtocolViolation, ViolationHeader, fmt.Sprintf("message kind %d", kind))
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

func (r *Reader) onBinary(data []byte, now time.Time, audioIn chan<- AudioIn) error {
	if !r.binary.Take() {
		return r.violation(errs.CodeProtocolViolation, ViolationRate, "binary frame rate")
	}
	if len(data) > r.cfg.MaxBinaryBytes {
		return r.violation(errs.CodeProtocolViolation, ViolationFrameSize, fmt.Sprintf("binary frame %d bytes", len(data)))
	}
	seq, opus, err := protocol.DecodeUpstream(data)
	switch {
	case errors.Is(err, protocol.ErrBadType):
		return r.violation(errs.CodeProtocolViolation, ViolationHeader, fmt.Sprintf("binary frame type 0x%02x", data[0]))
	case err != nil:
		return r.violation(errs.CodeProtocolViolation, ViolationFrameSize, fmt.Sprintf("binary frame %d bytes", len(data)))
	}
	select {
	case audioIn <- AudioIn{Seq: seq, Opus: opus, At: now}:
	default:
		r.cfg.Metrics.audioIn("overflow") // treated as a gap by the audio pipeline
	}
	return nil
}

func (r *Reader) onText(ctx context.Context, data []byte, cmdCh chan<- protocol.ClientEvent, notices chan<- ReaderNotice) error {
	if !r.json.Take() {
		return r.violation(errs.CodeProtocolViolation, ViolationRate, "json message rate")
	}
	if len(data) > r.cfg.MaxJSONBytes {
		return r.violation(errs.CodeProtocolViolation, ViolationFrameSize, fmt.Sprintf("json message %d bytes", len(data)))
	}
	ev, err := protocol.DecodeClientEvent(data)
	if errors.Is(err, protocol.ErrUnsupportedVersion) {
		return r.violation(errs.CodeUnsupportedProtocolVersion, ViolationVersion, "version != 1")
	}
	if err == nil && ev.SessionID != r.cfg.SessionID {
		return r.violation(errs.CodeProtocolViolation, ViolationSessionID, "session_id mismatch")
	}
	if err == nil && ev.EventID == uuid.Nil {
		err = errors.New("missing event_id")
	}
	if err != nil {
		return r.invalidMessage(ctx, data, err, notices)
	}
	if r.cfg.Seen.Seen(ev.EventID) {
		return nil // duplicate command: ignored silently (§7.4.2)
	}
	select {
	case cmdCh <- ev:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// invalidMessage handles a non-fatal INVALID_MESSAGE; more than 10 in 60 s is fatal.
func (r *Reader) invalidMessage(ctx context.Context, data []byte, cause error, notices chan<- ReaderNotice) error {
	if r.invalid.Add() {
		return r.violation(errs.CodeProtocolViolation, ViolationInvalidBurst, "more than 10 INVALID_MESSAGE in 60 s")
	}
	if notices == nil {
		return nil
	}
	n := ReaderNotice{Code: errs.CodeInvalidMessage, Detail: noticeDetail(cause)}
	var head struct {
		EventID uuid.UUID `json:"event_id"`
	}
	if json.Unmarshal(data, &head) == nil && head.EventID != uuid.Nil {
		n.RefEventID = &head.EventID
	}
	select {
	case notices <- n:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// noticeDetail classifies the decode error without echoing client content.
func noticeDetail(err error) string {
	switch {
	case errors.Is(err, protocol.ErrUnknownType):
		return "unknown type"
	case errors.Is(err, protocol.ErrInvalidPayload):
		return "invalid payload"
	}
	return err.Error()
}

func (r *Reader) violation(code errs.Code, kind, detail string) error {
	r.cfg.Metrics.protocolViolation(kind)
	return &ProtocolError{Code: code, Kind: kind, Detail: detail}
}
