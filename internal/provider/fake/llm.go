package fake

import (
	"context"
	"hash/fnv"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

const (
	openingTemplate = "こんにちは！今日はどんな一日でしたか？"
	deltaRunes      = 3
	longRepeat      = 5
	longMarker      = "#long"
	llmEventBuffer  = 128
	defaultLLMModel = "fake-llm"
)

var defaultTemplates = []string{
	"そうなんですね。それは面白いですね！もう少し詳しく教えてください。",
	"なるほど。私も最近、同じようなことを考えていました。どう思いますか？",
	"いいですね。私も行ってみたいです。週末は何をする予定ですか？",
	"大変でしたね。大丈夫でしたか？",
}

// LLM is the fake provider.LLMProvider: deterministic Japanese answers, 3-rune deltas.
type LLM struct {
	name      provider.Name
	cfg       CapConfig
	conf      Config
	deps      Deps
	dec       *decider
	jit       *jitterSource
	templates []string
}

var _ provider.LLMProvider = (*LLM)(nil)

// NewLLM builds a fake LLM. A bad FAKE_LLM_TEMPLATES_FILE falls back to the defaults
// (logged) so a typo never stops local development.
func NewLLM(name provider.Name, c CapConfig, cfg Config, d Deps) *LLM {
	d = d.WithDefaults()
	l := &LLM{name: name, cfg: c, conf: cfg, deps: d, templates: defaultTemplates,
		dec: newDecider(d.Seed, name, provider.CapLLM, c), jit: newJitterSource(d.Seed, 0x4c4c4d)}
	if cfg.LLMTemplatesFile != "" {
		if b, err := os.ReadFile(cfg.LLMTemplatesFile); err != nil {
			d.Log.Warn("fake llm: templates file unreadable, using defaults")
		} else if lines := nonEmptyLines(string(b)); len(lines) > 0 {
			l.templates = lines
		}
	}
	return l
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, ln := range strings.Split(s, "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			out = append(out, ln)
		}
	}
	return out
}

// Name implements provider.LLMProvider.
func (l *LLM) Name() provider.Name { return l.name }

func lastUserMessage(msgs []provider.Message) (string, bool) {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == provider.RoleUser {
			return msgs[i].Content, true
		}
	}
	return "", false
}

// answer builds the deterministic output text of a request.
func (l *LLM) answer(req provider.LLMRequest) (text string, finish provider.FinishReason) {
	user, ok := lastUserMessage(req.Messages)
	if !ok || strings.TrimSpace(user) == "" {
		text = openingTemplate
	} else {
		h := fnv.New32a()
		_, _ = h.Write([]byte(user))
		text = l.templates[int(h.Sum32())%len(l.templates)]
		if strings.Contains(user, longMarker) {
			text = strings.Repeat(text, longRepeat)
		}
	}
	finish = provider.FinishStop
	for _, s := range req.Stop {
		if s == "" {
			continue
		}
		if i := strings.Index(text, s); i >= 0 {
			text = text[:i]
		}
	}
	if req.MaxTokens > 0 && utf8.RuneCountInString(text) > req.MaxTokens {
		text = string([]rune(text)[:req.MaxTokens])
		finish = provider.FinishMaxTokens
	}
	return text, finish
}

func inputRunes(msgs []provider.Message) int {
	n := 0
	for _, m := range msgs {
		n += utf8.RuneCountInString(m.Content)
	}
	return n
}

type llmStream struct {
	ch     chan provider.LLMEvent
	cancel context.CancelFunc
	once   sync.Once
}

func (s *llmStream) Events() <-chan provider.LLMEvent { return s.ch }

func (s *llmStream) Close() error {
	s.once.Do(s.cancel)
	return nil
}

// Stream implements provider.LLMProvider.
func (l *LLM) Stream(ctx context.Context, req provider.LLMRequest) (provider.LLMStream, error) {
	if err := ctx.Err(); err != nil {
		return nil, cancelledErr(l.name, provider.CapLLM, err)
	}
	fault, _ := l.deps.Faults.Current().Lookup(provider.CapLLM, l.name)
	fail, plan := l.dec.next(fault)
	if fail && plan.Phase == PhaseStart {
		return nil, errorFor(l.name, provider.CapLLM, plan.Kind)
	}
	sctx, cancel := context.WithCancel(ctx)
	s := &llmStream{ch: make(chan provider.LLMEvent, llmEventBuffer), cancel: cancel}
	go l.run(sctx, s, req, fail, plan)
	return s, nil
}

func (l *LLM) emit(s *llmStream, ev provider.LLMEvent, model string) {
	ev.Provider, ev.Model, ev.ReceivedAt = l.name, model, l.deps.Clock.Now()
	select { // non-blocking: only used for the terminal CANCELLED event
	case s.ch <- ev:
	default:
	}
}

func (l *LLM) run(ctx context.Context, s *llmStream, req provider.LLMRequest, fail bool, plan failPlan) {
	defer close(s.ch)
	defer s.cancel()
	model := req.Model
	if model == "" {
		model = defaultLLMModel
	}
	text, finish := l.answer(req)
	inRunes := inputRunes(req.Messages)
	emitted := 0 // output runes sent so far

	cancelDone := func() {
		l.emit(s, provider.LLMEvent{Type: provider.LLMDone, FinishReason: provider.FinishCancelled,
			Usage: &provider.TokenUsage{InputTokens: inRunes, OutputTokens: emitted, Estimated: true}}, model)
	}
	send := func(ev provider.LLMEvent) bool {
		ev.Provider, ev.Model, ev.ReceivedAt = l.name, model, l.deps.Clock.Now()
		select {
		case s.ch <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}

	if fail && plan.Phase == PhaseHang && plan.HangAfter == 0 {
		<-ctx.Done()
		cancelDone()
		return
	}
	wait := l.jit.delay(l.cfg.LatencyMS, l.cfg.JitterMS) + time.Duration(plan.ExtraDelayMs)*time.Millisecond
	if !sleep(ctx, wait) {
		cancelDone()
		return
	}
	if fail && plan.Phase == PhaseBeforeFirst {
		send(provider.LLMEvent{Type: provider.LLMError, Err: errorFor(l.name, provider.CapLLM, plan.Kind)})
		return
	}
	runes := []rune(text)
	interval := time.Duration(l.conf.LLMTokenIntervalMS) * time.Millisecond
	deltas := 0
	for i := 0; i < len(runes); i += deltaRunes {
		if fail && plan.Phase == PhaseMid && deltas >= plan.After {
			send(provider.LLMEvent{Type: provider.LLMError, Err: errorFor(l.name, provider.CapLLM, plan.Kind)})
			return
		}
		if fail && plan.Phase == PhaseHang && deltas >= plan.HangAfter { // S-7: stall after some output
			<-ctx.Done()
			cancelDone()
			return
		}
		if deltas > 0 && !sleep(ctx, interval) {
			cancelDone()
			return
		}
		end := min(i+deltaRunes, len(runes))
		if !send(provider.LLMEvent{Type: provider.LLMDelta, Text: string(runes[i:end])}) {
			cancelDone()
			return
		}
		emitted += end - i
		deltas++
	}
	if fail && plan.Phase == PhaseMid { // the answer was shorter than FailAfter deltas
		send(provider.LLMEvent{Type: provider.LLMError, Err: errorFor(l.name, provider.CapLLM, plan.Kind)})
		return
	}
	send(provider.LLMEvent{Type: provider.LLMDone, FinishReason: finish,
		Usage: &provider.TokenUsage{InputTokens: inRunes, OutputTokens: emitted}})
}
