package audio

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeWAV(t *testing.T, format, ch uint16, rate uint32, samples []int16) string {
	t.Helper()
	data := Int16ToBytes(samples, nil)
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(data)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], format)
	binary.LittleEndian.PutUint16(h[22:], ch)
	binary.LittleEndian.PutUint32(h[24:], rate)
	binary.LittleEndian.PutUint32(h[28:], rate*uint32(ch)*2)
	binary.LittleEndian.PutUint16(h[32:], ch*2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(data)))
	p := filepath.Join(t.TempDir(), "a.wav")
	require.NoError(t, os.WriteFile(p, append(h, data...), 0o600))
	return p
}

func TestReadWAV(t *testing.T) {
	want := []int16{1, -1, 500, -500}
	pcm, err := ReadWAV(writeWAV(t, 1, 1, 24000, want))
	require.NoError(t, err)
	require.Equal(t, 24000, pcm.Rate)
	require.Equal(t, want, pcm.Samples)

	_, err = ReadWAV(writeWAV(t, 1, 2, 24000, want))
	require.Error(t, err)
	_, err = ReadWAV(writeWAV(t, 3, 1, 24000, want))
	require.Error(t, err)
	_, err = ReadWAV(filepath.Join(t.TempDir(), "missing.wav"))
	require.Error(t, err)
}
