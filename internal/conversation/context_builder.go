package conversation

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/memory"
	"github.com/nabePi/kotoki-backend/internal/prompt"
)

// Mode is what the builder is asked to produce.
type Mode string

// Builder modes.
const (
	ModeConversation Mode = "CONVERSATION"
	ModeOpening      Mode = "OPENING"
)

// TurnContext is one past turn as the prompt sees it.
type TurnContext struct {
	Seq           int
	Status        string  // COMPLETED|INTERRUPTED|FAILED (DISCARDED is never sent)
	UserText      *string // nil for the opening
	AssistantText string  // for INTERRUPTED: assistant_spoken_text
}

// ConversationInput is everything BuildConversation reads; it is a pure function of it (§7.8.3).
// The character is Config.Snapshot.Character and never the live row (S-2, D-014).
type ConversationInput struct { //nolint:revive // name fixed by the spec (7.8.3)
	Mode             Mode
	Prompt           content.PromptVersion // CONVERSATION or OPENING version of the session
	Config           content.SessionConfig
	Learner          prompt.LearnerContext
	Adaptive         learner.AdaptiveDifficulty
	Memory           memory.MemoryContext
	RollingSummary   string
	RecentTurns      []TurnContext // ascending; see SelectRecentWindow
	CurrentUserText  string        // empty for the opening
	RemainingSeconds int
	Budget           prompt.Budget

	// DurationMinutes and SessionMode fill Session.DurationMinutes / Session.Mode of PromptData
	// (§6.8.2); the spec's ConversationInput has no source for them. SessionMode "" means USER_FIRST
	// in CONVERSATION and AI_FIRST in OPENING.
	DurationMinutes int
	SessionMode     string
}

// Message is one chat message: role system, user or assistant.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// BuiltPrompt is the LLM request material.
type BuiltPrompt struct {
	Messages             []Message      `json:"messages"`
	MaxOutputTokens      int            `json:"max_output_tokens"`
	Temperature          float32        `json:"temperature"`
	EstimatedInputTokens int            `json:"estimated_input_tokens"`
	ComponentTokens      map[string]int `json:"component_tokens"`
	Truncations          []string       `json:"truncations"`
	PromptVersionID      uuid.UUID      `json:"prompt_version_id"`
}

// SummaryInput is the input of the in-session rolling summary (§7.8.5).
type SummaryInput struct {
	PreviousSummary string
	Turns           []TurnContext // turns to summarise
	MaxRunes        int           // 300
}

// PromptBuilder builds LLM requests from runtime caches; it never does I/O (§7.8.2).
type PromptBuilder interface {
	BuildConversation(in ConversationInput) (BuiltPrompt, error)
	BuildRollingSummary(in SummaryInput) (BuiltPrompt, error)
}

// Component names of BuiltPrompt.ComponentTokens and Truncations.
const (
	compSystem         = "system"
	compCharacter      = "character"
	compScenario       = "scenario"
	compLearner        = "learner"
	compMemoryCore     = "memory_core"
	compMemoryEpisodic = "memory_episodic"
	compSummary        = "summary"
	compSessionState   = "session_state"
	compRecentTurns    = "recent_turns"
	compCurrentInput   = "current_input"
)

const (
	defaultTemperature     = 0.8
	conversationMaxOutput  = 300
	openingMaxOutput       = 200
	recentWindow           = 8
	recentWindowMax        = 12
	maxUserTextRunes       = 300
	maxCoreItems           = 20
	maxEpisodicItems       = 3
	interruptedMarker      = "（ユーザーが割り込みました）"
	learningPatternPrefix  = "（学習の傾向）"
	focusSkillsLinePrefix  = "学習のねらい"
	rollingSummaryMaxOut   = 400
	rollingSummaryTemp     = 0.3
	defaultSummaryMaxRunes = 300
	closingThresholdSecs   = 60
	statusDiscarded        = "DISCARDED"
	statusInterrupted      = "INTERRUPTED"
	statusFailed           = "FAILED"
)

// jakarta is Asia/Jakarta (no DST), built without a tz database so the builder stays pure.
var jakarta = time.FixedZone("Asia/Jakarta", 7*3600)

type promptBuilder struct{}

// NewPromptBuilder returns the pure, deterministic builder.
func NewPromptBuilder() PromptBuilder { return promptBuilder{} }

// SelectRecentWindow returns the turns the prompt may carry (§7.8.4): the last 8 non-DISCARDED
// turns, extended backwards (at most 12) to cover turns the rolling summary has not covered yet
// (seq > coveredUntilSeq). It returns a new slice.
func SelectRecentWindow(turns []TurnContext, coveredUntilSeq int) []TurnContext {
	live := make([]TurnContext, 0, len(turns))
	for _, t := range turns {
		if t.Status != statusDiscarded {
			live = append(live, t)
		}
	}
	start := max(len(live)-recentWindow, 0)
	for start > 0 && len(live)-start < recentWindowMax && live[start-1].Seq > coveredUntilSeq {
		start--
	}
	return live[start:]
}

func (promptBuilder) BuildConversation(in ConversationInput) (BuiltPrompt, error) {
	if in.Mode != ModeConversation && in.Mode != ModeOpening {
		return BuiltPrompt{}, fmt.Errorf("conversation: unknown prompt mode %q", in.Mode)
	}
	if in.Mode == ModeConversation && in.CurrentUserText == "" {
		return BuiltPrompt{}, errors.New("conversation: empty user text for a CONVERSATION prompt")
	}
	b := in.Budget
	if b == (prompt.Budget{}) {
		b = prompt.DefaultBudget
	}
	c := newCollector()

	data := buildPromptData(in)

	// 2 character, 3 topic + scenario.
	data.Character.Persona = fitCharacter(&data.Character, b.Character, c)
	fitScenario(&data.Scenario, data.Topic, b.Scenario, c)
	c.tokens[compCharacter] = characterTokens(data.Character)
	c.tokens[compScenario] = scenarioTokens(data.Scenario, data.Topic)

	// 12 current input (kept from the front).
	current := in.CurrentUserText
	if prompt.EstimateTokens(current) > b.CurrentInput {
		current = truncateToTokens(current, b.CurrentInput)
		c.truncated(compCurrentInput)
	}

	// Trim order 8 → 7 → 10 → 6 → 11 (§7.8.1).
	data.Memory.Episodes = fitItems(episodicTexts(in.Memory.Episodic), scores(in.Memory.Episodic), b.MemoryEpisodic, maxEpisodicItems, compMemoryEpisodic, c)
	data.Memory.Items = fitItems(coreTexts(in.Memory.Core), scores(in.Memory.Core), b.MemoryCore, maxCoreItems, compMemoryCore, c)
	c.tokens[compMemoryEpisodic] = listTokens(data.Memory.Episodes)
	c.tokens[compMemoryCore] = listTokens(data.Memory.Items)

	data.Session.Summary = in.RollingSummary
	if prompt.EstimateTokens(data.Session.Summary) > b.Summary {
		data.Session.Summary = truncateToTokens(data.Session.Summary, b.Summary)
		c.truncated(compSummary)
	}
	c.tokens[compSummary] = prompt.EstimateTokens(data.Session.Summary)

	if learnerTokens(data.Learner) > b.Learner {
		data.Learner.Adaptation = dropFocusLine(data.Learner.Adaptation)
		c.truncated(compLearner)
	}
	c.tokens[compLearner] = learnerTokens(data.Learner)

	history := historyMessages(in.RecentTurns)
	if in.Mode == ModeOpening {
		history = nil
	}
	history = fitHistory(history, b, c)
	c.tokens[compRecentTurns] = messagesTokens(history)
	c.tokens[compCurrentInput] = prompt.EstimateTokens(current)

	// 1 fixed text (rules, output contract, template text) and 9 session state.
	fixed, err := renderSystem(in.Prompt, skeleton(data))
	if err != nil {
		return BuiltPrompt{}, err
	}
	c.tokens[compSystem] = prompt.EstimateTokens(fixed)
	if c.tokens[compSystem] > b.System {
		return BuiltPrompt{}, fmt.Errorf("conversation: prompt version %s fixed text is %d tokens, budget %d", in.Prompt.ID, c.tokens[compSystem], b.System)
	}
	c.tokens[compSessionState] = prompt.EstimateTokens(fmt.Sprint(data.Session.RemainingMinutes, data.Session.DurationMinutes))

	system, err := renderSystem(in.Prompt, data)
	if err != nil {
		return BuiltPrompt{}, err
	}
	msgs := make([]Message, 0, len(history)+2)
	msgs = append(msgs, Message{Role: "system", Content: system})
	msgs = append(msgs, history...)
	if in.Mode == ModeConversation {
		msgs = append(msgs, Message{Role: "user", Content: current})
	}

	maxOut := openingMaxOutput
	if in.Mode == ModeConversation {
		maxOut = conversationMaxOutput
	}
	return BuiltPrompt{
		Messages:             msgs,
		MaxOutputTokens:      in.Prompt.LLMParams.MaxOutputTokensOr(maxOut),
		Temperature:          float32(in.Prompt.LLMParams.TemperatureOr(defaultTemperature)),
		EstimatedInputTokens: prompt.EstimateTokens(system) + messagesTokens(msgs[1:]),
		ComponentTokens:      c.tokens,
		Truncations:          c.truncations,
		PromptVersionID:      in.Prompt.ID,
	}, nil
}

// rollingSummarySystem is the §7.8.5 system text; %d is MaxRunes.
const rollingSummarySystem = "あなたは会話記録の要約者です。以下の「これまでの要約」と「新しい会話」を統合し、\n" +
	"日本語で%d字以内の要約を書いてください。含めること: 話題の流れ、ユーザーが話した事実・意見、\n" +
	"AIが約束したこと/質問中のこと。含めないこと: 文法の訂正、評価、推測。箇条書き不可。"

func (promptBuilder) BuildRollingSummary(in SummaryInput) (BuiltPrompt, error) {
	maxRunes := in.MaxRunes
	if maxRunes <= 0 {
		maxRunes = defaultSummaryMaxRunes
	}
	var lines []string
	for _, t := range in.Turns {
		if t.Status == statusDiscarded {
			continue
		}
		if t.UserText != nil && *t.UserText != "" {
			lines = append(lines, "ユーザー: "+*t.UserText)
		}
		if t.Status != statusFailed && t.AssistantText != "" {
			lines = append(lines, "AI: "+t.AssistantText)
		}
	}
	msgs := []Message{
		{Role: "system", Content: fmt.Sprintf(rollingSummarySystem, maxRunes)},
		{Role: "user", Content: "これまでの要約:\n" + in.PreviousSummary + "\n\n新しい会話:\n" + strings.Join(lines, "\n")},
	}
	return BuiltPrompt{
		Messages: msgs, MaxOutputTokens: rollingSummaryMaxOut, Temperature: rollingSummaryTemp,
		EstimatedInputTokens: messagesTokens(msgs),
		ComponentTokens:      map[string]int{},
	}, nil
}

type collector struct {
	tokens      map[string]int
	truncations []string
}

func newCollector() *collector {
	return &collector{tokens: map[string]int{}, truncations: []string{}}
}

func (c *collector) truncated(name string) {
	if !slices.Contains(c.truncations, name) {
		c.truncations = append(c.truncations, name)
	}
}

// buildPromptData maps the session snapshot to PromptData (§6.8.2) without any I/O. It carries
// no full name, voice id or vendor id (S-4, D-014); the character is the snapshot's.
func buildPromptData(in ConversationInput) prompt.PromptData {
	ch := in.Config.Snapshot.Character
	sv := in.Config.ScenarioVersion
	band, _ := content.DifficultyBand(sv.DifficultyLevel)

	formality := string(in.Adaptive.Formality)
	if formality == "" {
		formality = string(in.Config.Snapshot.Formality)
	}
	sentences := in.Adaptive.MaxAISentencesPerTurn
	if sentences <= 0 {
		sentences = content.DefaultStyle.MaxSentencesPerTurn
	}

	remaining := max(in.RemainingSeconds, 0)
	remainingMin := (remaining + 59) / 60
	mode := in.SessionMode
	if mode == "" {
		mode = "USER_FIRST"
		if in.Mode == ModeOpening {
			mode = "AI_FIRST"
		}
	}
	hint := ""
	if ch.GreetingHint != nil {
		hint = *ch.GreetingHint
	}
	return prompt.PromptData{
		CoreRules:      prompt.CoreRules,
		OutputContract: prompt.OutputContract(remaining <= closingThresholdSecs),
		Character:      prompt.CharacterData{Name: ch.Name, Persona: ch.Persona, GreetingHint: hint},
		Topic:          prompt.TopicData{Code: in.Config.Topic.Code, Name: in.Config.Topic.Name},
		Scenario: prompt.ScenarioData{
			Situation: sv.Situation, AIRole: sv.AIRole, UserRole: sv.UserRole, Goal: sv.Goal,
			Tasks: sv.Tasks, Constraints: sv.Constraints, GrammarTargets: sv.GrammarTargets, VocabHints: sv.VocabHints,
			DifficultyLevel: sv.DifficultyLevel, DifficultyBand: band, OpeningLine: sv.OpeningLine,
		},
		Speech: prompt.SpeechData{
			Formality: formality, FormalityInstruction: prompt.FormalityInstruction(formality),
			MaxSentencesPerTurn: sentences, MaxConsecutiveQuestions: content.DefaultStyle.MaxConsecutiveQuestions,
		},
		Learner: in.Learner,
		Session: prompt.SessionData{
			Mode: mode, DurationMinutes: max(in.DurationMinutes, remainingMin), RemainingMinutes: remainingMin,
		},
	}
}

func renderSystem(pv content.PromptVersion, data prompt.PromptData) (string, error) {
	r, err := prompt.Render(pv.Template(), data)
	if err != nil {
		return "", err
	}
	return r.System, nil
}

// skeleton is data with every variable field emptied: rendering it measures component 1.
func skeleton(d prompt.PromptData) prompt.PromptData {
	return prompt.PromptData{
		CoreRules: d.CoreRules, OutputContract: d.OutputContract,
		Speech: d.Speech, Session: prompt.SessionData{Mode: d.Session.Mode},
	}
}

// fitCharacter cuts the persona at a paragraph end so name + persona + greeting hint fit budget.
func fitCharacter(ch *prompt.CharacterData, budget int, c *collector) string {
	if characterTokens(*ch) <= budget {
		return ch.Persona
	}
	c.truncated(compCharacter)
	room := budget - prompt.EstimateTokens(ch.Name) - prompt.EstimateTokens(ch.GreetingHint)
	return cutAtParagraph(ch.Persona, room)
}

func characterTokens(ch prompt.CharacterData) int {
	return prompt.EstimateTokens(ch.Name) + prompt.EstimateTokens(ch.Persona) + prompt.EstimateTokens(ch.GreetingHint)
}

// cutAtParagraph keeps whole paragraphs (split on blank lines, then lines) while they fit; when
// not even the first paragraph fits it is cut by tokens.
func cutAtParagraph(s string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	for _, sep := range []string{"\n\n", "\n"} {
		paras := strings.Split(s, sep)
		if len(paras) < 2 {
			continue
		}
		kept := ""
		for i, p := range paras {
			next := p
			if i > 0 {
				next = kept + sep + p
			}
			if prompt.EstimateTokens(next) > maxTokens {
				break
			}
			kept = next
		}
		if kept != "" {
			return kept
		}
	}
	return truncateToTokens(s, maxTokens)
}

// truncateToTokens keeps the longest rune prefix of s that fits maxTokens (EstimateTokens is
// monotone in the prefix length).
func truncateToTokens(s string, maxTokens int) string {
	if maxTokens <= 0 {
		return ""
	}
	r := []rune(s)
	lo, hi := 0, len(r)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if prompt.EstimateTokens(string(r[:mid])) <= maxTokens {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return string(r[:lo])
}

func scenarioTokens(sc prompt.ScenarioData, topic prompt.TopicData) int {
	n := prompt.EstimateTokens(topic.Name) + prompt.EstimateTokens(sc.Situation) + prompt.EstimateTokens(sc.AIRole) +
		prompt.EstimateTokens(sc.UserRole) + prompt.EstimateTokens(sc.Goal)
	for _, l := range [][]string{sc.Tasks, sc.Constraints, sc.GrammarTargets} {
		n += listTokens(l)
	}
	for _, h := range sc.VocabHints {
		n += prompt.EstimateTokens(h.TermJA+h.Reading+h.Meaning) + prompt.ItemOverheadTokens
	}
	return n
}

// fitScenario cuts the situation text first; when the lists alone are too large it drops their
// trailing items (vocabulary, grammar targets, constraints, tasks). It replaces slices, never
// edits them in place.
func fitScenario(sc *prompt.ScenarioData, topic prompt.TopicData, budget int, c *collector) {
	if scenarioTokens(*sc, topic) <= budget {
		return
	}
	c.truncated(compScenario)
	for scenarioTokens(prompt.ScenarioData{Situation: "", AIRole: sc.AIRole, UserRole: sc.UserRole, Goal: sc.Goal,
		Tasks: sc.Tasks, Constraints: sc.Constraints, GrammarTargets: sc.GrammarTargets, VocabHints: sc.VocabHints}, topic) > budget {
		switch {
		case len(sc.VocabHints) > 0:
			sc.VocabHints = sc.VocabHints[:len(sc.VocabHints)-1]
		case len(sc.GrammarTargets) > 0:
			sc.GrammarTargets = sc.GrammarTargets[:len(sc.GrammarTargets)-1]
		case len(sc.Constraints) > 0:
			sc.Constraints = sc.Constraints[:len(sc.Constraints)-1]
		case len(sc.Tasks) > 0:
			sc.Tasks = sc.Tasks[:len(sc.Tasks)-1]
		default:
			return
		}
	}
	rest := scenarioTokens(prompt.ScenarioData{AIRole: sc.AIRole, UserRole: sc.UserRole, Goal: sc.Goal,
		Tasks: sc.Tasks, Constraints: sc.Constraints, GrammarTargets: sc.GrammarTargets, VocabHints: sc.VocabHints}, topic)
	sc.Situation = cutAtParagraph(sc.Situation, budget-rest)
}

func listTokens(items []string) int {
	n := 0
	for _, it := range items {
		n += prompt.EstimateTokens(it) + prompt.ItemOverheadTokens
	}
	return n
}

var memorySanitizer = strings.NewReplacer("<", "＜", ">", "＞", "\r\n", "", "\n", "", "\r", "")

func coreTexts(items []memory.ContextItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		s := memorySanitizer.Replace(it.Content)
		if it.Category == memory.CategoryLearningPattern && !strings.HasPrefix(s, learningPatternPrefix) {
			s = learningPatternPrefix + s
		}
		out[i] = s
	}
	return out
}

func episodicTexts(items []memory.ContextItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ObservedAt.In(jakarta).Format("2006-01-02") + ": " + memorySanitizer.Replace(it.Content)
	}
	return out
}

func scores(items []memory.ContextItem) []float64 {
	out := make([]float64, len(items))
	for i, it := range items {
		out[i] = it.Score
	}
	return out
}

// fitItems drops the lowest-scored item (the later one on ties) until the list has at most
// maxCount items and fits budget; survivors keep their order.
func fitItems(texts []string, score []float64, budget, maxCount int, name string, c *collector) []string {
	idx := make([]int, len(texts))
	for i := range idx {
		idx[i] = i
	}
	cost := func() int {
		n := 0
		for _, i := range idx {
			n += prompt.EstimateTokens(texts[i]) + prompt.ItemOverheadTokens
		}
		return n
	}
	dropped := false
	for len(idx) > 0 && (len(idx) > maxCount || cost() > budget) {
		worst := 0
		for k, i := range idx {
			if score[i] <= score[idx[worst]] {
				worst = k
			}
		}
		idx = slices.Delete(idx, worst, worst+1)
		dropped = true
	}
	if dropped {
		c.truncated(name)
	}
	out := make([]string, len(idx))
	for k, i := range idx {
		out[k] = texts[i]
	}
	return out
}

func learnerTokens(l prompt.LearnerContext) int {
	return prompt.EstimateTokens(l.DisplayName) + prompt.EstimateTokens(l.Adaptation)
}

// dropFocusLine removes the 学習のねらい line; the dimension descriptor lines are never cut.
func dropFocusLine(adaptation string) string {
	lines := strings.Split(adaptation, "\n")
	kept := lines[:0:0]
	for _, l := range lines {
		if !strings.HasPrefix(strings.TrimSpace(l), focusSkillsLinePrefix) {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}

// historyMessages turns past turns into chat messages (§7.8.1): user then assistant per turn;
// INTERRUPTED carries the spoken text plus a marker; FAILED only the user; DISCARDED is skipped.
func historyMessages(turns []TurnContext) []Message {
	var out []Message
	for _, t := range turns {
		if t.Status == statusDiscarded {
			continue
		}
		if t.UserText != nil && *t.UserText != "" {
			out = append(out, Message{Role: "user", Content: *t.UserText})
		}
		switch {
		case t.Status == statusFailed || t.AssistantText == "":
		case t.Status == statusInterrupted:
			out = append(out, Message{Role: "assistant", Content: t.AssistantText + interruptedMarker})
		default:
			out = append(out, Message{Role: "assistant", Content: t.AssistantText})
		}
	}
	return out
}

func messagesTokens(ms []Message) int {
	n := 0
	for _, m := range ms {
		n += prompt.EstimateTokens(m.Content) + prompt.ItemOverheadTokens
	}
	return n
}

// fitHistory drops the oldest turns (never below MinRecentTurns) while over budget, then cuts
// every user message to 300 runes. A turn starts at a user message or at a lone assistant message.
func fitHistory(h []Message, b prompt.Budget, c *collector) []Message {
	if messagesTokens(h) <= b.RecentTurns {
		return h
	}
	c.truncated(compRecentTurns)
	h = slices.Clone(h)
	for messagesTokens(h) > b.RecentTurns && countTurns(h) > b.MinRecentTurns {
		h = h[nextTurnStart(h):]
	}
	if messagesTokens(h) > b.RecentTurns {
		for i := range h {
			if r := []rune(h[i].Content); h[i].Role == "user" && len(r) > maxUserTextRunes {
				h[i].Content = string(r[:maxUserTextRunes])
			}
		}
	}
	return h
}

// countTurns counts turn starts: every user message, and an assistant message that does not
// follow a user message.
func countTurns(h []Message) int {
	n := 0
	for i, m := range h {
		if m.Role == "user" || i == 0 || h[i-1].Role == "assistant" {
			n++
		}
	}
	return n
}

// nextTurnStart is the index of the second turn start (the first is index 0).
func nextTurnStart(h []Message) int {
	for i := 1; i < len(h); i++ {
		if h[i].Role == "user" || h[i-1].Role == "assistant" {
			return i
		}
	}
	return len(h)
}
