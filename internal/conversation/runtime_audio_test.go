package conversation

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/audio"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

// recSTT is the STT stream the audio pipeline writes to: it keeps every PCM chunk it is sent.
type recSTT struct {
	mu     sync.Mutex
	chunks [][]byte
}

var _ provider.RoutedSTTSession = (*recSTT)(nil)

func (s *recSTT) SendAudio(pcm []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chunks = append(s.chunks, append([]byte(nil), pcm...))
	return nil
}
func (s *recSTT) Finalize() error                  { return nil }
func (s *recSTT) Events() <-chan provider.STTEvent { return nil }
func (s *recSTT) Close() error                     { return nil }
func (s *recSTT) Stats() provider.STTStats         { return provider.STTStats{} }
func (s *recSTT) Provider() provider.Name          { return provider.FakePrimary }
func (s *recSTT) ReportFinalTimeout()              {}

func (s *recSTT) frames() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([][]byte(nil), s.chunks...)
}

// silent reports whether a PCM16LE chunk is all zeros.
func silent(pcm []byte) bool {
	for _, b := range pcm {
		if b != 0 {
			return false
		}
	}
	return true
}

type pipeRig struct {
	t      *testing.T
	clk    *clock.Fake
	m      *Metrics
	log    *bytes.Buffer
	stt    *recSTT
	events chan frameEvent
	p      *audioPipeline
	enc    *audio.Encoder
	tone   []byte // one opus packet of a 440 Hz tone, 20 ms at 48 kHz
}

func newPipeRig(t *testing.T) *pipeRig {
	t.Helper()
	clk := clock.NewFake(time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC))
	rig := &pipeRig{t: t, clk: clk, m: NewMetrics(prometheus.NewRegistry()), log: &bytes.Buffer{}, stt: &recSTT{}, events: make(chan frameEvent, 512)}
	r := &Runtime{
		d:           Deps{Clock: clk, Metrics: rig.m},
		log:         slog.New(slog.NewTextHandler(rig.log, nil)),
		frameEvents: rig.events,
	}
	r.sttPipe.Store(&sttBox{s: rig.stt})
	p, err := r.newAudioPipeline(&connState{ctx: context.Background()})
	require.NoError(t, err)
	rig.p = p
	rig.enc, err = audio.NewEncoder(48000, 32000, 5)
	require.NoError(t, err)
	block := make([]int16, 960)
	for i := range block {
		block[i] = int16(8000 * math.Sin(2*math.Pi*440*float64(i)/48000))
	}
	rig.tone, err = rig.enc.Encode(block)
	require.NoError(t, err)
	return rig
}

// send feeds one packet with the given seq.
func (g *pipeRig) send(seq uint32, opus []byte) error {
	g.t.Helper()
	ok, err := g.p.packet(context.Background(), AudioIn{Seq: seq, Opus: opus, At: g.clk.Now()})
	if err == nil {
		require.True(g.t, ok)
	}
	return err
}

func (g *pipeRig) drainEvents() []frameEvent {
	var out []frameEvent
	for {
		select {
		case fe := <-g.events:
			out = append(out, fe)
		default:
			return out
		}
	}
}

func TestSeqGapPLC(t *testing.T) {
	g := newPipeRig(t)
	require.NoError(t, g.send(0, g.tone))
	require.Len(t, g.stt.frames(), 1)

	// gap 3: three concealed frames (not silence) before the frame that showed the gap
	require.NoError(t, g.send(4, g.tone))
	fr := g.stt.frames()
	require.Len(t, fr, 1+3+1)
	for i, f := range fr[1:4] {
		require.Len(t, f, 640, "frame %d is 320 samples of PCM16", i)
		require.False(t, silent(f), "gap 3 → PLC: a concealed frame carries signal")
	}
	require.False(t, silent(fr[4]))
	require.Equal(t, 1.0, promtest.ToFloat64(g.m.audioFramesIn.WithLabelValues("gap")))

	// gap 30: thirty frames of silence, so the silence detector stays accurate
	require.NoError(t, g.send(35, g.tone)) // expected 5 → 35
	fr = g.stt.frames()
	require.Len(t, fr, 5+30+1)
	for _, f := range fr[5:35] {
		require.True(t, silent(f), "gap 30 → silence")
	}
	require.False(t, silent(fr[35]))

	// gap 80: only 50 frames of silence (1 s) and a WARN
	require.NotContains(t, g.log.String(), "upstream discontinuity")
	require.NoError(t, g.send(116, g.tone)) // expected 36 → 116
	fr = g.stt.frames()
	require.Len(t, fr, 36+50+1)
	for _, f := range fr[36:86] {
		require.True(t, silent(f))
	}
	require.Contains(t, g.log.String(), "level=WARN")
	require.Contains(t, g.log.String(), "upstream discontinuity")

	// a duplicate or late frame is dropped and counted
	n := len(g.stt.frames())
	require.NoError(t, g.send(10, g.tone))
	require.NoError(t, g.send(116, g.tone))
	require.Len(t, g.stt.frames(), n, "nothing reaches the STT")
	require.Equal(t, 2.0, promtest.ToFloat64(g.m.audioFramesIn.WithLabelValues("dup")))
	require.Equal(t, 3.0, promtest.ToFloat64(g.m.audioFramesIn.WithLabelValues("gap")))
	require.Equal(t, 1.0, promtest.ToFloat64(g.m.audioFramesIn.WithLabelValues("ok")), "only seq 0 arrived in order")

	// every frame the STT got was also reported to the loop (the detector's audio clock)
	require.Len(t, g.drainEvents(), len(g.stt.frames()))
}

func TestInvalidFrameSilenceInserted(t *testing.T) {
	g := newPipeRig(t)
	bad := []byte{0x00} // a 10 ms packet: libopus decodes it, but it is not 20 ms → invalid (§7.5.1)

	require.NoError(t, g.send(0, bad))
	fe := g.drainEvents()
	require.Len(t, fe, 1)
	require.True(t, fe[0].invalid, "the first invalid frame is announced")
	fr := g.stt.frames()
	require.Len(t, fr, 1, "a silence frame keeps the timeline moving")
	require.Len(t, fr[0], 640)
	require.True(t, silent(fr[0]))
	require.Equal(t, 1.0, promtest.ToFloat64(g.m.audioFramesIn.WithLabelValues("invalid")))

	// at most one notice per 5 s
	for seq := uint32(1); seq < 25; seq++ {
		g.clk.Advance(20 * time.Millisecond)
		require.NoError(t, g.send(seq, bad))
	}
	for _, e := range g.drainEvents() {
		require.False(t, e.invalid, "no second AUDIO_FRAME_INVALID inside 5 s")
	}
	g.clk.Advance(5 * time.Second)
	require.NoError(t, g.send(25, bad))
	fe = g.drainEvents()
	require.Len(t, fe, 1)
	require.True(t, fe[0].invalid, "5 s later it is announced again")

	// 50 invalid frames in 10 s are tolerated, the 51st is a violation
	g = newPipeRig(t)
	for seq := range uint32(50) {
		require.NoError(t, g.send(seq, bad), "frame %d", seq)
	}
	err := g.send(50, bad)
	var pe *ProtocolError
	require.True(t, errors.As(err, &pe), "the 51st invalid frame in 10 s: %v", err)
	require.Equal(t, errs.CodeProtocolViolation, pe.Code)
	require.Equal(t, ViolationInvalidBurst, pe.Kind)
	require.Equal(t, 1.0, promtest.ToFloat64(g.m.protocolViolations.WithLabelValues(ViolationInvalidBurst)))

	// frames spread over more than 10 s never reach it
	g = newPipeRig(t)
	for seq := range uint32(120) {
		g.clk.Advance(250 * time.Millisecond)
		require.NoError(t, g.send(seq, bad), "frame %d", seq)
	}
}
