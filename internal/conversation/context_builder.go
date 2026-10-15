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
	// (§6.8.2); the spec's ConversationInput has no source for them (sessions.duration_seconds and
	// sessions.conversation_start_mode). Both are required: DurationMinutes > 0 and SessionMode
	// USER_FIRST or AI_FIRST, otherwise BuildConversation fails instead of guessing.
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
	sessionUserFirst       = "USER_FIRST"
	sessionAIFirst         = "AI_FIRST"
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
	if in.DurationMinutes <= 0 {
		return BuiltPrompt{}, fmt.Errorf("conversation: DurationMinutes must be positive, got %d", in.DurationMinutes)
	}
	if in.SessionMode != sessionUserFirst && in.SessionMode != sessionAIFirst {
		return BuiltPrompt{}, fmt.Errorf("conversation: SessionMode must be %s or %s, got %q", sessionUserFirst, sessionAIFirst, in.SessionMode)
	}
	if (max(in.RemainingSeconds, 0)+59)/60 > in.DurationMinutes {
		return BuiltPrompt{}, fmt.Errorf("conversation: %d s remaining exceed DurationMinutes %d", in.RemainingSeconds, in.DurationMinutes)
	}
	band, _ := content.DifficultyBand(in.Config.ScenarioVersion.DifficultyLevel)
	if band == "" {
		return BuiltPrompt{}, fmt.Errorf("conversation: scenario difficulty level %d is out of range 1-5", in.Config.ScenarioVersion.DifficultyLevel)
	}
	b := in.Budget
	if b == (prompt.Budget{}) {
		b = prompt.DefaultBudget
	}
	c := newCollector()
	data := buildPromptData(in)

	// 2 character, 3 topic + scenario.
	data.Character.Persona = fitCharacter(data.Character, b.Character, c)
	fitScenario(&data.Scenario, data.Topic, b.Scenario, c)

	// 12 current input (kept from the front).
	current := in.CurrentUserText
	if prompt.EstimateTokens(current) > b.CurrentInput {
		current = truncateToTokens(current, b.CurrentInput)
		c.truncated(compCurrentInput)
	}

	// Per-component caps, in the order 8 → 7 → 10 → 6 → 11 (§7.8.1).
	episodes := memEntries(episodicTexts(in.Memory.Episodic), in.Memory.Episodic)
	core := memEntries(coreTexts(in.Memory.Core), in.Memory.Core)
	episodes = fitItems(episodes, b.MemoryEpisodic, maxEpisodicItems, compMemoryEpisodic, c)
	core = fitItems(core, b.MemoryCore, maxCoreItems, compMemoryCore, c)

	data.Session.Summary = in.RollingSummary
	if prompt.EstimateTokens(data.Session.Summary) > b.Summary {
		data.Session.Summary = truncateToTokens(data.Session.Summary, b.Summary)
		c.truncated(compSummary)
	}
	if learnerTokens(data.Learner) > b.Learner {
		data.Learner.Adaptation = dropFocusLine(data.Learner.Adaptation)
		c.truncated(compLearner)
	}

	var history []Message
	if in.Mode == ModeConversation {
		history = historyMessages(in.RecentTurns)
	}
	if messagesTokens(history) > b.RecentTurns {
		c.truncated(compRecentTurns)
		history = slices.Clone(history)
		for messagesTokens(history) > b.RecentTurns {
			if !trimHistoryOnce(&history, b.MinRecentTurns, false) {
				break
			}
		}
		if messagesTokens(history) > b.RecentTurns {
			trimHistoryOnce(&history, b.MinRecentTurns, true)
		}
	}

	// Component 1: fixed text with every conditional block present (headers, memory rules, ...).
	fixed, err := renderSystem(in.Prompt, skeleton(data))
	if err != nil {
		return BuiltPrompt{}, err
	}
	c.tokens[compSystem] = prompt.EstimateTokens(fixed)
	// The spec calls an over-budget component 1 a configuration error, but its own seed template
	// (§6.8.3, mandatory for M3) measures above 600 with every conditional block counted; the
	// excess is absorbed by the total-budget trimming below instead of failing every session.

	render := func() (string, int, error) {
		data.Memory.Items, data.Memory.Episodes = entryTexts(core), entryTexts(episodes)
		sys, err := renderSystem(in.Prompt, data)
		return sys, prompt.EstimateTokens(sys) + messagesTokens(history) + currentTokens(in.Mode, current), err
	}
	system, total, err := render()
	if err != nil {
		return BuiltPrompt{}, err
	}
	// Total input over budget: keep trimming 8 → 7 → 10 → 6 (focus line) → 11 (to the floor).
	for total > b.Total() {
		over := total - b.Total()
		switch {
		case len(episodes) > 0:
			episodes = dropLowest(episodes)
			c.truncated(compMemoryEpisodic)
		case len(core) > 0:
			core = dropLowest(core)
			c.truncated(compMemoryCore)
		case data.Session.Summary != "":
			data.Session.Summary = truncateToTokens(data.Session.Summary, max(prompt.EstimateTokens(data.Session.Summary)-over, 0))
			c.truncated(compSummary)
		case dropFocusLine(data.Learner.Adaptation) != data.Learner.Adaptation:
			data.Learner.Adaptation = dropFocusLine(data.Learner.Adaptation)
			c.truncated(compLearner)
		default:
			h := slices.Clone(history)
			if !trimHistoryOnce(&h, b.MinRecentTurns, false) && !trimHistoryOnce(&h, b.MinRecentTurns, true) {
				return finishPrompt(in, c, system, history, current, total, data, core, episodes), nil
			}
			history = h
			c.truncated(compRecentTurns)
		}
		if system, total, err = render(); err != nil {
			return BuiltPrompt{}, err
		}
	}
	return finishPrompt(in, c, system, history, current, total, data, core, episodes), nil
}

func currentTokens(mode Mode, current string) int {
	if mode != ModeConversation {
		return 0
	}
	return prompt.EstimateTokens(current) + prompt.ItemOverheadTokens
}

func finishPrompt(in ConversationInput, c *collector, system string, history []Message, current string, total int,
	data prompt.PromptData, core, episodes []memEntry) BuiltPrompt {
	c.tokens[compCharacter] = characterTokens(data.Character)
	c.tokens[compScenario] = scenarioTokens(data.Scenario, data.Topic)
	c.tokens[compLearner] = learnerTokens(data.Learner)
	c.tokens[compMemoryCore] = listTokens(entryTexts(core))
	c.tokens[compMemoryEpisodic] = listTokens(entryTexts(episodes))
	c.tokens[compSummary] = prompt.EstimateTokens(data.Session.Summary)
	c.tokens[compSessionState] = prompt.EstimateTokens(fmt.Sprint(data.Session.RemainingMinutes, data.Session.DurationMinutes))
	c.tokens[compRecentTurns] = messagesTokens(history)
	c.tokens[compCurrentInput] = prompt.EstimateTokens(current)

	msgs := make([]Message, 0, len(history)+2)
	msgs = append(msgs, Message{Role: "system", Content: system})
	msgs = append(msgs, history...)
	maxOut := openingMaxOutput
	if in.Mode == ModeConversation {
		msgs = append(msgs, Message{Role: "user", Content: current})
		maxOut = conversationMaxOutput
	}
	return BuiltPrompt{
		Messages:             msgs,
		MaxOutputTokens:      in.Prompt.LLMParams.MaxOutputTokensOr(maxOut),
		Temperature:          float32(in.Prompt.LLMParams.TemperatureOr(defaultTemperature)),
		EstimatedInputTokens: total,
		ComponentTokens:      c.tokens,
		Truncations:          c.truncations,
		PromptVersionID:      in.Prompt.ID,
	}
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
		Truncations:          []string{},
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
			Mode: in.SessionMode, DurationMinutes: in.DurationMinutes, RemainingMinutes: remainingMin,
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

// skeleton is data with every variable field replaced by a one-character placeholder, and the
// conditional template blocks switched on: rendering it measures component 1 (fixed text
// including headers and memory rules), slightly over-counted by the placeholders.
func skeleton(d prompt.PromptData) prompt.PromptData {
	return prompt.PromptData{
		CoreRules: d.CoreRules, OutputContract: prompt.OutputContract(false), // the closing text is component 9
		Character: prompt.CharacterData{GreetingHint: "x"},
		Scenario: prompt.ScenarioData{
			Tasks: []string{"x"}, Constraints: []string{"x"}, GrammarTargets: []string{"x"},
			VocabHints: []prompt.VocabHint{{TermJA: "x", Reading: "x", Meaning: "x"}},
		},
		Speech:  d.Speech,
		Learner: prompt.LearnerContext{DisplayName: "x", Adaptation: "x"},
		Memory:  prompt.MemoryData{Items: []string{"x"}, Episodes: []string{"x"}},
		Session: prompt.SessionData{Mode: d.Session.Mode, Summary: "x"},
	}
}

// fitCharacter cuts the persona at a paragraph end so name + persona fit budget (component 2).
func fitCharacter(ch prompt.CharacterData, budget int, c *collector) string {
	if characterTokens(ch) <= budget {
		return ch.Persona
	}
	c.truncated(compCharacter)
	return cutAtParagraph(ch.Persona, budget-prompt.EstimateTokens(ch.Name))
}

func characterTokens(ch prompt.CharacterData) int {
	return prompt.EstimateTokens(ch.Name) + prompt.EstimateTokens(ch.Persona)
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

// fitScenario enforces component 3. Over budget: the situation is cut first (at a paragraph end),
// preceded by dropping trailing list items (vocabulary, grammar targets, constraints, tasks) when
// the lists alone are too large; when goal and roles still do not fit, the situation is removed and
// goal, user role and AI role are cut by tokens in that order. It replaces slices, never edits them.
func fitScenario(sc *prompt.ScenarioData, topic prompt.TopicData, budget int, c *collector) {
	if scenarioTokens(*sc, topic) <= budget {
		return
	}
	c.truncated(compScenario)
	situation := sc.Situation
	sc.Situation = ""
	for scenarioTokens(*sc, topic) > budget {
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
			for _, f := range []*string{&sc.Goal, &sc.UserRole, &sc.AIRole} {
				over := scenarioTokens(*sc, topic) - budget
				if over <= 0 {
					break
				}
				*f = truncateToTokens(*f, max(prompt.EstimateTokens(*f)-over, 0))
			}
			return
		}
	}
	sc.Situation = cutAtParagraph(situation, budget-scenarioTokens(*sc, topic))
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

// memEntry is one memory line with the score that ranks it.
type memEntry struct {
	text  string
	score float64
}

func memEntries(texts []string, items []memory.ContextItem) []memEntry {
	out := make([]memEntry, len(texts))
	for i, t := range texts {
		out[i] = memEntry{text: t, score: items[i].Score}
	}
	return out
}

func entryTexts(es []memEntry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.text
	}
	return out
}

// dropLowest returns a copy without the lowest-scored entry (the later one on ties).
func dropLowest(es []memEntry) []memEntry {
	worst := 0
	for i, e := range es {
		if e.score <= es[worst].score {
			worst = i
		}
	}
	return slices.Delete(slices.Clone(es), worst, worst+1)
}

// fitItems drops the lowest-scored entries until at most maxCount remain and they fit budget;
// survivors keep their order.
func fitItems(es []memEntry, budget, maxCount int, name string, c *collector) []memEntry {
	for len(es) > 0 && (len(es) > maxCount || listTokens(entryTexts(es)) > budget) {
		es = dropLowest(es)
		c.truncated(name)
	}
	return es
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
//
// At most recentWindowMax non-DISCARDED turns are used even when the caller skipped
// SelectRecentWindow.
func historyMessages(turns []TurnContext) []Message {
	var out []Message
	for _, t := range SelectRecentWindow(turns, -1) { // 12 newest live turns
		if t.UserText != nil && *t.UserText != "" {
			out = append(out, Message{Role: "user", Content: *t.UserText})
		}
		switch t.Status {
		case statusFailed:
		case statusInterrupted:
			// §7.8.1 literally: spoken text + marker, so an interrupt before the first audio
			// still tells the model it was cut off.
			out = append(out, Message{Role: "assistant", Content: t.AssistantText + interruptedMarker})
		default:
			if t.AssistantText != "" {
				out = append(out, Message{Role: "assistant", Content: t.AssistantText})
			}
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

// trimHistoryOnce makes one step of recent-turn trimming: drop the oldest turn while more than
// minTurns remain, or (cutUsers) cut every user message to 300 runes. It reports whether it
// changed anything. h must be a private copy.
func trimHistoryOnce(h *[]Message, minTurns int, cutUsers bool) bool {
	if !cutUsers {
		if countTurns(*h) <= minTurns {
			return false
		}
		*h = (*h)[nextTurnStart(*h):]
		return true
	}
	changed := false
	for i, m := range *h {
		if r := []rune(m.Content); m.Role == "user" && len(r) > maxUserTextRunes {
			(*h)[i].Content = string(r[:maxUserTextRunes])
			changed = true
		}
	}
	return changed
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
