package fake

import (
	"context"
	"encoding/binary"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/providertest"
)

const rate = provider.STTSampleRate

// tone is a 400 Hz sine of amplitude amp lasting ms milliseconds.
func tone(ms int, amp float64) []byte {
	n := rate * ms / 1000
	out := make([]byte, n*2)
	for i := 0; i < n; i++ {
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(amp*math.Sin(2*math.Pi*400*float64(i)/rate))))
	}
	return out
}

func silence(ms int) []byte { return make([]byte, rate*ms/1000*2) }

func cat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// burstsA: three 400 ms bursts separated by 200 ms gaps.
func burstsA() []byte {
	return cat(tone(400, 8000), silence(200), tone(400, 8000), silence(200), tone(400, 8000))
}

// rampB: a 1.2 s ramp from silence to loud.
func rampB() []byte {
	n := rate * 1200 / 1000
	out := make([]byte, n*2)
	for i := 0; i < n; i++ {
		amp := 12000 * float64(i) / float64(n)
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(amp*math.Sin(2*math.Pi*400*float64(i)/rate))))
	}
	return out
}

// noisy multiplies each sample by 1 ± frac, imitating an Opus round trip.
func noisy(pcm []byte, frac float64) []byte {
	r := rand.New(rand.NewPCG(1, 2))
	out := make([]byte, len(pcm))
	for i := 0; i < len(pcm)/2; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
		v *= 1 + (r.Float64()*2-1)*frac
		binary.LittleEndian.PutUint16(out[i*2:], uint16(int16(v)))
	}
	return out
}

func writeFixture(t *testing.T, dir, name, text string, pcm []byte, sampleRate int) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name+".wav"), provider.EncodeWAV(pcm, sampleRate), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, name+".txt"), []byte(text+"\n"), 0o600))
}

func writeFixtures(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeFixture(t, dir, "a", "日本語の勉強は楽しいです。", burstsA(), rate)
	writeFixture(t, dir, "b", "昨日は雨でしたね。", rampB(), rate)
	return dir
}

func newSTT(t *testing.T, capCfg CapConfig, cfg Config, d Deps) *STT {
	t.Helper()
	if d.Clock == nil {
		d.Clock = clock.NewFake(t0)
	}
	d.Seed = 42
	s, err := NewSTT(provider.FakePrimary, capCfg, cfg, d)
	require.NoError(t, err)
	return s
}

func openFake(t *testing.T, cfg Config) provider.STTSession {
	t.Helper()
	return openWith(t, newSTT(t, CapConfig{}, cfg, Deps{}))
}

func openWith(t *testing.T, s *STT) provider.STTSession {
	t.Helper()
	ss, err := s.Open(context.Background(), provider.STTSessionConfig{Language: "ja", SampleRate: rate})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ss.Close() })
	return ss
}

func feed(t *testing.T, s provider.STTSession, pcm []byte) {
	t.Helper()
	for off := 0; off < len(pcm); off += 640 {
		end := min(off+640, len(pcm))
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := s.SendAudio(pcm[off:end])
			if err == nil {
				break
			}
			require.Equal(t, provider.KindUnavailable, provider.KindOf(err))
			require.True(t, time.Now().Before(deadline))
			time.Sleep(time.Millisecond)
		}
	}
}

func next(t *testing.T, s provider.STTSession) provider.STTEvent {
	t.Helper()
	select {
	case ev, ok := <-s.Events():
		require.True(t, ok, "channel closed")
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event")
	}
	return provider.STTEvent{}
}

func nextFinal(t *testing.T, s provider.STTSession) provider.STTEvent {
	t.Helper()
	for {
		if ev := next(t, s); ev.Type == provider.STTFinal {
			return ev
		}
	}
}

func partials(t *testing.T, s provider.STTSession) []provider.STTEvent {
	t.Helper()
	var out []provider.STTEvent
	for {
		ev := next(t, s)
		if ev.Type == provider.STTFinal {
			return out
		}
		out = append(out, ev)
	}
}

func TestFakeSTTConformance(t *testing.T) {
	providertest.STTSuite(t, func(t *testing.T) provider.STTProvider {
		return newSTT(t, CapConfig{ConnectMS: 5}, Config{}, Deps{})
	}, func() []byte { return tone(1200, 8000) })
}

func TestFakeSTTMatchesFixtureByEnvelope(t *testing.T) {
	dir := writeFixtures(t)
	s := openFake(t, Config{STTFixtureDir: dir, STTMatchThreshold: 0.8})
	feed(t, s, noisy(burstsA(), 0.02))
	require.NoError(t, s.Finalize())
	ev := nextFinal(t, s)
	require.True(t, ev.FromFinalize)
	require.Equal(t, "日本語の勉強は楽しいです。", ev.Text)
	require.InDelta(t, 0.95, *ev.Confidence, 1e-9)

	s2 := openFake(t, Config{STTFixtureDir: dir, STTMatchThreshold: 0.8})
	feed(t, s2, noisy(rampB(), 0.02))
	require.NoError(t, s2.Finalize())
	require.Equal(t, "昨日は雨でしたね。", nextFinal(t, s2).Text)
}

func TestFakeSTTNoFixtureMatchFallsBackToEnergy(t *testing.T) {
	dir := writeFixtures(t)
	s := openFake(t, Config{STTFixtureDir: dir, STTMatchThreshold: 0.8})
	feed(t, s, tone(1000, 8000)) // a flat envelope correlates with nothing
	require.NoError(t, s.Finalize())
	require.Equal(t, "はい、そうです。", nextFinal(t, s).Text)
}

func TestFakeSTTFixtureTieIsLexicographic(t *testing.T) {
	dir := t.TempDir()
	writeFixture(t, dir, "b", "二番目", burstsA(), rate)
	writeFixture(t, dir, "a", "一番目", burstsA(), rate)
	s := openFake(t, Config{STTFixtureDir: dir})
	feed(t, s, burstsA())
	require.NoError(t, s.Finalize())
	require.Equal(t, "一番目", nextFinal(t, s).Text)
}

func TestFakeSTTEnergyPhrasesRotatePerSegment(t *testing.T) {
	s := openFake(t, Config{})
	feed(t, s, cat(tone(500, 8000), silence(700), tone(500, 8000), silence(700)))
	a := nextFinal(t, s)
	b := nextFinal(t, s)
	require.Equal(t, "はい、そうです。", a.Text)
	require.Equal(t, "えっと、週末は友達と映画を見ました。", b.Text)
	require.False(t, a.FromFinalize, "a segment closed by 600 ms of silence is not a Finalize answer")
}

func TestFakeSTTSegmentNeedsTwoHundredMsOfSpeech(t *testing.T) {
	s := openFake(t, Config{})
	feed(t, s, cat(tone(180, 8000), silence(700)))
	require.NoError(t, s.Finalize())
	ev := next(t, s)
	require.Equal(t, provider.STTFinal, ev.Type)
	require.True(t, ev.FromFinalize)
	require.Empty(t, ev.Text, "too short to be a segment")
}

func TestFakeSTTFinalizeWithoutSegmentIsEmpty(t *testing.T) {
	s := openFake(t, Config{})
	require.NoError(t, s.Finalize())
	ev := next(t, s)
	require.Equal(t, provider.STTFinal, ev.Type)
	require.True(t, ev.FromFinalize)
	require.Empty(t, ev.Text)
	require.Nil(t, ev.Confidence)
}

func TestFakeSTTPartialsFollowAudioTime(t *testing.T) {
	s := openFake(t, Config{})
	// Feed 200 ms at a time and read each PARTIAL before sending more: a new PARTIAL replaces
	// queued ones, so a slow reader would legitimately miss some.
	prev := 0
	for i := 1; i <= 5; i++ {
		feed(t, s, tone(200, 8000))
		p := next(t, s)
		require.Equal(t, provider.STTPartial, p.Type, "one PARTIAL per 200 ms of segment audio")
		require.EqualValues(t, 0, p.StartMs)
		require.EqualValues(t, i*200, p.EndMs)
		n := len([]rune(p.Text))
		require.GreaterOrEqual(t, n, prev, "partial %d must not shrink", i)
		prev = n
		if i == 1 {
			// "はい、そうです。" = 8 runes, expected 960 ms: 200 ms → ceil(8×200/960) = 2 runes
			require.Equal(t, "はい", p.Text)
		}
	}
	require.NoError(t, s.Finalize())
	require.Equal(t, provider.STTFinal, nextFinal(t, s).Type)
}

func TestFakeSTTTimestampsFromSamplePosition(t *testing.T) {
	s := openFake(t, Config{})
	feed(t, s, cat(silence(300), tone(1000, 8000)))
	require.NoError(t, s.Finalize())
	ev := nextFinal(t, s)
	require.EqualValues(t, 300, ev.StartMs)
	require.EqualValues(t, 1300, ev.EndMs)
}

func TestFakeSTTTextIsNormalised(t *testing.T) {
	s := openFake(t, Config{STTPhrases: []string{"日本 語　です"}})
	feed(t, s, tone(500, 8000))
	require.NoError(t, s.Finalize())
	require.Equal(t, "日本語です", nextFinal(t, s).Text)
}

func TestFakeSTTFinalArrivesAfterLatency(t *testing.T) {
	s := newSTT(t, CapConfig{LatencyMS: 120}, Config{}, Deps{})
	ss := openWith(t, s)
	feed(t, ss, tone(500, 8000))
	start := time.Now()
	require.NoError(t, ss.Finalize())
	nextFinal(t, ss)
	require.GreaterOrEqual(t, time.Since(start), 110*time.Millisecond)
}

func TestFakeSTTOpenWaitsConnectMS(t *testing.T) {
	s := newSTT(t, CapConfig{ConnectMS: 80}, Config{}, Deps{})
	start := time.Now()
	ss, err := s.Open(context.Background(), provider.STTSessionConfig{})
	require.NoError(t, err)
	defer func() { _ = ss.Close() }()
	require.GreaterOrEqual(t, time.Since(start), 75*time.Millisecond)
}

func TestFakeSTTTimeoutFaultStopsEvents(t *testing.T) {
	faults := faultsFn(func() *FaultSet {
		return ParseFaultSet([]byte(`{"STT.FAKE_PRIMARY":{"mode":"timeout","rate":1,"after_ms":500}}`), t0, nil)
	})
	s := openWith(t, newSTT(t, CapConfig{}, Config{}, Deps{Faults: faults}))
	feed(t, s, tone(1200, 8000))
	require.NoError(t, s.Finalize())
	deadline := time.After(300 * time.Millisecond)
	var got []provider.STTEvent
loop:
	for {
		select {
		case ev := <-s.Events():
			got = append(got, ev)
		case <-deadline:
			break loop
		}
	}
	require.NotEmpty(t, got, "events before after_ms still arrive")
	for _, ev := range got {
		require.Equal(t, provider.STTPartial, ev.Type, "no FINAL: Finalize is not answered")
		require.LessOrEqual(t, ev.EndMs, int64(500))
	}
}

func TestFakeSTTDisconnectFault(t *testing.T) {
	faults := faultsFn(func() *FaultSet {
		return ParseFaultSet([]byte(`{"STT.FAKE_PRIMARY":{"mode":"disconnect_mid_stream","rate":1,"after_ms":400}}`), t0, nil)
	})
	s := openWith(t, newSTT(t, CapConfig{}, Config{}, Deps{Faults: faults}))
	feed(t, s, tone(1000, 8000))
	var kinds []provider.STTEventType
	for ev := range s.Events() {
		kinds = append(kinds, ev.Type)
		if ev.Type == provider.STTError {
			require.Equal(t, provider.KindUnavailable, ev.Err.Kind)
		}
	}
	require.Equal(t, provider.STTError, kinds[len(kinds)-2])
	require.Equal(t, provider.STTClosed, kinds[len(kinds)-1])
	require.ErrorIs(t, s.SendAudio(make([]byte, 640)), provider.ErrSessionClosed)
}

func TestFakeSTTOpenStartFailure(t *testing.T) {
	s := newSTT(t, CapConfig{FailEveryN: 1, FailureKind: provider.KindAuth}, Config{}, Deps{})
	_, err := s.Open(context.Background(), provider.STTSessionConfig{})
	var pe *provider.Error
	require.ErrorAs(t, err, &pe)
	require.Equal(t, provider.KindAuth, pe.Kind)
}

func TestFakeSTTMidFailureAfterSeconds(t *testing.T) {
	s := openWith(t, newSTT(t, CapConfig{FailEveryN: 1, FailurePhase: PhaseMid, FailAfter: 1, FailureKind: provider.KindUnavailable}, Config{}, Deps{}))
	feed(t, s, tone(2000, 8000))
	sawError := false
	for ev := range s.Events() {
		if ev.Type == provider.STTError {
			sawError = true
		}
	}
	require.True(t, sawError)
}

func TestFakeSTTFullBufferIsUnavailable(t *testing.T) {
	s := newSTT(t, CapConfig{LatencyMS: 5000}, Config{}, Deps{})
	ss := openWith(t, s)
	require.NoError(t, ss.SendAudio(tone(1500, 8000)))
	require.NoError(t, ss.Finalize()) // the worker is now busy waiting for LatencyMS on the FINAL
	var err error
	for i := 0; i < 10 && err == nil; i++ {
		err = ss.SendAudio(tone(500, 8000))
	}
	require.Error(t, err)
	require.Equal(t, provider.KindUnavailable, provider.KindOf(err))
}

func TestFakeSTTOddPCMRejected(t *testing.T) {
	s := openFake(t, Config{})
	err := s.SendAudio(make([]byte, 641))
	require.Equal(t, provider.KindInvalidRequest, provider.KindOf(err))
}

func TestFakeSTTPartialsAreCoalescedButFinalsSurvive(t *testing.T) {
	s := openFake(t, Config{})
	// 20 s of continuous speech = 100 PARTIAL opportunities; nobody reads the channel
	feed(t, s, tone(20000, 8000))
	require.NoError(t, s.Finalize())
	time.Sleep(50 * time.Millisecond)
	var nPartial, nFinal int
	for {
		ev := next(t, s)
		if ev.Type == provider.STTPartial {
			nPartial++
			continue
		}
		require.Equal(t, provider.STTFinal, ev.Type)
		require.True(t, ev.FromFinalize)
		nFinal++
		break
	}
	require.Equal(t, 1, nFinal)
	require.Less(t, nPartial, 100, "queued PARTIALs were replaced by newer ones")
	require.LessOrEqual(t, nPartial, 66, "64 buffered + 1 in flight + 1 coalesced")
}

func TestNewSTTFixtureErrors(t *testing.T) {
	_, err := NewSTT(provider.FakePrimary, CapConfig{}, Config{STTFixtureDir: filepath.Join(t.TempDir(), "missing")}, Deps{})
	require.Error(t, err)

	dir := t.TempDir()
	writeFixture(t, dir, "x", "テキスト", burstsA(), 8000)
	_, err = NewSTT(provider.FakePrimary, CapConfig{}, Config{STTFixtureDir: dir}, Deps{})
	require.ErrorContains(t, err, "sample rate")

	dir = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "y.wav"), provider.EncodeWAV(burstsA(), rate), 0o600))
	_, err = NewSTT(provider.FakePrimary, CapConfig{}, Config{STTFixtureDir: dir}, Deps{})
	require.ErrorContains(t, err, ".txt")

	dir = t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "z.wav"), []byte("not a wav"), 0o600))
	_, err = NewSTT(provider.FakePrimary, CapConfig{}, Config{STTFixtureDir: dir}, Deps{})
	require.Error(t, err)
}

func TestMatchScore(t *testing.T) {
	a := []float64{0, 1, 0, 1, 0, 1, 0, 1, 0, 1}
	require.InDelta(t, 1.0, matchScore(a, a, 3), 1e-9)
	shifted := append([]float64{0, 0}, a[:8]...)
	require.Greater(t, matchScore(a, shifted, 3), 0.7, "a small time shift still matches")
	flat := []float64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1}
	require.Zero(t, matchScore(a, flat, 3))
	require.Zero(t, matchScore(nil, a, 3))
	require.Less(t, matchScore(a[:4], append(a, a...), 3), 0.5, "a short input cannot match a long fixture")
}
