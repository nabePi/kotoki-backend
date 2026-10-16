package audio

import (
	"fmt"

	"gopkg.in/hraban/opus.v2"
)

const (
	// PlaybackRate is the sample rate PlaybackDecoder produces (what a client plays back).
	PlaybackRate = 48000
	// PlaybackFrame is the sample count of one 20 ms playback frame.
	PlaybackFrame = PlaybackRate / 50
)

// PlaybackDecoder decodes 20 ms downstream Opus packets to 48 kHz mono PCM, the way a client
// does. It lives here because only this package may link the cgo Opus library (§2.2 rule 5).
// Not safe for concurrent use.
type PlaybackDecoder struct {
	dec *opus.Decoder
	buf []int16
}

// NewPlaybackDecoder creates a 48 kHz mono decoder.
func NewPlaybackDecoder() (*PlaybackDecoder, error) {
	d, err := opus.NewDecoder(PlaybackRate, 1)
	if err != nil {
		return nil, fmt.Errorf("audio: new playback decoder: %w", err)
	}
	// Large enough for the 120 ms maximum so longer packets are detected, not truncated.
	return &PlaybackDecoder{dec: d, buf: make([]int16, PlaybackRate*120/1000)}, nil
}

// Decode decodes one packet; only 960-sample (20 ms) results are valid.
func (d *PlaybackDecoder) Decode(packet []byte) ([]int16, error) {
	n, err := d.dec.Decode(packet, d.buf)
	if err != nil || n != PlaybackFrame {
		return nil, ErrBadFrame
	}
	out := make([]int16, n)
	copy(out, d.buf[:n])
	return out, nil
}
