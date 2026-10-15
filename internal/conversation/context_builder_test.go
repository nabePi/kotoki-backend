package conversation

import (
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/prompt"
)

var update = flag.Bool("update", false, "rewrite golden files")

const (
	fullName = "山田 太郎 フルネーム"
	voiceID  = "VENDOR-VOICE-ID-9f3a"
)

func rep(unit string, n int) string {
	r := []rune(strings.Repeat(unit, n/len([]rune(unit))+1))
	return string(r[:n])
}

func sp(s string) *string { return &s }

func baseInput() ConversationInput {
	return ConversationInput{
		Mode: ModeConversation,
		Prompt: content.PromptVersion{
			ID: uuid.MustParse("0190f5a0-0000-7000-8000-0000000000c1"), Key: content.PromptConversation, Version: 1,
			SystemTemplate: prompt.ConversationV1.System,
		},
		Config: content.SessionConfig{
			Topic: content.Topic{Code: "TRAVEL", Name: "Travel and holidays"},
			ScenarioVersion: content.ScenarioVersion{
				DifficultyLevel: 3, Situation: "You meet at a train station.", AIRole: "A station clerk", UserRole: "A traveller",
				Goal: "Buy a ticket", OpeningLine: "いらっしゃいませ。", Formality: content.FormalityPolite,
			},
			Snapshot: content.SessionSnapshot{
				Formality: content.FormalityPolite,
				Character: content.CharacterSnapshot{Name: "葵", Persona: "A warm guide.", VoiceID: voiceID, GreetingHint: sp("やあ！")},
			},
		},
		Learner:          prompt.LearnerContext{DisplayName: "タロウ", Adaptation: "<conversation_difficulty>\n語彙: やさしい\n</conversation_difficulty>"},
		Adaptive:         learner.AdaptiveDifficulty{Formality: learner.Formality("POLITE"), MaxAISentencesPerTurn: 2},
		CurrentUserText:  "切符をください。",
		RemainingSeconds: 600,
		DurationMinutes:  15,
		Budget:           prompt.DefaultBudget,
	}
}

func maxInput() ConversationInput {
	in := baseInput()
	in.Config.Snapshot.Character.Persona = strings.TrimSpace(strings.Repeat("A warm, curious conversation partner who loves travel.\n\n", 60))
	in.Config.ScenarioVersion.Situation = rep("You meet at a train station. ", 2000)
	in.Config.ScenarioVersion.Tasks = []string{rep("あいうえお", 30), rep("かきくけこ", 30)}
	in.Config.ScenarioVersion.Constraints = []string{rep("さしすせそ", 30)}
	in.Config.ScenarioVersion.GrammarTargets = []string{"〜ください", "〜たいです"}
	in.Config.ScenarioVersion.VocabHints = []prompt.VocabHint{{TermJA: "切符", Reading: "きっぷ", Meaning: "ticket"}}
	in.Learner.Adaptation = "<conversation_difficulty>\n語彙: やさしい言葉を使う\n文法: 短い文で話す\n速さ: ゆっくり話す\n学習のねらい: 聞き取り、丁寧語\n</conversation_difficulty>"
	in.RollingSummary = rep("They talked about the trip. ", 300)
	for i := range 30 {
		in.Memory.Core = append(in.Memory.Core, memory.ContextItem{Content: rep("あいうえお", 40), Score: float64(100 - i)})
	}
	for i := range 3 {
		in.Memory.Episodic = append(in.Memory.Episodic, memory.ContextItem{
			Content: rep("かきくけこ", 60), Score: 0.9 - float64(i)/100, ObservedAt: time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC),
		})
	}
	for i := range 12 {
		in.RecentTurns = append(in.RecentTurns, TurnContext{Seq: i + 1, Status: "COMPLETED", UserText: sp(rep("ユーザー発話。", 120)), AssistantText: rep("返答です。", 120)})
	}
	in.CurrentUserText = rep("今日は天気がいいですね。", 900)
	in.RemainingSeconds = 12 * 60
	return in
}

func build(t *testing.T, in ConversationInput) BuiltPrompt {
	t.Helper()
	bp, err := NewPromptBuilder().BuildConversation(in)
	require.NoError(t, err)
	return bp
}

func TestBuildMaxFixtureWithinBudget(t *testing.T) {
	in := maxInput()
	bp := build(t, in)
	require.LessOrEqual(t, bp.EstimatedInputTokens, 6450)
	require.LessOrEqual(t, bp.ComponentTokens["learner"], 700)
	require.LessOrEqual(t, bp.ComponentTokens["memory_core"], 600)
	require.LessOrEqual(t, bp.ComponentTokens["memory_episodic"], 500)
	require.Contains(t, bp.Messages[0].Content, "語彙: やさしい言葉を使う")
	require.Contains(t, bp.Messages[0].Content, "2026-10-02: ") // 20:00 UTC = 03:00 next day, Asia/Jakarta

	got, err := json.MarshalIndent(bp, "", "  ")
	require.NoError(t, err)
	got = append(got, '\n')
	const path = "../../testdata/prompt/conversation_max.golden.json"
	if *update {
		require.NoError(t, os.WriteFile(path, got, 0o644))
	}
	want, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, string(want), string(got))
}

func TestTrimOrder(t *testing.T) {
	in := baseInput()
	in.Learner.Adaptation = "<conversation_difficulty>\n語彙: " + rep("や", 900) + "\n学習のねらい: 聞き取り\n</conversation_difficulty>"
	in.RollingSummary = rep("要約です。", 900)
	for i := range 30 {
		in.Memory.Core = append(in.Memory.Core, memory.ContextItem{Content: rep("あ", 100), Score: float64(i)})
	}
	for i := range 3 {
		in.Memory.Episodic = append(in.Memory.Episodic, memory.ContextItem{Content: rep("い", 600), Score: 0.8 + float64(i)/100})
	}
	for i := range 12 {
		in.RecentTurns = append(in.RecentTurns, TurnContext{Seq: i + 1, Status: "COMPLETED", UserText: sp(rep("あ", 400)), AssistantText: rep("い", 100)})
	}
	orig := append([]TurnContext(nil), in.RecentTurns...)
	bp := build(t, in)
	require.Equal(t, []string{"memory_episodic", "memory_core", "summary", "learner", "recent_turns"}, bp.Truncations)
	require.NotContains(t, bp.Messages[0].Content, "学習のねらい")
	require.Contains(t, bp.Messages[0].Content, "語彙: ")
	require.LessOrEqual(t, bp.ComponentTokens["memory_episodic"], 500)
	require.LessOrEqual(t, bp.ComponentTokens["memory_core"], 600)
	require.LessOrEqual(t, bp.ComponentTokens["summary"], 500)
	require.LessOrEqual(t, bp.ComponentTokens["recent_turns"], 2000)
	require.GreaterOrEqual(t, len(bp.Messages), 1+2*4+1)
	// highest scored core item survives, lowest is dropped (score is in order of the 'あ' count: all equal, so check count)
	require.Equal(t, orig, in.RecentTurns, "input is never mutated")
	require.Len(t, in.Memory.Core, 30)
}

func TestTrimDropsLowestScoreFirst(t *testing.T) {
	in := baseInput()
	in.Memory.Core = []memory.ContextItem{
		{Content: "高" + rep("あ", 300), Score: 9}, {Content: "低" + rep("あ", 300), Score: 1}, {Content: "中" + rep("あ", 300), Score: 5},
	}
	bp := build(t, in)
	require.Contains(t, bp.Truncations, "memory_core")
	require.Contains(t, bp.Messages[0].Content, "高")
	require.NotContains(t, bp.Messages[0].Content, "低")
}

func TestRecentTurnsKeepMinimumThenCutUserText(t *testing.T) {
	in := baseInput()
	for i := range 6 {
		in.RecentTurns = append(in.RecentTurns, TurnContext{Seq: i + 1, Status: "COMPLETED", UserText: sp(rep("あ", 1500)), AssistantText: rep("い", 1500)})
	}
	bp := build(t, in)
	require.Equal(t, 1+4*2+1, len(bp.Messages), "never fewer than 4 turns")
	for _, m := range bp.Messages[1 : len(bp.Messages)-1] {
		if m.Role == "user" {
			require.LessOrEqual(t, len([]rune(m.Content)), 300)
		}
	}
}

func TestCurrentInputTruncatedKeepsFront(t *testing.T) {
	in := baseInput()
	in.CurrentUserText = "先頭" + rep("あ", 2000)
	bp := build(t, in)
	last := bp.Messages[len(bp.Messages)-1]
	require.Equal(t, "user", last.Role)
	require.True(t, strings.HasPrefix(last.Content, "先頭"))
	require.LessOrEqual(t, prompt.EstimateTokens(last.Content), 600)
	require.Contains(t, bp.Truncations, "current_input")
}

func TestMessageShape(t *testing.T) {
	in := baseInput()
	in.RecentTurns = []TurnContext{
		{Seq: 1, Status: "COMPLETED", AssistantText: "いらっしゃいませ。"}, // opening
		{Seq: 2, Status: "COMPLETED", UserText: sp("こんにちは"), AssistantText: "どうも。"},
		{Seq: 3, Status: "INTERRUPTED", UserText: sp("あの"), AssistantText: "はい、えっと"},
		{Seq: 4, Status: "FAILED", UserText: sp("聞こえますか")},
		{Seq: 5, Status: "DISCARDED", UserText: sp("捨てる"), AssistantText: "捨てる"},
	}
	bp := build(t, in)
	want := []Message{
		{"system", bp.Messages[0].Content},
		{"assistant", "いらっしゃいませ。"},
		{"user", "こんにちは"}, {"assistant", "どうも。"},
		{"user", "あの"}, {"assistant", "はい、えっと（ユーザーが割り込みました）"},
		{"user", "聞こえますか"},
		{"user", "切符をください。"},
	}
	require.Equal(t, want, bp.Messages)
}

func TestOpeningOnlySystem(t *testing.T) {
	in := baseInput()
	in.Mode = ModeOpening
	in.Prompt.Key = content.PromptOpening
	in.Prompt.SystemTemplate = prompt.OpeningV1.System
	in.CurrentUserText = ""
	in.RecentTurns = []TurnContext{{Seq: 1, Status: "COMPLETED", UserText: sp("x"), AssistantText: "y"}}
	bp := build(t, in)
	require.Len(t, bp.Messages, 1)
	require.Equal(t, "system", bp.Messages[0].Role)
	require.Contains(t, bp.Messages[0].Content, "やあ！")
	require.Equal(t, 200, bp.MaxOutputTokens)
}

func TestSystemIsRender(t *testing.T) {
	in := baseInput()
	in.RemainingSeconds = 61
	in.RollingSummary = "旅の話をした。"
	in.Memory.Core = []memory.ContextItem{{Content: "名前は<タロウ>\nです", Score: 1}}
	in.Memory.Episodic = []memory.ContextItem{{Content: "京都へ行った", ObservedAt: time.Date(2026, 9, 30, 18, 0, 0, 0, time.UTC)}}
	bp := build(t, in)

	data := prompt.PromptData{
		CoreRules: prompt.CoreRules, OutputContract: prompt.OutputContract(false),
		Character: prompt.CharacterData{Name: "葵", Persona: "A warm guide.", GreetingHint: "やあ！"},
		Topic:     prompt.TopicData{Code: "TRAVEL", Name: "Travel and holidays"},
		Scenario: prompt.ScenarioData{
			Situation: "You meet at a train station.", AIRole: "A station clerk", UserRole: "A traveller", Goal: "Buy a ticket",
			DifficultyLevel: 3, DifficultyBand: "INTERMEDIATE", OpeningLine: "いらっしゃいませ。",
		},
		Speech:  prompt.SpeechData{Formality: "POLITE", FormalityInstruction: prompt.FormalityInstruction("POLITE"), MaxSentencesPerTurn: 2, MaxConsecutiveQuestions: 2},
		Learner: in.Learner,
		Memory:  prompt.MemoryData{Items: []string{"名前は＜タロウ＞です"}, Episodes: []string{"2026-10-01: 京都へ行った"}},
		Session: prompt.SessionData{Mode: "USER_FIRST", DurationMinutes: 15, RemainingMinutes: 2, Summary: "旅の話をした。"},
	}
	data.Session.Mode = bp2mode(in)
	r, err := prompt.Render(in.Prompt.Template(), data)
	require.NoError(t, err)
	require.Equal(t, r.System, bp.Messages[0].Content)
	require.NotContains(t, bp.Messages[0].Content, "remain 2 of")

	in.RemainingSeconds = 60
	closing := build(t, in)
	require.Contains(t, closing.Messages[0].Content, "wrap up the conversation")
	require.Contains(t, closing.Messages[0].Content, "About 1 of 15 minutes remain.")
	require.NotContains(t, bp.Messages[0].Content, "wrap up the conversation")
}

func bp2mode(in ConversationInput) string {
	if in.SessionMode != "" {
		return in.SessionMode
	}
	return "USER_FIRST"
}

func TestFullNameNeverInPrompt(t *testing.T) {
	in := baseInput()
	in.Learner.DisplayName = "タロウ"
	bp := build(t, in)
	for _, m := range bp.Messages {
		require.NotContains(t, m.Content, fullName)
	}
	require.Contains(t, bp.Messages[0].Content, "タロウ")
}

func TestCharacterFromSnapshot(t *testing.T) {
	in := baseInput()
	bp := build(t, in)
	for _, m := range bp.Messages {
		require.NotContains(t, m.Content, voiceID)
	}
	require.Contains(t, bp.Messages[0].Content, "You are 葵. A warm guide.")

	var paras []string
	for i := range 40 {
		paras = append(paras, "Paragraph "+string(rune('A'+i%26))+" "+strings.TrimSpace(strings.Repeat("about travel and food. ", 8)))
	}
	in.Config.Snapshot.Character.Persona = strings.Join(paras, "\n\n")
	bp = build(t, in)
	require.Contains(t, bp.Truncations, "character")
	require.LessOrEqual(t, bp.ComponentTokens["character"], 500)
	sys := bp.Messages[0].Content
	start := strings.Index(sys, "You are 葵. ") + len("You are 葵. ")
	persona := sys[start:strings.Index(sys, "\n\n# Situation")]
	require.True(t, strings.HasSuffix(persona, "food."), "cut at a paragraph end: %q", persona[len(persona)-20:])
	require.Contains(t, strings.Join(paras, "\n\n"), persona)
	require.Equal(t, 40, len(paras))
	require.Equal(t, strings.Join(paras, "\n\n"), in.Config.Snapshot.Character.Persona, "input not mutated")
}

func TestSpeechFromAdaptive(t *testing.T) {
	in := baseInput()
	in.Adaptive = learner.AdaptiveDifficulty{Formality: "KEIGO_LIGHT", MaxAISentencesPerTurn: 4}
	sys := build(t, in).Messages[0].Content
	require.Contains(t, sys, prompt.FormalityInstruction("KEIGO_LIGHT"))
	require.Contains(t, sys, "Never use more than 4 sentences")
	require.Contains(t, sys, "never end more than 2 replies")

	in.Adaptive = learner.AdaptiveDifficulty{}
	in.Config.Snapshot.Formality = content.FormalityCasual
	sys = build(t, in).Messages[0].Content
	require.Contains(t, sys, prompt.FormalityInstruction("CASUAL"))
	require.Contains(t, sys, "Never use more than 3 sentences")
}

func TestDeterministic(t *testing.T) {
	in := maxInput()
	first := build(t, in)
	for range 100 {
		require.Equal(t, first, build(t, in))
	}
}

func TestMaxOutputFromParams(t *testing.T) {
	in := baseInput()
	bp := build(t, in)
	require.Equal(t, 300, bp.MaxOutputTokens)
	require.InDelta(t, 0.8, float64(bp.Temperature), 1e-6)
	require.Equal(t, in.Prompt.ID, bp.PromptVersionID)

	mt, tp := 123, 0.3
	in.Prompt.LLMParams = content.LLMParams{MaxOutputTokens: &mt, Temperature: &tp}
	bp = build(t, in)
	require.Equal(t, 123, bp.MaxOutputTokens)
	require.InDelta(t, 0.3, float64(bp.Temperature), 1e-6)
}

func TestRollingSummaryPrompt(t *testing.T) {
	bp, err := NewPromptBuilder().BuildRollingSummary(SummaryInput{
		PreviousSummary: "旅の話。",
		Turns: []TurnContext{
			{Seq: 1, Status: "COMPLETED", UserText: sp("こんにちは"), AssistantText: "どうも。"},
			{Seq: 2, Status: "FAILED", UserText: sp("あの")},
			{Seq: 3, Status: "DISCARDED", UserText: sp("x"), AssistantText: "y"},
		},
		MaxRunes: 300,
	})
	require.NoError(t, err)
	require.Equal(t, []Message{
		{"system", "あなたは会話記録の要約者です。以下の「これまでの要約」と「新しい会話」を統合し、\n日本語で300字以内の要約を書いてください。含めること: 話題の流れ、ユーザーが話した事実・意見、\nAIが約束したこと/質問中のこと。含めないこと: 文法の訂正、評価、推測。箇条書き不可。"},
		{"user", "これまでの要約:\n旅の話。\n\n新しい会話:\nユーザー: こんにちは\nAI: どうも。\nユーザー: あの"},
	}, bp.Messages)
	require.Equal(t, 400, bp.MaxOutputTokens)
	require.InDelta(t, 0.3, float64(bp.Temperature), 1e-6)
	require.Positive(t, bp.EstimatedInputTokens)
}

func TestRecentWindow(t *testing.T) {
	mk := func(n int) []TurnContext {
		var ts []TurnContext
		for i := 1; i <= n; i++ {
			ts = append(ts, TurnContext{Seq: i, Status: "COMPLETED", AssistantText: "a"})
		}
		return ts
	}
	seqs := func(ts []TurnContext) []int {
		var o []int
		for _, t := range ts {
			o = append(o, t.Seq)
		}
		return o
	}
	require.Equal(t, []int{1, 2, 3}, seqs(SelectRecentWindow(mk(3), 0)))
	require.Equal(t, []int{13, 14, 15, 16, 17, 18, 19, 20}, seqs(SelectRecentWindow(mk(20), 12)), "covered up to oldest-1: window stays 8")
	require.Equal(t, []int{11, 12, 13, 14, 15, 16, 17, 18, 19, 20}, seqs(SelectRecentWindow(mk(20), 10)))
	require.Equal(t, []int{9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}, seqs(SelectRecentWindow(mk(20), 0)), "capped at 12")
	ts := mk(20)
	ts[18].Status = "DISCARDED"
	require.NotContains(t, seqs(SelectRecentWindow(ts, 12)), 19)
}
