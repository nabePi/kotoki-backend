package prompt

import "strings"

func registerConversationFixtures() {
	for _, key := range []string{"CONVERSATION", "OPENING"} {
		RegisterFixtures(key, func() []any { return []any{FixtureMinimal(), FixtureMaximal()} })
	}
}

func init() {
	builtinFixtures = append(builtinFixtures, registerConversationFixtures)
	registerConversationFixtures()
}

// FixtureMinimal is the smallest valid data: empty slices, empty optional strings, a zero
// Learner (spec 6.10.1 (a)).
func FixtureMinimal() PromptData {
	return PromptData{
		CoreRules: CoreRules, OutputContract: OutputContract(false),
		Character: CharacterData{Name: "葵", Persona: strings.Repeat("A calm, friendly guide. ", 3)},
		Topic:     TopicData{Code: "DAILY", Name: "Daily life"},
		Scenario: ScenarioData{
			Situation: "A short chat.", AIRole: "A friend", UserRole: "A learner", Goal: "Chat for a few minutes",
			DifficultyLevel: 1, DifficultyBand: "BEGINNER", OpeningLine: "こんにちは！",
		},
		Speech:  SpeechData{Formality: FormalityPolite, FormalityInstruction: FormalityInstruction(FormalityPolite), MaxSentencesPerTurn: 3, MaxConsecutiveQuestions: 2},
		Session: SessionData{Mode: "USER_FIRST", DurationMinutes: 5, RemainingMinutes: 5},
	}
}

// repeat builds an n-rune string from unit.
func repeat(unit string, n int) string {
	r := []rune(strings.Repeat(unit, n/len([]rune(unit))+1))
	return string(r[:n])
}

func items(n, runes int, label string) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = label + repeat("あいうえお", runes-len([]rune(label)))
	}
	return out
}

// FixtureMaximal fills every field up to its limit (spec 6.10.1 (b)): persona 3000 and
// situation 2000 runes, the maximum number of tasks, constraints, grammar targets, vocab hints,
// memory items and episodes, a full Learner.Adaptation block and a rolling summary. The three
// largest free-text fields use their full limit; repeated items use a realistic length (60
// runes), because every field at its literal maximum would exceed the 12,000-character render
// limit of spec 6.8.1 on its own.
func FixtureMaximal() PromptData {
	hints := make([]VocabHint, 20)
	for i := range hints {
		hints[i] = VocabHint{TermJA: "経験", Reading: "けいけん", Meaning: "experience, something lived"}
	}
	return PromptData{
		CoreRules: CoreRules, OutputContract: OutputContract(true),
		Character: CharacterData{Name: "葵", Persona: repeat("A warm, curious conversation partner. ", 3000), GreetingHint: repeat("やあ！", 200)},
		Topic:     TopicData{Code: "TRAVEL", Name: "Travel and holidays"},
		Scenario: ScenarioData{
			Situation: repeat("You meet at a train station. ", 2000), AIRole: "A station clerk", UserRole: "A traveller", Goal: "Buy a ticket and ask about the platform",
			Tasks: items(6, 60, "Task: "), Constraints: items(8, 60, "Rule: "), GrammarTargets: items(8, 40, ""), VocabHints: hints,
			DifficultyLevel: 5, DifficultyBand: "INTERMEDIATE", OpeningLine: repeat("いらっしゃいませ。", 80),
		},
		Speech: SpeechData{Formality: FormalityKeigoLite, FormalityInstruction: FormalityInstruction(FormalityKeigoLite), MaxSentencesPerTurn: 4, MaxConsecutiveQuestions: 2},
		Learner: LearnerContext{
			DisplayName: "タロウ",
			Adaptation: "<conversation_difficulty>\n語彙: やさしい言葉を使う\n文法: 短い文で話す\n速さ: ゆっくり話す\n" +
				"学習のねらい: 聞き取り、丁寧語\n</conversation_difficulty>",
			GoalLine: "Travel in Japan", FocusSkills: []string{"LISTENING", "POLITENESS"},
		},
		Memory:  MemoryData{Items: items(20, 40, "- "), Episodes: items(3, 60, "2026-10-01: ")},
		Session: SessionData{Mode: "AI_FIRST", DurationMinutes: 30, RemainingMinutes: 12, Summary: repeat("They talked about the trip. ", 300)},
	}
}
