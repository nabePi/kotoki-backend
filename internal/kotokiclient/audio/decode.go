package audio

import (
	"fmt"

	convaudio "github.com/nabePi/kotoki-backend/internal/conversation/audio"
)

// Decoder decodes 20 ms Opus packets (downstream frames) to PCM at 48 kHz. Not safe for
// concurrent use.
type Decoder struct{ dec *convaudio.PlaybackDecoder }

// NewDecoder creates a Decoder.
func NewDecoder() (*Decoder, error) {
	d, err := convaudio.NewPlaybackDecoder()
	if err != nil {
		return nil, fmt.Errorf("audio: %w", err)
	}
	return &Decoder{dec: d}, nil
}

// Decode decodes one packet; anything that is not exactly 20 ms is an error.
func (d *Decoder) Decode(packet []byte) ([]int16, error) { return d.dec.Decode(packet) }
