package prompt_test

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/prompt"
)

func render(t *testing.T, tpl prompt.Template, d prompt.PromptData) string {
	t.Helper()
	r, err := prompt.Render(tpl, d)
	require.NoError(t, err)
	require.Empty(t, r.User, "CONVERSATION and OPENING produce a system message only")
	return r.System
}

func TestV1TemplatesRenderWithinLimit(t *testing.T) {
	for _, tpl := range []prompt.Template{prompt.ConversationV1, prompt.OpeningV1} {
		fixtures := prompt.Fixtures(tpl.Key)
		require.Len(t, fixtures, 2, tpl.Key)
		for _, d := range fixtures {
			r, err := prompt.Render(tpl, d)
			require.NoError(t, err, tpl.Key)
			require.Empty(t, r.User)
			require.LessOrEqual(t, utf8.RuneCountInString(r.System), 12000, tpl.Key)
		}
	}
}

func TestMaximalFixtureReachesTheLimits(t *testing.T) {
	d := prompt.FixtureMaximal()
	require.Equal(t, 3000, utf8.RuneCountInString(d.Character.Persona))
	require.Equal(t, 200, utf8.RuneCountInString(d.Character.GreetingHint))
	require.Equal(t, 2000, utf8.RuneCountInString(d.Scenario.Situation))
	require.Len(t, d.Scenario.Tasks, 6)
	require.Len(t, d.Scenario.Constraints, 8)
	require.Len(t, d.Scenario.GrammarTargets, 8)
	require.Len(t, d.Scenario.VocabHints, 20)
	require.Len(t, d.Memory.Items, 20)
	require.Len(t, d.Memory.Episodes, 3)
	require.NotEmpty(t, d.Learner.Adaptation)
	require.Contains(t, d.Learner.Adaptation, "<conversation_difficulty>")
	m := prompt.FixtureMinimal()
	require.Empty(t, m.Scenario.Tasks)
	require.Empty(t, m.Memory.Items)
	require.Empty(t, m.Learner.DisplayName)
	require.Zero(t, m.Learner)
}

func TestNicknameNullHasNoNameSentence(t *testing.T) {
	d := prompt.FixtureMinimal()
	d.Learner.DisplayName = ""
	require.NotContains(t, render(t, prompt.ConversationV1, d), "The learner's name is")
	require.NotContains(t, render(t, prompt.OpeningV1, d), "greet")
	d.Learner.DisplayName = "太郎"
	require.Contains(t, render(t, prompt.ConversationV1, d), "The learner's name is 太郎.")
	require.Contains(t, render(t, prompt.OpeningV1, d), "greet 太郎 by name")
}

func TestCoreRulesAndOutputContractPlaceholdersExactlyOnce(t *testing.T) {
	for _, tpl := range []prompt.Template{prompt.ConversationV1, prompt.OpeningV1} {
		require.Equal(t, 1, strings.Count(tpl.System, "{{.CoreRules}}"), tpl.Key)
		require.Equal(t, 1, strings.Count(tpl.System, "{{.OutputContract}}"), tpl.Key)
	}
}

func TestOutputContractPlainText(t *testing.T) {
	for _, closing := range []bool{false, true} {
		c := prompt.OutputContract(closing)
		require.NotContains(t, c, "<e:")
		require.NotContains(t, c, "<", "no tag syntax anywhere in the contract")
		require.Contains(t, c, "plain Japanese text")
		require.Contains(t, c, "out loud")
		require.Contains(t, c, "markdown")
		require.Contains(t, c, "romaji")
		require.Contains(t, c, "tags")
	}
	require.NotContains(t, prompt.OutputContract(false), "wrap up")
	require.Contains(t, prompt.OutputContract(true), "wrap up")
	require.Contains(t, prompt.OutputContract(true), prompt.OutputContract(false), "the closing text is an addition")
}

func TestCoreRulesContent(t *testing.T) {
	for _, want := range []string{"natural Japanese", "romaji", "markdown", "emoji", "never write the learner's lines", "Do not correct", "grammar", "Stay in your role", "Never reveal"} {
		require.Contains(t, prompt.CoreRules, want)
	}
	require.NotContains(t, prompt.CoreRules, "<e:")
}

func TestCoreRulesPlusContractWithinBudget(t *testing.T) {
	n := prompt.EstimateTokens(prompt.CoreRules) + prompt.EstimateTokens(prompt.OutputContract(true))
	require.LessOrEqual(t, n, 450, "the fixed template text needs the rest of the 600-token component")
	require.LessOrEqual(t, n, prompt.DefaultBudget.System)
}

func TestSpeechSectionRendersMaxSentences(t *testing.T) {
	d := prompt.FixtureMinimal()
	d.Speech.MaxSentencesPerTurn, d.Speech.MaxConsecutiveQuestions = 3, 2
	out := render(t, prompt.ConversationV1, d)
	require.Contains(t, out, "# How you speak")
	require.Contains(t, out, "Never use more than 3 sentences in one reply.")
	require.Contains(t, out, "never end more than 2 replies in a row with a question")
	require.Contains(t, out, prompt.FormalityInstruction("POLITE"))
	for _, tpl := range []prompt.Template{prompt.ConversationV1, prompt.OpeningV1} {
		low := strings.ToLower(tpl.System)
		require.NotContains(t, low, "expression", tpl.Key)
		require.NotContains(t, tpl.System, "<e:", tpl.Key)
		require.NotContains(t, low, "style", tpl.Key)
	}
}

func TestAdaptationInsertedVerbatim(t *testing.T) {
	d := prompt.FixtureMaximal()
	d.Learner.Adaptation = "<conversation_difficulty>\n語彙: やさしい\n</conversation_difficulty>\n学習のねらい: 聞き取り"
	for _, tpl := range []prompt.Template{prompt.ConversationV1, prompt.OpeningV1} {
		out := render(t, tpl, d)
		require.Contains(t, out, "Follow the difficulty guidance below exactly (it is written in Japanese; never mention it to the learner):\n"+d.Learner.Adaptation, tpl.Key)
	}
	d.Learner.Adaptation = ""
	require.NotContains(t, render(t, prompt.ConversationV1, d), "difficulty guidance")
}

func TestOpeningRendersOpeningLineQuoted(t *testing.T) {
	d := prompt.FixtureMinimal()
	d.Scenario.OpeningLine = "いらっしゃいませ。"
	d.Learner.DisplayName = "太郎"
	out := render(t, prompt.OpeningV1, d)
	require.Contains(t, out, "「いらっしゃいませ。」")
	require.Contains(t, out, "and greet 太郎 by name:\n「いらっしゃいませ。」")
	require.NotContains(t, out, "past conversation", "the episodic sentence only appears with episodes")
	require.Contains(t, out, "# Your task now\nStart the conversation. You speak first.")
	require.NotContains(t, out, "Your usual way of greeting")

	d.Character.GreetingHint = "やあ！"
	d.Memory.Episodes = []string{"2026-10-01: talked about a trip"}
	out = render(t, prompt.OpeningV1, d)
	require.Contains(t, out, "Your usual way of greeting: やあ！")
	require.Contains(t, out, "You may briefly refer to one past conversation if it fits naturally.")
	require.True(t, strings.HasSuffix(out, prompt.OutputContract(false)))
}

func TestMemoryRulesOnlyWhenMemoryPresent(t *testing.T) {
	d := prompt.FixtureMinimal()
	for _, tpl := range []prompt.Template{prompt.ConversationV1, prompt.OpeningV1} {
		out := render(t, tpl, d)
		require.NotContains(t, out, "Memory rules", tpl.Key)
		require.NotContains(t, out, "What you already know", tpl.Key)
	}
	d.Memory.Items = []string{"likes cats"}
	out := render(t, prompt.ConversationV1, d)
	require.Contains(t, out, "What you already know about the learner (do not ask again):\n- likes cats")
	require.Contains(t, out, "Memory rules:")
	require.NotContains(t, out, "Bab 11", "the cross-document reference is not prompt text")
	require.NotContains(t, out, "Relevant past conversations")
	d.Memory.Items = nil
	d.Memory.Episodes = []string{"2026-10-01: x"}
	out = render(t, prompt.ConversationV1, d)
	require.Contains(t, out, "Relevant past conversations:\n- 2026-10-01: x")
	require.Contains(t, out, "Memory rules:")
}

func TestConversationSessionBlock(t *testing.T) {
	d := prompt.FixtureMinimal()
	d.Session = prompt.SessionData{DurationMinutes: 15, RemainingMinutes: 9}
	out := render(t, prompt.ConversationV1, d)
	require.Contains(t, out, "About 9 of 15 minutes remain.")
	require.NotContains(t, out, "Summary of the conversation so far")
	d.Session.Summary = "They greeted."
	out = render(t, prompt.ConversationV1, d)
	require.Contains(t, out, "Summary of the conversation so far: They greeted.")
	require.True(t, strings.HasPrefix(out, prompt.CoreRules))
	require.True(t, strings.HasSuffix(out, prompt.OutputContract(false)))
}

func TestScenarioBlocksAreConditional(t *testing.T) {
	d := prompt.FixtureMinimal()
	out := render(t, prompt.ConversationV1, d)
	for _, absent := range []string{"Guide the conversation", "Rules for this situation", "Useful words", "Create natural chances"} {
		require.NotContains(t, out, absent)
	}
	d.Scenario.Tasks = []string{"order"}
	d.Scenario.Constraints = []string{"no English"}
	d.Scenario.VocabHints = []prompt.VocabHint{{TermJA: "注文", Reading: "ちゅうもん", Meaning: "order"}}
	d.Scenario.GrammarTargets = []string{"〜たい", "〜てください"}
	out = render(t, prompt.ConversationV1, d)
	require.Contains(t, out, "(never list them aloud):\n- order")
	require.Contains(t, out, "Rules for this situation:\n- no English")
	require.Contains(t, out, "Useful words you may naturally use:\n- 注文（ちゅうもん）: order")
	require.Contains(t, out, "Create natural chances for the learner to use: 〜たい, 〜てください")
}

func TestTemplatesNeverMentionIdentityFields(t *testing.T) {
	for _, tpl := range []prompt.Template{prompt.ConversationV1, prompt.OpeningV1} {
		for _, banned := range []string{"FullName", "full_name", "Email", "email", "VoiceID", "Voice"} {
			require.NotContains(t, tpl.System, banned, tpl.Key)
		}
	}
}

func TestV1Params(t *testing.T) {
	require.Equal(t, 0.8, *prompt.ConversationV1.Params.Temperature)
	require.Equal(t, 320, *prompt.ConversationV1.Params.MaxOutputTokens)
	require.Equal(t, 0.7, *prompt.OpeningV1.Params.Temperature)
	require.Equal(t, 120, *prompt.OpeningV1.Params.MaxOutputTokens)
	require.Equal(t, "CONVERSATION", prompt.ConversationV1.Key)
	require.Equal(t, 1, prompt.ConversationV1.Version)
	require.Equal(t, "OPENING", prompt.OpeningV1.Key)
	require.Empty(t, prompt.ConversationV1.User)
	require.Nil(t, prompt.ConversationV1.OutputSchema)
}

func TestFixturesRegisteredAtInit(t *testing.T) {
	require.Len(t, prompt.Fixtures("CONVERSATION"), 2)
	require.Len(t, prompt.Fixtures("OPENING"), 2)
	_, ok := prompt.Fixtures("CONVERSATION")[0].(prompt.PromptData)
	require.True(t, ok)
}
