package provider

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPCMRingEvictsOldest(t *testing.T) { // cap 480000 bytes @16k = 15 s
	r := newPCMRing(480000, 16000)
	r.Write(make([]byte, 640000)) // 20 s
	require.Equal(t, int64(5000), r.OldestMs())
	require.Equal(t, int64(20000), r.EndMs())
	require.Len(t, r.ReadFrom(19900), 3200)
}

func TestPCMRingKeepsOrderAcrossWrap(t *testing.T) {
	r := newPCMRing(8, 16000) // 4 samples
	r.Write([]byte{1, 0, 2, 0, 3, 0})
	r.Write([]byte{4, 0, 5, 0}) // wraps; oldest sample evicted
	require.Equal(t, []byte{2, 0, 3, 0, 4, 0, 5, 0}, r.ReadFrom(0), "ReadFrom clamps to the oldest retained sample")
	require.Equal(t, int64(0), r.OldestMs()) // 2 bytes evicted = 1 sample = 0 ms at 16 kHz (integer)
	require.Empty(t, r.ReadFrom(1_000_000), "past the end is empty")
}

func TestPCMRingReadFromMiddle(t *testing.T) {
	r := newPCMRing(64000, 16000) // 2 s
	pcm := make([]byte, 32000)    // 1 s
	for i := range pcm {
		pcm[i] = byte(i % 251)
	}
	r.Write(pcm)
	got := r.ReadFrom(500)
	require.Equal(t, pcm[16000:], got)
}
