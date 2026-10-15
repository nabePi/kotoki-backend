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
		SessionMode:      "USER_FIRST",
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
	in.SessionMode = "AI_FIRST"
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
	r, err := prompt.Render(in.Prompt.Template(), data)
	require.NoError(t, err)
	require.Equal(t, r.System, bp.Messages[0].Content)
	require.Contains(t, bp.Messages[0].Content, "About 2 of 15 minutes remain.")

	in.RemainingSeconds = 60
	closing := build(t, in)
	require.Contains(t, closing.Messages[0].Content, "wrap up the conversation")
	require.Contains(t, closing.Messages[0].Content, "About 1 of 15 minutes remain.")
	require.NotContains(t, bp.Messages[0].Content, "wrap up the conversation")
}

func TestFullNameNeverInPrompt(t *testing.T) {
	in := baseInput()
	in.Learner.DisplayName = "タロウ"
	// A full name that leaked into any field the templates do not read must not reach the messages.
	in.Config.Snapshot.Character.Code = fullName
	in.Config.Snapshot.Character.AvatarKey = sp(fullName)
	in.Learner.GoalLine = fullName
	in.Learner.FocusSkills = []string{fullName}
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
	require.Equal(t, []int{12, 13, 14, 15, 16, 17, 18, 20}, seqs(SelectRecentWindow(ts, 12)))
}

// allCapsInput fills every capped component up to (or past) its own cap, so only the total
// budget can still force trimming.
func allCapsInput() ConversationInput {
	in := baseInput()
	in.Config.Snapshot.Character.Persona = strings.TrimSpace(strings.Repeat("A warm, curious conversation partner who loves travel and food.\n\n", 40))
	in.Config.ScenarioVersion.Situation = rep("駅で会います。", 600)
	in.Config.ScenarioVersion.Tasks = []string{rep("あいうえお", 20), rep("かきくけこ", 20)}
	in.Learner.Adaptation = "<conversation_difficulty>\n語彙: " + rep("や", 560) + "\n学習のねらい: 聞き取り\n</conversation_difficulty>"
	in.RollingSummary = rep("要約です。", 450)
	for i := range 20 {
		in.Memory.Core = append(in.Memory.Core, memory.ContextItem{Content: rep("あいうえお", 22), Score: float64(100 - i)})
	}
	for i := range 3 {
		in.Memory.Episodic = append(in.Memory.Episodic, memory.ContextItem{Content: rep("かきくけこ", 140), Score: 0.9 - float64(i)/100})
	}
	for i := range 12 {
		in.RecentTurns = append(in.RecentTurns, TurnContext{Seq: i + 1, Status: "COMPLETED", UserText: sp(rep("あ", 300)), AssistantText: rep("い", 300)})
	}
	in.CurrentUserText = rep("今日は天気がいいですね。", 700)
	return in
}

func TestAllCapsTotalTrimmed(t *testing.T) {
	in := allCapsInput()
	bp := build(t, in)
	require.LessOrEqual(t, bp.EstimatedInputTokens, in.Budget.Total())
	// the per-component caps leave the total over (component 1 is 646, recent turns sit at the
	// 4-turn floor): the ordered trimming takes episodic items first and leaves core and learner alone
	require.Contains(t, bp.Truncations, "memory_episodic")
	require.Less(t, bp.ComponentTokens["memory_episodic"], 2*170)
	require.Equal(t, 20*(prompt.EstimateTokens(rep("あいうえお", 22))+prompt.ItemOverheadTokens), bp.ComponentTokens["memory_core"])
	require.Equal(t, 650, bp.ComponentTokens["learner"])
	require.Equal(t, 495, bp.ComponentTokens["summary"])
	require.Contains(t, bp.Messages[0].Content, "学習のねらい")
	require.GreaterOrEqual(t, len(bp.Messages), 1+4*2+1)
	require.Equal(t, 12, len(in.RecentTurns))
}

func TestScenarioOverflowCutsGoalAndRoles(t *testing.T) {
	in := baseInput()
	sv := &in.Config.ScenarioVersion
	sv.Situation = rep("駅で会います。", 2000)
	sv.Goal, sv.UserRole, sv.AIRole = rep("目標", 500), rep("旅行者", 300), rep("駅員", 300)
	sv.Tasks = []string{rep("あ", 100)}
	bp := build(t, in)
	require.LessOrEqual(t, bp.ComponentTokens["scenario"], in.Budget.Scenario)
	require.Contains(t, bp.Truncations, "scenario")
	require.NotContains(t, bp.Messages[0].Content, "駅で会います")
	require.Equal(t, 2000, len([]rune(sv.Situation)), "input not mutated")
	require.Len(t, sv.Tasks, 1)
}

func TestHandoffFieldsRequired(t *testing.T) {
	for name, mut := range map[string]func(*ConversationInput){
		"duration zero":        func(in *ConversationInput) { in.DurationMinutes = 0 },
		"duration < 0":         func(in *ConversationInput) { in.DurationMinutes = -1 },
		"remaining > duration": func(in *ConversationInput) { in.RemainingSeconds = in.DurationMinutes*60 + 1 },
		"mode empty":           func(in *ConversationInput) { in.SessionMode = "" },
		"mode unknown":         func(in *ConversationInput) { in.SessionMode = "OTHER" },
		"difficulty range":     func(in *ConversationInput) { in.Config.ScenarioVersion.DifficultyLevel = 9 },
	} {
		t.Run(name, func(t *testing.T) {
			in := baseInput()
			mut(&in)
			_, err := NewPromptBuilder().BuildConversation(in)
			require.Error(t, err)
			require.NotContains(t, err.Error(), in.CurrentUserText)
		})
	}
}

func TestInterruptedWithoutSpokenTextKeepsMarker(t *testing.T) {
	in := baseInput()
	in.RecentTurns = []TurnContext{{Seq: 1, Status: "INTERRUPTED", UserText: sp("あの")}}
	bp := build(t, in)
	require.Equal(t, []Message{{"user", "あの"}, {"assistant", "（ユーザーが割り込みました）"}, {"user", "切符をください。"}}, bp.Messages[1:])
}

func TestBuilderCapsRecentTurnsAtTwelve(t *testing.T) {
	in := baseInput()
	for i := range 30 {
		in.RecentTurns = append(in.RecentTurns, TurnContext{Seq: i + 1, Status: "COMPLETED", UserText: sp("u"), AssistantText: "a"})
	}
	in.RecentTurns[29].Status = "DISCARDED"
	bp := build(t, in)
	require.Equal(t, 1+12*2+1, len(bp.Messages))
	require.Len(t, in.RecentTurns, 30)
}

func TestRollingSummaryTruncationsNotNull(t *testing.T) {
	bp, err := NewPromptBuilder().BuildRollingSummary(SummaryInput{})
	require.NoError(t, err)
	b, err := json.Marshal(bp)
	require.NoError(t, err)
	require.Contains(t, string(b), `"truncations":[]`)
}

// maxFit is the largest n with measure(n) <= budget (measure is monotone).
func maxFit(budget int, measure func(n int) int) int {
	lo, hi := 0, 5000
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if measure(mid) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

func TestCapBoundaries(t *testing.T) {
	b := prompt.DefaultBudget
	turns := func(n int) []TurnContext {
		var ts []TurnContext
		for i := range 4 {
			ts = append(ts, TurnContext{Seq: i + 1, Status: "COMPLETED", UserText: sp(rep("あ", 150)), AssistantText: rep("い", 150)})
		}
		return append(ts, TurnContext{Seq: 5, Status: "COMPLETED", UserText: sp(rep("あ", 150)), AssistantText: rep("い", n)})
	}
	cases := []struct {
		name    string
		measure func(n int) int
		apply   func(in *ConversationInput, n int)
		budget  int
	}{
		{"summary", func(n int) int { return prompt.EstimateTokens(rep("あ", n)) }, func(in *ConversationInput, n int) { in.RollingSummary = rep("あ", n) }, b.Summary},
		{"current_input", func(n int) int { return prompt.EstimateTokens(rep("あ", n)) }, func(in *ConversationInput, n int) { in.CurrentUserText = rep("あ", n) }, b.CurrentInput},
		{"memory_core", func(n int) int { return prompt.EstimateTokens(rep("あ", n)) + prompt.ItemOverheadTokens }, func(in *ConversationInput, n int) {
			in.Memory.Core = []memory.ContextItem{{Content: rep("あ", n), Score: 1}}
		}, b.MemoryCore},
		{"memory_episodic", func(n int) int {
			return prompt.EstimateTokens("2026-10-01: "+rep("あ", n)) + prompt.ItemOverheadTokens
		}, func(in *ConversationInput, n int) {
			in.Memory.Episodic = []memory.ContextItem{{Content: rep("あ", n), Score: 1, ObservedAt: time.Date(2026, 10, 1, 5, 0, 0, 0, time.UTC)}}
		}, b.MemoryEpisodic},
		{"learner", func(n int) int { return learnerTokens(learnerWith(n)) }, func(in *ConversationInput, n int) { in.Learner = learnerWith(n) }, b.Learner},
		{"character", func(n int) int { return characterTokens(prompt.CharacterData{Name: "葵", Persona: rep("あ", n)}) }, func(in *ConversationInput, n int) {
			in.Config.Snapshot.Character.GreetingHint = nil
			in.Config.Snapshot.Character.Persona = rep("あ", n)
		}, b.Character},
		{"scenario", func(n int) int { return scenarioWith(n).tokens() }, func(in *ConversationInput, n int) {
			in.Config.ScenarioVersion.Situation = rep("あ", n)
		}, b.Scenario},
		{"recent_turns", func(n int) int { return messagesTokens(historyMessages(turns(n))) }, func(in *ConversationInput, n int) { in.RecentTurns = turns(n) }, b.RecentTurns},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := maxFit(c.budget, c.measure)
			require.Equal(t, c.budget, c.measure(n), "boundary is reachable exactly")
			in := baseInput()
			c.apply(&in, n)
			require.NotContains(t, build(t, in).Truncations, c.name, "exactly at the limit")
			in = baseInput()
			c.apply(&in, n+1)
			require.Contains(t, build(t, in).Truncations, c.name, "one token over")
		})
	}
}

func learnerWith(n int) prompt.LearnerContext {
	return prompt.LearnerContext{DisplayName: "タロウ", Adaptation: "<conversation_difficulty>\n語彙: " + rep("や", n) + "\n学習のねらい: 聞き取り\n</conversation_difficulty>"}
}

type scenarioProbe struct {
	sc    prompt.ScenarioData
	topic prompt.TopicData
}

func (p scenarioProbe) tokens() int { return scenarioTokens(p.sc, p.topic) }

func scenarioWith(n int) scenarioProbe {
	b := baseInput()
	sv := b.Config.ScenarioVersion
	return scenarioProbe{
		sc:    prompt.ScenarioData{Situation: rep("あ", n), AIRole: sv.AIRole, UserRole: sv.UserRole, Goal: sv.Goal},
		topic: prompt.TopicData{Name: b.Config.Topic.Name},
	}
}

// trimFixture keeps every component well under its own cap (so no per-component trim runs) and
// lets the tests aim the TOTAL budget: Budget.Total() == target after withTotal.
func trimFixture() ConversationInput {
	in := baseInput()
	in.Learner.Adaptation = "<conversation_difficulty>\n語彙: やさしい言葉を使う\n学習のねらい: 聞き取り、丁寧語\n</conversation_difficulty>"
	in.RollingSummary = rep("旅の話をした。", 150)
	for i := range 3 {
		in.Memory.Core = append(in.Memory.Core, memory.ContextItem{Content: rep("あいうえお", 20), Score: float64(10 - i)})
	}
	for i := range 2 {
		in.Memory.Episodic = append(in.Memory.Episodic, memory.ContextItem{Content: rep("かきくけこ", 30), Score: 0.9 - float64(i)/10})
	}
	for i := range 8 {
		in.RecentTurns = append(in.RecentTurns, TurnContext{Seq: i + 1, Status: "COMPLETED", UserText: sp(rep("こんにちは", 30)), AssistantText: rep("どうも", 30)})
	}
	return in
}

func withTotal(in ConversationInput, total int) ConversationInput {
	in.Budget.System = total - (in.Budget.Total() - in.Budget.System)
	return in
}

// TestTotalTrimOrderPinned pins the §7.8.1 order 8 → 7 → 10 → 6 → 11 and that trimming stops as
// soon as the total fits: every row is the exact outcome for one target total (numbers measured
// from trimFixture; E = 1568 untrimmed).
func TestTotalTrimOrderPinned(t *testing.T) {
	base := build(t, trimFixture())
	require.Equal(t, 1568, base.EstimatedInputTokens)
	require.Empty(t, base.Truncations)

	const (
		epi, core, summ, learn, turns = "memory_episodic", "memory_core", "summary", "learner", "recent_turns"
	)
	rows := []struct {
		total                           int
		truncations                     []string
		est, epiTok, coreTok, summTok   int
		learnTok, turnTok, messageCount int
	}{
		{1560, []string{epi}, 1531, 41, 78, 165, 49, 592, 1 + 8*2 + 1},
		{1450, []string{epi, core}, 1439, 0, 26, 165, 49, 592, 1 + 8*2 + 1},
		{1300, []string{epi, core, summ}, 1299, 0, 0, 135, 49, 592, 1 + 8*2 + 1},
		{1150, []string{epi, core, summ, learn}, 1138, 0, 0, 0, 32, 592, 1 + 8*2 + 1},
		{1100, []string{epi, core, summ, learn, turns}, 1064, 0, 0, 0, 32, 518, 1 + 7*2 + 1},
		{300, []string{epi, core, summ, learn, turns}, 842, 0, 0, 0, 32, 296, 1 + 4*2 + 1},
	}
	for _, r := range rows {
		bp := build(t, withTotal(trimFixture(), r.total))
		require.Equal(t, r.truncations, bp.Truncations, "total %d", r.total)
		require.Equal(t, r.est, bp.EstimatedInputTokens, "total %d", r.total)
		ct := bp.ComponentTokens
		require.Equal(t, []int{r.epiTok, r.coreTok, r.summTok, r.learnTok, r.turnTok},
			[]int{ct[epi], ct[core], ct[summ], ct[learn], ct[turns]}, "total %d", r.total)
		require.Len(t, bp.Messages, r.messageCount, "total %d", r.total)
		if r.learnTok == 32 {
			require.NotContains(t, bp.Messages[0].Content, "学習のねらい")
			require.Contains(t, bp.Messages[0].Content, "語彙: やさしい言葉を使う")
		} else {
			require.Contains(t, bp.Messages[0].Content, "学習のねらい")
		}
	}
}

// TestAllCapsTruncationOrder pins the whole Truncations slice when caps and total both trim.
func TestAllCapsTruncationOrder(t *testing.T) {
	bp := build(t, allCapsInput())
	require.Equal(t, []string{"character", "scenario", "current_input", "recent_turns", "memory_episodic"}, bp.Truncations)
}
