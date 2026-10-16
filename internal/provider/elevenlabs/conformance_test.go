package elevenlabs_test

import (
	"math"
	"testing"
	"time"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/elevenlabs"
	"github.com/nabePi/kotoki-backend/internal/provider/providertest"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

// speech is 1.2 s of a 220 Hz tone at about -12 dBFS: PCM16LE mono 16 kHz above -40 dBFS.
func speech() []byte {
	const n = 19200
	pcm := make([]byte, n*2)
	for i := 0; i < n; i++ {
		v := int16(8000 * math.Sin(2*math.Pi*220*float64(i)/16000))
		pcm[2*i], pcm[2*i+1] = byte(v), byte(v>>8)
	}
	return pcm
}

// The TTS conformance test is TestELTTSConformance in tts_test.go (fixture tts_conformance).

func TestELSTTConformance(t *testing.T) {
	providertest.STTSuite(t, func(t *testing.T) provider.STTProvider {
		return newSTT(t, replay.NewWSServer(t, recording(t, "stt_suite")), func(c *elevenlabs.Config) {
			c.SyntheticFinalDelay = 30 * time.Millisecond
		})
	}, speech)
}
