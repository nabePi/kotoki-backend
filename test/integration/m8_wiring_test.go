//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

const wiringFullName = "Test User" // users.full_name of testutil.SeedUser

// conversationInput is what the runtime hands to the prompt builder at the start of a session:
// the session configuration read back through the real content loader (the snapshot of rc is what
// session.Create stores), the learner block of the real learner service and the adaptive
// difficulty of the session.
func (r *adaptiveRig) conversationInput(uid uuid.UUID, ad learner.AdaptiveDifficulty) conversation.ConversationInput {
	r.t.Helper()
	ctx := context.Background()
	snap := r.rc.ConfigSnapshot
	snap.Character.Name, snap.Character.Persona = "葵", "A warm guide."
	raw, err := json.Marshal(snap)
	require.NoError(r.t, err)
	cfg, err := content.NewSessionConfigLoader(postgres.NewContentRepo(r.pool)).Load(ctx, content.SessionRefs{
		TopicID: r.rc.TopicID, ScenarioVersionID: r.rc.ScenarioVersionID, PromptVersionID: r.rc.PromptVersionID, ConfigSnapshot: raw,
	})
	require.NoError(r.t, err)
	cfg.PromptVersion.SystemTemplate = prompt.ConversationV1.System // the seed template of 6.8.3 (M3 seed)
	lc, err := r.svc.PromptContext(ctx, uid, ad)
	require.NoError(r.t, err)
	return conversation.ConversationInput{
		Mode: conversation.ModeConversation, Prompt: cfg.PromptVersion, Config: cfg, Learner: lc, Adaptive: ad,
		CurrentUserText: "切符をください。", RemainingSeconds: 600, DurationMinutes: 15, SessionMode: "USER_FIRST",
		Budget: prompt.DefaultBudget,
	}
}

func TestAdaptiveInPrompt_NewSessionAfterEvaluation(t *testing.T) {
	r := newAdaptiveRig(t)
	uid := r.user("N2")
	_, err := r.svc.PatchProfile(context.Background(), uid, learner.PatchInput{Nickname: lset("タロウ")})
	require.NoError(t, err)
	before := r.compute(uid)

	for i := 0; i < 3; i++ {
		ev, sid := r.eval(uid, learnerEval{
			endedAt: r.base.AddDate(0, 0, i), nTurns: 12, scores: [4]int{0, 100, 100, 100},
			adaptive: `{"grammar_complexity":5,"vocabulary_complexity":5,"expected_response_length":5,"scenario_difficulty":5}`,
		})
		require.NoError(t, r.handle(r.delivery(uid, ev, sid)))
	}
	after := r.compute(uid)
	require.LessOrEqual(t, after.GrammarComplexity, before.GrammarComplexity, "grammar complexity follows the worse GRAMMAR level")
	require.Greater(t, after.Scaffolding, before.Scaffolding)
	require.Contains(t, after.FocusSkills, learner.SkillGrammar)

	in := r.conversationInput(uid, after)
	require.Contains(t, in.Learner.Adaptation, "<conversation_difficulty>")
	bp, err := conversation.NewPromptBuilder().BuildConversation(in)
	require.NoError(t, err)
	system := bp.Messages[0].Content
	require.Contains(t, system, "<conversation_difficulty>")
	require.Contains(t, system, "学習のねらい:")
	require.Contains(t, system, prompt.DimensionDescriptor("grammar", after.GrammarComplexity), "descriptor of the new grammar level")
	require.Contains(t, system, prompt.ScaffoldingDescriptor(after.Scaffolding))
	require.Contains(t, system, "タロウ", "the nickname is the display name")
	require.NotContains(t, system, wiringFullName, "users.full_name never reaches the prompt")

	// the session before the evaluations was given the easier-to-follow prompt of its own level
	bpBefore, err := conversation.NewPromptBuilder().BuildConversation(r.conversationInput(uid, before))
	require.NoError(t, err)
	require.NotEqual(t, bpBefore.Messages[0].Content, system)
}

// worstCaseAdaptive picks the longest descriptor of every dimension and the two longest focus lines.
func worstCaseAdaptive() learner.AdaptiveDifficulty {
	longest := func(dim string) int {
		best, bestN := 1, 0
		for l := 1; l <= 5; l++ {
			if n := utf8.RuneCountInString(prompt.DimensionDescriptor(dim, l)); n > bestN {
				best, bestN = l, n
			}
		}
		return best
	}
	longestOf := func(vals []string, get func(string) string) string {
		best, bestN := "", -1
		for _, v := range vals {
			if n := utf8.RuneCountInString(get(v)); n > bestN {
				best, bestN = v, n
			}
		}
		return best
	}
	sc := 0
	for i := 0; i <= 3; i++ {
		if utf8.RuneCountInString(prompt.ScaffoldingDescriptor(i)) > utf8.RuneCountInString(prompt.ScaffoldingDescriptor(sc)) {
			sc = i
		}
	}
	skills := learner.AllSkills()
	a, b := skills[0], skills[1]
	for _, s := range skills {
		n := utf8.RuneCountInString(prompt.FocusDescriptor(string(s)))
		switch {
		case n > utf8.RuneCountInString(prompt.FocusDescriptor(string(a))):
			a, b = s, a
		case s != a && n > utf8.RuneCountInString(prompt.FocusDescriptor(string(b))):
			b = s
		}
	}
	return learner.AdaptiveDifficulty{
		AlgorithmVersion: 1, ScenarioDifficulty: 5,
		VocabularyComplexity: longest("vocabulary"), GrammarComplexity: longest("grammar"),
		SpeechSpeed: longest("speech_speed"), TopicNovelty: longest("topic_novelty"),
		TopicAbstraction: longest("topic_abstraction"), QuestionComplexity: longest("question_complexity"),
		FollowUpDepth: longest("follow_up_depth"), ExpectedResponseLength: longest("expected_response_length"),
		Spontaneity: longest("spontaneity"), TurnTakingComplexity: longest("turn_taking"), Scaffolding: sc,
		Formality:             learner.Formality(longestOf([]string{"CASUAL", "POLITE", "KEIGO_LIGHT", "KEIGO"}, prompt.FormalityDescriptor)),
		DialectExposure:       learner.DialectExposure(longestOf([]string{"NONE", "LIGHT", "FULL"}, prompt.DialectDescriptor)),
		MaxAISentencesPerTurn: 4, FocusSkills: []learner.Skill{a, b},
	}
}

func TestWorstCaseLearnerContext_TotalInputWithinBudget(t *testing.T) {
	r := newAdaptiveRig(t)
	uid := r.user("N2")
	goal := "LIVING_IN_JAPAN"
	nick := strings.Repeat("あ", 30)
	_, err := r.svc.PatchProfile(context.Background(), uid, learner.PatchInput{Nickname: lset(nick), LearningGoal: lset(goal)})
	require.NoError(t, err)

	ad := worstCaseAdaptive()
	in := r.conversationInput(uid, ad)
	text := func(unit string, n int) string {
		return string([]rune(strings.Repeat(unit, n/utf8.RuneCountInString(unit)+1))[:n])
	}
	in.Memory.Core, in.Memory.Episodic = nil, nil
	for i := 0; i < 20; i++ {
		in.Memory.Core = append(in.Memory.Core, memory.ContextItem{Content: text("あいうえお", 40), Score: float64(100 - i)})
	}
	for i := 0; i < 3; i++ {
		in.Memory.Episodic = append(in.Memory.Episodic, memory.ContextItem{Content: text("かきくけこ", 150), Score: 0.9 - float64(i)/100})
	}
	in.RollingSummary = text("要約です。", 500)
	for i := 0; i < 12; i++ {
		u := text("ユーザー発話。", 300)
		in.RecentTurns = append(in.RecentTurns, conversation.TurnContext{Seq: i + 1, Status: "COMPLETED", UserText: &u, AssistantText: text("返答です。", 300)})
	}
	in.CurrentUserText = text("今日は天気がいいですね。", 600)

	// the same input with no limit at all: what the prompt weighs before any trimming
	unlimited := in
	unlimited.Budget = prompt.Budget{System: 1 << 20, Character: 1 << 20, Scenario: 1 << 20, Learner: 1 << 20, MemoryCore: 1 << 20,
		MemoryEpisodic: 1 << 20, Summary: 1 << 20, SessionState: 1 << 20, RecentTurns: 1 << 20, CurrentInput: 1 << 20, MinRecentTurns: 4}
	full, err := conversation.NewPromptBuilder().BuildConversation(unlimited)
	require.NoError(t, err)

	bp, err := conversation.NewPromptBuilder().BuildConversation(in)
	require.NoError(t, err)
	require.LessOrEqual(t, bp.EstimatedInputTokens, prompt.DefaultBudget.Total(), "6450 tokens (D-014) after trimming")
	require.LessOrEqual(t, bp.ComponentTokens["learner"], prompt.DefaultBudget.Learner)

	system := bp.Messages[0].Content
	require.Greater(t, full.EstimatedInputTokens, prompt.DefaultBudget.Total()+500, "the fixture really needs trimming")
	// the focus block goes only after episodic, core and summary are spent, and only when the
	// total before trimming is more than 6950 tokens (6450 plus the 500-token slack of the block)
	if strings.Contains(system, "学習のねらい:") {
		require.NotContains(t, bp.Truncations, "learner")
	} else {
		require.Contains(t, bp.Truncations, "learner")
		require.Greater(t, full.EstimatedInputTokens, prompt.DefaultBudget.Total()+500)
	}
	for _, line := range strings.Split(in.Learner.Adaptation, "\n") {
		if strings.HasPrefix(line, "- ") && !isFocusLine(line, ad) {
			require.Contains(t, system, line, "a dimension descriptor is never truncated")
		}
	}
	require.NotContains(t, system, wiringFullName)
	t.Logf("untrimmed total %d, trimmed %d, truncations %v", full.EstimatedInputTokens, bp.EstimatedInputTokens, bp.Truncations)
}

func isFocusLine(line string, ad learner.AdaptiveDifficulty) bool {
	for _, s := range ad.FocusSkills {
		if strings.Contains(line, prompt.FocusDescriptor(string(s))) {
			return true
		}
	}
	return false
}

func TestInSessionAdjustment_ViaContextBuilder(t *testing.T) {
	r := newAdaptiveRig(t)
	uid := r.user("N4")
	snapshot := r.compute(uid)
	require.Greater(t, snapshot.SpeechSpeed, 1, "room to slow down")
	raw, err := json.Marshal(snapshot)
	require.NoError(t, err)
	sid := testutil.SeedSession(t, r.pool, uid, r.content, testutil.Row{"adaptive_difficulty": raw})

	texts := []string{"こんにちは。", "元気です。", "もう一度お願いします。", "はい。", "もう一度言ってください。", "ありがとう。"}
	st := conversation.AdaptiveState{Current: snapshot}
	var adjustedAt []int
	var seen []string
	for i, tx := range texts {
		seen = append(seen, tx)
		var ok bool
		st, ok = conversation.MaybeAdjust(st, seen)
		if ok {
			adjustedAt = append(adjustedAt, i+1)
		}
	}
	require.Equal(t, []int{5}, adjustedAt, "two clarification signals in the last six turns, at least four turns since the start")
	cur := st.Current
	require.Equal(t, snapshot.SpeechSpeed-1, cur.SpeechSpeed)
	require.Equal(t, max(1, snapshot.QuestionComplexity-1), cur.QuestionComplexity)
	require.Equal(t, min(3, snapshot.Scaffolding+1), cur.Scaffolding)
	require.Equal(t, 1, cur.InSessionAdjustments)
	require.InDelta(t, learner.TTSSpeakingRate(cur.SpeechSpeed), cur.TTSSpeakingRate, 1e-9)
	require.Equal(t, learner.MaxAISentences(cur.SpeechSpeed), cur.MaxAISentencesPerTurn)

	// the prompt of the next turn carries the new descriptors
	in := r.conversationInput(uid, snapshot)
	lc := r.svc.Rerender(in.Learner, cur)
	in.Learner, in.Adaptive = lc, cur
	bp, err := conversation.NewPromptBuilder().BuildConversation(in)
	require.NoError(t, err)
	require.Contains(t, bp.Messages[0].Content, prompt.DimensionDescriptor("speech_speed", cur.SpeechSpeed))
	require.Contains(t, bp.Messages[0].Content, prompt.ScaffoldingDescriptor(cur.Scaffolding))
	before, err := conversation.NewPromptBuilder().BuildConversation(r.conversationInput(uid, snapshot))
	require.NoError(t, err)
	require.NotEqual(t, before.Messages[0].Content, bp.Messages[0].Content)

	// a new clarification signal less than four turns later does not adjust again
	st2, ok := conversation.MaybeAdjust(st, append(seen, "もう一度。"))
	require.False(t, ok)
	require.Equal(t, 1, st2.Current.InSessionAdjustments)

	// at most two adjustments per session, however many signals follow
	adjusted := 1
	for i := 0; i < 30; i++ {
		var ok bool
		st, ok = conversation.MaybeAdjust(st, []string{"もう一度。", "ゆっくり話してください。"})
		if ok {
			adjusted++
		}
	}
	require.Equal(t, 2, adjusted)
	require.Equal(t, 2, st.Current.InSessionAdjustments)

	// the snapshot in the database is the session's, untouched by the runtime's in-memory state
	var stored []byte
	require.NoError(t, r.pool.QueryRow(context.Background(), `SELECT adaptive_difficulty FROM sessions WHERE id = $1`, sid).Scan(&stored))
	require.JSONEq(t, string(raw), string(stored))
}

// §7.6.3: a learner or memory failure never fails a session start. The runtime degrades on the
// error of the learner (loadLearner) and on Degraded of memory (loadMemory); here the real services
// over a broken database report exactly that.
func TestWiring_LearnerAndMemoryFailureDegradeSafely(t *testing.T) {
	rr, r := newRetrievalRigPG(t)
	fs := rr.extract(uuid.Nil)
	uid := fs.UserID
	ctx := context.Background()

	ad := learner.AdaptiveDifficulty{AlgorithmVersion: 1, ScenarioDifficulty: 3, Formality: learner.FormalityPolite, MaxAISentencesPerTurn: 2}
	ls := learner.NewService(learner.Deps{Store: postgres.NewStore(r.pool).LearnerStore(), Clock: clock.System{}, IDs: idgen.NewV7()})
	ok, err := ls.PromptContext(ctx, uid, ad)
	require.NoError(t, err)
	require.NotEmpty(t, ok.Adaptation)

	_, err = r.pool.Exec(ctx, `ALTER TABLE memories RENAME TO memories_gone`)
	require.NoError(t, err)
	_, err = r.pool.Exec(ctx, `ALTER TABLE learner_profiles RENAME TO learner_profiles_gone`)
	require.NoError(t, err)

	lc, err := ls.PromptContext(ctx, uid, ad)
	require.Error(t, err, "the learner reports the failure; the runtime starts without the block")
	require.Empty(t, lc.Adaptation)

	mc, err := rr.ret.BuildSessionContext(ctx, memory.SessionContextInput{UserID: uid, SessionID: uuid.Must(uuid.NewV7()), ScenarioText: "面接練習"})
	require.NoError(t, err, "memory never fails the start")
	require.True(t, mc.Degraded, "the runtime drops a degraded context")
}
