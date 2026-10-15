// Package audio is the only cgo package of the realtime core (spec §2.2 rule 5):
// Opus codec, PCM framing/resampling and WAV reading.
package audio

import (
	"errors"
	"fmt"

	"gopkg.in/hraban/opus.v2"
)

const (
	// UpstreamRate is the sample rate the upstream decoder produces.
	UpstreamRate = 16000
	// UpstreamFrame is the sample count of one 20 ms upstream frame.
	UpstreamFrame = UpstreamRate / 50
	maxPacket     = 4096
)

var (
	// ErrBadFrame is returned when a packet is undecodable or is not exactly 20 ms.
	ErrBadFrame = errors.New("audio: bad opus frame")
	// ErrBlockSize is returned when an encoder block is not sampleRate/50 samples.
	ErrBlockSize = errors.New("audio: bad encoder block size")
)

// OutFrame is one encoded downstream audio frame.
type OutFrame struct {
	Gen, Seq       uint32
	SentenceIdx    int
	LastOfSentence bool
	Opus           []byte
	// Epoch is the TTS epoch the frame was synthesised in (server-internal, never sent): after a
	// TTS fallback switch the frames of older epochs are dropped before they are queued.
	Epoch int
}

// Decoder decodes 20 ms Opus packets to 16 kHz mono PCM. Not safe for concurrent use.
type Decoder struct {
	dec *opus.Decoder
	buf []int16
}

// NewDecoder creates a 16 kHz mono decoder.
func NewDecoder() (*Decoder, error) {
	d, err := opus.NewDecoder(UpstreamRate, 1)
	if err != nil {
		return nil, fmt.Errorf("audio: new decoder: %w", err)
	}
	// Large enough for the 120 ms maximum so longer packets are detected, not truncated.
	return &Decoder{dec: d, buf: make([]int16, UpstreamRate*120/1000)}, nil
}

// Decode decodes one packet; only 320-sample (20 ms) results are valid.
func (d *Decoder) Decode(packet []byte) ([]int16, error) {
	n, err := d.dec.Decode(packet, d.buf)
	if err != nil || n != UpstreamFrame {
		return nil, ErrBadFrame
	}
	out := make([]int16, n)
	copy(out, d.buf[:n])
	return out, nil
}

// DecodePLC conceals one lost 20 ms frame.
func (d *Decoder) DecodePLC() ([]int16, error) {
	out := make([]int16, UpstreamFrame)
	if err := d.dec.DecodePLC(out); err != nil {
		return nil, fmt.Errorf("audio: plc: %w", err)
	}
	return out, nil
}

// Encoder encodes 20 ms mono blocks to Opus. Not safe for concurrent use.
type Encoder struct {
	enc   *opus.Encoder
	block int
	buf   []byte
}

// NewEncoder creates a VoIP encoder (DTX and in-band FEC off).
func NewEncoder(sampleRate, bitrate, complexity int) (*Encoder, error) {
	e, err := opus.NewEncoder(sampleRate, 1, opus.AppVoIP)
	if err != nil {
		return nil, fmt.Errorf("audio: new encoder: %w", err)
	}
	if err := e.SetBitrate(bitrate); err != nil {
		return nil, fmt.Errorf("audio: bitrate: %w", err)
	}
	if err := e.SetComplexity(complexity); err != nil {
		return nil, fmt.Errorf("audio: complexity: %w", err)
	}
	if err := e.SetDTX(false); err != nil {
		return nil, fmt.Errorf("audio: dtx: %w", err)
	}
	if err := e.SetInBandFEC(false); err != nil {
		return nil, fmt.Errorf("audio: fec: %w", err)
	}
	return &Encoder{enc: e, block: sampleRate / 50, buf: make([]byte, maxPacket)}, nil
}

// Encode encodes exactly sampleRate/50 samples.
func (e *Encoder) Encode(block []int16) ([]byte, error) {
	if len(block) != e.block {
		return nil, fmt.Errorf("%w: got %d, want %d", ErrBlockSize, len(block), e.block)
	}
	n, err := e.enc.Encode(block, e.buf)
	if err != nil {
		return nil, fmt.Errorf("audio: encode: %w", err)
	}
	out := make([]byte, n)
	copy(out, e.buf[:n])
	return out, nil
}
