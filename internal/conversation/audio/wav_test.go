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

func chunk(id string, body []byte, declared int) []byte {
	h := make([]byte, 8)
	copy(h, id)
	binary.LittleEndian.PutUint32(h[4:], uint32(declared))
	out := append(append([]byte{}, h...), body...)
	if len(body)%2 == 1 && declared == len(body) {
		out = append(out, 0)
	}
	return out
}

func fmtBody(format, ch uint16, rate uint32, bits uint16) []byte {
	b := make([]byte, 16)
	binary.LittleEndian.PutUint16(b[0:], format)
	binary.LittleEndian.PutUint16(b[2:], ch)
	binary.LittleEndian.PutUint32(b[4:], rate)
	binary.LittleEndian.PutUint16(b[14:], bits)
	return b
}

func riff(chunks ...[]byte) []byte {
	out := []byte("RIFF\x00\x00\x00\x00WAVE")
	for _, c := range chunks {
		out = append(out, c...)
	}
	return out
}

func TestReadWAVEdgeCases(t *testing.T) {
	good := fmtBody(1, 1, 16000, 16)
	data := []byte{1, 0, 2, 0}
	cat := func(parts ...[]byte) []byte {
		var o []byte
		for _, p := range parts {
			o = append(o, p...)
		}
		return o
	}
	tests := []struct {
		name    string
		file    []byte
		wantErr bool
		want    []int16
	}{
		{"odd-size chunk padded", riff(chunk("junk", []byte{9, 9, 9}, 3), chunk("fmt ", good, 16), chunk("data", data, 4)), false, []int16{1, 2}},
		{"LIST before data", riff(chunk("fmt ", good, 16), chunk("LIST", []byte("INFOabcd"), 8), chunk("data", data, 4)), false, []int16{1, 2}},
		{"data before fmt", riff(chunk("data", data, 4), chunk("fmt ", good, 16)), true, nil},
		{"data size exceeds file", riff(chunk("fmt ", good, 16), chunk("data", data, 100)), true, nil},
		{"truncated non-data chunk", riff(chunk("fmt ", good, 16), chunk("LIST", []byte("ab"), 50)), true, nil},
		{"short fmt", riff(chunk("fmt ", good[:8], 8), chunk("data", data, 4)), true, nil},
		{"not RIFF", cat([]byte("RIFX\x00\x00\x00\x00WAVE"), chunk("data", data, 4)), true, nil},
		{"tiny file", []byte("RIFF"), true, nil},
		{"empty file", nil, true, nil},
		{"no data chunk", riff(chunk("fmt ", good, 16)), true, nil},
		{"extensible", riff(chunk("fmt ", fmtBody(0xFFFE, 1, 16000, 16), 16), chunk("data", data, 4)), true, nil},
		{"8-bit", riff(chunk("fmt ", fmtBody(1, 1, 16000, 8), 16), chunk("data", data, 4)), true, nil},
		{"24-bit", riff(chunk("fmt ", fmtBody(1, 1, 16000, 24), 16), chunk("data", data, 4)), true, nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "x.wav")
			require.NoError(t, os.WriteFile(p, tc.file, 0o600))
			pcm, err := ReadWAV(p)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, 16000, pcm.Rate)
			require.Equal(t, tc.want, pcm.Samples)
		})
	}
}
