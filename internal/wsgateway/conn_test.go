package wsgateway_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/wsgateway"
)

// adapterServer upgrades each request and hands the wrapped connection to fn.
func adapterServer(t *testing.T, fn func(ctx context.Context, c conversation.Conn)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		fn(r.Context(), wsgateway.NewConn(ws))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func dialAdapter(t *testing.T, srv *httptest.Server) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.Dial(ctx5(t), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func TestConnAdapter(t *testing.T) {
	t.Run("read and write keep the kind of the message", func(t *testing.T) {
		testutil.NoGoroutineLeak(t, wsPkg)
		type got struct {
			kind conversation.MessageKind
			data []byte
		}
		recv := make(chan got, 4)
		srv := adapterServer(t, func(ctx context.Context, c conversation.Conn) {
			for range 2 {
				k, b, err := c.Read(ctx)
				if err != nil {
					return
				}
				recv <- got{k, b}
				if err := c.Write(ctx, k, b); err != nil { // echo with the same kind
					return
				}
			}
			_ = c.Close(1000, "done")
		})
		c := dialAdapter(t, srv)
		require.NoError(t, c.Write(ctx5(t), websocket.MessageText, []byte(`{"a":1}`)))
		require.NoError(t, c.Write(ctx5(t), websocket.MessageBinary, []byte{0x01, 0, 0, 0, 0, 9}))

		g := <-recv
		require.Equal(t, conversation.MessageText, g.kind)
		require.Equal(t, `{"a":1}`, string(g.data))
		g = <-recv
		require.Equal(t, conversation.MessageBinary, g.kind)
		require.Equal(t, []byte{0x01, 0, 0, 0, 0, 9}, g.data)

		typ, b, err := c.Read(ctx5(t))
		require.NoError(t, err)
		require.Equal(t, websocket.MessageText, typ)
		require.Equal(t, `{"a":1}`, string(b))
		typ, b, err = c.Read(ctx5(t))
		require.NoError(t, err)
		require.Equal(t, websocket.MessageBinary, typ)
		require.Equal(t, []byte{0x01, 0, 0, 0, 0, 9}, b)
		_, _, err = c.Read(ctx5(t))
		require.Equal(t, websocket.StatusNormalClosure, websocket.CloseStatus(err))
	})

	t.Run("read returns a buffer the caller owns", func(t *testing.T) {
		testutil.NoGoroutineLeak(t, wsPkg)
		bufs := make(chan [][]byte, 1)
		srv := adapterServer(t, func(ctx context.Context, c conversation.Conn) {
			var all [][]byte
			for range 2 {
				_, b, err := c.Read(ctx)
				if err != nil {
					return
				}
				all = append(all, b)
			}
			bufs <- all
			_ = c.Close(1000, "")
		})
		c := dialAdapter(t, srv)
		require.NoError(t, c.Write(ctx5(t), websocket.MessageBinary, []byte("first")))
		require.NoError(t, c.Write(ctx5(t), websocket.MessageBinary, []byte("other")))
		all := <-bufs
		require.Equal(t, "first", string(all[0]), "a later Read does not overwrite the earlier buffer")
		require.Equal(t, "other", string(all[1]))
	})

	t.Run("a read error surfaces when the peer closes", func(t *testing.T) {
		testutil.NoGoroutineLeak(t, wsPkg)
		errc := make(chan error, 1)
		srv := adapterServer(t, func(ctx context.Context, c conversation.Conn) {
			_, _, err := c.Read(ctx)
			errc <- err
		})
		c := dialAdapter(t, srv)
		require.NoError(t, c.Close(websocket.StatusGoingAway, "bye"))
		err := <-errc
		require.Error(t, err)
		require.Equal(t, websocket.StatusGoingAway, websocket.CloseStatus(err))
	})

	t.Run("ping is answered by a client that is reading", func(t *testing.T) {
		testutil.NoGoroutineLeak(t, wsPkg)
		var once sync.Once
		pinged := make(chan error, 1)
		srv := adapterServer(t, func(ctx context.Context, c conversation.Conn) {
			gone := make(chan struct{})
			go func() { // the reader goroutine of the Manager: Ping needs a concurrent Read
				defer close(gone)
				for {
					if _, _, err := c.Read(ctx); err != nil {
						return
					}
				}
			}()
			pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			once.Do(func() { pinged <- c.Ping(pctx) })
			<-gone
		})
		c := dialAdapter(t, srv)
		go func() { _, _, _ = c.Read(context.Background()) }() // answers pings
		require.NoError(t, <-pinged)
	})

	t.Run("ping fails once the peer is gone", func(t *testing.T) {
		testutil.NoGoroutineLeak(t, wsPkg)
		res := make(chan error, 1)
		release := make(chan struct{})
		srv := adapterServer(t, func(ctx context.Context, c conversation.Conn) {
			go func() {
				for {
					if _, _, err := c.Read(ctx); err != nil {
						close(release)
						return
					}
				}
			}()
			<-release
			pctx, cancel := context.WithTimeout(ctx, time.Second)
			defer cancel()
			res <- c.Ping(pctx)
		})
		c := dialAdapter(t, srv)
		_ = c.CloseNow()
		require.Error(t, <-res)
	})

	t.Run("close maps the 4xxx code and the reason", func(t *testing.T) {
		for code, reason := range map[int]string{
			4001: "TICKET_INVALID", 4011: "SESSION_TAKEN_OVER", 4100: "SERVER_DRAINING", 4503: "SERVICE_UNAVAILABLE",
			1008: "PROTOCOL_VIOLATION", 1011: "INTERNAL_ERROR", 1000: "",
		} {
			testutil.NoGoroutineLeak(t, wsPkg)
			srv := adapterServer(t, func(_ context.Context, c conversation.Conn) { require.NoError(t, c.Close(code, reason)) })
			c := dialAdapter(t, srv)
			_, _, err := c.Read(ctx5(t))
			require.Equal(t, websocket.StatusCode(code), websocket.CloseStatus(err), reason)
			var ce websocket.CloseError
			require.ErrorAs(t, err, &ce)
			require.Equal(t, reason, ce.Reason)
		}
	})

	t.Run("a reason over 123 bytes is cut on a rune boundary", func(t *testing.T) {
		testutil.NoGoroutineLeak(t, wsPkg)
		long := "x" + strings.Repeat("あ", 60) // 181 bytes; byte 123 falls inside a rune
		srv := adapterServer(t, func(_ context.Context, c conversation.Conn) { require.NoError(t, c.Close(4100, long)) })
		c := dialAdapter(t, srv)
		_, _, err := c.Read(ctx5(t))
		require.Equal(t, websocket.StatusCode(4100), websocket.CloseStatus(err))
		var ce websocket.CloseError
		require.ErrorAs(t, err, &ce)
		require.LessOrEqual(t, len(ce.Reason), 123)
		require.Equal(t, "x"+strings.Repeat("あ", 40), ce.Reason, "121 bytes: the cut rune is dropped whole")

		ascii := strings.Repeat("x", 500)
		srv2 := adapterServer(t, func(_ context.Context, c conversation.Conn) { require.NoError(t, c.Close(4100, ascii)) })
		c2 := dialAdapter(t, srv2)
		_, _, err = c2.Read(ctx5(t))
		require.ErrorAs(t, err, &ce)
		require.Equal(t, strings.Repeat("x", 123), ce.Reason)
	})
}
