package cartesia

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
)

const (
	dialTimeout   = 10 * time.Second
	ttsReadLimit  = 4 << 20
	callEventsCap = 64
)

// ttsMessage is a server message of the TTS WebSocket (docs api-reference/tts/websocket): every
// response carries context_id; chunk carries base64 audio in data.
type ttsMessage struct {
	Type      string `json:"type"`
	ContextID string `json:"context_id"`
	Data      string `json:"data"`
	vendorError
}

// event is what a connection's reader hands to the call that owns the active context: a server
// message, or the read error that killed the connection.
type event struct {
	msg *ttsMessage
	err error
}

// call is one generation (one context) on a connection.
type call struct {
	id     string
	events chan event
	done   chan struct{}
	once   sync.Once
}

func newCall(id string) *call {
	return &call{id: id, events: make(chan event, callEventsCap), done: make(chan struct{})}
}

func (c *call) deliver(ev event) {
	select {
	case c.events <- ev:
	case <-c.done:
	}
}

func (c *call) finish() { c.once.Do(func() { close(c.done) }) }

// wsConn is one persistent connection; at most one context is active on it (multiplexing is
// only worth adding if the benchmark proves it, spec 8.4.14).
type wsConn struct {
	t      *TTS
	conn   *websocket.Conn
	cancel context.CancelFunc
	reused bool // taken from the idle list (may have gone stale while idle)

	mu     sync.Mutex
	active *call
	dead   bool
}

func (c *wsConn) isDead() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dead
}

func (c *wsConn) setActive(cl *call) {
	c.mu.Lock()
	c.active = cl
	c.mu.Unlock()
}

// readLoop routes messages of the active context to its call; messages of any other context
// (a cancelled generation still finishing) are dropped.
func (c *wsConn) readLoop(ctx context.Context) {
	for {
		typ, data, err := c.conn.Read(ctx)
		if err != nil {
			c.kill(err)
			return
		}
		c.t.tap("recv", typ == websocket.MessageText, data)
		if typ != websocket.MessageText {
			continue
		}
		var m ttsMessage
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		c.mu.Lock()
		a := c.active
		c.mu.Unlock()
		if a == nil || (m.ContextID != "" && m.ContextID != a.id) {
			continue
		}
		a.deliver(event{msg: &m})
	}
}

// kill marks the connection dead, closes it and tells the active call why.
func (c *wsConn) kill(err error) {
	c.mu.Lock()
	c.dead = true
	a := c.active
	c.mu.Unlock()
	c.cancel()
	_ = c.conn.CloseNow()
	if a != nil && err != nil {
		a.deliver(event{err: err})
	}
}

func (c *wsConn) write(ctx context.Context, text []byte) error {
	c.t.tap("send", true, text)
	return c.conn.Write(ctx, websocket.MessageText, text)
}

// pool bounds and reuses the persistent connections of one TTS adapter: a slot is held for the
// whole life of a call (including a cancelled call's drain), so size bounds the connections.
type pool struct {
	t    *TTS
	sem  chan struct{}
	mu   sync.Mutex
	idle []*wsConn
	shut bool
}

func newPool(t *TTS, size int) *pool { return &pool{t: t, sem: make(chan struct{}, size)} }

// acquire returns a connection with a slot held; ctx ends the wait for a slot.
func (p *pool) acquire(ctx context.Context) (*wsConn, error) {
	if err := ctx.Err(); err != nil {
		return nil, httpx.ClassifyError(provider.Cartesia, provider.CapTTS, err)
	}
	select {
	case p.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, httpx.ClassifyError(provider.Cartesia, provider.CapTTS, ctx.Err())
	}
	if wc := p.takeIdle(); wc != nil {
		return wc, nil
	}
	wc, err := p.dial(ctx)
	if err != nil {
		<-p.sem
		return nil, err
	}
	return wc, nil
}

func (p *pool) takeIdle() *wsConn {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.idle) > 0 {
		wc := p.idle[len(p.idle)-1]
		p.idle = p.idle[:len(p.idle)-1]
		if !wc.isDead() {
			wc.reused = true
			return wc
		}
	}
	return nil
}

// replace swaps a stale connection for a fresh one without giving up the slot.
func (p *pool) replace(ctx context.Context, old *wsConn) (*wsConn, error) {
	old.kill(nil)
	wc, err := p.dial(ctx)
	if err != nil {
		<-p.sem
		return nil, err
	}
	return wc, nil
}

// release returns the slot; a healthy connection goes back to the idle list when reuse is set.
func (p *pool) release(wc *wsConn, reuse bool) {
	wc.setActive(nil)
	p.mu.Lock()
	keep := reuse && !p.shut && !wc.isDead()
	if keep {
		p.idle = append(p.idle, wc)
	}
	p.mu.Unlock()
	if !keep {
		wc.kill(nil)
	}
	<-p.sem
}

func (p *pool) close() {
	p.mu.Lock()
	p.shut = true
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()
	for _, wc := range idle {
		wc.kill(nil)
	}
}

func (p *pool) endpoint() string {
	return p.t.cfg.WSBaseURL + ttsPath + "?cartesia_version=" + url.QueryEscape(p.t.cfg.Version)
}

func (p *pool) dial(ctx context.Context) (*wsConn, error) {
	dctx, dcancel := context.WithTimeout(ctx, dialTimeout)
	defer dcancel()
	conn, resp, err := websocket.Dial(dctx, p.endpoint(), &websocket.DialOptions{
		HTTPClient: p.t.hc,
		HTTPHeader: http.Header{"X-API-Key": {p.t.cfg.APIKey}},
	})
	if err != nil {
		return nil, p.t.dialError(ctx, resp, err)
	}
	conn.SetReadLimit(ttsReadLimit)
	cctx, cancel := context.WithCancel(context.WithoutCancel(ctx)) // the connection outlives the call that dialled it
	wc := &wsConn{t: p.t, conn: conn, cancel: cancel}
	go wc.readLoop(cctx)
	return wc, nil
}

func (t *TTS) dialError(ctx context.Context, resp *http.Response, err error) error {
	if resp != nil && resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusSwitchingProtocols {
		var body []byte
		if resp.Body != nil {
			body, _ = io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
			_ = resp.Body.Close()
		}
		return classifyHTTP(provider.CapTTS, resp, body, t.scrub)
	}
	if cerr := ctx.Err(); cerr != nil {
		return httpx.ClassifyError(provider.Cartesia, provider.CapTTS, cerr)
	}
	e := httpx.ClassifyError(provider.Cartesia, provider.CapTTS, err)
	e.Message = t.scrub(e.Message)
	return e
}
