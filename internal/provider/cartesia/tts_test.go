package cartesia_test

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
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/cartesia"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
	"github.com/nabePi/kotoki-backend/internal/provider/providertest"
	"github.com/nabePi/kotoki-backend/internal/testutil"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

func firstBody(t *testing.T, srv *replay.Server) map[string]any {
	t.Helper()
	msgs := waitMsgs(t, srv, 1)
	var m map[string]any
	require.NoError(t, json.Unmarshal(msgs[0].Data, &m))
	return m
}

func TestCartesiaTTSRequest(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "tts_basic"))
	tts := newTTS(t, srv.URL, nil)
	require.Equal(t, provider.Cartesia, tts.Name())
	st, err := tts.Synthesize(ctx, ttsReq())
	require.NoError(t, err)
	chunks := drainTTS(t, st.Chunks())
	u := srv.UpgradeRequest()
	require.Equal(t, "/tts/websocket", u.Path)
	require.Equal(t, testKey, u.Header.Get("X-API-Key"))
	require.Equal(t, "v1", u.Query.Get("cartesia_version"))
	require.Empty(t, u.Header.Get("Cartesia-Version"), "the version travels as a query parameter (Step 0b)")
	msgs := waitMsgs(t, srv, 1)
	require.JSONEq(t, `{"model_id":"M1","transcript":"こんにちは。","voice":"V1","language":"ja","context_id":"sid:1:0","continue":false,
		"output_format":{"container":"raw","encoding":"pcm_s16le","sample_rate":24000},"generation_config":{"speed":1.0}}`, string(msgs[0].Data))
	require.Len(t, chunks, 4)
	for _, c := range chunks[:3] {
		require.Len(t, c.PCM, 4800)
		require.Equal(t, provider.Cartesia, c.Provider)
		require.Equal(t, "M1", c.Model)
		require.False(t, c.ReceivedAt.IsZero())
	}
	last := chunks[3]
	require.True(t, last.Final)
	require.Empty(t, last.PCM)
	require.Equal(t, 6, last.Usage.Characters)
	require.EqualValues(t, 300, last.Usage.AudioMs) // 14.400 bytes / 48
	require.Equal(t, totalBytes(chunks)/48, int(last.Usage.AudioMs))
}

func TestCartesiaTTSNoLocaleWithLanguage(t *testing.T) {
	srv := replay.NewWSServer(t, ttsScript(t, onRule(matchGenerate, chunk(0, doneMsg("sid:1:0")))))
	st, err := newTTS(t, srv.URL, nil).Synthesize(ctx, ttsReq())
	require.NoError(t, err)
	drainTTS(t, st.Chunks())
	body := firstBody(t, srv)
	require.Equal(t, "ja", body["language"])
	require.NotContains(t, body, "locale")
	// an empty language is omitted instead of sent empty
	srv2 := replay.NewWSServer(t, ttsScript(t, onRule(matchGenerate, chunk(0, doneMsg("sid:1:0")))))
	req := ttsReq()
	req.Language = ""
	st, err = newTTS(t, srv2.URL, nil).Synthesize(ctx, req)
	require.NoError(t, err)
	drainTTS(t, st.Chunks())
	b2 := firstBody(t, srv2)
	require.NotContains(t, b2, "language")
	require.NotContains(t, b2, "locale")
}

func TestCartesiaTTSModelPassedVerbatim(t *testing.T) {
	srv := replay.NewWSServer(t, ttsScript(t, onRule(matchGenerate, chunk(0, doneMsg("sid:1:0")))))
	req := ttsReq()
	req.Model = "sonic3.6" // not normalised to a hyphenated name
	req.VoiceID = "voice-from-character"
	st, err := newTTS(t, srv.URL, nil).Synthesize(ctx, req)
	require.NoError(t, err)
	drainTTS(t, st.Chunks())
	body := firstBody(t, srv)
	require.Equal(t, "sonic3.6", body["model_id"])
	require.Equal(t, "voice-from-character", body["voice"])
}

func TestCartesiaTTSEmptyModelNotGuessed(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "tts_error_model_not_found"))
	req := ttsReq()
	req.Model = ""
	st, err := newTTS(t, srv.URL, nil).Synthesize(ctx, req)
	require.NoError(t, err)
	chunks := drainTTS(t, st.Chunks())
	require.Equal(t, "", firstBody(t, srv)["model_id"], "sent empty, no default model")
	require.Len(t, chunks, 1)
	e := chunks[0].Err
	require.NotNil(t, e)
	require.Equal(t, provider.KindInvalidRequest, e.Kind)
	require.Equal(t, "model_not_found", e.VendorCode)
	require.Equal(t, 400, e.HTTPStatus)
	require.False(t, e.Retryable)
	require.False(t, chunks[0].Final)
}

func TestCartesiaTTSOnlySpeedInGenerationConfig(t *testing.T) {
	for _, c := range []struct{ in, want float64 }{{0, 1.0}, {0.80, 0.8}, {1.05, 1.05}, {0.5, 0.6}, {2, 1.5}, {0.6, 0.6}, {1.5, 1.5}} {
		t.Run(fmt.Sprint(c.in), func(t *testing.T) {
			srv := replay.NewWSServer(t, ttsScript(t, onRule(matchGenerate, chunk(0, doneMsg("sid:1:0")))))
			req := ttsReq()
			req.SpeakingRate = c.in
			req.PreviousText = "前の文。"
			st, err := newTTS(t, srv.URL, nil).Synthesize(ctx, req)
			require.NoError(t, err)
			drainTTS(t, st.Chunks())
			body := firstBody(t, srv)
			require.Equal(t, map[string]any{"speed": c.want}, body["generation_config"])
			for _, k := range []string{"volume", "emotion", "previous_text", "pronunciation_dict_id", "add_timestamps", "max_buffer_delay_ms"} {
				require.NotContains(t, body, k)
			}
		})
	}
}

func TestCartesiaTTSDecodesBase64Chunks(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "tts_odd_bytes"))
	st, err := newTTS(t, srv.URL, nil).Synthesize(ctx, ttsReq())
	require.NoError(t, err)
	chunks := drainTTS(t, st.Chunks())
	for i, c := range chunks {
		require.Zero(t, len(c.PCM)%2, "chunk %d odd", i)
		if !c.Final {
			require.NotEmpty(t, c.PCM, "an empty vendor frame yields no chunk")
		}
	}
	require.Equal(t, 2000, totalBytes(chunks)) // 1001 + 999, the held byte is not lost
	require.True(t, chunks[len(chunks)-1].Final)
	require.EqualValues(t, 2000/48, chunks[len(chunks)-1].Usage.AudioMs)
}

func TestCartesiaTTSBadBase64IsInvalidResponse(t *testing.T) {
	srv := replay.NewWSServer(t, ttsScript(t, onRule(matchGenerate,
		chunk(0, `{"type":"chunk","data":"!!not base64!!","context_id":"sid:1:0"}`))))
	st, err := newTTS(t, srv.URL, nil).Synthesize(ctx, ttsReq())
	require.NoError(t, err)
	chunks := drainTTS(t, st.Chunks())
	require.Len(t, chunks, 1)
	require.Equal(t, provider.KindInvalidResponse, chunks[0].Err.Kind)
}

func TestCartesiaTTSDoneBecomesFinalChunk(t *testing.T) {
	srv := replay.NewWSServer(t, ttsScript(t, onRule(matchGenerate,
		chunk(0, chunkMsg("sid:1:0", 960)),
		chunk(0, `{"type":"flush_done","done":false,"flush_done":true,"flush_id":1,"status_code":206,"context_id":"sid:1:0"}`),
		chunk(0, `{"type":"timestamps","done":false,"status_code":206,"context_id":"sid:1:0","word_timestamps":{"words":["a"],"start":[0],"end":[1]}}`),
		chunk(0, `{"type":"phoneme_timestamps","done":false,"status_code":206,"context_id":"sid:1:0"}`),
		chunk(0, doneMsg("sid:1:0")),
		chunk(5, chunkMsg("sid:1:0", 960)))))
	st, err := newTTS(t, srv.URL, nil).Synthesize(ctx, ttsReq())
	require.NoError(t, err)
	chunks := drainTTS(t, st.Chunks())
	require.Len(t, chunks, 2)
	require.Len(t, chunks[0].PCM, 960)
	require.True(t, chunks[1].Final)
	require.EqualValues(t, 20, chunks[1].Usage.AudioMs)
}

func TestCartesiaTTSIgnoresOtherContexts(t *testing.T) {
	srv := replay.NewWSServer(t, ttsScript(t, onRule(matchGenerate,
		chunk(0, chunkMsg("other", 960)), chunk(0, doneMsg("other")),
		chunk(0, chunkMsg("sid:1:0", 480)), chunk(0, doneMsg("sid:1:0")))))
	st, err := newTTS(t, srv.URL, nil).Synthesize(ctx, ttsReq())
	require.NoError(t, err)
	chunks := drainTTS(t, st.Chunks())
	require.Len(t, chunks, 2)
	require.Len(t, chunks[0].PCM, 480)
}

func wsErrMsg(status int, code string) string {
	m := `{"type":"error","status_code":` + itoa(status) + `,"title":"T","message":"vendor message","context_id":"sid:1:0"`
	if code != "" {
		m += `,"error_code":"` + code + `"`
	}
	return m + `}`
}

func TestCartesiaTTSErrors(t *testing.T) {
	cases := []struct {
		status int
		code   string
		kind   provider.ErrorKind
		retry  bool
	}{
		{400, "model_not_found", provider.KindInvalidRequest, false},
		{404, "voice_not_found", provider.KindInvalidRequest, false},
		{422, "", provider.KindInvalidRequest, false},
		{401, "", provider.KindAuth, false},
		{403, "forbidden", provider.KindAuth, false},
		{429, "rate_limit_exceeded", provider.KindRateLimited, true},
		{429, "insufficient_credits", provider.KindRateLimited, false}, // GUESS: quota code hints
		{500, "internal", provider.KindUnavailable, true},
		{503, "", provider.KindUnavailable, true},
	}
	for _, c := range cases {
		t.Run(itoa(c.status)+"/"+c.code, func(t *testing.T) {
			srv := replay.NewWSServer(t, ttsScript(t, onRule(matchGenerate, chunk(0, wsErrMsg(c.status, c.code)))))
			st, err := newTTS(t, srv.URL, nil).Synthesize(ctx, ttsReq())
			require.NoError(t, err)
			chunks := drainTTS(t, st.Chunks())
			require.Len(t, chunks, 1)
			e := chunks[0].Err
			require.NotNil(t, e)
			require.Equal(t, c.kind, e.Kind)
			require.Equal(t, c.retry, e.Retryable)
			require.Equal(t, c.code, e.VendorCode)
			require.Equal(t, c.status, e.HTTPStatus)
			require.Equal(t, provider.CapTTS, e.Capability)
			require.Equal(t, "vendor message", e.Message)
		})
	}
}

func TestCartesiaTTSErrorFixtures(t *testing.T) {
	for name, want := range map[string]provider.ErrorKind{"tts_error_401": provider.KindAuth, "tts_error_429": provider.KindRateLimited} {
		t.Run(name, func(t *testing.T) {
			srv := replay.NewWSServer(t, recording(t, name))
			st, err := newTTS(t, srv.URL, nil).Synthesize(ctx, ttsReq())
			require.NoError(t, err)
			chunks := drainTTS(t, st.Chunks())
			require.Equal(t, want, chunks[len(chunks)-1].Err.Kind)
		})
	}
}

func TestCartesiaTTSHandshakeErrorsAreSynchronous(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		kind   provider.ErrorKind
		code   string
	}{
		{401, `{"status_code":401,"title":"Unauthorized","message":"bad key ` + testKey + `","error_code":"bad_key"}`, provider.KindAuth, "bad_key"},
		{403, ``, provider.KindAuth, ""},
		{503, `not json`, provider.KindUnavailable, ""},
	} {
		hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.status)
			_, _ = w.Write([]byte(tc.body))
		}))
		lb := &logBuf{}
		tts, err := cartesia.NewTTS(cartesia.Config{APIKey: testKey, WSBaseURL: "ws" + hs.URL[len("http"):], Version: "v1"}, httpx.NewClient(), bufLogger(lb))
		require.NoError(t, err)
		st, err := tts.Synthesize(ctx, ttsReq())
		require.Nil(t, st)
		var pe *provider.Error
		require.ErrorAs(t, err, &pe)
		require.Equal(t, tc.kind, pe.Kind)
		require.Equal(t, tc.status, pe.HTTPStatus)
		require.Equal(t, tc.code, pe.VendorCode)
		require.NotContains(t, pe.Message, testKey)
		require.NotContains(t, lb.String(), testKey)
		hs.Close()
		_ = tts.Close()
	}
}

func TestCartesiaTTSMidStreamError(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "tts_mid_error"))
	st, err := newTTS(t, srv.URL, nil).Synthesize(ctx, ttsReq())
	require.NoError(t, err)
	chunks := drainTTS(t, st.Chunks())
	require.GreaterOrEqual(t, len(chunks), 2)
	last := chunks[len(chunks)-1]
	require.NotNil(t, last.Err)
	require.Equal(t, provider.KindUnavailable, last.Err.Kind)
	require.True(t, last.Err.Retryable)
	require.Equal(t, "ws_close_1011", last.Err.VendorCode)
	require.False(t, last.Final)
	for _, c := range chunks[:len(chunks)-1] {
		require.Nil(t, c.Err)
	}
}

func TestCartesiaTTSCancelSendsCancelAndKeepsConn(t *testing.T) {
	srv := newFakeSrv(t, func(c *srvConn, m map[string]any) {
		id, _ := m["context_id"].(string)
		switch {
		case m["cancel"] == true:
			c.send(t, doneMsg(id)) // the vendor ends the cancelled context
		case m["transcript"] == "長い文。":
			go func() { // the read loop must stay free to see the cancel
				for i := 0; i < 5; i++ {
					c.send(t, chunkMsg(id, 4800))
					time.Sleep(20 * time.Millisecond)
				}
			}()
		default:
			replyAudio(t, 2, 960)(c, m)
		}
	})
	tts := newTTS(t, srv.URL, func(c *cartesia.Config) { c.PoolSize = 1; c.CancelGrace = 500 * time.Millisecond })
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req := ttsReq()
	req.Text = "長い文。"
	req.RequestID = "sid:1:0"
	st, err := tts.Synthesize(cctx, req)
	require.NoError(t, err)
	select {
	case <-st.Chunks():
	case <-time.After(3 * time.Second):
		t.Fatal("no first chunk")
	}
	start := time.Now()
	cancel()
	for c := range st.Chunks() {
		require.False(t, c.Final)
		require.Nil(t, c.Err)
	}
	require.Less(t, time.Since(start), 50*time.Millisecond)
	cm := srv.waitMsg(isCancel)
	require.Less(t, cm.At.Sub(start), 50*time.Millisecond, "cancel reaches the vendor within 50 ms")
	require.Equal(t, "sid:1:0", cm.Body["context_id"])
	// the next sentence (PoolSize 1: it waits for the drain) reuses the same connection
	req2 := ttsReq()
	req2.RequestID = "sid:1:1"
	st2, err := tts.Synthesize(ctx, req2)
	require.NoError(t, err)
	chunks := drainTTS(t, st2.Chunks())
	require.True(t, chunks[len(chunks)-1].Final)
	require.Equal(t, 1, srv.connCount(), "a cancel the vendor ended keeps the connection")
}

func TestCartesiaTTSCancelFixtureAck(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "tts_cancel_ack"))
	cctx, cancel := context.WithCancel(ctx)
	st, err := newTTS(t, srv.URL, nil).Synthesize(cctx, ttsReq())
	require.NoError(t, err)
	<-st.Chunks()
	cancel()
	for range st.Chunks() {
	}
	msgs := waitMsgs(t, srv, 2)
	var m map[string]any
	require.NoError(t, json.Unmarshal(msgs[1].Data, &m))
	require.Equal(t, map[string]any{"context_id": "sid:1:0", "cancel": true}, m)
}

func TestCartesiaTTSCancelWithin50ms(t *testing.T) {
	// the vendor ignores cancel and never finishes: no chunk after cancel, the connection is
	// closed after CancelGrace and the next sentence runs on a fresh one
	srv := newFakeSrv(t, func(c *srvConn, m map[string]any) {
		id, _ := m["context_id"].(string)
		if m["transcript"] == "ハング。" {
			c.send(t, chunkMsg(id, 4800))
			return // hang
		}
		replyAudio(t, 1, 960)(c, m)
	})
	tts := newTTS(t, srv.URL, func(c *cartesia.Config) { c.PoolSize = 1; c.CancelGrace = 40 * time.Millisecond })
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req := ttsReq()
	req.Text = "ハング。"
	st, err := tts.Synthesize(cctx, req)
	require.NoError(t, err)
	<-st.Chunks()
	start := time.Now()
	cancel()
	for c := range st.Chunks() {
		t.Fatalf("chunk after cancel: %+v", c)
	}
	require.Less(t, time.Since(start), 50*time.Millisecond)
	req2 := ttsReq()
	req2.RequestID = "sid:1:1"
	st2, err := tts.Synthesize(ctx, req2)
	require.NoError(t, err)
	chunks := drainTTS(t, st2.Chunks())
	require.True(t, chunks[len(chunks)-1].Final)
	require.Equal(t, 2, srv.connCount(), "the hung connection was replaced")
}

func TestCartesiaTTSStreamCloseCancels(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "tts_hang"))
	st, err := newTTS(t, srv.URL, nil).Synthesize(ctx, ttsReq())
	require.NoError(t, err)
	<-st.Chunks()
	require.NoError(t, st.Close())
	require.NoError(t, st.Close())
	start := time.Now()
	for range st.Chunks() {
	}
	require.Less(t, time.Since(start), 50*time.Millisecond)
}

func TestCartesiaTTSReusesConnectionAcrossSentences(t *testing.T) {
	srv := newFakeSrv(t, replyAudio(t, 2, 960))
	tts := newTTS(t, srv.URL, nil)
	for i := 0; i < 3; i++ {
		req := ttsReq()
		req.RequestID = fmt.Sprintf("sid:1:%d", i)
		st, err := tts.Synthesize(ctx, req)
		require.NoError(t, err)
		chunks := drainTTS(t, st.Chunks())
		require.True(t, chunks[len(chunks)-1].Final)
	}
	require.Equal(t, 1, srv.connCount())
	require.Equal(t, "v1", srv.queries[0].Get("cartesia_version"))
}

func TestCartesiaTTSErrorKeepsConnectionForNextSentence(t *testing.T) {
	srv := newFakeSrv(t, func(c *srvConn, m map[string]any) {
		id, _ := m["context_id"].(string)
		if id == "sid:1:0" {
			c.send(t, `{"type":"error","status_code":400,"title":"T","message":"m","error_code":"bad","context_id":"sid:1:0"}`)
			return
		}
		replyAudio(t, 1, 960)(c, m)
	})
	tts := newTTS(t, srv.URL, nil)
	st, err := tts.Synthesize(ctx, ttsReq())
	require.NoError(t, err)
	require.NotNil(t, drainTTS(t, st.Chunks())[0].Err)
	req := ttsReq()
	req.RequestID = "sid:1:1"
	st, err = tts.Synthesize(ctx, req)
	require.NoError(t, err)
	chunks := drainTTS(t, st.Chunks())
	require.True(t, chunks[len(chunks)-1].Final)
	require.Equal(t, 1, srv.connCount())
}

func TestCartesiaPoolBoundsConcurrency(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	active, peak := 0, 0
	srv := newFakeSrv(t, func(c *srvConn, m map[string]any) {
		id, _ := m["context_id"].(string)
		if _, ok := m["transcript"]; !ok {
			return
		}
		mu.Lock()
		active++
		peak = max(peak, active)
		mu.Unlock()
		c.send(t, chunkMsg(id, 960))
		<-release
		mu.Lock()
		active--
		mu.Unlock()
		c.send(t, doneMsg(id))
	})
	tts := newTTS(t, srv.URL, func(c *cartesia.Config) { c.PoolSize = 2 })
	var streams []provider.TTSStream
	for i := 0; i < 2; i++ {
		req := ttsReq()
		req.RequestID = fmt.Sprintf("sid:1:%d", i)
		st, err := tts.Synthesize(ctx, req)
		require.NoError(t, err)
		streams = append(streams, st)
	}
	// the third waits for a slot and gives up cleanly when its context ends
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	req := ttsReq()
	req.RequestID = "sid:1:2"
	start := time.Now()
	st3, err := tts.Synthesize(cctx, req)
	require.Nil(t, st3)
	require.Equal(t, provider.KindTimeout, provider.KindOf(err))
	require.GreaterOrEqual(t, time.Since(start), 90*time.Millisecond)
	// a freed slot lets a waiting call through
	got := make(chan error, 1)
	go func() {
		r := ttsReq()
		r.RequestID = "sid:1:3"
		s, e := tts.Synthesize(ctx, r)
		if e == nil {
			drainTTS(t, s.Chunks())
		}
		got <- e
	}()
	select {
	case <-got:
		t.Fatal("the fourth call ran while both slots were busy")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	for _, s := range streams {
		drainTTS(t, s.Chunks())
	}
	require.NoError(t, <-got)
	require.Equal(t, 2, srv.connCount())
	mu.Lock()
	defer mu.Unlock()
	require.LessOrEqual(t, peak, 2)
}

func TestCartesiaPoolOneActiveContextPerConnection(t *testing.T) {
	release := make(chan struct{})
	var mu sync.Mutex
	perConn := map[int]int{}
	maxPer := 0
	srv := newFakeSrv(t, func(c *srvConn, m map[string]any) {
		id, _ := m["context_id"].(string)
		if _, ok := m["transcript"]; !ok {
			return
		}
		mu.Lock()
		perConn[c.Idx]++
		maxPer = max(maxPer, perConn[c.Idx])
		mu.Unlock()
		<-release
		mu.Lock()
		perConn[c.Idx]--
		mu.Unlock()
		c.send(t, doneMsg(id))
	})
	tts := newTTS(t, srv.URL, func(c *cartesia.Config) { c.PoolSize = 3 })
	var streams []provider.TTSStream
	for i := 0; i < 3; i++ {
		req := ttsReq()
		req.RequestID = fmt.Sprintf("sid:1:%d", i)
		st, err := tts.Synthesize(ctx, req)
		require.NoError(t, err)
		streams = append(streams, st)
	}
	require.Eventually(t, func() bool { return srv.connCount() == 3 }, 2*time.Second, 5*time.Millisecond)
	close(release)
	for _, s := range streams {
		drainTTS(t, s.Chunks())
	}
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, maxPer)
}

func TestCartesiaPoolReconnectsAfterIdleClose(t *testing.T) {
	closers := make(chan func(), 1)
	srv := newFakeSrv(t, func(c *srvConn, m map[string]any) {
		replyAudio(t, 1, 960)(c, m)
		if c.Idx == 0 {
			select {
			case closers <- func() { _ = c.ws.Close(websocket.StatusGoingAway, "idle") }:
			default:
			}
		}
	})
	tts := newTTS(t, srv.URL, nil)
	st, err := tts.Synthesize(ctx, ttsReq())
	require.NoError(t, err)
	drainTTS(t, st.Chunks())
	(<-closers)() // the vendor closes the idle connection
	time.Sleep(50 * time.Millisecond)
	req := ttsReq()
	req.RequestID = "sid:1:1"
	st, err = tts.Synthesize(ctx, req)
	require.NoError(t, err)
	chunks := drainTTS(t, st.Chunks())
	require.Nil(t, chunks[len(chunks)-1].Err, "no error reaches the caller")
	require.True(t, chunks[len(chunks)-1].Final)
	require.Equal(t, 2, srv.connCount())
}

func TestCartesiaPoolRetriesStaleConnectionOnce(t *testing.T) {
	// the idle close is not noticed before the next write: the socket dies without answering
	// that sentence, which is retried once on a fresh connection
	var first sync.Once
	srv := newFakeSrv(t, func(c *srvConn, m map[string]any) {
		if c.Idx == 0 {
			var firstCall bool
			first.Do(func() { firstCall = true })
			if !firstCall {
				_ = c.ws.Close(websocket.StatusGoingAway, "idle") // dies instead of answering
				return
			}
		}
		replyAudio(t, 1, 960)(c, m)
	})
	tts := newTTS(t, srv.URL, nil)
	st, err := tts.Synthesize(ctx, ttsReq())
	require.NoError(t, err)
	drainTTS(t, st.Chunks())
	req := ttsReq()
	req.RequestID = "sid:1:1"
	st, err = tts.Synthesize(ctx, req)
	require.NoError(t, err)
	chunks := drainTTS(t, st.Chunks())
	require.Nil(t, chunks[len(chunks)-1].Err)
	require.True(t, chunks[len(chunks)-1].Final)
	require.Equal(t, 2, srv.connCount())
}

func TestCartesiaTTSContextIDFallback(t *testing.T) {
	srv := newFakeSrv(t, replyAudio(t, 1, 960))
	tts := newTTS(t, srv.URL, nil)
	ids := map[string]bool{}
	for i := 0; i < 2; i++ {
		req := ttsReq()
		req.RequestID = ""
		st, err := tts.Synthesize(ctx, req)
		require.NoError(t, err)
		require.True(t, drainTTS(t, st.Chunks())[1].Final)
	}
	for _, m := range srv.messages() {
		id, _ := m.Body["context_id"].(string)
		require.NotEmpty(t, id)
		ids[id] = true
	}
	require.Len(t, ids, 2)
}

func TestCartesiaTTSNewTTSValidationAndDefaults(t *testing.T) {
	_, err := cartesia.NewTTS(cartesia.Config{}, nil, nil)
	require.Error(t, err)
	require.Error(t, cartesia.Config{}.Validate())
	require.NoError(t, cartesia.Config{APIKey: testKey}.Validate())
	srv := newFakeSrv(t, replyAudio(t, 1, 960))
	tts, err := cartesia.NewTTS(cartesia.Config{APIKey: testKey, WSBaseURL: srv.URL}, nil, nil)
	require.NoError(t, err)
	defer func() { _ = tts.Close() }()
	st, err := tts.Synthesize(ctx, ttsReq())
	require.NoError(t, err)
	drainTTS(t, st.Chunks())
	require.Equal(t, "2026-08-14", srv.queries[0].Get("cartesia_version"))
}

func TestCartesiaTTSNoSecretInLogsOrErrors(t *testing.T) {
	lb := &logBuf{}
	srv := replay.NewWSServer(t, ttsScript(t, onRule(matchGenerate,
		chunk(0, `{"type":"error","status_code":401,"title":"T","message":"key `+testKey+` rejected","error_code":"`+testKey+`","context_id":"sid:1:0"}`))))
	tts, err := cartesia.NewTTS(cartesia.Config{APIKey: testKey, WSBaseURL: srv.URL, Version: "v1"}, httpx.NewClient(), bufLogger(lb))
	require.NoError(t, err)
	defer func() { _ = tts.Close() }()
	st, err := tts.Synthesize(ctx, ttsReq())
	require.NoError(t, err)
	chunks := drainTTS(t, st.Chunks())
	e := chunks[len(chunks)-1].Err
	require.NotNil(t, e)
	require.NotContains(t, e.Message, testKey)
	require.NotContains(t, e.VendorCode, testKey)
	require.NotContains(t, e.Error(), testKey)
	require.NotContains(t, lb.String(), testKey)
	require.Contains(t, lb.String(), "provider call failed")
	require.NotContains(t, lb.String(), "こんにちは", "no user text in logs")
}

func TestCartesiaTTSSpan(t *testing.T) {
	tr := testutil.NewTraceRecorder(t)
	srv := replay.NewWSServer(t, recording(t, "tts_basic"))
	st, err := newTTS(t, srv.URL, nil).Synthesize(ctx, ttsReq())
	require.NoError(t, err)
	drainTTS(t, st.Chunks())
	attrs := spanAttrs(tr, "provider.TTS")
	require.Equal(t, "CARTESIA", attrs["provider.name"])
	require.Equal(t, "M1", attrs["provider.model"])
	require.Equal(t, "success", attrs["provider.outcome"])
}

func TestCartesiaTTSSpanOnError(t *testing.T) {
	tr := testutil.NewTraceRecorder(t)
	srv := replay.NewWSServer(t, recording(t, "tts_error_429"))
	st, err := newTTS(t, srv.URL, nil).Synthesize(ctx, ttsReq())
	require.NoError(t, err)
	drainTTS(t, st.Chunks())
	attrs := spanAttrs(tr, "provider.TTS")
	require.Equal(t, "error", attrs["provider.outcome"])
	require.Equal(t, "RATE_LIMITED", attrs["provider.error_kind"])
}

func TestCartesiaTTSConformance(t *testing.T) {
	providertest.TTSSuite(t, func(t *testing.T) provider.TTSProvider {
		return newTTS(t, replay.NewWSServer(t, recording(t, "tts_suite")).URL, nil)
	}, ttsReq)
}
