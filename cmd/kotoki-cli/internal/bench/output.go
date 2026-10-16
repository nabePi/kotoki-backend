package bench

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// Result file names inside the --out directory.
const (
	ResultsFile = "results.jsonl"
	SummaryFile = "summary.csv"
)

// SummaryColumns are the fixed columns of summary.csv.
var SummaryColumns = []string{"suite", "capability", "provider", "model", "voice", "metric", "p50", "p95", "gate", "pass"}

// SummaryRow is one line of summary.csv.
type SummaryRow struct{ Suite, Capability, Provider, Model, Voice, Metric, P50, P95, Gate, Pass string }

func (r SummaryRow) record() []string {
	return []string{r.Suite, r.Capability, r.Provider, r.Model, r.Voice, r.Metric, r.P50, r.P95, r.Gate, r.Pass}
}

func num(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return ""
	}
	return strconv.FormatFloat(v, 'g', 6, 64)
}

// gateHome says in which suite a gate is measured, for the summary.
var gateHome = map[string]string{
	MetricSTTFinalMs: SuiteSTT, "cer_native": SuiteSTT, "cer_learner": SuiteSTT,
	MetricLLMFirstSentMs: SuiteLLM, "format_ok": SuiteLLM, "json_after_repair_ok": SuiteLLMJSON, "quality_mean": SuiteHuman,
	MetricTTSFirstAudioMs: SuiteTTS, "mos": SuiteHuman, "kanji_err_rate": SuiteHuman,
	"error_rate_conc100": SuiteConcurrency,
}

var gateAtLeast = map[string]bool{"format_ok": true, "json_after_repair_ok": true, "quality_mean": true, "mos": true}

func gateLabel(g Gate) string {
	op := "<="
	if gateAtLeast[g.Name] {
		op = ">="
	}
	return g.Name + op + num(g.Limit)
}

// SummaryRows builds the rows of summary.csv from results alone: per candidate and suite the P50/P95
// of every measured value (concurrency: one pair per step, as `metric@step`), then one row per gate
// (the actual value in p50 and p95, the gate as `name<=limit` or `name>=limit`, pass true/false; a
// metric that was not measured is empty and fails) and `gates/all_gates`. E2E stacks have values only.
func SummaryRows(rs []Result) []SummaryRow {
	groups := map[string][]Result{}
	var keys []string
	for _, r := range rs {
		c := Candidate{Capability: provider.Capability(r.Capability), Provider: provider.Name(r.Provider), Model: r.Model, Voice: r.Voice, Role: r.Role}
		k := candKey(c)
		if _, ok := groups[k]; !ok {
			keys = append(keys, k)
		}
		groups[k] = append(groups[k], r)
	}
	sort.Strings(keys)
	var rows []SummaryRow
	for _, k := range keys {
		g := groups[k]
		c := Candidate{Capability: provider.Capability(g[0].Capability), Provider: provider.Name(g[0].Provider), Model: g[0].Model, Voice: g[0].Voice, Role: g[0].Role}
		base := SummaryRow{Capability: string(c.Capability), Provider: string(c.Provider), Model: c.Model, Voice: c.Voice}
		for _, s := range append(slices.Clone(AllSuites), SuiteCost) { // human ratings appear as their gate rows
			rows = append(rows, suiteRows(base, s, g)...)
		}
		if c.Capability == CapE2E {
			continue
		}
		gates := EvalGates(c.Capability, c.Role, Aggregate(c, g))
		for _, gt := range gates {
			row := base
			row.Suite, row.Metric, row.P50, row.P95 = gateHome[gt.Name], gt.Name, num(gt.Actual), num(gt.Actual)
			row.Gate, row.Pass = gateLabel(gt), strconv.FormatBool(gt.Pass)
			rows = append(rows, row)
		}
		if len(gates) > 0 {
			row := base
			row.Suite, row.Metric, row.Pass = "gates", "all_gates", strconv.FormatBool(AllPass(gates))
			rows = append(rows, row)
		}
	}
	return rows
}

func suiteRows(base SummaryRow, suite string, g []Result) []SummaryRow {
	vals := map[string][]float64{}
	var attempts, errs int
	for _, r := range g {
		if r.Suite != suite {
			continue
		}
		attempts++
		if r.ErrorKind != "" {
			errs++
			continue
		}
		for k, v := range r.Values {
			if suite == SuiteConcurrency {
				if k == "step" {
					continue
				}
				k += "@" + strconv.Itoa(int(r.Values["step"]))
			}
			vals[k] = append(vals[k], v)
		}
	}
	if attempts == 0 {
		return nil
	}
	names := make([]string, 0, len(vals))
	for k := range vals {
		names = append(names, k)
	}
	sort.Strings(names)
	var rows []SummaryRow
	for _, k := range names {
		row := base
		row.Suite, row.Metric = suite, k
		row.P50, row.P95 = num(Percentile(vals[k], 50)), num(Percentile(vals[k], 95))
		rows = append(rows, row)
	}
	if suite != SuiteConcurrency && suite != SuiteCost {
		row := base
		row.Suite, row.Metric = suite, "error_pct"
		row.P50 = num(100 * float64(errs) / float64(attempts))
		row.P95 = row.P50
		rows = append(rows, row)
	}
	return rows
}

// WriteResults writes dir/results.jsonl (one line per attempt, as recorded) and dir/summary.csv
// (SummaryColumns). The directory is created; existing files are replaced.
func WriteResults(dir string, rs []Result) error {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("bench: %w", err)
	}
	if err := writeFile(filepath.Join(dir, ResultsFile), func(w *bufio.Writer) error {
		enc := json.NewEncoder(w)
		for _, r := range rs {
			if err := enc.Encode(r); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		return err
	}
	return writeFile(filepath.Join(dir, SummaryFile), func(w *bufio.Writer) error {
		if err := writeCSVRecord(w, SummaryColumns); err != nil {
			return err
		}
		for _, row := range SummaryRows(rs) {
			if err := writeCSVRecord(w, row.record()); err != nil {
				return err
			}
		}
		return nil
	})
}

func writeCSVRecord(w *bufio.Writer, rec []string) error {
	for i, f := range rec {
		if i > 0 {
			_ = w.WriteByte(',')
		}
		if strings.ContainsAny(f, ",\"\n\r") {
			f = `"` + strings.ReplaceAll(f, `"`, `""`) + `"`
		}
		if _, err := w.WriteString(f); err != nil {
			return err
		}
	}
	return w.WriteByte('\n')
}

func writeFile(path string, fill func(*bufio.Writer) error) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // operator-chosen output directory (--out)
	if err != nil {
		return fmt.Errorf("bench: %w", err)
	}
	defer func() {
		if cerr := f.Close(); err == nil && cerr != nil {
			err = fmt.Errorf("bench: %w", cerr)
		}
	}()
	w := bufio.NewWriter(f)
	if err := fill(w); err != nil {
		return fmt.Errorf("bench: %s: %w", filepath.Base(path), err)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("bench: %s: %w", filepath.Base(path), err)
	}
	return nil
}

// EnvOption adds optional content to WriteRecommendationEnv.
type EnvOption func(*envOpts)

type envOpts struct {
	sttBackups, llmBackups []ScoredCandidate
	ttsVendors             []provider.Name
	concurrency            map[string]int
	maxTokens              int
	temperature            float64
	haveLimits             bool
}

// WithBackups lists the manual backups (STT, LLM) with their numbers as comment lines.
func WithBackups(stt, llm []ScoredCandidate) EnvOption {
	return func(o *envOpts) { o.sttBackups, o.llmBackups = stt, llm }
}

// WithTTSVendors sets the TTS vendors used by the recommended character voices; without it (or empty)
// both TTS switches keep their default `true`.
func WithTTSVendors(v []provider.Name) EnvOption { return func(o *envOpts) { o.ttsVendors = v } }

// WithConcurrency sets PROVIDER_CONCURRENCY_<key> values; keys are VENDOR_CAPABILITY (e.g. CARTESIA_TTS).
func WithConcurrency(c map[string]int) EnvOption { return func(o *envOpts) { o.concurrency = c } }

// WithLLMLimits sets LLM_MAX_OUTPUT_TOKENS and LLM_TEMPERATURE.
func WithLLMLimits(maxTokens int, temperature float64) EnvOption {
	return func(o *envOpts) { o.maxTokens, o.temperature, o.haveLimits = maxTokens, temperature, true }
}

var (
	concurrencyKeyRE = regexp.MustCompile(`^(OPENAI|DEEPSEEK|ELEVENLABS|CARTESIA)_(STT|LLM|TTS|EVALUATION)$`)
	envValueRE       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/+@-]*$`)
)

var (
	sttVendors = []provider.Name{provider.OpenAI, provider.ElevenLabs, provider.Cartesia}
	llmVendors = []provider.Name{provider.OpenAI, provider.DeepSeek}
)

// oneLine keeps a comment on one line (no way to smuggle a variable into the file).
func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.NewReplacer("\r", " ", "\n", " ").Replace(s)), " ")
}

// WriteRecommendationEnv writes the RECOMMENDED provider.env (spec 8.10.5 item 1; deploy/config/provider.env
// is copied from it by hand): exactly one `*_STT_ENABLED=true` and one `*_LLM_ENABLED=true` for the
// recommended candidates (a capability without a recommendation gets commented lines and no `true`),
// the TTS switches, the model per vendor (sttModels, llmModels, evalModels; an evaluation model
// defaults to the LLM model of that vendor), LLM_MAX_OUTPUT_TOKENS/LLM_TEMPERATURE (WithLLMLimits),
// ROLLING_SUMMARY_LLM_MODEL (the recommended LLM model), PROVIDER_CONCURRENCY_<VENDOR>_<CAP> with
// concurrencyComments as `# limit plan akun:` lines, and the backups as `# backup manual: <flag>=true`
// comments with their numbers. The header says the final vendor choice is the product owner's, by
// hand. No API key is ever written; a value that is not a plain token is rejected.
func WriteRecommendationEnv(path string, stt, llm *ScoredCandidate, sttModels, llmModels, evalModels map[provider.Name]string,
	concurrencyComments map[string]string, opts ...EnvOption) error {
	var o envOpts
	for _, f := range opts {
		f(&o)
	}
	var b strings.Builder
	b.WriteString("# provider.env - REKOMENDASI hasil `kotoki-cli provider-bench`; keputusan akhir vendor aktif manual pemilik produk.\n")
	b.WriteString("# Tepat satu *_STT_ENABLED dan tepat satu *_LLM_ENABLED bernilai true. Tanpa kunci API; tanpa fallback vendor otomatis.\n")

	models := func(caps string, vendors []provider.Name, m map[provider.Name]string, rec *ScoredCandidate, suffix string) error {
		for _, v := range vendors {
			val := m[v]
			if rec != nil && rec.Provider == v {
				val = rec.Model
			}
			if val == "" && suffix == "EVALUATION" && llmRec(llm, v) != "" {
				val = llmRec(llm, v)
			}
			if val == "" {
				continue
			}
			if !envValueRE.MatchString(val) {
				return fmt.Errorf("bench: model %q of %s %s is not a plain token", val, v, caps)
			}
			fmt.Fprintf(&b, "%s_%s_MODEL=%s\n", v, suffix, val)
		}
		return nil
	}
	flags := func(name string, vendors []provider.Name, suffix string, rec *ScoredCandidate, backups []ScoredCandidate) {
		fmt.Fprintf(&b, "\n# %s\n", name)
		if rec == nil {
			fmt.Fprintf(&b, "# tidak ada rekomendasi: %s\n", NoQualifierNote)
			for _, v := range vendors {
				fmt.Fprintf(&b, "# %s_%s_ENABLED=  (belum ditentukan)\n", v, suffix)
			}
			return
		}
		fmt.Fprintf(&b, "# rekomendasi: %s\n", oneLine(rec.Key()))
		for _, v := range vendors {
			fmt.Fprintf(&b, "%s_%s_ENABLED=%t\n", v, suffix, rec.Provider == v)
		}
		for _, bk := range backups {
			fmt.Fprintf(&b, "# backup manual: %s_%s_ENABLED=true (%s)\n", bk.Provider, suffix, oneLine(backupNumbers(bk)))
		}
	}

	flags("STT", sttVendors, "STT", stt, o.sttBackups)
	if err := models("STT", sttVendors, sttModels, stt, "STT"); err != nil {
		return err
	}
	flags("LLM + EVALUATION (vendor yang sama)", llmVendors, "LLM", llm, o.llmBackups)
	if err := models("LLM", llmVendors, llmModels, llm, "LLM"); err != nil {
		return err
	}
	if err := models("EVALUATION", llmVendors, evalModels, nil, "EVALUATION"); err != nil {
		return err
	}
	if llm != nil {
		if !envValueRE.MatchString(llm.Model) {
			return fmt.Errorf("bench: model %q is not a plain token", llm.Model)
		}
		fmt.Fprintf(&b, "ROLLING_SUMMARY_LLM_MODEL=%s\n", llm.Model)
	}
	if o.haveLimits {
		if o.maxTokens <= 0 || o.temperature < 0 {
			return errors.New("bench: LLM_MAX_OUTPUT_TOKENS must be > 0 and LLM_TEMPERATURE >= 0")
		}
		fmt.Fprintf(&b, "LLM_MAX_OUTPUT_TOKENS=%d\nLLM_TEMPERATURE=%s\n", o.maxTokens, strconv.FormatFloat(o.temperature, 'f', -1, 64))
	}

	b.WriteString("\n# TTS (vendor per karakter, D-015): sakelar sesuai vendor di character-voices.json\n")
	used := map[provider.Name]bool{}
	for _, v := range o.ttsVendors {
		used[v] = true
	}
	if len(used) == 0 {
		b.WriteString("# belum ada voice karakter terpilih: kedua sakelar tetap default true\n")
		used[provider.ElevenLabs], used[provider.Cartesia] = true, true
	}
	fmt.Fprintf(&b, "ELEVENLABS_TTS_ENABLED=%t\nCARTESIA_TTS_ENABLED=%t\n", used[provider.ElevenLabs], used[provider.Cartesia])

	keys := map[string]bool{}
	for k := range o.concurrency {
		keys[k] = true
	}
	for k := range concurrencyComments {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		if !concurrencyKeyRE.MatchString(k) {
			return fmt.Errorf("bench: concurrency key %q is not VENDOR_CAPABILITY", k)
		}
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	if len(sorted) > 0 {
		b.WriteString("\n# konkurensi per pod (PROVIDER_CONCURRENCY_<VENDOR>_<CAP>)\n")
	}
	for _, k := range sorted {
		n, hasVal := o.concurrency[k]
		cm, hasCm := concurrencyComments[k]
		switch {
		case hasVal && n <= 0:
			return fmt.Errorf("bench: PROVIDER_CONCURRENCY_%s must be > 0", k)
		case hasVal:
			if hasCm {
				fmt.Fprintf(&b, "# limit plan akun: %s\n", oneLine(cm))
			}
			fmt.Fprintf(&b, "PROVIDER_CONCURRENCY_%s=%d\n", k, n)
		default:
			fmt.Fprintf(&b, "# PROVIDER_CONCURRENCY_%s (belum diisi) - limit plan akun: %s\n", k, oneLine(cm))
		}
	}
	return writeFileString(path, b.String())
}

func llmRec(llm *ScoredCandidate, v provider.Name) string {
	if llm != nil && llm.Provider == v {
		return llm.Model
	}
	return ""
}

func backupNumbers(s ScoredCandidate) string {
	m := s.Metrics
	switch s.Capability {
	case provider.CapSTT:
		return fmt.Sprintf("%s; CER native %s, learner %s, final P95 %s ms", s.Key(), fmtNum(m.CERNative), fmtNum(m.CERLearner), fmtNum(m.p95(MetricSTTFinalMs)))
	case provider.CapLLM:
		return fmt.Sprintf("%s; kualitas %s, %s USD/menit, kalimat pertama P95 %s ms, JSON setelah repair %s", s.Key(),
			fmtNum(m.QualityMean), fmtNum(s.cost()), fmtNum(m.p95(MetricLLMFirstSentMs)), fmtNum(m.JSONAfterRepairOK))
	}
	return s.Key()
}

func writeFileString(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("bench: %w", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		return fmt.Errorf("bench: %w", err)
	}
	return nil
}

type voiceJSON struct {
	ProviderName string `json:"provider_name"`
	ModelName    string `json:"model_name"`
	VoiceID      string `json:"voice_id"`
}

type characterVoiceJSON struct {
	Code string `json:"code"`
	voiceJSON
	Fallback *voiceJSON `json:"fallback,omitempty"`
	Notes    []string   `json:"notes,omitempty"`
}

// WriteCharacterVoices writes character-voices.json: per character provider_name, model_name and
// voice_id of the main voice (empty when none qualified), `fallback{...}` when there is one, and the
// notes. It is the input for seed/env/<env>/0900_voice_ids.sql, filled in by hand (Task 23).
func WriteCharacterVoices(path string, cv []CharacterVoice) error {
	out := struct {
		Characters []characterVoiceJSON `json:"characters"`
	}{Characters: make([]characterVoiceJSON, 0, len(cv))}
	for _, c := range cv {
		e := characterVoiceJSON{Code: c.Code, voiceJSON: voiceJSON(c.Primary), Notes: c.Notes}
		if c.Fallback != nil {
			fb := voiceJSON(*c.Fallback)
			e.Fallback = &fb
		}
		out.Characters = append(out.Characters, e)
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return fmt.Errorf("bench: %w", err)
	}
	return writeFileString(path, string(b)+"\n")
}
