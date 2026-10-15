package audio

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFramerCarriesOddByte(t *testing.T) {
	f := NewFramer(24000)
	blocks := f.Write(make([]byte, 959))
	require.Len(t, blocks, 0)
	blocks = f.Write(make([]byte, 961))
	require.Len(t, blocks, 2)
	for _, b := range blocks {
		require.Len(t, b, 480)
	}
	require.Nil(t, f.Flush())

	// odd chunk must not split a sample: bytes 0x01 0x02 -> 0x0201
	g := NewFramer(24000)
	raw := make([]byte, 960)
	raw[0], raw[1] = 0x01, 0x02
	require.Empty(t, g.Write(raw[:1]))
	out := g.Write(raw[1:])
	require.Len(t, out, 1)
	require.Equal(t, int16(0x0201), out[0][0])
}

func TestFramerFlushPads(t *testing.T) {
	f := NewFramer(24000)
	pcm := Int16ToBytes(sine(24000, 100, 0), nil)
	require.Empty(t, f.Write(pcm))
	last := f.Flush()
	require.Len(t, last, 480)
	for _, v := range last[100:] {
		require.Zero(t, v)
	}
	require.Equal(t, sine(24000, 100, 0), last[:100])
	require.Nil(t, f.Flush())
}

func TestDecimate(t *testing.T) {
	require.Equal(t, []int16{2, 5}, Decimate48to16([]int16{1, 2, 3, 4, 5, 6, 7}))
}

func TestResampleAndBytes(t *testing.T) {
	r := Resample([]int16{0, 10}, 1, 2)
	require.Len(t, r, 4)
	require.Equal(t, int16(0), r[0])
	require.Equal(t, int16(5), r[1])
	s := []int16{1, -2, 300}
	require.Equal(t, s, BytesToInt16(Int16ToBytes(s, nil)))
}

func TestNewFramerInvalidRatePanics(t *testing.T) {
	require.Panics(t, func() { NewFramer(0) })
	require.Panics(t, func() { NewFramer(49) })
	require.NotPanics(t, func() { NewFramer(50) })
}
