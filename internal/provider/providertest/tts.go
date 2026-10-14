package providertest

import (
	"context"
	"errors"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// collectTTS reads chunks until the channel closes.
func collectTTS(t *testing.T, s provider.TTSStream) []provider.TTSChunk {
	t.Helper()
	var out []provider.TTSChunk
	deadline := time.After(eventTimeout)
	for {
		select {
		case c, ok := <-s.Chunks():
			if !ok {
				return out
			}
			out = append(out, c)
		case <-deadline:
			t.Fatal("providertest: TTS stream did not finish in time")
		}
	}
}

// TTSSuite checks the provider.TTSProvider contract (spec 8.2.4). newReq must return a
// sentence long enough (≥ 400 ms of audio) for the cancellation subtest.
func TTSSuite(t *testing.T, newProvider func(t *testing.T) provider.TTSProvider, newReq func() provider.TTSRequest) {
	t.Helper()

	t.Run("PCMLengthAlwaysEven", func(t *testing.T) {
		p := newProvider(t)
		s, err := p.Synthesize(context.Background(), newReq())
		require.NoError(t, err)
		chunks := collectTTS(t, s)
		require.NotEmpty(t, chunks)
		for i, c := range chunks {
			require.Zero(t, len(c.PCM)%2, "chunk %d has odd length", i)
		}
	})

	t.Run("LastChunkIsFinalWithUsage", func(t *testing.T) {
		p := newProvider(t)
		req := newReq()
		s, err := p.Synthesize(context.Background(), req)
		require.NoError(t, err)
		chunks := collectTTS(t, s)
		last := chunks[len(chunks)-1]
		require.True(t, last.Final)
		require.Nil(t, last.Err)
		require.NotNil(t, last.Usage)
		require.Equal(t, utf8.RuneCountInString(req.Text), last.Usage.Characters)
		require.Positive(t, last.Usage.AudioMs)
		for _, c := range chunks[:len(chunks)-1] {
			require.False(t, c.Final)
			require.Nil(t, c.Usage, "usage only on the final chunk")
		}
	})

	t.Run("FirstChunkAfterLatency", func(t *testing.T) {
		p := newProvider(t)
		s, err := p.Synthesize(context.Background(), newReq())
		require.NoError(t, err)
		defer func() { _ = s.Close() }()
		select {
		case c := <-s.Chunks():
			require.Equal(t, p.Name(), c.Provider)
			require.False(t, c.ReceivedAt.IsZero())
			require.Nil(t, c.Err)
		case <-time.After(eventTimeout):
			t.Fatal("no first chunk")
		}
	})

	t.Run("CancelClosesChannelWithin50ms", func(t *testing.T) {
		p := newProvider(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s, err := p.Synthesize(ctx, newReq())
		require.NoError(t, err)
		select {
		case <-s.Chunks():
		case <-time.After(eventTimeout):
			t.Fatal("no first chunk")
		}
		start := time.Now()
		cancel()
		for c := range s.Chunks() {
			require.False(t, c.Final, "a cancelled stream never completes")
			require.Nil(t, c.Err)
		}
		require.Less(t, time.Since(start), 50*time.Millisecond)
		require.NoError(t, s.Close())
		require.NoError(t, s.Close(), "Close is idempotent")
	})

	t.Run("SyncErrorIsProviderError", func(t *testing.T) {
		p := newProvider(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := p.Synthesize(ctx, newReq())
		require.Error(t, err)
		var pe *provider.Error
		require.True(t, errors.As(err, &pe), "synchronous failures are *provider.Error, got %T", err)
		require.Equal(t, provider.KindCancelled, pe.Kind)
	})
}
