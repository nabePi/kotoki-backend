package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

// PCM is mono PCM16 audio at Rate Hz.
type PCM struct {
	Rate    int
	Samples []int16
}

// ReadWAV reads a mono PCM16 WAV file; anything else is rejected.
func ReadWAV(path string) (PCM, error) {
	b, err := os.ReadFile(path) //nolint:gosec // caller-provided fixture path
	if err != nil {
		return PCM{}, fmt.Errorf("audio: read wav: %w", err)
	}
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return PCM{}, errors.New("audio: not a RIFF/WAVE file")
	}
	var rate int
	haveFmt := false
	for p := 12; p+8 <= len(b); {
		id := string(b[p : p+4])
		size := int(binary.LittleEndian.Uint32(b[p+4:]))
		body := p + 8
		end := body + size
		if end > len(b) {
			return PCM{}, errors.New("audio: truncated wav chunk")
		}
		switch id {
		case "fmt ":
			if end-body < 16 {
				return PCM{}, errors.New("audio: short fmt chunk")
			}
			format := binary.LittleEndian.Uint16(b[body:])
			ch := binary.LittleEndian.Uint16(b[body+2:])
			rate = int(binary.LittleEndian.Uint32(b[body+4:]))
			bits := binary.LittleEndian.Uint16(b[body+14:])
			if format != 1 || bits != 16 {
				return PCM{}, errors.New("audio: wav must be PCM16")
			}
			if ch != 1 {
				return PCM{}, errors.New("audio: wav must be mono")
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return PCM{}, errors.New("audio: data before fmt chunk")
			}
			return PCM{Rate: rate, Samples: BytesToInt16(b[body:end])}, nil
		}
		p = end + size%2
	}
	return PCM{}, errors.New("audio: no data chunk")
}
