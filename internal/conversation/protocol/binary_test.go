package protocol

import (
	"bytes"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func readHex(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "api", "ws", "frames", name))
	require.NoError(t, err)
	b, err := hex.DecodeString(strings.Join(strings.Fields(string(raw)), ""))
	require.NoError(t, err)
	return b
}

func TestBinaryFrameGolden(t *testing.T) {
	up := EncodeUpstream(258, []byte{0x78, 0x9A, 0xBC})
	require.Equal(t, []byte{0x01, 0, 0, 1, 2, 0x78, 0x9A, 0xBC}, up)
	require.Equal(t, up, readHex(t, "upstream_audio.hex"))
	seq, opus, err := DecodeUpstream(up)
	require.NoError(t, err)
	require.EqualValues(t, 258, seq)
	require.Equal(t, []byte{0x78, 0x9A, 0xBC}, opus)

	down := EncodeDownstream(7, 0, []byte{0xAA, 0xBB})
	require.Equal(t, []byte{0x02, 0, 0, 0, 7, 0, 0, 0, 0, 0xAA, 0xBB}, down)
	require.Equal(t, down, readHex(t, "downstream_audio.hex"))
	gen, seq, opus, err := DecodeDownstream(down)
	require.NoError(t, err)
	require.EqualValues(t, 7, gen)
	require.EqualValues(t, 0, seq)
	require.Equal(t, []byte{0xAA, 0xBB}, opus)
}

func TestUpstreamValidation(t *testing.T) {
	_, _, err := DecodeUpstream([]byte{1, 0, 0, 0, 0})
	require.ErrorIs(t, err, ErrFrameTooShort)
	_, _, err = DecodeUpstream([]byte{2, 0, 0, 0, 0, 1})
	require.ErrorIs(t, err, ErrBadType)
	_, _, err = DecodeUpstream(append([]byte{1, 0, 0, 0, 0}, make([]byte, 4092)...))
	require.ErrorIs(t, err, ErrFrameTooLarge)
	_, opus, err := DecodeUpstream(append([]byte{1, 0, 0, 0, 0}, bytes.Repeat([]byte{1}, 4091)...))
	require.NoError(t, err)
	require.Len(t, opus, 4091)
}

func TestDownstreamValidation(t *testing.T) {
	_, _, _, err := DecodeDownstream(make([]byte, 9))
	require.ErrorIs(t, err, ErrFrameTooShort)
	_, _, _, err = DecodeDownstream(append([]byte{1, 0, 0, 0, 0, 0, 0, 0, 0}, 1))
	require.ErrorIs(t, err, ErrBadType)
	_, _, _, err = DecodeDownstream(append([]byte{2, 0, 0, 0, 0, 0, 0, 0, 0}, make([]byte, 4088)...))
	require.ErrorIs(t, err, ErrFrameTooLarge)
	_, opus, _, err := DecodeDownstream(append([]byte{2, 0, 0, 0, 0, 0, 0, 0, 0}, make([]byte, 4087)...))
	_ = opus
	require.NoError(t, err)
}
