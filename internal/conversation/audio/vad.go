package audio

import (
	"fmt"

	webrtcvad "github.com/maxhawkins/go-webrtcvad"
)

// VAD wraps the WebRTC voice activity detector for 20 ms / 16 kHz frames.
// It is NOT safe for concurrent use.
type VAD struct {
	v     *webrtcvad.VAD
	bytes []byte
}

// NewVAD creates a detector; mode is the aggressiveness 0..3 (the realtime core uses 2).
func NewVAD(mode int) (*VAD, error) {
	if mode < 0 || mode > 3 {
		return nil, fmt.Errorf("audio: vad mode %d out of range 0..3", mode)
	}
	v, err := webrtcvad.New()
	if err != nil {
		return nil, fmt.Errorf("audio: new vad: %w", err)
	}
	if err := v.SetMode(mode); err != nil {
		return nil, fmt.Errorf("audio: vad mode: %w", err)
	}
	return &VAD{v: v, bytes: make([]byte, 0, UpstreamFrame*2)}, nil
}

// Process reports whether a 320-sample (20 ms @ 16 kHz) frame contains speech.
// Any error, including a wrong frame length, yields false.
func (d *VAD) Process(pcm []int16) bool {
	if len(pcm) != UpstreamFrame {
		return false
	}
	d.bytes = Int16ToBytes(pcm, d.bytes)
	ok, err := d.v.Process(UpstreamRate, d.bytes)
	return err == nil && ok
}
