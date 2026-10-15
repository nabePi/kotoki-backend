package conversation

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/audio"
	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

type writerRig struct {
	conn  *pipeConn
	out   *Outbound
	gen   *atomic.Uint32
	acks  chan WriterAck
	w     *Writer
	m     *Metrics
	tos   *fakeTimeouts
	done  chan error
	stop  context.CancelFunc
	clock *clock.Fake
}

func startWriter(t *testing.T, activeGen uint32) *writerRig {
	t.Helper()
	noLeak(t)
	clk := clock.NewFake(t0)
	r := &writerRig{conn: newPipeConn(), gen: &atomic.Uint32{}, acks: make(chan WriterAck, 64), m: newTestMetrics(),
		done: make(chan error, 1), clock: clk, tos: newFakeTimeouts(clk)}
	r.out = NewOutbound(256, r.m)
	r.gen.Store(activeGen)
	r.w = NewWriter(5*time.Second, r.m)
	r.w.WithTimeout = r.tos.WithTimeout
	ctx, cancel := context.WithCancel(context.Background())
	r.stop = cancel
	go func() { r.done <- r.w.Run(ctx, r.conn, r.out, r.gen, r.acks) }()
	t.Cleanup(func() {
		cancel()
		<-r.done
	})
	return r
}

func (r *writerRig) waitDrained(t *testing.T) {
	t.Helper()
	require.Eventually(t, r.out.Drained, 2*time.Second, time.Millisecond)
}

func TestWriterWritesControlAndAudio(t *testing.T) {
	r := startWriter(t, 1)
	require.NoError(t, r.out.EnqueueControl(env("ai.generation_started", 1), true))
	r.out.EnqueueAudio(audio.OutFrame{Gen: 1, Seq: 0, SentenceIdx: 0, Opus: []byte{1, 2, 3}})
	r.out.EnqueueAudio(audio.OutFrame{Gen: 1, Seq: 1, SentenceIdx: 0, LastOfSentence: true, Opus: []byte{4}})
	r.out.EnqueueAudio(audio.OutFrame{Gen: 1, Seq: 2, SentenceIdx: 1, LastOfSentence: true, Opus: []byte{5}})
	r.out.EnqueueAudioEnd(1)
	r.waitDrained(t)

	w := r.conn.written()
	require.Len(t, w, 4)
	require.Equal(t, MessageText, w[0].Kind)
	require.JSONEq(t, string(env("ai.generation_started", 1)), string(w[0].Data))
	for i, x := range w[1:] {
		require.Equal(t, MessageBinary, x.Kind)
		gen, seq, _, err := protocol.DecodeDownstream(x.Data)
		require.NoError(t, err)
		require.Equal(t, uint32(1), gen)
		require.Equal(t, uint32(i), seq)
	}
	_, _, opus, _ := protocol.DecodeDownstream(w[1].Data)
	require.Equal(t, []byte{1, 2, 3}, opus)

	require.Equal(t, WriterAck{Kind: AckSentenceWritten, Gen: 1, SentenceIdx: 0, Sentences: 1}, <-r.acks)
	require.Equal(t, WriterAck{Kind: AckSentenceWritten, Gen: 1, SentenceIdx: 1, Sentences: 2}, <-r.acks)
	require.Equal(t, WriterAck{Kind: AckAudioWrittenAll, Gen: 1, Sentences: 2}, <-r.acks)
	require.Equal(t, "AUDIO_WRITTEN_ALL", AckAudioWrittenAll.String())
	require.Equal(t, 2, r.w.SentencesWritten(1))
	require.Equal(t, 0, r.w.SentencesWritten(2))
	require.Equal(t, 3.0, promtest.ToFloat64(r.m.audioFramesOut.WithLabelValues("written")))
}

// Review Focus 2: a frame whose generation is no longer active is never written, even when it
// is already out of the queue (barge-in between PurgeAudio and the write).
func TestWriterDropsStaleGen(t *testing.T) {
	r := startWriter(t, 1)
	r.gen.Store(2) // barge-in: generation 2 is active before the gen-1 frame reaches the writer
	r.out.EnqueueAudio(audio.OutFrame{Gen: 1, Seq: 0, LastOfSentence: true, Opus: []byte{1}})
	r.out.EnqueueAudioEnd(1)
	r.waitDrained(t)

	require.Empty(t, r.conn.written())
	require.Equal(t, 1.0, promtest.ToFloat64(r.m.audioFramesOut.WithLabelValues("dropped_stale")))
	require.Equal(t, 0.0, promtest.ToFloat64(r.m.audioFramesOut.WithLabelValues("written")))
	require.Empty(t, r.acks, "no sentence or AUDIO_WRITTEN_ALL ack for a stale generation")
	require.Equal(t, 0, r.w.SentencesWritten(1))

	// The active generation still flows.
	r.out.EnqueueAudio(audio.OutFrame{Gen: 2, Seq: 0, Opus: []byte{9}})
	r.waitDrained(t)
	require.Len(t, r.conn.written(), 1)
}

func TestWriterTimeout(t *testing.T) {
	r := startWriter(t, 1)
	r.conn.mu.Lock()
	r.conn.blockWrites = true
	r.conn.mu.Unlock()
	require.NoError(t, r.out.EnqueueControl(env("ai.sentence", 1), true))
	waitSignal(t, r.conn.writeEnter, "Write to start")
	require.Equal(t, []time.Duration{5 * time.Second}, r.tos.durations(), "5 s per message (§7.12.3)")

	r.tos.Advance(4 * time.Second)
	select {
	case err := <-r.done:
		t.Fatalf("writer stopped before 5 s: %v", err)
	default:
	}
	r.tos.Advance(time.Second)
	err := waitErr(t, r.done, "writer")
	require.ErrorIs(t, err, ErrWriteTimeout)
	r.done <- err // for the cleanup
}

func TestWriterStopsOnCtx(t *testing.T) {
	r := startWriter(t, 1)
	r.stop()
	err := waitErr(t, r.done, "writer")
	require.NoError(t, err)
	r.done <- err
}

func TestWriterNilAcksAndMetrics(t *testing.T) {
	noLeak(t)
	conn := newPipeConn()
	out := NewOutbound(8, nil)
	var gen atomic.Uint32
	gen.Store(3)
	w := NewWriter(0, nil) // defaults: 5 s, no metrics
	require.Equal(t, 5*time.Second, w.Timeout)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx, conn, out, &gen, nil) }()
	out.EnqueueAudio(audio.OutFrame{Gen: 3, LastOfSentence: true, Opus: []byte{1}})
	out.EnqueueAudioEnd(3)
	require.Eventually(t, out.Drained, 2*time.Second, time.Millisecond)
	require.Len(t, conn.written(), 1)
	require.Equal(t, 1, w.SentencesWritten(3))
	cancel()
	require.NoError(t, waitErr(t, done, "writer"))
}
