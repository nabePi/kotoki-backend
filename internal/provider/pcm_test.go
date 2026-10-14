package provider_test

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

func sine(rate int, hz float64, ms int, amp float64) []byte {
	n := rate * ms / 1000
	out := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := int16(amp * math.Sin(2*math.Pi*hz*float64(i)/float64(rate)))
		binary.LittleEndian.PutUint16(out[i*2:], uint16(v))
	}
	return out
}

func TestWAVRoundTrip(t *testing.T) {
	pcm := sine(16000, 440, 1000, 10000)
	wav := provider.EncodeWAV(pcm, 16000)
	require.Equal(t, 44+len(pcm), len(wav))
	require.Equal(t, "RIFF", string(wav[:4]))
	got, rate, err := provider.DecodeWAV(wav)
	require.NoError(t, err)
	require.Equal(t, 16000, rate)
	require.Equal(t, pcm, got)
	ms, err := provider.WAVDurationMs(wav)
	require.NoError(t, err)
	require.EqualValues(t, 1000, ms)
}

func TestDecodeWAVRejectsNonPCM16(t *testing.T) {
	wav := provider.EncodeWAV(make([]byte, 64), 16000)
	bad := append([]byte(nil), wav...)
	binary.LittleEndian.PutUint16(bad[20:], 3) // IEEE float
	_, _, err := provider.DecodeWAV(bad)
	require.Error(t, err)
	bad = append([]byte(nil), wav...)
	binary.LittleEndian.PutUint16(bad[34:], 24) // 24 bits
	_, _, err = provider.DecodeWAV(bad)
	require.Error(t, err)
	_, _, err = provider.DecodeWAV([]byte("not a wav"))
	require.Error(t, err)
}

func TestResample24kTo16k(t *testing.T) {
	in := sine(24000, 440, 500, 12000)
	out := provider.ResamplePCM16(in, 24000, 16000)
	require.InDelta(t, len(in)*2/3, len(out), 2)
	require.Zero(t, len(out)%2)
	require.InDelta(t, provider.RMSdBFS(in), provider.RMSdBFS(out), 1.0)
}

func TestRMSdBFS(t *testing.T) {
	require.InDelta(t, -3.01, provider.RMSdBFS(sine(16000, 440, 1000, 32767)), 0.1)
	require.Equal(t, -120.0, provider.RMSdBFS(make([]byte, 640)))
	require.Equal(t, -120.0, provider.RMSdBFS(nil))
}
