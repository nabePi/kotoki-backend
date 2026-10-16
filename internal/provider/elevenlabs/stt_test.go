package elevenlabs_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/elevenlabs"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

func newSTT(t *testing.T, srv *replay.Server, mod func(*elevenlabs.Config)) *elevenlabs.STT {
	t.Helper()
	cfg := elevenlabs.Config{APIKey: "k", WSBaseURL: srv.URL, STTModel: "m_stt"}
	if mod != nil {
		mod(&cfg)
	}
	stt, err := elevenlabs.NewSTT(cfg, httpx.NewClient(), bufLogger(&logBuf{}))
	require.NoError(t, err)
	return stt
}

func sttCfg() provider.STTSessionConfig {
	return provider.STTSessionConfig{Language: "ja", SampleRate: provider.STTSampleRate}
}

func openSession(t *testing.T, srv *replay.Server, mod func(*elevenlabs.Config)) provider.STTSession {
	t.Helper()
	sess, err := newSTT(t, srv, mod).Open(ctx, sttCfg())
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// sttScript builds an inline fixture: the given lines (rules) then session_started.
func sttScript(t *testing.T, lines ...string) *replay.Script {
	t.Helper()
	head := `{"meta":{"vendor":"ELEVENLABS","capability":"STT","source":"inline test fixture","verified":false}}`
	tail := `{"ws":{"dir":"send","text":"{\"message_type\":\"session_started\",\"session_id\":\"s\"}"}}`
	return replay.LoadScriptString(t, strings.Join(append(append([]string{head}, lines...), tail), "\n"))
}

func onRule(match string, sends ...string) string {
	return `{"ws":{"dir":"on","match":` + match + `,"send":[` + strings.Join(sends, ",") + `]}}`
}

func chunk(at int, v any) string {
	b, _ := json.Marshal(v)
	return `{"at_ms":` + itoa(at) + `,"text":` + quote(string(b)) + `}`
}

const (
	matchAppend = `{"message_type":"input_audio_chunk","commit":false}`
	matchCommit = `{"message_type":"input_audio_chunk","commit":true}`
)

func evCommitted(text string) map[string]any {
	return map[string]any{"message_type": "committed_transcript", "text": text}
}

func evPartial(text string) map[string]any {
	return map[string]any{"message_type": "partial_transcript", "text": text}
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

type audioMsg struct {
	MessageType string `json:"message_type"`
	Audio       string `json:"audio_base_64"`
	Commit      bool   `json:"commit"`
	SampleRate  int    `json:"sample_rate"`
}

func audioMsgs(t *testing.T, srv *replay.Server) []audioMsg {
	t.Helper()
	var out []audioMsg
	for _, m := range srv.ClientMessages() {
		var a audioMsg
		require.NoError(t, json.Unmarshal(m.Data, &a))
		out = append(out, a)
	}
	return out
}

func (a audioMsg) pcm(t *testing.T) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(a.Audio)
	require.NoError(t, err)
	return b
}

func sendPCM(t *testing.T, sess provider.STTSession, frames int) {
	t.Helper()
	for i := 0; i < frames; i++ {
		require.NoError(t, sess.SendAudio(make([]byte, 640)))
	}
}

func TestELSTTOpenQueryAndAuth(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	stt := newSTT(t, srv, nil)
	require.Equal(t, provider.ElevenLabs, stt.Name())
	require.Equal(t, "m_stt", stt.Model())
	sess, err := stt.Open(ctx, sttCfg())
	require.NoError(t, err)
	defer func() { _ = sess.Close() }()
	u := srv.UpgradeRequest()
	require.Equal(t, "/v1/speech-to-text/realtime", u.Path)
	require.Equal(t, url.Values{"model_id": {"m_stt"}, "language_code": {"ja"}, "audio_format": {"pcm_16000"}, "commit_strategy": {"manual"}, "include_timestamps": {"false"}, "enable_logging": {"false"}}, u.Query)
	require.Equal(t, "k", u.Header.Get("xi-api-key"))
}

func TestELSTTBatchesTwoFramesAndFinalizeCommits(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 3)
	require.NoError(t, sess.Finalize())
	ev := waitFinal(t, sess)
	require.True(t, ev.FromFinalize)
	require.Equal(t, "日本語の勉強", ev.Text)
	require.Equal(t, provider.ElevenLabs, ev.Provider)
	require.Equal(t, int64(0), ev.StartMs)
	require.Equal(t, int64(60), ev.EndMs)
	msgs := audioMsgs(t, srv)
	require.Len(t, msgs, 2)
	require.Equal(t, "input_audio_chunk", msgs[0].MessageType)
	require.Equal(t, 16000, msgs[0].SampleRate)
	require.Len(t, msgs[0].pcm(t), 1280)
	require.False(t, msgs[0].Commit)
	require.True(t, msgs[1].Commit)
	require.Len(t, msgs[1].pcm(t), 640)
}

func TestELSTTPartialMapping(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_partial_cumulative"))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 2)
	var got []string
	for i := 0; i < 3; i++ {
		ev := nextEv(t, sess)
		require.Equal(t, provider.STTPartial, ev.Type)
		require.Nil(t, ev.Confidence)
		require.Equal(t, int64(0), ev.StartMs)
		require.Equal(t, int64(40), ev.EndMs) // 1.280 bytes of 16 kHz audio
		require.Equal(t, provider.ElevenLabs, ev.Provider)
		got = append(got, ev.Text)
	}
	require.Equal(t, []string{"日本", "日本語の", "日本語の勉強"}, got)
}

func TestELSTTPartialTextIsNormalized(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t,
		onRule(matchAppend, chunk(0, evPartial("ＡＢＣ　テスト")))))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 2)
	ev := nextEv(t, sess)
	require.Equal(t, provider.NormalizeJA("ＡＢＣ　テスト"), ev.Text)
	require.NotEqual(t, "ＡＢＣ　テスト", ev.Text)
}

func TestELSTTFinalizeWithNoBufferedFrameSendsSilence20ms(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 2)
	require.NoError(t, sess.Finalize())
	require.Equal(t, "日本語の勉強", waitFinal(t, sess).Text)
	msgs := audioMsgs(t, srv)
	require.Len(t, msgs, 2)
	require.True(t, msgs[1].Commit)
	pcm := msgs[1].pcm(t)
	require.Len(t, pcm, 640)
	require.Equal(t, make([]byte, 640), pcm)
}

func TestELSTTFinalizeWithoutAnyAudioStillCommits(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	sess := openSession(t, srv, nil)
	require.NoError(t, sess.Finalize())
	require.True(t, waitFinal(t, sess).FromFinalize)
	msgs := audioMsgs(t, srv)
	require.Len(t, msgs, 1)
	require.True(t, msgs[0].Commit)
}

func TestELSTTFinalizeSyntheticEmptyFinal(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_commit_empty"))
	sess := openSession(t, srv, func(c *elevenlabs.Config) { c.SyntheticFinalDelay = 30 * time.Millisecond })
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

func TestELSTTSyntheticDefaultDelayIs400ms(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_commit_empty"))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 2)
	start := time.Now()
	require.NoError(t, sess.Finalize())
	waitFinal(t, sess)
	require.GreaterOrEqual(t, time.Since(start), 380*time.Millisecond)
}

func TestELSTTRealFinalCancelsSyntheticOne(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	sess := openSession(t, srv, func(c *elevenlabs.Config) { c.SyntheticFinalDelay = 50 * time.Millisecond })
	sendPCM(t, sess, 2)
	require.NoError(t, sess.Finalize())
	require.Equal(t, "日本語の勉強", waitFinal(t, sess).Text)
	expectNoEvent(t, sess, 200*time.Millisecond)
}

func TestELSTTLateCommittedAfterSyntheticFinal(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t,
		onRule(matchAppend),
		onRule(matchCommit, chunk(150, evCommitted("遅い")))))
	sess := openSession(t, srv, func(c *elevenlabs.Config) { c.SyntheticFinalDelay = 30 * time.Millisecond })
	sendPCM(t, sess, 2)
	require.NoError(t, sess.Finalize())
	first := waitFinal(t, sess)
	require.True(t, first.FromFinalize)
	require.Empty(t, first.Text)
	late := waitFinal(t, sess)
	require.False(t, late.FromFinalize, "the Finalize was already answered")
	require.Equal(t, "遅い", late.Text)
}

func TestELSTTAutoCommitIsPlainFinal(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_auto_commit"))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 2)
	plain := waitFinal(t, sess)
	require.False(t, plain.FromFinalize)
	require.Equal(t, "自動確定", plain.Text)
	require.Equal(t, int64(0), plain.StartMs)
	require.Equal(t, int64(40), plain.EndMs)
	sendPCM(t, sess, 2)
	require.NoError(t, sess.Finalize())
	fin := waitFinal(t, sess)
	require.True(t, fin.FromFinalize) // pendingCommits did not go negative
	require.Empty(t, fin.Text)        // second commit: vendor stays silent here, synthetic after 400 ms
	require.Equal(t, int64(40), fin.StartMs, "segStartMs advanced to the auto commit")
}

func TestELSTTEmptyCommittedWithoutFinalizeIsDropped(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchAppend, chunk(0, evCommitted("")))))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 2)
	expectNoEvent(t, sess, 150*time.Millisecond)
}

func TestELSTTIgnoresTimestampedCommit(t *testing.T) {
	ts := map[string]any{"message_type": "committed_transcript_with_timestamps", "text": "重複", "language_code": "ja", "words": []any{}}
	srv := replay.NewWSServer(t, sttScript(t,
		onRule(matchAppend),
		onRule(matchCommit, chunk(0, evCommitted("本物")), chunk(1, ts))))
	sess := openSession(t, srv, func(c *elevenlabs.Config) { c.SyntheticFinalDelay = 5 * time.Second })
	sendPCM(t, sess, 2)
	require.NoError(t, sess.Finalize())
	require.Equal(t, "本物", waitFinal(t, sess).Text)
	expectNoEvent(t, sess, 150*time.Millisecond)
}

func TestELSTTOpenFailsWithoutSessionStarted(t *testing.T) {
	srv := replay.NewWSServer(t, replay.LoadScriptString(t,
		`{"meta":{"vendor":"ELEVENLABS","capability":"STT","source":"inline","verified":false}}`))
	cctx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()
	_, err := newSTT(t, srv, nil).Open(cctx, sttCfg())
	require.Equal(t, provider.KindTimeout, provider.KindOf(err))
}

func TestELSTTOpenAuthFailure(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"detail":{"status":"invalid_api_key","message":"Invalid API key k-secret"}}`))
	}))
	defer hs.Close()
	stt, err := elevenlabs.NewSTT(elevenlabs.Config{APIKey: "k-secret", WSBaseURL: "ws" + hs.URL[4:], STTModel: "m_stt"}, httpx.NewClient(), bufLogger(&logBuf{}))
	require.NoError(t, err)
	_, err = stt.Open(ctx, sttCfg())
	require.Equal(t, provider.KindAuth, provider.KindOf(err))
	require.NotContains(t, err.Error(), "k-secret")
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	require.Equal(t, provider.CapSTT, pe.Capability)
	require.Equal(t, "invalid_api_key", pe.VendorCode)
	require.Equal(t, 401, pe.HTTPStatus)
}

func TestELSTTNewSTTRequiresConfig(t *testing.T) {
	for _, c := range []elevenlabs.Config{{WSBaseURL: "ws://x", STTModel: "m"}, {APIKey: "k", STTModel: "m"}, {APIKey: "k", WSBaseURL: "ws://x"}} {
		_, err := elevenlabs.NewSTT(c, nil, nil)
		require.Error(t, err)
	}
}

func TestELSTTSendAudioOddLengthRejected(t *testing.T) {
	sess := openSession(t, replay.NewWSServer(t, recording(t, "stt_basic")), nil)
	require.Equal(t, provider.KindInvalidRequest, provider.KindOf(sess.SendAudio(make([]byte, 641))))
}

func TestELSTTUsageAudioMsSent(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 3)
	require.NoError(t, sess.Finalize())
	waitFinal(t, sess)
	st := sess.Stats()
	require.Equal(t, int64(60), st.AudioMsSent)
	require.False(t, st.OpenedAt.IsZero())
	require.False(t, st.FirstEventAt.IsZero())
}

func TestELSTTTapSeesFrames(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	var dirs []string
	mu := make(chan struct{}, 1)
	mu <- struct{}{}
	sess := openSession(t, srv, func(c *elevenlabs.Config) {
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

// A late answer to a commit that was already answered synthetically must not consume the
// next, still pending commit.
func TestELSTTLateCommittedDoesNotAnswerNextPendingCommit(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t,
		onRule(matchAppend),
		`{"ws":{"dir":"on","match":`+matchCommit+`,"times":1,"send":[`+chunk(130, evCommitted("遅い"))+`]}}`,
		onRule(matchCommit)))
	sess := openSession(t, srv, func(c *elevenlabs.Config) { c.SyntheticFinalDelay = 60 * time.Millisecond })
	sendPCM(t, sess, 2)
	require.NoError(t, sess.Finalize())
	time.Sleep(100 * time.Millisecond) // commit 1 was answered synthetically at 60 ms
	sendPCM(t, sess, 2)
	require.NoError(t, sess.Finalize()) // commit 2 stays pending until its own synthetic FINAL at ~160 ms
	first := waitFinal(t, sess)
	require.True(t, first.FromFinalize)
	late := waitFinal(t, sess)
	require.False(t, late.FromFinalize, "the late text belongs to commit 1")
	require.Equal(t, "遅い", late.Text)
	second := waitFinal(t, sess)
	require.True(t, second.FromFinalize)
	require.Empty(t, second.Text)
}
