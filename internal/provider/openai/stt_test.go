package openai_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
	"github.com/nabePi/kotoki-backend/internal/provider/openai"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

func newSTT(t *testing.T, srv *replay.Server, mod func(*openai.Config)) *openai.STT {
	t.Helper()
	cfg := openai.Config{APIKey: "sk-test", WSBaseURL: srv.URL, STTModel: "m_stt"}
	if mod != nil {
		mod(&cfg)
	}
	stt, err := openai.NewSTT(cfg, httpx.NewClient(), bufLogger(&logBuf{}))
	require.NoError(t, err)
	return stt
}

func sttCfg() provider.STTSessionConfig {
	return provider.STTSessionConfig{Language: "ja", SampleRate: provider.STTSampleRate}
}

func openSession(t *testing.T, srv *replay.Server, mod func(*openai.Config)) provider.STTSession {
	t.Helper()
	sess, err := newSTT(t, srv, mod).Open(ctx, sttCfg())
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// sttScript builds an inline fixture: meta, upgrade check, created, the session.update recv,
// then the given lines (which must end with the session.updated send).
func sttScript(t *testing.T, lines ...string) *replay.Script {
	t.Helper()
	head := []string{
		`{"meta":{"vendor":"OPENAI","capability":"STT","source":"inline test fixture","verified":false}}`,
		`{"ws":{"dir":"send","text":"{\"type\":\"session.created\"}"}}`,
		`{"ws":{"dir":"recv","json":{"type":"session.update"}}}`,
	}
	return replay.LoadScriptString(t, strings.Join(append(head, lines...), "\n"))
}

const updatedLine = `{"ws":{"dir":"send","text":"{\"type\":\"session.updated\"}"}}`

func onRule(match string, sends ...string) string {
	return `{"ws":{"dir":"on","match":` + match + `,"send":[` + strings.Join(sends, ",") + `]}}`
}

func chunk(at int, v any) string {
	b, _ := json.Marshal(v)
	return `{"at_ms":` + itoa(at) + `,"text":` + quote(string(b)) + `}`
}

const (
	matchAppend = `{"type":"input_audio_buffer.append"}`
	matchCommit = `{"type":"input_audio_buffer.commit"}`
)

func evCommitted(id string) map[string]any {
	return map[string]any{"type": "input_audio_buffer.committed", "item_id": id}
}

func evCompleted(id, text string) map[string]any {
	return map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": id, "transcript": text}
}

func evDelta(id, d string) map[string]any {
	return map[string]any{"type": "conversation.item.input_audio_transcription.delta", "item_id": id, "delta": d}
}

func nextEv(t *testing.T, sess provider.STTSession) provider.STTEvent {
	t.Helper()
	select {
	case ev, ok := <-sess.Events():
		require.True(t, ok, "events closed")
		return ev
	case <-time.After(3 * time.Second):
		t.Fatal("no STT event")
	}
	return provider.STTEvent{}
}

func waitFinal(t *testing.T, sess provider.STTSession) provider.STTEvent {
	t.Helper()
	for {
		ev := nextEv(t, sess)
		require.NotEqual(t, provider.STTError, ev.Type, "unexpected ERROR: %v", ev.Err)
		if ev.Type == provider.STTFinal {
			return ev
		}
	}
}

func expectNoEvent(t *testing.T, sess provider.STTSession, d time.Duration) {
	t.Helper()
	select {
	case ev := <-sess.Events():
		t.Fatalf("unexpected event %+v", ev)
	case <-time.After(d):
	}
}

func clientTypes(srv *replay.Server) []string {
	var out []string
	for _, m := range srv.ClientMessages() {
		var v struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(m.Data, &v)
		out = append(out, v.Type)
	}
	return out
}

func appendMsgs(srv *replay.Server) []replay.Message {
	var out []replay.Message
	for _, m := range srv.ClientMessages() {
		if strings.Contains(string(m.Data), `"input_audio_buffer.append"`) {
			out = append(out, m)
		}
	}
	return out
}

func b64len(t *testing.T, m replay.Message) int {
	t.Helper()
	var v struct {
		Audio string `json:"audio"`
	}
	require.NoError(t, json.Unmarshal(m.Data, &v))
	b, err := base64.StdEncoding.DecodeString(v.Audio)
	require.NoError(t, err)
	return len(b)
}

func sendPCM(t *testing.T, sess provider.STTSession, frames int) {
	t.Helper()
	for i := 0; i < frames; i++ {
		require.NoError(t, sess.SendAudio(make([]byte, 640)))
	}
}

func TestOpenAISTTOpenHandshakeAndSessionUpdate(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	stt := newSTT(t, srv, nil)
	require.Equal(t, provider.OpenAI, stt.Name())
	require.Equal(t, "m_stt", stt.Model())
	sess, err := stt.Open(ctx, sttCfg())
	require.NoError(t, err)
	defer func() { _ = sess.Close() }()
	u := srv.UpgradeRequest()
	require.Equal(t, "Bearer sk-test", u.Header.Get("Authorization"))
	require.Empty(t, u.Header.Get("OpenAI-Beta"))
	require.Equal(t, "transcription", u.Query.Get("intent"))
	require.JSONEq(t, `{"type":"session.update","session":{"type":"transcription","audio":{"input":{"format":{"type":"audio/pcm","rate":24000},"transcription":{"model":"m_stt","languages":["ja"]},"turn_detection":null}}}}`, string(srv.ClientMessages()[0].Data))
}

func TestOpenAISTTSingleLanguageForm(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	openSession(t, srv, func(c *openai.Config) { c.STTSingleLanguage = true })
	require.Contains(t, string(srv.ClientMessages()[0].Data), `"transcription":{"language":"ja","model":"m_stt"}`)
}

func TestOpenAISTTResamplesBatchesAndCommits(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 3)
	require.NoError(t, sess.Finalize())
	ev := waitFinal(t, sess)
	require.True(t, ev.FromFinalize)
	require.Equal(t, "日本語の勉強", ev.Text)
	msgs := appendMsgs(srv)
	require.Len(t, msgs, 2)
	require.Equal(t, 1920, b64len(t, msgs[0])) // 2 x 20 ms at 24 kHz
	require.Equal(t, 960, b64len(t, msgs[1]))  // the remaining 20 ms, flushed by Finalize
	types := clientTypes(srv)
	require.Equal(t, "input_audio_buffer.commit", types[len(types)-1])
	require.Equal(t, provider.OpenAI, ev.Provider)
	require.Equal(t, int64(0), ev.StartMs)
	require.Equal(t, int64(60), ev.EndMs)
}

func TestOpenAISTTPartialIsCumulativePerItem(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_partial_cumulative"))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 2)
	require.NoError(t, sess.Finalize())
	var got []string
	var evs []provider.STTEvent
	for len(evs) < 6 {
		evs = append(evs, nextEv(t, sess))
	}
	for _, e := range evs {
		got = append(got, string(e.Type)+":"+e.Text)
	}
	require.Equal(t, []string{"PARTIAL:日本", "PARTIAL:日本語の", "PARTIAL:日本語の勉強", "FINAL:日本語の勉強", "PARTIAL:今日", "FINAL:今日"}, got)
	for _, e := range evs[:3] {
		require.Nil(t, e.Confidence)
		require.Equal(t, int64(0), e.StartMs)
		require.Equal(t, int64(40), e.EndMs) // 1.280 bytes of 16 kHz audio, not 24 kHz
	}
	require.True(t, evs[3].FromFinalize)
	require.False(t, evs[5].FromFinalize) // second item: no commit pending
	require.Equal(t, int64(40), evs[5].StartMs)
}

func TestOpenAISTTFinalizeSyntheticEmptyFinal(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_commit_empty"))
	sess := openSession(t, srv, func(c *openai.Config) { c.SyntheticFinalDelay = 30 * time.Millisecond })
	for i := 0; i < 3; i++ {
		sendPCM(t, sess, 2)
		require.NoError(t, sess.Finalize())
	}
	for i := 0; i < 3; i++ {
		ev := waitFinal(t, sess)
		require.Empty(t, ev.Text)
		require.True(t, ev.FromFinalize)
	}
	expectNoEvent(t, sess, 150*time.Millisecond)
}

func TestOpenAISTTSyntheticDefaultDelayIs400ms(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_commit_empty"))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 2)
	start := time.Now()
	require.NoError(t, sess.Finalize())
	waitFinal(t, sess)
	require.GreaterOrEqual(t, time.Since(start), 380*time.Millisecond)
}

func TestOpenAISTTLateCompletedAfterSyntheticFinal(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t,
		onRule(matchAppend),
		onRule(matchCommit, chunk(0, evCommitted("item_1")), chunk(150, evCompleted("item_1", "遅い"))),
		updatedLine))
	sess := openSession(t, srv, func(c *openai.Config) { c.SyntheticFinalDelay = 30 * time.Millisecond })
	sendPCM(t, sess, 2)
	require.NoError(t, sess.Finalize())
	first := waitFinal(t, sess)
	require.True(t, first.FromFinalize)
	require.Empty(t, first.Text)
	late := waitFinal(t, sess)
	require.False(t, late.FromFinalize, "the Finalize was already answered")
	require.Equal(t, "遅い", late.Text)
}

func TestOpenAISTTFinalizeWithoutAudioSkipsCommit(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	sess := openSession(t, srv, nil)
	require.NoError(t, sess.Finalize())
	ev := waitFinal(t, sess)
	require.True(t, ev.FromFinalize)
	require.Empty(t, ev.Text)
	sendPCM(t, sess, 2)
	require.NoError(t, sess.Finalize())
	require.Equal(t, "日本語の勉強", waitFinal(t, sess).Text)
	require.NoError(t, sess.Finalize()) // nothing since the last commit again
	require.True(t, waitFinal(t, sess).FromFinalize)
	n := 0
	for _, ty := range clientTypes(srv) {
		if ty == "input_audio_buffer.commit" {
			n++
		}
	}
	require.Equal(t, 1, n)
}

func TestOpenAISTTCompletedWithoutPendingIsPlainFinal(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t,
		`{"ws":{"dir":"on","match":`+matchAppend+`,"times":1,"send":[`+chunk(0, evCompleted("item_a", "ひとり"))+`]}}`,
		onRule(matchAppend),
		onRule(matchCommit, chunk(0, evCommitted("item_b")), chunk(1, evCompleted("item_b", "ふたり"))),
		updatedLine))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 2)
	plain := waitFinal(t, sess)
	require.False(t, plain.FromFinalize)
	require.Equal(t, "ひとり", plain.Text)
	sendPCM(t, sess, 2)
	require.NoError(t, sess.Finalize())
	fin := waitFinal(t, sess)
	require.True(t, fin.FromFinalize) // pendingCommits did not go negative
	require.Equal(t, "ふたり", fin.Text)
}

func TestOpenAISTTCompletedOrderFollowsCommit(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t,
		onRule(matchAppend),
		`{"ws":{"dir":"on","match":`+matchCommit+`,"times":1,"send":[`+chunk(0, evCommitted("item_1"))+`]}}`,
		`{"ws":{"dir":"on","match":`+matchCommit+`,"times":1,"send":[`+chunk(0, evCommitted("item_2"))+`,`+chunk(1, evCompleted("item_2", "二"))+`,`+chunk(30, evCompleted("item_1", "一"))+`]}}`,
		updatedLine))
	sess := openSession(t, srv, func(c *openai.Config) { c.SyntheticFinalDelay = 5 * time.Second })
	sendPCM(t, sess, 2)
	require.NoError(t, sess.Finalize())
	sendPCM(t, sess, 2)
	require.NoError(t, sess.Finalize())
	a, b := waitFinal(t, sess), waitFinal(t, sess)
	require.Equal(t, []string{"一", "二"}, []string{a.Text, b.Text})
	require.True(t, a.FromFinalize && b.FromFinalize)
}

func TestOpenAISTTKeytermsMapped(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	cfg := sttCfg()
	cfg.Keyterms = []string{"先生", "<script>", "", "漢\n字"}
	sess, err := newSTT(t, srv, nil).Open(ctx, cfg)
	require.NoError(t, err)
	defer func() { _ = sess.Close() }()
	var m struct {
		Session struct {
			Audio struct {
				Input struct {
					Transcription struct {
						Keywords []string `json:"keywords"`
					} `json:"transcription"`
				} `json:"input"`
			} `json:"audio"`
		} `json:"session"`
	}
	require.NoError(t, json.Unmarshal(srv.ClientMessages()[0].Data, &m))
	require.Equal(t, []string{"先生"}, m.Session.Audio.Input.Transcription.Keywords)
}

func TestOpenAISTTOpenFailsWithoutSessionConfirmation(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t))
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, err := newSTT(t, srv, nil).Open(cctx, sttCfg())
	require.Equal(t, provider.KindTimeout, provider.KindOf(err))
}

func TestOpenAISTTOpenAuthFailure(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided: sk-test.","code":"invalid_api_key"}}`))
	}))
	defer hs.Close()
	stt, err := openai.NewSTT(openai.Config{APIKey: "sk-test", WSBaseURL: "ws" + hs.URL[4:], STTModel: "m_stt"}, httpx.NewClient(), bufLogger(&logBuf{}))
	require.NoError(t, err)
	_, err = stt.Open(ctx, sttCfg())
	require.Equal(t, provider.KindAuth, provider.KindOf(err))
	require.NotContains(t, err.Error(), "sk-test")
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	require.Equal(t, "invalid_api_key", pe.VendorCode)
	require.Equal(t, 401, pe.HTTPStatus)
}

func TestOpenAISTTNewSTTRequiresConfig(t *testing.T) {
	for _, c := range []openai.Config{{WSBaseURL: "ws://x", STTModel: "m"}, {APIKey: "k", STTModel: "m"}, {APIKey: "k", WSBaseURL: "ws://x"}} {
		_, err := openai.NewSTT(c, nil, nil)
		require.Error(t, err)
	}
}

func TestOpenAISTTSendAudioOddLengthRejected(t *testing.T) {
	sess := openSession(t, replay.NewWSServer(t, recording(t, "stt_basic")), nil)
	require.Equal(t, provider.KindInvalidRequest, provider.KindOf(sess.SendAudio(make([]byte, 641))))
}

func TestOpenAISTTUsageAudioMsSent(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 3)
	require.NoError(t, sess.Finalize())
	waitFinal(t, sess)
	st := sess.Stats()
	require.Equal(t, int64(60), st.AudioMsSent) // bytes/32 on the original 16 kHz samples
	require.False(t, st.OpenedAt.IsZero())
	require.False(t, st.FirstEventAt.IsZero())
}

// Regression (M9-T13, found by STTSuite): AudioMsSent counts the audio SendAudio accepted, not what
// the writer goroutine has put on the socket yet.
func TestOpenAISTTStatsCountAcceptedAudioAtOnce(t *testing.T) {
	sess := openSession(t, replay.NewWSServer(t, recording(t, "stt_basic")), nil)
	require.NoError(t, sess.SendAudio(make([]byte, 32000)))
	require.EqualValues(t, 1000, sess.Stats().AudioMsSent)
}

func TestOpenAISTTTapSeesFrames(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	var dirs []string
	var mu = make(chan struct{}, 1)
	mu <- struct{}{}
	sess := openSession(t, srv, func(c *openai.Config) {
		c.Tap = func(dir string, _ bool, _ []byte) { <-mu; dirs = append(dirs, dir); mu <- struct{}{} }
	})
	sendPCM(t, sess, 2)
	require.NoError(t, sess.Finalize())
	waitFinal(t, sess)
	<-mu
	defer func() { mu <- struct{}{} }()
	require.Contains(t, dirs, "send")
	require.Contains(t, dirs, "recv")
}

func TestOpenAISTTEmptyCompletedWithoutFinalizeIsDropped(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t,
		`{"ws":{"dir":"on","match":`+matchAppend+`,"times":1,"send":[`+chunk(0, evCompleted("item_a", ""))+`]}}`,
		onRule(matchAppend),
		updatedLine))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 2)
	expectNoEvent(t, sess, 150*time.Millisecond)
	st := sess.Stats()
	require.True(t, st.FirstEventAt.IsZero())
}

func TestOpenAISTTRealFinalCancelsSyntheticOne(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	sess := openSession(t, srv, func(c *openai.Config) { c.SyntheticFinalDelay = 50 * time.Millisecond })
	sendPCM(t, sess, 2)
	require.NoError(t, sess.Finalize())
	require.Equal(t, "日本語の勉強", waitFinal(t, sess).Text)
	expectNoEvent(t, sess, 200*time.Millisecond) // the 50 ms timer must not add a second FINAL
}
