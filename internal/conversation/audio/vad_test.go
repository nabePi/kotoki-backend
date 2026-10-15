package audio

import (
	"math/rand"
	"testing"
)

func TestVADSilenceAndTone(t *testing.T) {
	v, err := NewVAD(2)
	if err != nil {
		t.Fatal(err)
	}
	zero := make([]int16, UpstreamFrame)
	for i := 0; i < 50; i++ {
		if v.Process(zero) {
			t.Fatalf("silence frame %d detected as speech", i)
		}
	}
	rng := rand.New(rand.NewSource(1))
	falsePos := 0
	for i := 0; i < 100; i++ {
		f := make([]int16, UpstreamFrame)
		for j := range f {
			f[j] = int16(rng.Intn(401) - 200)
		}
		if v.Process(f) {
			falsePos++
		}
	}
	if falsePos > 5 {
		t.Fatalf("noise classified as speech in %d/100 frames", falsePos)
	}
	if v.Process(zero[:100]) {
		t.Fatal("wrong-length frame must yield false")
	}
}

func TestVADRejectsBadMode(t *testing.T) {
	for _, m := range []int{-1, 4, 100} {
		if _, err := NewVAD(m); err == nil {
			t.Fatalf("mode %d accepted", m)
		}
	}
}
