// Command genapology generates the embedded apology clip (fake mode: synthetic tones).
// A real TTS mode (-tts) arrives in M9. Dev tool; it may import the cgo audio package.
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"

	"github.com/nabePi/kotoki-backend/internal/conversation/audio"
)

const (
	rate     = 24000
	bitrate  = 32000
	cplx     = 5
	seconds  = 2.4
	syllable = 0.2 // seconds per rising/falling tone
)

func synth() []byte {
	n := int(seconds * rate)
	pcm := make([]int16, n)
	phase := 0.0
	for i := range pcm {
		t := float64(i) / rate
		k := int(t / syllable)
		frac := (t - float64(k)*syllable) / syllable
		lo, hi := 220.0, 330.0
		f := lo + (hi-lo)*frac
		if k%2 == 1 {
			f = hi - (hi-lo)*frac
		}
		phase += 2 * math.Pi * f / rate
		env := math.Sin(math.Pi * frac) // fade each syllable
		pcm[i] = int16(8000 * env * math.Sin(phase))
	}
	return audio.Int16ToBytes(pcm, nil)
}

func generate() ([]byte, error) {
	enc, err := audio.NewEncoder(rate, bitrate, cplx)
	if err != nil {
		return nil, err
	}
	fr := audio.NewFramer(rate)
	blocks := fr.Write(synth())
	if last := fr.Flush(); last != nil {
		blocks = append(blocks, last)
	}
	var out bytes.Buffer
	for _, b := range blocks {
		p, err := enc.Encode(b)
		if err != nil {
			return nil, err
		}
		if err := binary.Write(&out, binary.BigEndian, uint16(len(p))); err != nil { //nolint:gosec // packet <= 4096
			return nil, err
		}
		out.Write(p)
	}
	return out.Bytes(), nil
}

func main() {
	o := flag.String("o", "internal/conversation/audio/assets/apology_ja.opus", "output file")
	flag.Parse()
	data, err := generate()
	if err != nil {
		fmt.Fprintln(os.Stderr, "genapology:", err)
		os.Exit(1)
	}
	if err := os.WriteFile(*o, data, 0o644); err != nil { //nolint:gosec // asset is public
		fmt.Fprintln(os.Stderr, "genapology:", err)
		os.Exit(1)
	}
}
