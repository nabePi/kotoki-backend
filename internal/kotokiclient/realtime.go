package kotokiclient

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
)

const (
	// Subprotocol is the WebSocket subprotocol offered at the handshake (§7.4.1).
	Subprotocol = "kotoki.v1"
	// MaxUpstreamOpus is the largest Opus packet that fits an upstream frame (§7.4.7).
	MaxUpstreamOpus = protocol.MaxBinary - 5

	readLimit   = 64 << 10
	eventBuffer = 512
	audioBuffer = 1024
)

// ErrBadPacket is returned by SendAudio for an Opus packet outside 1..4091 bytes.
var ErrBadPacket = errors.New("kotokiclient: opus packet must be 1..4091 bytes")

// Received is a value with the local time it was received.
type Received[T any] struct {
	Value T
	At    time.Time
}

// AudioFrame is one downstream audio frame (§7.4.7): Payload is the Opus packet.
type AudioFrame struct {
	Generation uint32
	Seq        uint32
	Payload    []byte
	At         time.Time
}

// DialOption configures Dial.
type DialOption func(*dialConfig)

type dialConfig struct {
	clk clock.Clock
	ids idgen.Generator
}

// WithDialClock sets the clock for receive times (AudioFrame.At, Received.At) and for the ts
// of sent events.
func WithDialClock(clk clock.Clock) DialOption { return func(d *dialConfig) { d.clk = clk } }

// WithDialIDs sets the generator of event ids.
func WithDialIDs(g idgen.Generator) DialOption { return func(d *dialConfig) { d.ids = g } }

// RealtimeConn is one WebSocket v1 connection. A reader goroutine decodes everything the server
// sends into Events and Audio; both channels close when the connection ends (see Err).
//
// Both channels are buffered but the reader blocks when a buffer is full, so a consumer must
// drain both (or Close the connection).
//
// Frames of an old generation are dropped before they reach Audio (Review Focus 2): the active
// generation follows the last ai.generation_started, ai.interrupted.active_generation and
// session.snapshot.active_generation seen. Events and frames share one stream, so the
// ordering is exactly the server's.
type RealtimeConn struct {
	ws  *websocket.Conn
	clk clock.Clock
	ids idgen.Generator

	events chan Received[protocol.ServerEvent]
	audio  chan AudioFrame

	sendMu sync.Mutex
	seq    uint32 // next upstream sequence number, guarded by sendMu

	activeGen atomic.Uint32
	stale     atomic.Int64
	done      chan struct{}
	err       error // terminal read error, valid after done is closed
	closeCh   chan struct{}
	closeOnce sync.Once
	closeErr  error
}

// Dial opens the WebSocket at wsURL with the one-time ticket (?ticket=…) and the given Origin
// header ("" = none).
func Dial(ctx context.Context, wsURL, ticket, origin string, opts ...DialOption) (*RealtimeConn, error) {
	cfg := dialConfig{clk: clock.System{}, ids: idgen.NewV7()}
	for _, o := range opts {
		o(&cfg)
	}
	u, err := url.Parse(wsURL)
	if err != nil {
		return nil, fmt.Errorf("kotokiclient: ws url: %w", err)
	}
	q := u.Query()
	q.Set("ticket", ticket)
	u.RawQuery = q.Encode()
	hdr := http.Header{}
	if origin != "" {
		hdr.Set("Origin", origin)
	}
	ws, resp, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{
		HTTPHeader:      hdr,
		Subprotocols:    []string{Subprotocol},
		CompressionMode: websocket.CompressionDisabled,
	})
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		// The URL carries the one-time ticket; never echo it back in an error.
		return nil, fmt.Errorf("kotokiclient: dial: %w", redactDialError(err, resp))
	}
	ws.SetReadLimit(readLimit)
	rc := &RealtimeConn{
		ws: ws, clk: cfg.clk, ids: cfg.ids,
		events: make(chan Received[protocol.ServerEvent], eventBuffer),
		audio:  make(chan AudioFrame, audioBuffer),
		done:   make(chan struct{}), closeCh: make(chan struct{}),
	}
	go rc.readLoop(context.WithoutCancel(ctx)) // outlives the dial context
	return rc, nil
}

// DialError is a handshake refused over HTTP (before the upgrade): the gateway answers 403,
// 429 or 503 with the REST error body.
type DialError struct {
	Status int
	API    *APIError
}

// Error implements error.
func (e *DialError) Error() string {
	if e.API != nil {
		return fmt.Sprintf("handshake refused: %d %s", e.Status, e.API.Code)
	}
	return fmt.Sprintf("handshake refused: %d", e.Status)
}

func redactDialError(err error, resp *http.Response) error {
	if resp != nil && resp.StatusCode != http.StatusSwitchingProtocols {
		de := &DialError{Status: resp.StatusCode}
		if resp.Body != nil {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			if ae := decodeAPIError(resp.StatusCode, raw); ae.Code != "" {
				de.API = ae
			}
		}
		return de
	}
	// Library errors may embed the URL (and with it the ticket); keep only the cause class.
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return errors.New("connection failed")
}

func (rc *RealtimeConn) readLoop(ctx context.Context) {
	defer close(rc.done)
	defer close(rc.audio)
	defer close(rc.events)
	for {
		typ, data, err := rc.ws.Read(ctx)
		if err != nil {
			rc.err = err
			return
		}
		at := rc.clk.Now()
		switch typ {
		case websocket.MessageText:
			ev, err := protocol.DecodeServerEvent(data)
			if err != nil {
				continue // unknown type or payload: forward compatibility (§7.4.2)
			}
			rc.trackGeneration(ev)
			select {
			case rc.events <- Received[protocol.ServerEvent]{Value: ev, At: at}:
			case <-rc.closing():
				return
			}
		case websocket.MessageBinary:
			gen, seq, opus, err := protocol.DecodeDownstream(data)
			if err != nil {
				continue
			}
			if gen < rc.activeGen.Load() {
				rc.stale.Add(1)
				continue
			}
			select {
			case rc.audio <- AudioFrame{Generation: gen, Seq: seq, Payload: append([]byte(nil), opus...), At: at}:
			case <-rc.closing():
				return
			}
		}
	}
}

// closing is closed by Close so a reader blocked on a full channel can leave.
func (rc *RealtimeConn) closing() <-chan struct{} { return rc.closeCh }

// trackGeneration moves the active generation per §7.4.5: ai.generation_started carries it in the
// envelope, ai.interrupted and session.snapshot in the payload.
func (rc *RealtimeConn) trackGeneration(ev protocol.ServerEvent) {
	switch p := ev.Payload.(type) {
	case protocol.AIGenerationStarted:
		if ev.Generation != nil {
			rc.activeGen.Store(*ev.Generation)
		}
	case protocol.AIInterrupted:
		rc.activeGen.Store(p.ActiveGeneration)
	case protocol.SessionSnapshot:
		rc.activeGen.Store(p.ActiveGeneration)
	}
}

// Events delivers every decoded server event, in order. It is closed when the connection ends.
func (rc *RealtimeConn) Events() <-chan Received[protocol.ServerEvent] { return rc.events }

// Audio delivers downstream frames of the active generation. It is closed when the connection
// ends.
func (rc *RealtimeConn) Audio() <-chan AudioFrame { return rc.audio }

// ActiveGeneration is the generation frames must carry to be delivered.
func (rc *RealtimeConn) ActiveGeneration() uint32 { return rc.activeGen.Load() }

// StaleDropped counts the frames dropped for carrying an old generation.
func (rc *RealtimeConn) StaleDropped() int64 { return rc.stale.Load() }

// Done is closed when the reader has stopped.
func (rc *RealtimeConn) Done() <-chan struct{} { return rc.done }

// Err is the reason the connection ended (a websocket.CloseError carries the close code); nil
// while it is still open.
func (rc *RealtimeConn) Err() error {
	select {
	case <-rc.done:
		return rc.err
	default:
		return nil
	}
}

// Send writes one client event as a text frame. Version, EventID and TS are filled when zero.
func (rc *RealtimeConn) Send(ctx context.Context, ev protocol.ClientEvent) error {
	if ev.Version == 0 {
		ev.Version = protocol.Version
	}
	if ev.EventID == uuid.Nil {
		ev.EventID = rc.ids.New()
	}
	if ev.TS.IsZero() {
		ev.TS = rc.clk.Now()
	}
	b, err := protocol.MarshalClient(ev)
	if err != nil {
		return fmt.Errorf("kotokiclient: encode event: %w", err)
	}
	rc.sendMu.Lock()
	defer rc.sendMu.Unlock()
	if err := rc.ws.Write(ctx, websocket.MessageText, b); err != nil {
		return fmt.Errorf("kotokiclient: send %s: %w", ev.Type, err)
	}
	return nil
}

// SendAudio writes one upstream frame [0x01][u32 seq][opus] (D-073). The sequence number starts
// at 0 per connection and advances by one per frame sent.
func (rc *RealtimeConn) SendAudio(ctx context.Context, opusPacket []byte) error {
	if n := len(opusPacket); n < 1 || n > MaxUpstreamOpus {
		return ErrBadPacket
	}
	rc.sendMu.Lock()
	defer rc.sendMu.Unlock()
	if err := rc.ws.Write(ctx, websocket.MessageBinary, protocol.EncodeUpstream(rc.seq, opusPacket)); err != nil {
		return fmt.Errorf("kotokiclient: send audio: %w", err)
	}
	rc.seq++
	return nil
}

// Close closes the connection with the code and reason and waits for the reader to stop. Only
// the first call does anything; later calls return its result.
func (rc *RealtimeConn) Close(code websocket.StatusCode, reason string) error {
	rc.closeOnce.Do(func() {
		close(rc.closeCh)
		err := rc.ws.Close(code, reason)
		<-rc.done
		var ce websocket.CloseError
		if err != nil && !errors.As(rc.err, &ce) { // a peer-initiated close is not a failure
			rc.closeErr = fmt.Errorf("kotokiclient: close: %w", err)
		}
	})
	return rc.closeErr
}
