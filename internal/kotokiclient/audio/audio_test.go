package audio

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	convaudio "github.com/nabePi/kotoki-backend/internal/conversation/audio"
)

// rawWAV writes a PCM16 WAV with interleaved samples and the given header values.
func rawWAV(t *testing.T, channels uint16, rate uint32, interleaved []int16) string {
	t.Helper()
	data := convaudio.Int16ToBytes(interleaved, nil)
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(data)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)
	binary.LittleEndian.PutUint16(h[22:], channels)
	binary.LittleEndian.PutUint32(h[24:], rate)
	binary.LittleEndian.PutUint32(h[28:], rate*uint32(channels)*2)
	binary.LittleEndian.PutUint16(h[32:], channels*2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(data)))
	p := filepath.Join(t.TempDir(), "in.wav")
	require.NoError(t, os.WriteFile(p, append(h, data...), 0o600))
	return p
}

func sine(rate, n int) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(10000 * math.Sin(2*math.Pi*440*float64(i)/float64(rate)))
	}
	return out
}

func rms(s []int16) float64 {
	var sum float64
	for _, v := range s {
		sum += float64(v) * float64(v)
	}
	return math.Sqrt(sum / float64(len(s)))
}

func TestReadWAVResamples(t *testing.T) {
	const n = 16000 // 1 s at 16 kHz
	mono := sine(16000, n)
	stereo := make([]int16, 2*n)
	for i, v := range mono {
		stereo[2*i] = v
		stereo[2*i+1] = v
	}
	pcm, err := ReadWAV(rawWAV(t, 2, 16000, stereo))
	require.NoError(t, err)
	require.Equal(t, 48000, pcm.Rate)
	require.Len(t, pcm.Samples, 48000)
	// Identical channels average to the original; linear resampling keeps it within a few LSB
	// of the original at every third sample.
	for i := 0; i < n-1; i++ {
		require.InDelta(t, float64(mono[i]), float64(pcm.Samples[3*i]), 1, "sample %d", i)
	}
}

func TestReadWAVDownmixAverages(t *testing.T) {
	pcm, err := ReadWAV(rawWAV(t, 2, 48000, []int16{1000, 3000, -2000, 0, 10, 11}))
	require.NoError(t, err)
	require.Equal(t, []int16{2000, -1000, 10}, pcm.Samples)
}

func TestReadWAVRejectsBadInput(t *testing.T) {
	_, err := ReadWAV(filepath.Join(t.TempDir(), "missing.wav"))
	require.Error(t, err)

	p := filepath.Join(t.TempDir(), "x.wav")
	require.NoError(t, os.WriteFile(p, []byte("not a wav file at all"), 0o600))
	_, err = ReadWAV(p)
	require.Error(t, err)

	// 8-bit PCM is rejected.
	good := rawWAV(t, 1, 48000, []int16{1, 2, 3, 4})
	b, err := os.ReadFile(good)
	require.NoError(t, err)
	binary.LittleEndian.PutUint16(b[34:], 8)
	require.NoError(t, os.WriteFile(p, b, 0o600))
	_, err = ReadWAV(p)
	require.Error(t, err)
}

func TestWriteWAVRoundTrip(t *testing.T) {
	in := PCM{Rate: 48000, Samples: sine(48000, 4800)}
	p := filepath.Join(t.TempDir(), "out.wav")
	require.NoError(t, WriteWAV(p, in))
	out, err := ReadWAV(p)
	require.NoError(t, err)
	require.Equal(t, in, out)
	st, err := os.Stat(p)
	require.NoError(t, err)
	require.Equal(t, int64(44+2*4800), st.Size())
}

func TestPreEncodeShape(t *testing.T) {
	pkts, err := PreEncode(PCM{Rate: 48000, Samples: sine(48000, 48000)})
	require.NoError(t, err)
	require.Len(t, pkts, 50)
	for i, p := range pkts {
		require.True(t, len(p) >= 1 && len(p) <= 4091, "packet %d: %d bytes", i, len(p))
	}
	// 20 ms each: every packet decodes to exactly 960 samples, and the tone survives.
	dec, err := NewDecoder()
	require.NoError(t, err)
	var out []int16
	for i, p := range pkts {
		pcm, err := dec.Decode(p)
		require.NoError(t, err, "packet %d", i)
		require.Len(t, pcm, 960)
		if i >= 5 {
			out = append(out, pcm...)
		}
	}
	require.GreaterOrEqual(t, rms(out), 3000.0)

	// The silence packet is a valid 20 ms packet that decodes to (near) silence.
	silDec, err := NewDecoder() // fresh: the tone decoder still rings from the last tone packet
	require.NoError(t, err)
	pcm, err := silDec.Decode(SilencePacket())
	require.NoError(t, err)
	require.Len(t, pcm, 960)
	require.Less(t, rms(pcm), 100.0)
}

func TestPreEncodePadsPartialFrameAndResamples(t *testing.T) {
	pkts, err := PreEncode(PCM{Rate: 48000, Samples: sine(48000, 960+1)})
	require.NoError(t, err)
	require.Len(t, pkts, 2)

	pkts, err = PreEncode(PCM{Rate: 16000, Samples: sine(16000, 16000)})
	require.NoError(t, err)
	require.Len(t, pkts, 50)

	pkts, err = PreEncode(PCM{Rate: 48000})
	require.NoError(t, err)
	require.Empty(t, pkts)
}

func TestSilencePacketIsACopy(t *testing.T) {
	a := SilencePacket()
	a[0] ^= 0xff
	b := SilencePacket()
	require.NotEqual(t, a[0], b[0])
}

func TestDecoderRejectsGarbage(t *testing.T) {
	dec, err := NewDecoder()
	require.NoError(t, err)
	_, err = dec.Decode([]byte{0xff, 0xff, 0xff})
	require.ErrorIs(t, err, convaudio.ErrBadFrame)
}
