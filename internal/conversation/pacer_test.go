package conversation

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/audio"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

var pacerT0 = time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)

// fakeSleep advances the fake clock by d (the pacer's only source of waiting).
func fakeSleep(clk *clock.Fake, calls *int) func(context.Context, time.Duration) error {
	return func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		*calls++
		clk.Advance(d)
		return nil
	}
}

// frameCh returns a closed channel holding n frames, each its own sentence (so OnSentenceStart
// fires right after every enqueue and observes its time).
// drainOne pops one queued item, standing in for the writer.
func drainOne(out *Outbound) {
	out.mu.Lock()
	defer out.mu.Unlock()
	out.pop()
}

func frameCh(gen uint32, n int) chan audio.OutFrame {
	ch := make(chan audio.OutFrame, n)
	for i := 0; i < n; i++ {
		ch <- audio.OutFrame{Gen: gen, Seq: uint32(i), SentenceIdx: i, LastOfSentence: true, Opus: []byte{1}}
	}
	close(ch)
	return ch
}

func TestPacerBurstThenRealtime(t *testing.T) {
	clk := clock.NewFake(pacerT0)
	var sleeps int
	p := NewPacer(clk, fakeSleep(clk, &sleeps), 400)
	out := NewOutbound(256, nil)
	var times []time.Time
	err := p.Run(context.Background(), frameCh(1, 50), out, PacerHooks{
		OnSentenceStart: func(audio.OutFrame) { times = append(times, clk.Now()); drainOne(out) },
	})
	require.NoError(t, err)
	require.Len(t, times, 50)
	for i := 0; i < 20; i++ {
		require.True(t, times[i].Equal(pacerT0), "frame %d must burst without waiting", i)
	}
	for i := 21; i < 50; i++ {
		require.Equal(t, 20*time.Millisecond, times[i].Sub(times[i-1]), "frame %d", i)
	}
	require.Equal(t, time.Millisecond, times[20].Sub(pacerT0))
}

func TestPacerUnderrunResetsClock(t *testing.T) {
	clk := clock.NewFake(pacerT0)
	var sleeps int
	p := NewPacer(clk, fakeSleep(clk, &sleeps), 400)
	out := NewOutbound(256, nil)
	var maxLead time.Duration
	err := p.Run(context.Background(), frameCh(1, 60), out, PacerHooks{
		OnSentenceStart: func(f audio.OutFrame) {
			drainOne(out)
			if f.Seq == 4 {
				clk.Advance(time.Second) // TTS gap
			}
			if f.Seq > 4 {
				if lead := p.EstimatedPlaybackEnd().Sub(clk.Now()); lead > maxLead {
					maxLead = lead
				}
			}
		},
	})
	require.NoError(t, err)
	require.LessOrEqual(t, maxLead, 400*time.Millisecond, "no catch-up burst after an underrun")
}

func TestPacerBackpressure(t *testing.T) {
	clk := clock.NewFake(pacerT0)
	out := NewOutbound(256, nil)
	for i := 0; i < 25; i++ {
		out.EnqueueAudio(audio.OutFrame{Gen: 1})
	}
	sleeps := 0
	sleep := func(ctx context.Context, d time.Duration) error {
		require.Positive(t, d)
		sleeps++
		clk.Advance(d)
		if sleeps == 3 {
			_, ok := out.Next(ctx) // the writer drains one frame
			require.True(t, ok)
		}
		return nil
	}
	p := NewPacer(clk, sleep, 400)
	sleepsAtFirst := -1
	err := p.Run(context.Background(), frameCh(1, 1), out, PacerHooks{
		BeforeFirst: func() { sleepsAtFirst = sleeps },
	})
	require.NoError(t, err)
	require.Equal(t, 3, sleepsAtFirst, "must wait while AudioLen() >= 25")
}

func TestPacerHookOrder(t *testing.T) {
	clk := clock.NewFake(pacerT0)
	var sleeps int
	p := NewPacer(clk, fakeSleep(clk, &sleeps), 400)
	out := NewOutbound(256, nil)
	var events []string
	lenAt := func(name string) { events = append(events, name+":"+string(rune('0'+out.AudioLen()))) }
	ch := make(chan audio.OutFrame, 4)
	ch <- audio.OutFrame{Gen: 1, Seq: 0, SentenceIdx: 0}
	ch <- audio.OutFrame{Gen: 1, Seq: 1, SentenceIdx: 0, LastOfSentence: true}
	ch <- audio.OutFrame{Gen: 1, Seq: 2, SentenceIdx: 1}
	close(ch)
	var done [2]int64
	err := p.Run(context.Background(), ch, out, PacerHooks{
		BeforeFirst:     func() { lenAt("before") },
		OnFirst:         func() { lenAt("first") },
		OnSentenceStart: func(f audio.OutFrame) { events = append(events, "sentence"+string(rune('0'+f.SentenceIdx))) },
		OnDone:          func(n int, ms int64) { done = [2]int64{int64(n), ms} },
	})
	require.NoError(t, err)
	require.Equal(t, []string{"before:0", "first:1", "sentence0", "sentence1"}, events)
	require.Equal(t, [2]int64{3, 60}, done)
}

func TestPacerCancel(t *testing.T) {
	clk := clock.NewFake(pacerT0)
	out := NewOutbound(256, nil)
	for i := 0; i < 25; i++ {
		out.EnqueueAudio(audio.OutFrame{Gen: 1})
	}
	ctx, cancel := context.WithCancel(context.Background())
	sleep := func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	p := NewPacer(clk, sleep, 400)
	err := p.Run(ctx, frameCh(1, 5), out, PacerHooks{})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, 25, out.AudioLen(), "nothing enqueued after cancel")
	require.Equal(t, 25, countItems(out), "no end marker for a cancelled generation")
}

// countItems drains out and returns the number of audio items (frames and end markers).
func countItems(out *Outbound) int {
	n := 0
	for {
		out.mu.Lock()
		l := len(out.audio)
		out.mu.Unlock()
		if l == 0 {
			return n
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = out.Next(ctx)
		cancel()
		n++
	}
}

func TestPacerCancelledBeforeStartEnqueuesNothing(t *testing.T) {
	clk := clock.NewFake(pacerT0)
	out := NewOutbound(256, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := NewPacer(clk, func(context.Context, time.Duration) error { return nil }, 400)
	err := p.Run(ctx, frameCh(1, 5), out, PacerHooks{})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, countItems(out))
}

func TestPlaybackEnd(t *testing.T) {
	clk := clock.NewFake(pacerT0)
	var sleeps int
	p := NewPacer(clk, fakeSleep(clk, &sleeps), 400)
	require.True(t, p.EstimatedPlaybackEnd().IsZero())
	out := NewOutbound(256, nil)
	require.NoError(t, p.Run(context.Background(), frameCh(1, 10), out, PacerHooks{}))
	require.True(t, p.EstimatedPlaybackEnd().Equal(pacerT0.Add(200*time.Millisecond)))
}

func TestPacerEndMarkerOnlyOnNormalEnd(t *testing.T) {
	clk := clock.NewFake(pacerT0)
	var sleeps int
	p := NewPacer(clk, fakeSleep(clk, &sleeps), 400)

	// normal end: frames then one end marker of the generation
	out := NewOutbound(256, nil)
	require.NoError(t, p.Run(context.Background(), frameCh(7, 3), out, PacerHooks{}))
	require.Equal(t, 4, countItemsKeep(t, out, 7))

	// no frames at all: nothing to ack, no marker
	out = NewOutbound(256, nil)
	require.NoError(t, NewPacer(clk, fakeSleep(clk, &sleeps), 400).Run(context.Background(), frameCh(7, 0), out, PacerHooks{}))
	require.Zero(t, countItems(out))
}

// countItemsKeep drains out, asserting the last item is the end marker of gen; returns the item count.
func countItemsKeep(t *testing.T, out *Outbound, gen uint32) int {
	t.Helper()
	n := 0
	var last OutItem
	for out.AudioLen() > 0 || n == 0 || !last.GenEnd {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		it, ok := out.Next(ctx)
		cancel()
		require.True(t, ok)
		last = it
		n++
		if last.GenEnd {
			break
		}
	}
	require.True(t, last.GenEnd)
	require.Equal(t, gen, last.Frame.Gen)
	return n
}
