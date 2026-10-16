package deepseek_test

import (
	"strings"
	"testing"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/providertest"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

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

func TestDeepSeekLLMConformance(t *testing.T) {
	providertest.LLMSuite(t, func(t *testing.T) provider.LLMProvider {
		return newLLM(t, replay.NewHTTPServer(t, recording(t, llmSuiteFixture(t))), nil)
	})
}

func TestDeepSeekEvaluationConformance(t *testing.T) {
	providertest.EvaluationSuite(t, func(t *testing.T) provider.EvaluationProvider {
		return newEval(t, replay.NewHTTPServer(t, recording(t, "eval_suite")), nil)
	}, evalReq())
}
