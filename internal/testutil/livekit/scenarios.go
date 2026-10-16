package livekit

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

const liveSystemPrompt = "あなたは日本語学習者の会話相手です。自然な話し言葉で、一回に一文から三文だけ答えてください。" +
	"マークダウン、記号、タグ、絵文字は使わず、普通の文章だけで話してください。"

// goldenPrompts are the ten user utterances of the LLM live check (plain conversation, no tools).
var goldenPrompts = []string{
	"こんにちは、はじめまして。",
	"週末は何をしましたか。",
	"好きな食べ物は何ですか。",
	"最近、仕事がとても忙しいです。",
	"東京でおすすめの場所を教えてください。",
	"日本語の勉強は難しいと思いますか。",
	"朝ごはんはいつも何を食べますか。",
	"趣味は何ですか。",
	"雨の日はどうやって過ごしますか。",
	"今日はいい天気ですね。",
}

// RunLLM checks one LLM adapter against the golden prompts: the answer is plain Japanese without
// tags or markup that SentenceParser cuts into valid sentences (D-081, D-014), the stream ends
// with DONE STOP, and the first token arrives in under 2 s.
func RunLLM(t testing.TB, llm provider.LLMProvider, model string) {
	t.Helper()
	for i, user := range goldenPrompts {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		start := time.Now()
		st, err := llm.Stream(ctx, provider.LLMRequest{
			Model: model, MaxTokens: 200, Temperature: 0.7, RequestID: "live:" + string(rune('a'+i)),
			Messages: []provider.Message{
				{Role: provider.RoleSystem, Content: liveSystemPrompt},
				{Role: provider.RoleUser, Content: user},
			},
		})
		if err != nil {
			cancel()
			t.Fatalf("livekit: prompt %d Stream: %v", i, err)
		}
		var raw strings.Builder
		var first time.Duration
		var done *provider.LLMEvent
		for ev := range st.Events() {
			switch ev.Type {
			case provider.LLMDelta:
				if first == 0 && ev.Text != "" {
					first = time.Since(start)
				}
				raw.WriteString(ev.Text)
			case provider.LLMError:
				t.Errorf("livekit: prompt %d stream error: %v", i, ev.Err)
			case provider.LLMDone:
				done = &ev
			}
		}
		_ = st.Close()
		cancel()
		if done == nil {
			t.Fatalf("livekit: prompt %d: stream ended without DONE", i)
		}
		if done.FinishReason != provider.FinishStop {
			t.Errorf("livekit: prompt %d finish reason %s, want STOP", i, done.FinishReason)
		}
		if first == 0 || first >= 2*time.Second {
			t.Errorf("livekit: prompt %d first token after %s, want < 2s", i, first)
		}
		text := raw.String()
		if strings.ContainsAny(text, "<>*#`") {
			t.Errorf("livekit: prompt %d answer has tag or markup characters", i)
		}
		if !hasJapanese(text) {
			t.Errorf("livekit: prompt %d answer is not Japanese", i)
		}
		p := prompt.NewSentenceParser(8)
		_, out := p.Feed(text)
		_, rest := p.Close()
		sentences := append(append([]prompt.Sentence(nil), out...), rest...)
		if len(sentences) == 0 {
			t.Errorf("livekit: prompt %d produced no sentence", i)
		}
		for _, s := range sentences {
			if strings.TrimSpace(s.Text) == "" || !hasJapanese(s.Text) {
				t.Errorf("livekit: prompt %d invalid sentence %d", i, s.Index)
			}
		}
		t.Logf("prompt %d: ttft=%s sentences=%d", i, first.Round(time.Millisecond), len(sentences))
	}
}

func hasJapanese(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.Hiragana, unicode.Katakana, unicode.Han) {
			return true
		}
	}
	return false
}

// evalSchema is the small schema of the evaluation live check.
const evalSchema = `{"type":"object","properties":{"score":{"type":"integer","minimum":1,"maximum":5},` +
	`"comment":{"type":"string"}},"required":["score","comment"],"additionalProperties":false}`

type evalAnswer struct {
	Score   int    `json:"score"`
	Comment string `json:"comment"`
}

// RunEval asks an evaluation adapter for a small JSON document and validates it with a strict
// decoder (the vendor may not enforce the schema). With tolerateInvalid (DeepSeek JSON mode,
// which may answer with empty content now and then) a *provider.Error INVALID_RESPONSE that is
// retryable is accepted for a call, provided at least one of attempts calls succeeds; any other
// error, and any panic, fails the test.
func RunEval(t testing.TB, ev provider.EvaluationProvider, model string, attempts int, tolerateInvalid bool) {
	t.Helper()
	ok := 0
	for i := 0; i < attempts; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		res, err := ev.GenerateJSON(ctx, provider.StructuredRequest{
			Task: "EVALUATION", Model: model, SchemaName: "live_check", JSONSchema: json.RawMessage(evalSchema),
			MaxOutputTokens: 300, Temperature: 0, RequestID: "EVALUATION:live",
			Messages: []provider.Message{
				{Role: provider.RoleSystem, Content: "Reply with one JSON object only: {\"score\": integer 1-5, \"comment\": short Japanese string}."},
				{Role: provider.RoleUser, Content: "学習者の発話「昨日、友達と映画を見に行きました。」の自然さを1から5で評価してください。"},
			},
		})
		cancel()
		if err != nil {
			var pe *provider.Error
			if tolerateInvalid && errors.As(err, &pe) && pe.Kind == provider.KindInvalidResponse && pe.Retryable {
				t.Logf("attempt %d: INVALID_RESPONSE (tolerated)", i)
				continue
			}
			t.Fatalf("livekit: GenerateJSON attempt %d: %v", i, err)
		}
		dec := json.NewDecoder(bytes.NewReader(res.JSON))
		dec.DisallowUnknownFields()
		var a evalAnswer
		if err := dec.Decode(&a); err != nil {
			t.Fatalf("livekit: attempt %d: JSON does not match the schema: %v", i, err)
		}
		if a.Score < 1 || a.Score > 5 || strings.TrimSpace(a.Comment) == "" {
			t.Fatalf("livekit: attempt %d: score %d / comment out of schema", i, a.Score)
		}
		ok++
	}
	if ok == 0 {
		t.Fatalf("livekit: no valid evaluation in %d attempts", attempts)
	}
}

// DrainTTS reads st to its end and returns the PCM byte count, the time to the first audio
// chunk since start and the last chunk. A chunk error or an odd PCM length fails the test.
func DrainTTS(t testing.TB, st provider.TTSStream, start time.Time) (pcm int, ttfa time.Duration, last provider.TTSChunk) {
	t.Helper()
	defer func() { _ = st.Close() }()
	for c := range st.Chunks() {
		if c.Err != nil {
			t.Fatalf("livekit: TTS chunk error: %v", c.Err)
		}
		if ttfa == 0 && len(c.PCM) > 0 {
			ttfa = time.Since(start)
		}
		if len(c.PCM)%2 != 0 {
			t.Fatalf("livekit: odd PCM chunk length %d", len(c.PCM))
		}
		pcm += len(c.PCM)
		last = c
	}
	return pcm, ttfa, last
}
