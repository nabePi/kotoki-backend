package audio

import (
	"fmt"
	"sync"

	convaudio "github.com/nabePi/kotoki-backend/internal/conversation/audio"
)

const (
	// FrameSamples is the sample count of one 20 ms upstream frame at Rate.
	FrameSamples = Rate / 50
	bitrate      = 32000
	complexity   = 5
)

// PreEncode encodes pcm as 20 ms (960-sample) Opus packets: 48 kHz mono, VoIP, 32 kbps,
// complexity 5, DTX and FEC off (§7.4.4). The last partial frame is zero-padded. PCM at another
// rate is resampled to 48 kHz first.
func PreEncode(pcm PCM) ([][]byte, error) {
	enc, err := convaudio.NewEncoder(Rate, bitrate, complexity)
	if err != nil {
		return nil, fmt.Errorf("audio: pre-encode: %w", err)
	}
	s := convaudio.Resample(pcm.Samples, pcm.Rate, Rate)
	out := make([][]byte, 0, (len(s)+FrameSamples-1)/FrameSamples)
	for off := 0; off < len(s); off += FrameSamples {
		block := make([]int16, FrameSamples)
		copy(block, s[off:])
		pkt, err := enc.Encode(block)
		if err != nil {
			return nil, fmt.Errorf("audio: pre-encode: %w", err)
		}
		out = append(out, pkt)
	}
	return out, nil
}

var (
	silenceOnce   sync.Once
	silencePacket []byte
)

// SilencePacket returns a pre-encoded 20 ms Opus packet of silence, sent while the mic is
// "open but quiet" (D-071: the upstream never pauses). The caller gets its own copy.
func SilencePacket() []byte {
	silenceOnce.Do(func() {
		enc, err := convaudio.NewEncoder(Rate, bitrate, complexity)
		if err != nil {
			panic("audio: silence encoder: " + err.Error()) // fixed, valid parameters
		}
		silencePacket, err = enc.Encode(make([]int16, FrameSamples))
		if err != nil {
			panic("audio: silence encode: " + err.Error())
		}
	})
	return append([]byte(nil), silencePacket...)
}
