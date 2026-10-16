package openai_test

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/openai"
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

// llmSuiteFixture picks the reply for the running LLMSuite subtest: an HTTP script answers by
// order, not by request content, and every subtest gets its own server.
func llmSuiteFixture(t *testing.T) string {
	switch name := t.Name(); {
	case strings.Contains(name, "MaxTokens"):
		return "llm_suite_length"
	case strings.Contains(name, "Cancel"), strings.Contains(name, "CloseCancels"), strings.Contains(name, "BeforeFirstToken"):
		return "llm_suite_slow"
	}
	return "llm_suite"
}

func TestOpenAILLMConformance(t *testing.T) {
	providertest.LLMSuite(t, func(t *testing.T) provider.LLMProvider {
		return newLLM(t, replay.NewHTTPServer(t, recording(t, llmSuiteFixture(t))), nil)
	})
}

func TestOpenAIEvaluationConformance(t *testing.T) {
	providertest.EvaluationSuite(t, func(t *testing.T) provider.EvaluationProvider {
		return newEval(t, replay.NewHTTPServer(t, recording(t, "eval_suite")), nil)
	}, evalReq())
}

func TestOpenAIEmbeddingConformance(t *testing.T) {
	providertest.EmbeddingSuite(t, func(t *testing.T) provider.EmbeddingProvider {
		return newEmbed(t, replay.NewHTTPServer(t, recording(t, "embed_suite")))
	})
}

func TestOpenAISTTConformance(t *testing.T) {
	providertest.STTSuite(t, func(t *testing.T) provider.STTProvider {
		return newSTT(t, replay.NewWSServer(t, recording(t, "stt_suite")), func(c *openai.Config) {
			c.SyntheticFinalDelay = 30 * time.Millisecond
		})
	}, speech)
}
