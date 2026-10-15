package conversation

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/conversation/audio"
	"github.com/nabePi/kotoki-backend/internal/conversation/protocol"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

func newTestMetrics() *Metrics { return NewMetrics(prometheus.NewRegistry()) }

// env builds a small control envelope of the given type, tagged n so tests can tell them apart.
func env(typ string, n int) []byte {
	return []byte(fmt.Sprintf(`{"type":%q,"version":1,"server_seq":%d,"payload":{}}`, typ, n))
}

func envType(t *testing.T, b []byte) (string, int) {
	t.Helper()
	var e struct {
		Type string `json:"type"`
		Seq  int    `json:"server_seq"`
	}
	require.NoError(t, json.Unmarshal(b, &e))
	return e.Type, e.Seq
}

func frame(gen, seq uint32) audio.OutFrame {
	return audio.OutFrame{Gen: gen, Seq: seq, Opus: []byte{0xF8, byte(seq)}}
}

func nextNow(t *testing.T, o *Outbound) OutItem {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	it, ok := o.Next(ctx)
	require.True(t, ok, "Next: no item")
	o.done()
	return it
}

func TestOutboundPriority(t *testing.T) {
	o := NewOutbound(256, nil)
	for i := range uint32(3) {
		o.EnqueueAudio(frame(1, i))
	}
	require.NoError(t, o.EnqueueControl(env("ai.sentence", 1), true))
	require.NoError(t, o.EnqueueControl(env("stt.partial", 2), false))

	// All control first, in FIFO order, then one audio frame.
	it := nextNow(t, o)
	require.Equal(t, MessageText, it.Kind)
	typ, _ := envType(t, it.Env)
	require.Equal(t, "ai.sentence", typ)
	it = nextNow(t, o)
	require.Equal(t, MessageText, it.Kind)
	typ, _ = envType(t, it.Env)
	require.Equal(t, "stt.partial", typ)
	it = nextNow(t, o)
	require.Equal(t, MessageBinary, it.Kind)
	require.Equal(t, uint32(0), it.Frame.Seq)

	// Control enqueued between frames still overtakes the remaining audio.
	require.NoError(t, o.EnqueueControl(env("ai.audio_completed", 3), true))
	it = nextNow(t, o)
	require.Equal(t, MessageText, it.Kind)
	it = nextNow(t, o)
	require.Equal(t, MessageBinary, it.Kind)
	require.Equal(t, uint32(1), it.Frame.Seq)
	require.Equal(t, 1, o.AudioLen())
	require.False(t, o.Drained())
	it = nextNow(t, o)
	require.Equal(t, uint32(2), it.Frame.Seq)
	require.True(t, o.Drained())
}

func TestOutboundNextBlocksAndHonoursCtx(t *testing.T) {
	noLeak(t)
	o := NewOutbound(8, nil)
	got := make(chan OutItem, 1)
	go func() {
		it, ok := o.Next(context.Background())
		if ok {
			got <- it
		}
	}()
	select {
	case <-got:
		t.Fatal("Next returned on an empty queue")
	case <-time.After(20 * time.Millisecond):
	}
	o.EnqueueAudio(frame(1, 7))
	select {
	case it := <-got:
		require.Equal(t, uint32(7), it.Frame.Seq)
	case <-time.After(2 * time.Second):
		t.Fatal("Next did not wake on enqueue")
	}
	o.done()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, ok := o.Next(ctx)
	require.False(t, ok)
}

func TestPurgeAudioDropsStale(t *testing.T) {
	m := newTestMetrics()
	o := NewOutbound(256, m)
	for i := range uint32(10) {
		o.EnqueueAudio(frame(1, i))
	}
	o.EnqueueAudioEnd(1)
	require.Equal(t, 10, o.AudioLen())
	require.NoError(t, o.EnqueueControl(env("ai.interrupted", 1), true))

	o.PurgeAudio(2)

	require.Equal(t, 0, o.AudioLen())
	require.Equal(t, 1, o.ControlLen(), "purge never touches control")
	require.Equal(t, 10.0, promtest.ToFloat64(m.audioFramesOut.WithLabelValues("dropped_stale")))
	it := nextNow(t, o)
	require.Equal(t, MessageText, it.Kind)
	require.True(t, o.Drained(), "the end marker of the stale generation is purged too")

	// Frames of the active generation survive a purge.
	o.EnqueueAudio(frame(2, 0))
	o.PurgeAudio(2)
	require.Equal(t, 1, o.AudioLen())
}

func TestSlowConsumer(t *testing.T) {
	m := newTestMetrics()
	o := NewOutbound(256, m)
	for i := range 256 {
		require.NoError(t, o.EnqueueControl(env("ai.sentence", i), true))
	}
	require.ErrorIs(t, o.EnqueueControl(env("ai.sentence", 256), true), ErrSlowConsumer)
	require.ErrorIs(t, o.EnqueueControl(env("turn.completed", 257), true), ErrSlowConsumer)
	require.Equal(t, 256, o.ControlLen())
	require.Equal(t, 1.0, promtest.ToFloat64(m.slowConsumer), "counted once per connection")

	// A transient message on a full queue is dropped, not an error.
	require.NoError(t, o.EnqueueControl(env("stt.partial", 258), false))
	require.Equal(t, 256, o.ControlLen())
}

func TestSlowConsumerCountsAudio(t *testing.T) {
	o := NewOutbound(4, nil)
	o.EnqueueAudio(frame(1, 0))
	o.EnqueueAudio(frame(1, 1))
	require.NoError(t, o.EnqueueControl(env("ai.sentence", 1), true))
	require.NoError(t, o.EnqueueControl(env("ai.sentence", 2), true))
	require.ErrorIs(t, o.EnqueueControl(env("ai.sentence", 3), true), ErrSlowConsumer, "cap counts control + audio")
}

func TestTransientCoalescing(t *testing.T) {
	m := newTestMetrics()
	o := NewOutbound(256, m)
	for i := range 128 {
		require.NoError(t, o.EnqueueControl(env("ai.sentence", i), true))
	}
	// len(control) == 128 is not > 128: the first tick is queued normally.
	require.NoError(t, o.EnqueueControl(env("balance.tick", 1000), false))
	require.Equal(t, 129, o.ControlLen())
	// From now on a repeated tick replaces the queued one (newest kept, moved to the tail).
	require.NoError(t, o.EnqueueControl(env("balance.tick", 1001), false))
	require.NoError(t, o.EnqueueControl(env("balance.tick", 1002), false))
	require.Equal(t, 129, o.ControlLen())
	// A transient with no queued twin is dropped.
	require.NoError(t, o.EnqueueControl(env("stt.partial", 2000), false))
	require.Equal(t, 129, o.ControlLen())
	// Durable messages are never coalesced.
	require.NoError(t, o.EnqueueControl(env("turn.completed", 3000), true))
	require.Equal(t, 130, o.ControlLen())

	require.Equal(t, 2.0, promtest.ToFloat64(m.outboundDropped.WithLabelValues("coalesced")))
	require.Equal(t, 1.0, promtest.ToFloat64(m.outboundDropped.WithLabelValues("dropped")))

	var ticks []int
	var order []string
	for o.ControlLen() > 0 {
		typ, seq := envType(t, nextNow(t, o).Env)
		order = append(order, typ)
		if typ == "balance.tick" {
			ticks = append(ticks, seq)
		}
	}
	require.Equal(t, []int{1002}, ticks)
	require.Equal(t, "balance.tick", order[128])
	require.Equal(t, "turn.completed", order[129])
}

// The durability of a server event: protocol.IsDurable plus non-fatal `error` (§7.4.3).
func TestEventDurable(t *testing.T) {
	nonFatal := protocol.NewError(errs.CodeInvalidMessage, testSID, nil, nil)
	fatal := protocol.NewError(errs.CodeProtocolViolation, testSID, nil, nil)
	cases := []struct {
		name string
		ev   protocol.ServerEvent
		want bool
	}{
		{"durable type", protocol.ServerEvent{Type: "ai.sentence"}, true},
		{"transient type", protocol.ServerEvent{Type: "balance.tick"}, false},
		{"non-fatal error", protocol.ServerEvent{Type: "error", Payload: nonFatal}, true},
		{"non-fatal error pointer", protocol.ServerEvent{Type: "error", Payload: &nonFatal}, true},
		{"fatal error", protocol.ServerEvent{Type: "error", Payload: fatal}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { require.Equal(t, tc.want, EventDurable(tc.ev)) })
	}
}

func TestEnqueueEventNonFatalErrorIsDurable(t *testing.T) {
	o := NewOutbound(2, nil)
	require.NoError(t, o.EnqueueControl(env("ai.sentence", 1), true))
	require.NoError(t, o.EnqueueControl(env("ai.sentence", 2), true))

	ev := protocol.ServerEvent{Type: "error", SessionID: testSID, TS: t0, Payload: protocol.NewError(errs.CodeInvalidMessage, testSID, nil, nil)}
	require.ErrorIs(t, o.EnqueueEvent(ev), ErrSlowConsumer, "non-fatal error is durable: full queue → slow consumer")

	fatal := ev
	fatal.Payload = protocol.NewError(errs.CodeProtocolViolation, testSID, nil, nil)
	require.NoError(t, o.EnqueueEvent(fatal), "fatal error is transient: dropped on a full queue")

	o2 := NewOutbound(4, nil)
	require.NoError(t, o2.EnqueueEvent(ev))
	it := nextNow(t, o2)
	got, err := protocol.DecodeServerEvent(it.Env)
	require.NoError(t, err)
	require.Equal(t, "error", got.Type)
}
