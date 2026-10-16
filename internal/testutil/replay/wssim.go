package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// recvTimeout bounds how long a `recv` line waits for the client before the script is
// abandoned with a test error (a hung adapter must fail, not hang the suite).
const recvTimeout = 10 * time.Second

type wsState struct {
	mu        sync.Mutex
	connected bool
	done      map[*WSStep]bool
	rules     []*WSStep
	fired     map[*WSStep]int
}

// NewWSServer serves one WebSocket connection per test according to the script: the
// `ws_upgrade` line is checked against the handshake, then `ws` lines run in order while
// `on` rules (registered when reached) answer matching client messages.
func NewWSServer(t testing.TB, s *Script) *Server {
	t.Helper()
	sv := newServer(t, s)
	sv.ws = &wsState{done: map[*WSStep]bool{}, fired: map[*WSStep]int{}}
	sv.srv = httptest.NewServer(http.HandlerFunc(sv.serveWS))
	sv.URL = "ws" + sv.srv.URL[len("http"):]
	return sv
}

func (sv *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	sv.wg.Add(1)
	defer sv.wg.Done()
	req := Request{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Header: r.Header.Clone()}
	sv.mu.Lock()
	sv.upgrade = req
	sv.mu.Unlock()
	if u := sv.s.WSUpgrade; u != nil {
		exp := HTTPExpect{Method: http.MethodGet, Path: u.Path, Query: u.Query, Headers: u.Headers}
		if exp.Path == "" {
			exp.Path = req.Path
		}
		for _, p := range matchHTTP(exp, req) {
			sv.t.Errorf("replay: line %d (ws_upgrade): %s", u.line, p)
		}
	}
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow() //nolint:errcheck // best effort at teardown
	sv.ws.mu.Lock()
	sv.ws.connected = true
	sv.ws.mu.Unlock()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	inbox := make(chan Message, 1024)
	readerDone := make(chan struct{})
	sv.wg.Add(1)
	go func() {
		defer sv.wg.Done()
		defer close(readerDone)
		sv.readLoop(ctx, conn, inbox)
	}()

	sv.runSteps(ctx, conn, inbox)

	select {
	case <-readerDone:
	case <-sv.stop:
	}
}

func (sv *Server) readLoop(ctx context.Context, conn *websocket.Conn, inbox chan<- Message) {
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		msg := Message{Text: typ == websocket.MessageText, Data: data, At: time.Now()}
		sv.mu.Lock()
		sv.messages = append(sv.messages, msg)
		sv.mu.Unlock()
		if rule := sv.claimRule(msg); rule != nil {
			base := msg.At
			for _, c := range rule.Send {
				if !sleepUntil(base.Add(time.Duration(c.AtMs)*time.Millisecond), ctx.Done(), sv.stop) {
					return
				}
				if err := writeChunk(ctx, conn, c); err != nil {
					return
				}
			}
			continue
		}
		select {
		case inbox <- msg:
		default:
			sv.t.Errorf("replay: client message backlog overflow")
		}
	}
}

func (sv *Server) claimRule(msg Message) *WSStep {
	ws := sv.ws
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for _, r := range ws.rules {
		if r.Times > 0 && ws.fired[r] >= r.Times {
			continue
		}
		if matchesRule(r.Match, msg) {
			ws.fired[r]++
			return r
		}
	}
	return nil
}

func matchesRule(match json.RawMessage, msg Message) bool {
	var m map[string]any
	if err := json.Unmarshal(match, &m); err != nil {
		return false
	}
	if b, ok := m["binary"].(bool); ok && len(m) == 1 && b {
		return !msg.Text
	}
	if !msg.Text {
		return false
	}
	var got any
	if err := json.Unmarshal(msg.Data, &got); err != nil {
		return false
	}
	return len(jsonSubset("match", m, got)) == 0
}

func writeChunk(ctx context.Context, conn *websocket.Conn, c Chunk) error {
	data, text, err := c.bytes()
	if err != nil {
		return err
	}
	typ := websocket.MessageBinary
	if text {
		typ = websocket.MessageText
	}
	return conn.Write(ctx, typ, data)
}

func (sv *Server) runSteps(ctx context.Context, conn *websocket.Conn, inbox <-chan Message) {
	ws := sv.ws
	mark := func(st *WSStep) {
		ws.mu.Lock()
		ws.done[st] = true
		ws.mu.Unlock()
	}
	for _, st := range sv.s.WS {
		switch st.Dir {
		case "on":
			ws.mu.Lock()
			ws.rules = append(ws.rules, st)
			ws.mu.Unlock()
			mark(st)
		case "send":
			if !sleepUntil(time.Now().Add(time.Duration(st.AtMs)*time.Millisecond), ctx.Done(), sv.stop) {
				return
			}
			if err := writeChunk(ctx, conn, Chunk{Text: st.Text, B64: st.B64}); err != nil {
				return
			}
			mark(st)
		case "recv":
			timer := time.NewTimer(recvTimeout)
			select {
			case msg := <-inbox:
				timer.Stop()
				for _, p := range matchRecv(st, msg) {
					sv.t.Errorf("replay: line %d (ws recv): %s", st.line, p)
				}
				mark(st)
			case <-timer.C:
				sv.t.Errorf("replay: line %d (ws recv): no client message within %s", st.line, recvTimeout)
				return
			case <-ctx.Done():
				timer.Stop()
				return
			case <-sv.stop:
				timer.Stop()
				return
			}
		case "close":
			mark(st)
			_ = conn.Close(websocket.StatusCode(st.Code), "")
			return
		}
	}
}

func matchRecv(st *WSStep, msg Message) []string {
	if st.Binary {
		if msg.Text {
			return []string{"want binary frame got text"}
		}
		if st.Len > 0 && len(msg.Data) != st.Len {
			return []string{fmt.Sprintf("binary: want %d bytes got %d", st.Len, len(msg.Data))}
		}
		return nil
	}
	if !msg.Text {
		return []string{"want text frame got binary"}
	}
	return matchJSONBody(st.JSON, msg.Data)
}

func (ws *wsState) assertDone(sv *Server) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if !ws.connected {
		sv.t.Errorf("replay: no client ever connected")
		return
	}
	for _, st := range sv.s.WS {
		if st.Dir == "on" {
			if st.Times > 0 && ws.fired[st] < st.Times {
				sv.t.Errorf("replay: line %d (ws on): fired %d of %d times", st.line, ws.fired[st], st.Times)
			}
			continue
		}
		if !ws.done[st] {
			sv.t.Errorf("replay: line %d (ws %s) was never reached", st.line, st.Dir)
		}
	}
}
