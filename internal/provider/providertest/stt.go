package providertest

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

const sttChunkBytes = 640 // 20 ms of PCM16LE mono 16 kHz

// feedSTT sends pcm in 20 ms chunks, retrying while the adapter's 2 s buffer is full.
func feedSTT(t *testing.T, s provider.STTSession, pcm []byte) {
	t.Helper()
	for off := 0; off < len(pcm); off += sttChunkBytes {
		end := min(off+sttChunkBytes, len(pcm))
		deadline := time.Now().Add(eventTimeout)
		for {
			err := s.SendAudio(pcm[off:end])
			if err == nil {
				break
			}
			require.Equal(t, provider.KindUnavailable, provider.KindOf(err), "only a full buffer may refuse audio")
			require.True(t, time.Now().Before(deadline), "audio buffer never drained")
			time.Sleep(time.Millisecond)
		}
	}
}

func openSTT(t *testing.T, p provider.STTProvider) provider.STTSession {
	t.Helper()
	s, err := p.Open(context.Background(), provider.STTSessionConfig{SessionID: uuid.Nil, Language: "ja", SampleRate: provider.STTSampleRate})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// nextSTT returns the next event or fails after the timeout.
func nextSTT(t *testing.T, s provider.STTSession) provider.STTEvent {
	t.Helper()
	select {
	case ev, ok := <-s.Events():
		require.True(t, ok, "events channel closed unexpectedly")
		return ev
	case <-time.After(eventTimeout):
		t.Fatal("providertest: no STT event in time")
	}
	return provider.STTEvent{}
}

// untilFinalFromFinalize reads events until a FINAL with FromFinalize, returning all of them.
func untilFinalFromFinalize(t *testing.T, s provider.STTSession) []provider.STTEvent {
	t.Helper()
	var evs []provider.STTEvent
	for {
		ev := nextSTT(t, s)
		evs = append(evs, ev)
		if ev.Type == provider.STTFinal && ev.FromFinalize {
			return evs
		}
		require.NotEqual(t, provider.STTError, ev.Type, "unexpected ERROR event")
	}
}

// STTSuite checks the provider.STTProvider contract (spec 8.2.2). speech returns PCM16LE
// mono 16 kHz audio with energy above −40 dBFS lasting at least 1 s.
func STTSuite(t *testing.T, newProvider func(t *testing.T) provider.STTProvider, speech func() []byte) {
	t.Helper()

	t.Run("FinalizeYieldsExactlyOneFinalFromFinalize", func(t *testing.T) {
		s := openSTT(t, newProvider(t))
		feedSTT(t, s, speech())
		for i := 0; i < 3; i++ {
			require.NoError(t, s.Finalize())
		}
		finals := 0
		for finals < 3 {
			ev := nextSTT(t, s)
			if ev.Type == provider.STTFinal {
				require.True(t, ev.FromFinalize, "no other FINAL may appear here")
				finals++
			}
		}
		select {
		case ev := <-s.Events():
			if ev.Type == provider.STTFinal {
				t.Fatal("a fourth FINAL arrived for three Finalize calls")
			}
		case <-time.After(100 * time.Millisecond):
		}
	})

	t.Run("PartialsPrecedeFinal", func(t *testing.T) {
		s := openSTT(t, newProvider(t))
		feedSTT(t, s, speech())
		require.NoError(t, s.Finalize())
		evs := untilFinalFromFinalize(t, s)
		require.Greater(t, len(evs), 1, "at least one PARTIAL before the FINAL")
		for _, ev := range evs[:len(evs)-1] {
			require.Equal(t, provider.STTPartial, ev.Type)
		}
	})

	t.Run("CloseEmitsClosedThenClosesChannel", func(t *testing.T) {
		s := openSTT(t, newProvider(t))
		require.NoError(t, s.Close())
		require.NoError(t, s.Close(), "Close is idempotent")
		var last provider.STTEvent
		deadline := time.After(eventTimeout)
	loop:
		for {
			select {
			case ev, ok := <-s.Events():
				if !ok {
					break loop
				}
				last = ev
			case <-deadline:
				t.Fatal("channel not closed after Close")
			}
		}
		require.Equal(t, provider.STTClosed, last.Type)
	})

	t.Run("SendAfterCloseReturnsErrSessionClosed", func(t *testing.T) {
		s := openSTT(t, newProvider(t))
		require.NoError(t, s.Close())
		require.ErrorIs(t, s.SendAudio(make([]byte, sttChunkBytes)), provider.ErrSessionClosed)
		require.ErrorIs(t, s.Finalize(), provider.ErrSessionClosed)
	})

	t.Run("EventsBufferCap64", func(t *testing.T) {
		s := openSTT(t, newProvider(t))
		require.Equal(t, 64, cap(s.Events()))
	})

	t.Run("StatsAudioMsSentCountsAudio", func(t *testing.T) {
		s := openSTT(t, newProvider(t))
		require.NoError(t, s.SendAudio(make([]byte, 32000))) // 1000 ms
		require.EqualValues(t, 1000, s.Stats().AudioMsSent)
		require.False(t, s.Stats().OpenedAt.IsZero())
	})
}
