package audio

// Framer cuts a little-endian PCM16 byte stream into 20 ms blocks.
type Framer struct {
	block int
	buf   []int16
	odd   []byte
}

// NewFramer returns a framer producing sampleRate/50-sample blocks.
// It panics when sampleRate < 50 (zero-length blocks), which is a programmer error.
func NewFramer(sampleRate int) *Framer {
	if sampleRate < 50 {
		panic("audio: NewFramer requires sampleRate >= 50")
	}
	return &Framer{block: sampleRate / 50}
}

// Write appends PCM bytes and returns every complete block; a trailing odd byte is carried.
func (f *Framer) Write(pcm []byte) [][]int16 {
	data := pcm
	if len(f.odd) > 0 {
		data = append(append([]byte{}, f.odd...), pcm...)
		f.odd = nil
	}
	if len(data)%2 == 1 {
		f.odd = []byte{data[len(data)-1]}
		data = data[:len(data)-1]
	}
	f.buf = append(f.buf, BytesToInt16(data)...)
	var out [][]int16
	for len(f.buf) >= f.block {
		b := make([]int16, f.block)
		copy(b, f.buf[:f.block])
		out = append(out, b)
		f.buf = f.buf[f.block:]
	}
	return out
}

// Flush returns the remaining samples zero-padded to one block, or nil when empty.
func (f *Framer) Flush() []int16 {
	f.odd = nil
	if len(f.buf) == 0 {
		return nil
	}
	b := make([]int16, f.block)
	copy(b, f.buf)
	f.buf = nil
	return b
}
