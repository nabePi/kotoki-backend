package cartesia_test

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/cartesia"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

func newSTT(t *testing.T, srv *replay.Server, mod func(*cartesia.Config)) *cartesia.STT {
	t.Helper()
	cfg := cartesia.Config{APIKey: testKey, WSBaseURL: srv.URL, STTModel: "m_ink", Version: "v1"}
	if mod != nil {
		mod(&cfg)
	}
	stt, err := cartesia.NewSTT(cfg, httpx.NewClient(), bufLogger(&logBuf{}))
	require.NoError(t, err)
	return stt
}

func sttCfg() provider.STTSessionConfig {
	return provider.STTSessionConfig{Language: "ja", SampleRate: provider.STTSampleRate}
}

func openSession(t *testing.T, srv *replay.Server, mod func(*cartesia.Config)) provider.STTSession {
	t.Helper()
	sess, err := newSTT(t, srv, mod).Open(ctx, sttCfg())
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.Close() })
	return sess
}

// sttScript builds an inline fixture: the given lines (rules) after the meta line.
func sttScript(t *testing.T, lines ...string) *replay.Script {
	t.Helper()
	head := `{"meta":{"vendor":"CARTESIA","capability":"STT","source":"inline test fixture","verified":false}}`
	return replay.LoadScriptString(t, strings.Join(append([]string{head}, lines...), "\n"))
}

func onRule(match string, sends ...string) string {
	return `{"ws":{"dir":"on","match":` + match + `,"send":[` + strings.Join(sends, ",") + `]}}`
}

func chunk(at int, text string) string {
	return `{"at_ms":` + itoa(at) + `,"text":` + quote(text) + `}`
}

const (
	matchBinary   = `{"binary":true}`
	matchFinalize = `{"raw_text":"finalize"}`
	flushDoneJSON = `{"type":"flush_done","request_id":"r"}`
)

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

func sendPCM(t *testing.T, sess provider.STTSession, frames int) {
	t.Helper()
	for i := 0; i < frames; i++ {
		require.NoError(t, sess.SendAudio(make([]byte, 640)))
	}
}

func waitMsgs(t *testing.T, srv *replay.Server, n int) []replay.Message {
	t.Helper()
	require.Eventually(t, func() bool { return len(srv.ClientMessages()) >= n }, 3*time.Second, 5*time.Millisecond)
	return srv.ClientMessages()
}

func TestCartesiaSTTOpenQueryAndAuth(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	stt := newSTT(t, srv, nil)
	require.Equal(t, provider.Cartesia, stt.Name())
	require.Equal(t, "m_ink", stt.Model())
	cfg := sttCfg()
	cfg.Keyterms = []string{"歌舞伎", "two words"}
	sess, err := stt.Open(ctx, cfg)
	require.NoError(t, err)
	defer func() { _ = sess.Close() }()
	u := srv.UpgradeRequest()
	require.Equal(t, "/stt/websocket", u.Path)
	require.Equal(t, testKey, u.Header.Get("X-API-Key"))
	require.Empty(t, u.Header.Get("Cartesia-Version"), "the version travels as a query parameter (Step 0a)")
	require.Equal(t, url.Values{"model": {"m_ink"}, "encoding": {"pcm_s16le"}, "sample_rate": {"16000"}, "cartesia_version": {"v1"},
		"keyterm": {"歌舞伎", "two words"}}, u.Query)
}

func TestCartesiaSTTWhisperModelTakesLanguageNotKeyterms(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchBinary)))
	stt := newSTT(t, srv, func(c *cartesia.Config) { c.STTModel = "m_whisper" })
	cfg := sttCfg()
	cfg.Keyterms = []string{"歌舞伎"}
	sess, err := stt.Open(ctx, cfg)
	require.NoError(t, err)
	defer func() { _ = sess.Close() }()
	require.Equal(t, url.Values{"model": {"m_whisper"}, "encoding": {"pcm_s16le"}, "sample_rate": {"16000"}, "cartesia_version": {"v1"},
		"language": {"ja"}}, srv.UpgradeRequest().Query)
}

func TestCartesiaSTTNewSTTValidation(t *testing.T) {
	for name, cfg := range map[string]cartesia.Config{
		"no key":   {STTModel: "m"},
		"no model": {APIKey: testKey},
	} {
		_, err := cartesia.NewSTT(cfg, nil, nil)
		require.Error(t, err, name)
	}
	// the version and URLs default
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchBinary)))
	stt, err := cartesia.NewSTT(cartesia.Config{APIKey: testKey, STTModel: "m", WSBaseURL: srv.URL}, nil, nil)
	require.NoError(t, err)
	sess, err := stt.Open(ctx, sttCfg())
	require.NoError(t, err)
	defer func() { _ = sess.Close() }()
	require.Equal(t, "2026-08-14", srv.UpgradeRequest().Query.Get("cartesia_version"))
}

func TestCartesiaSTTBatchesFiveFramesAndFinalizeFlush(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 7)
	require.NoError(t, sess.Finalize())
	ev := waitFinal(t, sess)
	require.True(t, ev.FromFinalize)
	require.Equal(t, "日本語の勉強", ev.Text)
	require.Equal(t, provider.Cartesia, ev.Provider)
	require.Equal(t, int64(0), ev.StartMs)
	require.Equal(t, int64(140), ev.EndMs) // 7 frames of 20 ms
	require.Equal(t, []provider.STTWord{{Text: "日本語", StartMs: 0, EndMs: 120}, {Text: "の勉強", StartMs: 120, EndMs: 200}}, ev.Words)
	msgs := waitMsgs(t, srv, 3)
	require.Len(t, msgs, 3)
	require.False(t, msgs[0].Text)
	require.Len(t, msgs[0].Data, 3200)
	require.False(t, msgs[1].Text)
	require.Len(t, msgs[1].Data, 1280)
	require.True(t, msgs[2].Text)
	require.Equal(t, "finalize", string(msgs[2].Data))
}

func TestCartesiaSTTFinalizeWithNoAudioSendsOnlyFinalize(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_flush_empty"))
	sess := openSession(t, srv, nil)
	require.NoError(t, sess.Finalize())
	ev := waitFinal(t, sess)
	require.True(t, ev.FromFinalize)
	require.Empty(t, ev.Text)
	msgs := srv.ClientMessages()
	require.Len(t, msgs, 1)
	require.Equal(t, "finalize", string(msgs[0].Data))
}

func TestCartesiaSTTKeytermsTruncated(t *testing.T) {
	many := make([]string, 120)
	for i := range many {
		many[i] = "k" + itoa(i)
	}
	long := []string{strings.Repeat("あ", 700), strings.Repeat("い", 600), "short"}
	cases := []struct {
		name  string
		in    []string
		wantN int
		check func(t *testing.T, got []string)
	}{
		{"count", many, 100, func(t *testing.T, got []string) { require.Equal(t, many[:100], got) }},
		{"chars", long, 2, func(t *testing.T, got []string) {
			require.Equal(t, []string{long[0], "short"}, got) // 1200 chars in all: the 600-char term no longer fits
			n := 0
			for _, k := range got {
				n += len([]rune(k))
			}
			require.LessOrEqual(t, n, 1200)
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := replay.NewWSServer(t, sttScript(t, onRule(matchBinary)))
			cfg := sttCfg()
			cfg.Keyterms = c.in
			sess, err := newSTT(t, srv, nil).Open(ctx, cfg)
			require.NoError(t, err)
			defer func() { _ = sess.Close() }()
			got := srv.UpgradeRequest().Query["keyterm"]
			require.Len(t, got, c.wantN)
			c.check(t, got)
		})
	}
}

func TestCartesiaSTTKeytermMultiWordUsesPercent20(t *testing.T) {
	stt, err := cartesia.NewSTT(cartesia.Config{APIKey: testKey, STTModel: "m", WSBaseURL: "ws://x"}, nil, nil)
	require.NoError(t, err)
	cfg := sttCfg()
	cfg.Keyterms = []string{"two words"}
	// the docs ask for %20 in multi-word phrases, not '+'
	require.Contains(t, cartesia.STTEndpoint(stt, cfg), "&keyterm=two%20words")
}

func TestCartesiaSTTPartialMapping(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchBinary,
		chunk(0, `{"type":"transcript","is_final":false,"request_id":"r","text":"日本"}`),
		chunk(0, `{"type":"transcript","is_final":false,"request_id":"r","text":"日本語の"}`))))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 5)
	var got []string
	for i := 0; i < 2; i++ {
		ev := nextEv(t, sess)
		require.Equal(t, provider.STTPartial, ev.Type)
		require.Nil(t, ev.Confidence)
		require.Equal(t, int64(0), ev.StartMs)
		require.Equal(t, int64(100), ev.EndMs)
		got = append(got, ev.Text)
	}
	require.Equal(t, []string{"日本", "日本語の"}, got)
}

func TestCartesiaSTTPartialTextIsNormalized(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchBinary,
		chunk(0, `{"type":"transcript","is_final":false,"text":"ＡＢＣ　テスト"}`))))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 5)
	ev := nextEv(t, sess)
	require.Equal(t, provider.NormalizeJA("ＡＢＣ　テスト"), ev.Text)
	require.NotEqual(t, "ＡＢＣ　テスト", ev.Text)
}

func TestCartesiaSTTFinalBeforeFinalizeIsPlainFinal(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchBinary,
		chunk(0, `{"type":"transcript","is_final":true,"text":"こんにちは","words":[{"word":"こんにちは","start":0.5,"end":1.25}]}`))))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 5)
	ev := nextEv(t, sess)
	require.Equal(t, provider.STTFinal, ev.Type)
	require.False(t, ev.FromFinalize)
	require.Equal(t, "こんにちは", ev.Text)
	require.Equal(t, []provider.STTWord{{Text: "こんにちは", StartMs: 500, EndMs: 1250}}, ev.Words)
	require.Equal(t, int64(0), ev.StartMs)
	require.Equal(t, int64(100), ev.EndMs)
	// a later plain FINAL starts where the previous one ended
	sendPCM(t, sess, 5)
	ev = nextEv(t, sess)
	require.Equal(t, int64(100), ev.StartMs)
	require.Equal(t, int64(200), ev.EndMs)
}

func TestCartesiaSTTEmptyPlainFinalDropped(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchBinary,
		chunk(0, `{"type":"transcript","is_final":true,"text":""}`))))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 5)
	expectNoEvent(t, sess, 150*time.Millisecond)
}

// TestCartesiaSTTFinalizeFlushDone: two finals between finalize and flush_done are joined
// verbatim and answered by exactly one FINAL.
func TestCartesiaSTTFinalizeFlushDone(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_finalize_chunks"))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 2)
	require.NoError(t, sess.Finalize())
	ev := waitFinal(t, sess)
	require.True(t, ev.FromFinalize)
	require.Equal(t, "日本語の勉強", ev.Text)
	expectNoEvent(t, sess, 100*time.Millisecond)
}

func TestCartesiaSTTFinalizeJoinsChunksWithoutAddedSpaces(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchFinalize,
		chunk(0, `{"type":"transcript","is_final":true,"text":"Hello"}`),
		chunk(0, `{"type":"transcript","is_final":true,"text":" world"}`),
		chunk(0, `{"type":"transcript","is_final":true,"text":"!"}`),
		chunk(0, flushDoneJSON))))
	sess := openSession(t, srv, nil)
	require.NoError(t, sess.Finalize())
	require.Equal(t, "Hello world!", waitFinal(t, sess).Text) // the vendor's own space is kept, none is added
}

func TestCartesiaSTTFlushDoneWithoutTranscriptIsEmptyFinal(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_flush_empty"))
	sess := openSession(t, srv, nil)
	for i := 0; i < 3; i++ {
		require.NoError(t, sess.Finalize())
	}
	for i := 0; i < 3; i++ {
		ev := waitFinal(t, sess)
		require.True(t, ev.FromFinalize)
		require.Empty(t, ev.Text)
	}
	expectNoEvent(t, sess, 100*time.Millisecond)
}

// TestCartesiaSTTFinalizeSyntheticEmptyFinal: flush_done never comes; the safety net answers
// once, and a late flush_done does not produce a second FINAL.
func TestCartesiaSTTFinalizeSyntheticEmptyFinal(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchFinalize)))
	sess := openSession(t, srv, func(c *cartesia.Config) { c.SyntheticFinalDelay = 30 * time.Millisecond })
	for i := 0; i < 3; i++ {
		require.NoError(t, sess.Finalize())
	}
	for i := 0; i < 3; i++ {
		ev := waitFinal(t, sess)
		require.True(t, ev.FromFinalize)
		require.Empty(t, ev.Text)
	}
	expectNoEvent(t, sess, 120*time.Millisecond)
}

func TestCartesiaSTTLateFlushDoneAfterSyntheticIsSwallowed(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchFinalize, chunk(120, flushDoneJSON))))
	sess := openSession(t, srv, func(c *cartesia.Config) { c.SyntheticFinalDelay = 30 * time.Millisecond })
	require.NoError(t, sess.Finalize())
	require.True(t, waitFinal(t, sess).FromFinalize)
	expectNoEvent(t, sess, 250*time.Millisecond) // the late flush_done adds nothing
}

func TestCartesiaSTTCloseSendsClose(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_basic"))
	sess := openSession(t, srv, nil)
	start := time.Now()
	require.NoError(t, sess.Close())
	require.NoError(t, sess.Close())
	require.Less(t, time.Since(start), 150*time.Millisecond, "the vendor's done ends Close early")
	msgs := waitMsgs(t, srv, 1)
	require.Equal(t, "close", string(msgs[len(msgs)-1].Data))
	var last provider.STTEvent
	for ev := range sess.Events() {
		last = ev
	}
	require.Equal(t, provider.STTClosed, last.Type)
}

func TestCartesiaSTTSendAudioOddLengthRejected(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchBinary)))
	sess := openSession(t, srv, nil)
	err := sess.SendAudio(make([]byte, 641))
	require.Equal(t, provider.KindInvalidRequest, provider.KindOf(err))
}

func TestCartesiaSTTUsageAudioMsSent(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t, onRule(matchBinary)))
	sess := openSession(t, srv, nil)
	require.NoError(t, sess.SendAudio(make([]byte, 32000)))
	st := sess.Stats()
	require.EqualValues(t, 1000, st.AudioMsSent)
	require.False(t, st.OpenedAt.IsZero())
}

func TestCartesiaSTTStatsFirstEventAt(t *testing.T) {
	srv := replay.NewWSServer(t, recording(t, "stt_flush_empty"))
	sess := openSession(t, srv, nil)
	require.True(t, sess.Stats().FirstEventAt.IsZero())
	require.NoError(t, sess.Finalize())
	waitFinal(t, sess)
	require.False(t, sess.Stats().FirstEventAt.IsZero())
}

func TestCartesiaSTTLateFlushDoneDoesNotStealNextFinalize(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t,
		`{"ws":{"dir":"on","match":`+matchFinalize+`,"times":1,"send":[`+chunk(300, flushDoneJSON)+`]}}`,
		onRule(matchFinalize, chunk(0, `{"type":"transcript","is_final":true,"text":"二回目"}`), chunk(0, flushDoneJSON))))
	sess := openSession(t, srv, func(c *cartesia.Config) { c.SyntheticFinalDelay = 150 * time.Millisecond })
	require.NoError(t, sess.Finalize())
	require.Empty(t, waitFinal(t, sess).Text) // synthetic
	time.Sleep(400 * time.Millisecond)        // the late flush_done of the first finalize arrives and is swallowed
	start := time.Now()
	require.NoError(t, sess.Finalize())
	ev := waitFinal(t, sess)
	require.Less(t, time.Since(start), 100*time.Millisecond, "its own flush_done answers, not the synthetic safety net")
	require.True(t, ev.FromFinalize)
	require.Equal(t, "二回目", ev.Text) // answered by its own flush_done, not swallowed
}

func TestCartesiaSTTSegmentStartAdvancesAfterFinalize(t *testing.T) {
	srv := replay.NewWSServer(t, sttScript(t,
		onRule(matchBinary, chunk(0, `{"type":"transcript","is_final":false,"text":"あ"}`)),
		onRule(matchFinalize, chunk(0, flushDoneJSON))))
	sess := openSession(t, srv, nil)
	sendPCM(t, sess, 5)
	p := nextEv(t, sess)
	require.Equal(t, int64(0), p.StartMs)
	require.NoError(t, sess.Finalize())
	f := waitFinal(t, sess)
	require.Equal(t, int64(0), f.StartMs)
	require.Equal(t, int64(100), f.EndMs)
	sendPCM(t, sess, 5)
	p = nextEv(t, sess)
	require.Equal(t, provider.STTPartial, p.Type)
	require.Equal(t, int64(100), p.StartMs)
	require.Equal(t, int64(200), p.EndMs)
}
