package main

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/nabePi/kotoki-backend/cmd/internal/providerwire"
	"github.com/nabePi/kotoki-backend/cmd/kotoki-cli/internal/bench"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/fake"
	"github.com/nabePi/kotoki-backend/internal/provider/pricing"
)

// benchFlags are the flags of `kotoki-cli provider-bench` (spec 8.10.1).
type benchFlags struct {
	suite          string
	dataset        string
	modelsFile     string
	out            string
	repeat         int
	audioRoot      string
	turnEndMs      int
	steps          string
	human          string
	schemasDir     string
	pacing         bool
	e2eItems       int
	attemptTimeout time.Duration
	maxCostPerMin  float64
}

func (a *app) cmdProviderBench(ctx context.Context, args []string) error {
	var f benchFlags
	fs := a.newFlagSet("provider-bench")
	fs.StringVar(&f.suite, "suite", "all", "stt|llm|llm-json|tts|e2e|concurrency|all")
	fs.StringVar(&f.dataset, "dataset", "testdata/benchmark", "dataset directory holding manifest.jsonl")
	fs.StringVar(&f.modelsFile, "models-file", "bench/models.yaml", "candidates per capability")
	fs.StringVar(&f.out, "out", "", "output directory, e.g. bench/results/2026-10-17/ (required)")
	fs.IntVar(&f.repeat, "repeat", 3, "attempts per item and candidate")
	fs.StringVar(&f.audioRoot, "audio-root", "", "directory with the dataset audio (aws s3 sync of the private bucket); needed for s3:// and relative audio_uri")
	fs.IntVar(&f.turnEndMs, "turn-end-ms", 0, "turn-end delay added to the e2e latency (no VAD or turn engine runs)")
	fs.StringVar(&f.steps, "concurrency-steps", "10,50,100,200", "concurrency ramp; the error-rate gate reads step 100")
	fs.StringVar(&f.human, "human", "", "CSV with the human ratings (item_id,reviewer,candidate,score_naturalness,score_level,score_character,mos,kanji_misread)")
	fs.StringVar(&f.schemasDir, "schemas-dir", "bench/schemas", "JSON Schema files <schema_name>.json of the llm-json suite")
	fs.BoolVar(&f.pacing, "pacing", true, "send STT audio in real time (one 20 ms frame per 20 ms)")
	fs.IntVar(&f.e2eItems, "e2e-items", 10, "number of STT-native items per e2e stack")
	fs.DurationVar(&f.attemptTimeout, "attempt-timeout", 30*time.Second, "timeout of one attempt")
	fs.Float64Var(&f.maxCostPerMin, "max-cost-per-min", 0, "LLM cost limit in USD per minute (0 = 3x the cheapest qualifying candidate)")
	if err := a.parse(fs, args); err != nil {
		return err
	}
	suites, steps, err := f.validate()
	if err != nil {
		return err
	}
	return a.runProviderBench(ctx, f, suites, steps)
}

func (f benchFlags) validate() (suites []string, steps []int, err error) {
	switch {
	case f.suite == "all":
		suites = slices.Clone(bench.AllSuites)
	case slices.Contains(bench.AllSuites, f.suite):
		suites = []string{f.suite}
	default:
		return nil, nil, usagef("--suite must be one of %s or all, got %q", strings.Join(bench.AllSuites, "|"), f.suite)
	}
	switch {
	case strings.TrimSpace(f.out) == "":
		return nil, nil, usagef("--out is required")
	case f.repeat < 1:
		return nil, nil, usagef("--repeat must be >= 1, got %d", f.repeat)
	case f.turnEndMs < 0:
		return nil, nil, usagef("--turn-end-ms must be >= 0, got %d", f.turnEndMs)
	case f.e2eItems < 1:
		return nil, nil, usagef("--e2e-items must be >= 1, got %d", f.e2eItems)
	case f.attemptTimeout <= 0:
		return nil, nil, usagef("--attempt-timeout must be > 0")
	case f.maxCostPerMin < 0:
		return nil, nil, usagef("--max-cost-per-min must be >= 0")
	}
	for _, s := range strings.Split(f.steps, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n < 1 {
			return nil, nil, usagef("--concurrency-steps must be positive integers separated by commas, got %q", f.steps)
		}
		steps = append(steps, n)
	}
	return suites, steps, nil
}

// benchAdapters are the ports of one candidate.
type benchAdapters struct {
	STT  provider.STTProvider
	LLM  provider.LLMProvider
	Eval provider.EvaluationProvider
	TTS  provider.TTSProvider
}

// benchConfigFor narrows the router configuration to ONE candidate: only its vendor is enabled for
// the capability and its model (LLM: also the evaluation model, the llm-json suite uses it) is the
// one from the models file. A real vendor needs the capability in mode real (PROVIDER_MODE); a fake
// candidate (FAKE_PRIMARY) always runs in mode fake, so a dry run can never reach a vendor.
func benchConfigFor(base provider.RouterConfig, c bench.Candidate) (provider.RouterConfig, error) {
	cfg := base
	if c.Provider == provider.FakePrimary {
		cfg.Mode, cfg.ModeSTT, cfg.ModeLLM, cfg.ModeTTS, cfg.ModeEvaluation, cfg.ModeEmbedding = provider.ModeFake, "", "", "", "", ""
		return cfg, nil
	}
	caps := []provider.Capability{c.Capability}
	if c.Capability == provider.CapLLM {
		caps = append(caps, provider.CapEvaluation)
	}
	for _, cp := range caps {
		if base.ModeFor(cp) != provider.ModeReal {
			return cfg, fmt.Errorf("candidate %s needs PROVIDER_MODE=real for %s (it is %q): a vendor candidate never runs against the fakes", c.Key(), cp, base.ModeFor(cp))
		}
	}
	switch c.Capability {
	case provider.CapSTT:
		cfg.OpenAISTTEnabled, cfg.ElevenLabsSTTEnabled, cfg.CartesiaSTTEnabled = c.Provider == provider.OpenAI, c.Provider == provider.ElevenLabs, c.Provider == provider.Cartesia
		switch c.Provider {
		case provider.OpenAI:
			cfg.OpenAISTTModel = c.Model
		case provider.ElevenLabs:
			cfg.ElevenLabsSTTModel = c.Model
		case provider.Cartesia:
			cfg.CartesiaSTTModel = c.Model
		}
	case provider.CapLLM:
		cfg.OpenAILLMEnabled, cfg.DeepSeekLLMEnabled = c.Provider == provider.OpenAI, c.Provider == provider.DeepSeek
		switch c.Provider {
		case provider.OpenAI:
			cfg.OpenAILLMModel, cfg.OpenAIEvaluationModel = c.Model, c.Model
		case provider.DeepSeek:
			cfg.DeepSeekLLMModel, cfg.DeepSeekEvaluationModel = c.Model, c.Model
		}
	case provider.CapTTS:
		cfg.ElevenLabsTTSEnabled, cfg.CartesiaTTSEnabled = c.Provider == provider.ElevenLabs, c.Provider == provider.Cartesia
	}
	return cfg, nil
}

// buildBenchAdapters assembles the adapter of one candidate through providerwire (validation first:
// a missing key names its variable and nothing is dialled).
func (a *app) buildBenchAdapters(base provider.RouterConfig, fcfg fake.Config, c bench.Candidate) (benchAdapters, error) {
	cfg, err := benchConfigFor(base, c)
	if err != nil {
		return benchAdapters{}, err
	}
	caps := []provider.Capability{c.Capability}
	asm, err := providerwire.Assemble(cfg, fcfg, caps, providerwire.Deps{
		Lookup: func(k string) (string, bool) { v := a.getenv(k); return v, v != "" },
	})
	if err != nil {
		return benchAdapters{}, fmt.Errorf("candidate %s: %w", c.Key(), err)
	}
	switch c.Capability {
	case provider.CapSTT:
		return benchAdapters{STT: asm.Deps.STT}, nil
	case provider.CapLLM:
		return benchAdapters{LLM: asm.Deps.LLM, Eval: asm.Deps.Evaluation}, nil
	case provider.CapTTS:
		t, ok := asm.Deps.TTS[c.Provider]
		if !ok {
			return benchAdapters{}, fmt.Errorf("candidate %s: no TTS adapter for %s was built", c.Key(), c.Provider)
		}
		return benchAdapters{TTS: t}, nil
	}
	return benchAdapters{}, fmt.Errorf("candidate %s: capability %s cannot be benchmarked", c.Key(), c.Capability)
}

// benchRun is the state of one provider-bench run.
type benchRun struct {
	a        *app
	f        benchFlags
	steps    []int
	cands    []bench.Candidate
	items    []bench.Item
	adapters map[string]benchAdapters
	audio    bench.AudioSource
	schemas  *bench.SchemaDir
	clk      clock.Clock
	opts     []bench.Option
	results  []bench.Result
}

// of lists the candidates of a capability; with a non-empty scope a FALLBACK-role voice is left out
// (the e2e stacks use main voices).
func (r *benchRun) of(cp provider.Capability, scope string) []bench.Candidate {
	var out []bench.Candidate
	for _, c := range r.cands {
		if c.Capability == cp && (scope == "" || c.Role != bench.RoleFallback) {
			out = append(out, c)
		}
	}
	return out
}

func candID(c bench.Candidate) string { return string(c.Capability) + "|" + c.Key() + "|" + c.Role }

func (a *app) runProviderBench(ctx context.Context, f benchFlags, suites []string, steps []int) error {
	cands, err := bench.LoadModels(f.modelsFile)
	if err != nil {
		return err
	}
	items, err := bench.LoadManifest(filepath.Join(f.dataset, "manifest.jsonl"))
	if err != nil {
		return err
	}
	rc, fc, err := providerwire.ParseConfigs()
	if err != nil {
		return fmt.Errorf("provider configuration: %w", err)
	}
	r := &benchRun{a: a, f: f, steps: steps, cands: cands, items: items, adapters: map[string]benchAdapters{}, clk: clock.System{},
		audio: bench.DirAudio{Root: f.audioRoot}, schemas: bench.NewSchemaDir(f.schemasDir)}
	r.opts = []bench.Option{bench.WithPacing(f.pacing), bench.WithTimeout(f.attemptTimeout), bench.WithTurnEndMs(f.turnEndMs),
		bench.WithLLMParams(rc.LLMMaxOutputTokens, rc.LLMTemperature)}

	// every adapter is built before the first call: a missing key or a mode mismatch stops the run now
	for _, c := range cands {
		if !r.needed(c, suites) {
			continue
		}
		as, err := a.buildBenchAdapters(rc, fc, c)
		if err != nil {
			return err
		}
		r.adapters[candID(c)] = as
	}
	for _, s := range suites {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		n, err := r.runSuite(ctx, s)
		if err != nil {
			return err
		}
		if n == 0 {
			if f.suite != "all" {
				return fmt.Errorf("suite %s has nothing to run: no matching candidates or dataset items", s)
			}
			a.printf("%s: skipped (no matching candidates or dataset items)\n", s)
		}
	}
	return r.finish(rc)
}

// needed says whether a candidate takes part in any of the suites (so its adapter is built, and a
// vendor that no suite touches needs no key).
func (r *benchRun) needed(c bench.Candidate, suites []string) bool {
	for _, s := range suites {
		switch {
		case s == bench.SuiteSTT && c.Capability == provider.CapSTT,
			(s == bench.SuiteLLM || s == bench.SuiteLLMJSON) && c.Capability == provider.CapLLM,
			s == bench.SuiteTTS && c.Capability == provider.CapTTS,
			s == bench.SuiteE2E, s == bench.SuiteConcurrency:
			return true
		}
	}
	return false
}

func (r *benchRun) report(suite string, c bench.Candidate, rs []bench.Result) {
	var errs int
	for _, x := range rs {
		if x.ErrorKind != "" {
			errs++
		}
	}
	r.a.printf("%s %s: %d attempts, %d errors\n", suite, c.Key(), len(rs), errs)
	r.results = append(r.results, rs...)
}

func (r *benchRun) runSuite(ctx context.Context, suite string) (int, error) {
	before := len(r.results)
	var err error
	switch suite {
	case bench.SuiteSTT:
		items := bench.ItemsOf(r.items, bench.SetSTTNative, bench.SetSTTLearner, bench.SetSTTNoise)
		if len(items) > 0 {
			for _, c := range r.of(provider.CapSTT, "") {
				r.report(suite, c, bench.RunSTT(ctx, r.adapters[candID(c)].STT, c, items, r.audio, r.f.repeat, r.clk, r.opts...))
			}
		}
	case bench.SuiteLLM:
		items := bench.ItemsOf(r.items, bench.SetLLMContext)
		if len(items) > 0 {
			for _, c := range r.of(provider.CapLLM, "") {
				r.report(suite, c, bench.RunLLM(ctx, r.adapters[candID(c)].LLM, c, items, r.f.repeat, r.clk, r.opts...))
			}
		}
	case bench.SuiteLLMJSON:
		items := bench.ItemsOf(r.items, bench.SetLLMJSON)
		if len(items) > 0 {
			for _, c := range r.of(provider.CapLLM, "") {
				r.report(suite, c, bench.RunLLMJSON(ctx, r.adapters[candID(c)].Eval, c, items, r.schemas.Resolve, r.schemas.Validate, r.f.repeat, r.clk, r.opts...))
			}
		}
	case bench.SuiteTTS:
		items := bench.ItemsOf(r.items, bench.SetTTSSentences)
		if len(items) > 0 {
			for _, c := range r.of(provider.CapTTS, "") {
				r.report(suite, c, bench.RunTTS(ctx, r.adapters[candID(c)].TTS, c, items, r.f.repeat, r.clk, r.opts...))
			}
		}
	case bench.SuiteE2E:
		r.runE2E(ctx)
	case bench.SuiteConcurrency:
		err = r.runConcurrency(ctx)
	}
	return len(r.results) - before, err
}

func (r *benchRun) runE2E(ctx context.Context) {
	items := bench.ItemsOf(r.items, bench.SetSTTNative)
	if len(items) > r.f.e2eItems {
		items = items[:r.f.e2eItems]
	}
	for _, s := range r.of(provider.CapSTT, "") {
		for _, l := range r.of(provider.CapLLM, "") {
			for _, t := range r.of(provider.CapTTS, "main") {
				st := bench.E2EStack{STT: r.adapters[candID(s)].STT, STTCand: s, LLM: r.adapters[candID(l)].LLM, LLMCand: l, TTS: r.adapters[candID(t)].TTS, TTSCand: t}
				rs := bench.RunE2E(ctx, st, items, r.audio, r.f.repeat, r.clk, r.opts...)
				prov, model := st.Label()
				r.report(bench.SuiteE2E, bench.Candidate{Capability: bench.CapE2E, Provider: provider.Name(prov), Model: model}, rs)
			}
		}
	}
}

func (r *benchRun) runConcurrency(ctx context.Context) error {
	sttItems := bench.ItemsOf(r.items, bench.SetSTTNative, bench.SetSTTLearner)
	llmItems := bench.ItemsOf(r.items, bench.SetLLMContext)
	ttsItems := bench.ItemsOf(r.items, bench.SetTTSSentences)
	for _, c := range r.cands {
		as, ok := r.adapters[candID(c)]
		if !ok {
			continue
		}
		var call func(context.Context) error
		switch c.Capability {
		case provider.CapSTT:
			if len(sttItems) == 0 {
				continue
			}
			var err error
			if call, err = bench.STTCall(as.STT, sttItems[0], r.audio, r.clk, r.opts...); err != nil {
				return fmt.Errorf("concurrency %s: %w", c.Key(), err)
			}
		case provider.CapLLM:
			if len(llmItems) == 0 {
				continue
			}
			call = bench.LLMCall(as.LLM, c, llmItems[0], r.clk, r.opts...)
		case provider.CapTTS:
			if len(ttsItems) == 0 {
				continue
			}
			call = bench.TTSCall(as.TTS, c, ttsItems[0], r.clk, r.opts...)
		}
		r.report(bench.SuiteConcurrency, c, bench.RunConcurrency(ctx, c, r.steps, call, r.clk))
	}
	return nil
}

// finish adds the human ratings and the cost model, writes the result files and prints the verdicts
// and the recommendations (numbers and ids only, never dataset text).
func (r *benchRun) finish(rc provider.RouterConfig) error {
	if r.f.human != "" {
		hs, err := bench.LoadHuman(r.f.human, r.cands)
		if err != nil {
			return err
		}
		r.results = append(r.results, hs...)
	}
	prices, err := pricing.Load(rc.PricesFile)
	if err != nil {
		return err
	}
	for _, c := range bench.CandidatesOf(r.results) {
		r.results = append(r.results, bench.CostResult(c, prices, bench.DefaultUsage()))
	}
	if err := bench.WriteResults(r.f.out, r.results); err != nil {
		return err
	}
	scored := bench.ScoreAll(r.results)
	for _, s := range scored {
		r.a.printf("gates %s: %s\n", candID(s.Candidate), verdict(s))
	}
	stt, sttBackups, notes := bench.RecommendSTT(scored)
	llm, llmBackups, llmNotes := bench.RecommendLLM(scored, r.f.maxCostPerMin)
	voices := bench.RecommendCharacterVoices(scored, r.characters())
	for _, n := range slices.Concat(notes, llmNotes) {
		r.a.printf("%s\n", n)
	}
	for _, v := range voices {
		r.a.printf("voice %s: %s\n", v.Code, voiceLine(v))
	}
	var ttsVendors []provider.Name
	for _, v := range voices {
		for _, ch := range []bench.VoiceChoice{v.Primary, derefChoice(v.Fallback)} {
			if ch.ProviderName != "" && !slices.Contains(ttsVendors, provider.Name(ch.ProviderName)) {
				ttsVendors = append(ttsVendors, provider.Name(ch.ProviderName))
			}
		}
	}
	first := r.firstModels()
	if err := bench.WriteRecommendationEnv(filepath.Join(r.f.out, "provider.env"), stt, llm, first[provider.CapSTT], first[provider.CapLLM], first[provider.CapLLM], nil,
		bench.WithBackups(sttBackups, llmBackups), bench.WithTTSVendors(ttsVendors), bench.WithLLMLimits(rc.LLMMaxOutputTokens, rc.LLMTemperature)); err != nil {
		return err
	}
	if len(voices) > 0 {
		if err := bench.WriteCharacterVoices(filepath.Join(r.f.out, "character-voices.json"), voices); err != nil {
			return err
		}
	} else {
		r.a.printf("character-voices.json not written: no TTS candidate lists `characters`\n")
	}
	r.a.printf("results written to %s\n", r.f.out)
	return nil
}

func derefChoice(c *bench.VoiceChoice) bench.VoiceChoice {
	if c == nil {
		return bench.VoiceChoice{}
	}
	return *c
}

func voiceLine(v bench.CharacterVoice) string {
	if v.Primary.ProviderName == "" {
		return "no qualified voice (" + strings.Join(v.Notes, "; ") + ")"
	}
	s := fmt.Sprintf("primary %s/%s/%s", v.Primary.ProviderName, v.Primary.ModelName, v.Primary.VoiceID)
	if v.Fallback != nil {
		s += fmt.Sprintf(", fallback %s/%s/%s", v.Fallback.ProviderName, v.Fallback.ModelName, v.Fallback.VoiceID)
	} else {
		s += ", no fallback"
	}
	return s
}

func verdict(s bench.ScoredCandidate) string {
	var bad []string
	for _, g := range s.Gates() {
		if !g.Pass {
			bad = append(bad, fmt.Sprintf("%s %.4g (limit %.4g)", g.Name, g.Actual, g.Limit))
		}
	}
	if len(bad) == 0 {
		return "PASS"
	}
	return "FAIL: " + strings.Join(bad, "; ")
}

// characters is the union of the character codes the TTS candidates are proposed for.
func (r *benchRun) characters() []bench.CharacterSpec {
	var codes []string
	for _, c := range r.cands {
		if c.Capability != provider.CapTTS {
			continue
		}
		for _, ch := range c.Characters {
			if !slices.Contains(codes, ch) {
				codes = append(codes, ch)
			}
		}
	}
	slices.Sort(codes)
	out := make([]bench.CharacterSpec, len(codes))
	for i, c := range codes {
		out[i] = bench.CharacterSpec{Code: c}
	}
	return out
}

// firstModels is, per capability and vendor, the first model of the models file (the env file lists
// the model of every vendor; the recommended candidate overrides its vendor).
func (r *benchRun) firstModels() map[provider.Capability]map[provider.Name]string {
	out := map[provider.Capability]map[provider.Name]string{provider.CapSTT: {}, provider.CapLLM: {}}
	for _, c := range r.cands {
		if m, ok := out[c.Capability]; ok {
			if _, seen := m[c.Provider]; !seen && c.Provider != provider.FakePrimary {
				m[c.Provider] = c.Model
			}
		}
	}
	return out
}
