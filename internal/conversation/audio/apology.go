package audio

import (
	_ "embed"
	"encoding/binary"
	"errors"
)

// ApologyText is the sentence the apology clip stands for.
const ApologyText = "すみません、うまく聞き取れませんでした。もう一度お願いします。"

//go:embed assets/apology_ja.opus
var apologyOpus []byte

// ErrBadApologyAsset is returned when the embedded asset is truncated or malformed.
var ErrBadApologyAsset = errors.New("audio: malformed apology asset")

// ApologyFrames parses the embedded asset ([uint16 BE len][packet]...) into 20 ms Opus packets.
func ApologyFrames() ([][]byte, error) {
	var out [][]byte
	b := apologyOpus
	for len(b) > 0 {
		if len(b) < 2 {
			return nil, ErrBadApologyAsset
		}
		n := int(binary.BigEndian.Uint16(b))
		b = b[2:]
		if n == 0 || n > len(b) {
			return nil, ErrBadApologyAsset
		}
		out = append(out, append([]byte(nil), b[:n]...))
		b = b[n:]
	}
	if len(out) == 0 {
		return nil, ErrBadApologyAsset
	}
	return out, nil
}
