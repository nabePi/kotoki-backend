// Package audio is the audio side of the test client (spec §14.11.1): WAV in/out, Opus
// pre-encoding for upstream and Opus decoding of downstream frames. It wraps
// conversation/audio (cgo), the only package allowed to link the Opus library.
package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"

	convaudio "github.com/nabePi/kotoki-backend/internal/conversation/audio"
)

// Rate is the sample rate of every PCM the client works with (the wire format of §7.4.4).
const Rate = 48000

// PCM is mono PCM16 audio at PCM.Rate Hz. ReadWAV always returns Rate (48 kHz).
type PCM = convaudio.PCM

// maxWAVChannels bounds the channel count a WAV header may claim.
const maxWAVChannels = 8

// ReadWAV reads a PCM16 WAV file of any channel count and sample rate and returns mono PCM at
// 48 kHz: channels are averaged, other rates are resampled linearly.
func ReadWAV(path string) (PCM, error) {
	b, err := os.ReadFile(path) //nolint:gosec // caller-provided fixture path
	if err != nil {
		return PCM{}, fmt.Errorf("audio: read wav: %w", err)
	}
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return PCM{}, errors.New("audio: not a RIFF/WAVE file")
	}
	var rate, channels int
	haveFmt := false
	for p := 12; p+8 <= len(b); {
		id := string(b[p : p+4])
		size := int(binary.LittleEndian.Uint32(b[p+4:]))
		body := p + 8
		end := body + size
		switch id {
		case "fmt ":
			if end > len(b) || end-body < 16 {
				return PCM{}, errors.New("audio: short fmt chunk")
			}
			format := binary.LittleEndian.Uint16(b[body:])
			channels = int(binary.LittleEndian.Uint16(b[body+2:]))
			rate = int(binary.LittleEndian.Uint32(b[body+4:]))
			bits := binary.LittleEndian.Uint16(b[body+14:])
			if format != 1 || bits != 16 {
				return PCM{}, errors.New("audio: wav must be PCM16")
			}
			if channels < 1 || channels > maxWAVChannels || rate < 1 {
				return PCM{}, errors.New("audio: bad wav fmt chunk")
			}
			haveFmt = true
		case "data":
			if !haveFmt {
				return PCM{}, errors.New("audio: data before fmt chunk")
			}
			if end > len(b) {
				end = len(b) // streamed WAVs may claim more than they hold
			}
			mono := downmix(convaudio.BytesToInt16(b[body:end]), channels)
			return PCM{Rate: Rate, Samples: convaudio.Resample(mono, rate, Rate)}, nil
		}
		if end > len(b) {
			break
		}
		p = end + size%2
	}
	return PCM{}, errors.New("audio: no data chunk")
}

// downmix averages interleaved channels into mono; a trailing partial frame is dropped.
func downmix(s []int16, channels int) []int16 {
	if channels == 1 {
		return s
	}
	out := make([]int16, len(s)/channels)
	for i := range out {
		sum := 0
		for c := 0; c < channels; c++ {
			sum += int(s[i*channels+c])
		}
		out[i] = int16(sum / channels) //nolint:gosec // mean of int16 values fits int16
	}
	return out
}

// WriteWAV writes pcm as a mono PCM16 WAV file.
func WriteWAV(path string, pcm PCM) error {
	data := convaudio.Int16ToBytes(pcm.Samples, nil)
	h := make([]byte, 44)
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(data))) //nolint:gosec // WAV sizes are 32-bit by format
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1)                  // PCM
	binary.LittleEndian.PutUint16(h[22:], 1)                  // mono
	binary.LittleEndian.PutUint32(h[24:], uint32(pcm.Rate))   //nolint:gosec // positive sample rate
	binary.LittleEndian.PutUint32(h[28:], uint32(pcm.Rate)*2) //nolint:gosec // positive sample rate
	binary.LittleEndian.PutUint16(h[32:], 2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(data))) //nolint:gosec // WAV sizes are 32-bit by format
	if err := os.WriteFile(path, append(h, data...), 0o600); err != nil {
		return fmt.Errorf("audio: write wav: %w", err)
	}
	return nil
}
