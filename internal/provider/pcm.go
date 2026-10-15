package provider

import (
	"encoding/binary"
	"errors"
	"math"
)

const wavHeaderLen = 44

// u32 converts a non-negative length or rate; WAV fields are 32-bit by definition.
func u32(n int) uint32 { return uint32(n) } //nolint:gosec // lengths and rates are bounded far below 2^32

// readS16 and putS16 reinterpret between the unsigned wire value and the signed sample.
func readS16(b []byte, i int) int16 { return int16(binary.LittleEndian.Uint16(b[i*2:])) } //nolint:gosec // two's complement reinterpretation

func putS16(b []byte, i int, v int16) { binary.LittleEndian.PutUint16(b[i*2:], uint16(v)) } //nolint:gosec // two's complement reinterpretation

// EncodeWAV wraps mono PCM16LE in a canonical 44-byte RIFF/WAVE header.
func EncodeWAV(pcm []byte, sampleRate int) []byte {
	out := make([]byte, wavHeaderLen+len(pcm))
	copy(out[0:], "RIFF")
	binary.LittleEndian.PutUint32(out[4:], u32(36+len(pcm)))
	copy(out[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(out[16:], 16)
	binary.LittleEndian.PutUint16(out[20:], 1) // PCM
	binary.LittleEndian.PutUint16(out[22:], 1) // mono
	binary.LittleEndian.PutUint32(out[24:], u32(sampleRate))
	binary.LittleEndian.PutUint32(out[28:], u32(sampleRate*2))
	binary.LittleEndian.PutUint16(out[32:], 2)
	binary.LittleEndian.PutUint16(out[34:], 16)
	copy(out[36:], "data")
	binary.LittleEndian.PutUint32(out[40:], u32(len(pcm)))
	copy(out[wavHeaderLen:], pcm)
	return out
}

// DecodeWAV parses a canonical PCM16 mono WAV; any other format is rejected.
func DecodeWAV(b []byte) (pcm []byte, sampleRate int, err error) {
	if len(b) < wavHeaderLen || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" || string(b[12:16]) != "fmt " {
		return nil, 0, errors.New("provider: not a canonical WAV file")
	}
	if f := binary.LittleEndian.Uint16(b[20:]); f != 1 {
		return nil, 0, errors.New("provider: WAV is not PCM")
	}
	if ch := binary.LittleEndian.Uint16(b[22:]); ch != 1 {
		return nil, 0, errors.New("provider: WAV is not mono")
	}
	if bits := binary.LittleEndian.Uint16(b[34:]); bits != 16 {
		return nil, 0, errors.New("provider: WAV is not 16-bit")
	}
	if string(b[36:40]) != "data" {
		return nil, 0, errors.New("provider: WAV data chunk not found")
	}
	n := int(binary.LittleEndian.Uint32(b[40:]))
	if n < 0 || wavHeaderLen+n > len(b) {
		n = len(b) - wavHeaderLen // streaming writers leave the size at 0 or too large
	}
	return b[wavHeaderLen : wavHeaderLen+n : wavHeaderLen+n], int(binary.LittleEndian.Uint32(b[24:])), nil
}

// WAVDurationMs returns the audio duration of a canonical WAV.
func WAVDurationMs(b []byte) (int64, error) {
	pcm, rate, err := DecodeWAV(b)
	if err != nil {
		return 0, err
	}
	if rate <= 0 {
		return 0, errors.New("provider: WAV sample rate is zero")
	}
	return int64(len(pcm)) / 2 * 1000 / int64(rate), nil
}

// ResamplePCM16 converts mono PCM16LE between sample rates by linear interpolation.
func ResamplePCM16(pcm []byte, from, to int) []byte {
	n := len(pcm) / 2
	if from == to || n == 0 {
		return append([]byte(nil), pcm[:n*2]...)
	}
	outN := int(int64(n) * int64(to) / int64(from))
	out := make([]byte, outN*2)
	sample := func(i int) float64 { return float64(readS16(pcm, i)) }
	for i := 0; i < outN; i++ {
		pos := float64(i) * float64(from) / float64(to)
		lo := int(pos)
		hi := lo + 1
		if hi >= n {
			hi = n - 1
		}
		frac := pos - float64(lo)
		v := sample(lo)*(1-frac) + sample(hi)*frac
		putS16(out, i, int16(math.Round(v)))
	}
	return out
}

// RMSdBFS is the RMS level of mono PCM16 in dBFS; silence (or empty input) is -120.
func RMSdBFS(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return -120
	}
	var sum float64
	for i := 0; i < n; i++ {
		v := float64(readS16(pcm, i))
		sum += v * v
	}
	rms := math.Sqrt(sum / float64(n))
	if rms == 0 {
		return -120
	}
	return math.Max(20*math.Log10(rms/32768), -120)
}

// pcmRing keeps the most recent capBytes of a PCM16 mono stream and addresses it by
// absolute logical position (ms since the first sample).
type pcmRing struct {
	buf   []byte
	head  int // index of the oldest retained byte
	size  int // retained bytes
	total int64
	rate  int
}

func newPCMRing(capBytes, sampleRate int) *pcmRing {
	capBytes -= capBytes % 2
	return &pcmRing{buf: make([]byte, capBytes), rate: sampleRate}
}

// Write appends pcm, evicting the oldest bytes beyond capacity.
func (r *pcmRing) Write(pcm []byte) {
	r.total += int64(len(pcm))
	if len(pcm) >= len(r.buf) {
		copy(r.buf, pcm[len(pcm)-len(r.buf):])
		r.head, r.size = 0, len(r.buf)
		return
	}
	for _, b := range pcm {
		if r.size < len(r.buf) {
			r.buf[(r.head+r.size)%len(r.buf)] = b
			r.size++
			continue
		}
		r.buf[r.head] = b
		r.head = (r.head + 1) % len(r.buf)
	}
}

func (r *pcmRing) msOfByte(bytes int64) int64 { return bytes / 2 * 1000 / int64(r.rate) }

// OldestMs is the logical position of the oldest retained sample.
func (r *pcmRing) OldestMs() int64 { return r.msOfByte(r.total - int64(r.size)) }

// EndMs is the logical position just after the newest sample.
func (r *pcmRing) EndMs() int64 { return r.msOfByte(r.total) }

// ReadFrom returns the retained audio from logical position ms to the end, clamped to the
// oldest retained sample.
func (r *pcmRing) ReadFrom(ms int64) []byte {
	return r.readFromBytes(ms * int64(r.rate) / 1000 * 2)
}

// oldestBytes is the logical byte position of the oldest retained sample.
func (r *pcmRing) oldestBytes() int64 { return r.total - int64(r.size) }

// readFromBytes is ReadFrom addressed by absolute logical byte position.
func (r *pcmRing) readFromBytes(startByte int64) []byte {
	oldest := r.total - int64(r.size)
	if startByte < oldest {
		startByte = oldest
	}
	if startByte >= r.total {
		return nil
	}
	skip := int(startByte - oldest)
	out := make([]byte, 0, r.size-skip)
	for i := skip; i < r.size; i++ {
		out = append(out, r.buf[(r.head+i)%len(r.buf)])
	}
	return out
}
