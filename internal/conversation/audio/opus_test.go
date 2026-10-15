package audio

import (
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/hraban/opus.v2"
)

func sine(rate, n, offset int) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(10000 * math.Sin(2*math.Pi*440*float64(i+offset)/float64(rate)))
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

func TestEncodeDecodeRoundTrip(t *testing.T) {
	enc, err := NewEncoder(24000, 32000, 5)
	require.NoError(t, err)
	dec, err := NewDecoder()
	require.NoError(t, err)
	var in, out []int16
	for i := 0; i < 50; i++ {
		blk := sine(24000, 480, i*480)
		pkt, err := enc.Encode(blk)
		require.NoError(t, err)
		require.True(t, len(pkt) >= 1 && len(pkt) <= 4087, "len %d", len(pkt))
		pcm, err := dec.Decode(pkt)
		require.NoError(t, err)
		require.Len(t, pcm, 320)
		if i >= 5 {
			in = append(in, blk...)
			out = append(out, pcm...)
		}
	}
	require.GreaterOrEqual(t, rms(out), 0.3*rms(in))
}

func TestDecodeInvalid(t *testing.T) {
	dec, err := NewDecoder()
	require.NoError(t, err)
	_, err = dec.Decode([]byte{0xff, 0xff, 0xff})
	require.ErrorIs(t, err, ErrBadFrame)

	e40, err := opus.NewEncoder(16000, 1, opus.AppVoIP)
	require.NoError(t, err)
	buf := make([]byte, 4000)
	n, err := e40.Encode(sine(16000, 640, 0), buf)
	require.NoError(t, err)
	_, err = dec.Decode(buf[:n])
	require.ErrorIs(t, err, ErrBadFrame)
}

func TestPLC(t *testing.T) {
	dec, err := NewDecoder()
	require.NoError(t, err)
	pcm, err := dec.DecodePLC()
	require.NoError(t, err)
	require.Len(t, pcm, 320)
}

func TestEncoderBlockSizeError(t *testing.T) {
	enc, err := NewEncoder(24000, 32000, 5)
	require.NoError(t, err)
	_, err = enc.Encode(make([]int16, 100))
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrBlockSize))
}
