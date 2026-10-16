package bench

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

// maxTTSSentenceRunes is the longest sentence a TTS request takes (spec 8.2, 300 runes).
const maxTTSSentenceRunes = 300

// E2EStack is one STT, LLM and TTS combination composed in process.
type E2EStack struct {
	STT     provider.STTProvider
	STTCand Candidate
	LLM     provider.LLMProvider
	LLMCand Candidate
	TTS     provider.TTSProvider
	TTSCand Candidate
}

// Label names the stack in the Provider/Model columns: "STT+LLM+TTS".
func (s E2EStack) Label() (prov, model string) {
	return fmt.Sprintf("%s+%s+%s", s.STTCand.Provider, s.LLMCand.Provider, s.TTSCand.Provider),
		fmt.Sprintf("%s+%s+%s", s.STTCand.Model, s.LLMCand.Model, s.TTSCand.Model)
}

// RunE2E composes STT, then the LLM on the recognised text, then TTS on the first sentence of the
// answer, and records turn_end_to_first_audio_ms = stt_final + llm_first_sentence + tts_first_audio +
// turn_end_ms (WithTurnEndMs). This is a DEVIATION from spec 8.10.3: no VAD or turn engine runs, the
// turn-end delay is a parameter and the three stages run one after the other (recorded in
// docs/implementation-deviations.md). Items are the STT items (audio + transcript); the LLM prompt is
// the system prompt (WithSystemPrompt, optional) and the recognised text. The failing stage is in
// Stage. Nothing but numbers is recorded.
func RunE2E(ctx context.Context, st E2EStack, items []Item, audio AudioSource, repeat int, clk clock.Clock, opts ...Option) []Result {
	o := newRunOpts(opts)
	prov, model := st.Label()
	var rs []Result
	for _, it := range items {
		pcm, aerr := audio.PCM16k(it)
		for n := 1; n <= repeat; n++ {
			if ctx.Err() != nil {
				return rs
			}
			r := newResult(SuiteE2E, st.TTSCand, it, n)
			r.Capability, r.Provider, r.Model, r.Role = string(CapE2E), prov, model, ""
			if aerr != nil {
				r.Values, r.Flags, r.ErrorKind, r.Stage = nil, nil, "AUDIO", "STT"
				rs = append(rs, r)
				continue
			}
			fail := func(stage string, err error) {
				r = r.failed(err)
				r.Stage = stage
				rs = append(rs, r)
			}
			so, err := sttOnce(ctx, st.STT, pcm, clk, o)
			if err != nil {
				fail("STT", err)
				continue
			}
			msgs := make([]provider.Message, 0, 2)
			if o.system != "" {
				msgs = append(msgs, provider.Message{Role: provider.RoleSystem, Content: o.system})
			}
			msgs = append(msgs, provider.Message{Role: provider.RoleUser, Content: userTextOrPlaceholder(so.text)})
			lo, err := llmOnce(ctx, st.LLM, provider.LLMRequest{
				Model: st.LLMCand.Model, Messages: msgs, MaxTokens: o.maxTokens, Temperature: o.temperature,
				RequestID: fmt.Sprintf("bench:e2e:%s:%d", it.ID, n),
			}, clk, o.timeout, true)
			if err != nil {
				fail("LLM", err)
				continue
			}
			sentence := strings.TrimSpace(lo.sentence)
			if utf8.RuneCountInString(sentence) > maxTTSSentenceRunes {
				sentence = string([]rune(sentence)[:maxTTSSentenceRunes])
			}
			to, err := ttsOnce(ctx, st.TTS, st.TTSCand, sentence, fmt.Sprintf("bench:e2e:%s:%d", it.ID, n), clk, o.timeout)
			if err != nil {
				fail("TTS", err)
				continue
			}
			r.Values["stt_final_ms"] = so.finalMs
			r.Values["llm_first_sentence_ms"] = lo.firstSentenceMs
			r.Values["tts_first_audio_ms"] = to.firstAudioMs
			r.Values["turn_end_ms"] = float64(o.turnEndMs)
			r.Values[MetricE2EMs] = so.finalMs + lo.firstSentenceMs + to.firstAudioMs + float64(o.turnEndMs)
			rs = append(rs, r)
		}
	}
	return rs
}

// userTextOrPlaceholder keeps the LLM stage runnable when the recogniser returned nothing (a silent
// or unrecognised item): the stage is still measured, with an empty-utterance prompt.
func userTextOrPlaceholder(s string) string {
	if strings.TrimSpace(s) == "" {
		return "…"
	}
	return s
}
