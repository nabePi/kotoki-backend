package golden

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"os"
	"sort"

	"github.com/nabePi/kotoki-backend/internal/evaluation"
)

// regressionMargin is how many MAE points a dimension may lose against the baseline (spec 10.12).
const regressionMargin = 2.0

// defaultExportCount is the number of findings a rater judges for the precision metric.
const defaultExportCount = 100

// RatingItem is one finding handed to a rater to judge valid or not. It carries only the quoted
// text of the finding (original/corrected), not the transcript.
type RatingItem struct {
	ID        int    `json:"id"`
	SampleID  string `json:"sample_id"`
	TurnSeq   int    `json:"turn_seq"`
	Type      string `json:"finding_type"`
	Dimension string `json:"dimension"`
	Severity  string `json:"severity"`
	Original  string `json:"original"`
	Corrected string `json:"corrected"`
}

// ExportFindingsForRating draws n findings (all of them when fewer) from the report's findings, in an
// order that depends only on seed.
func ExportFindingsForRating(r Report, n int, seed int64) []RatingItem {
	pool := append([]RatedFinding(nil), r.Findings...)
	sort.SliceStable(pool, func(i, j int) bool {
		if pool[i].SampleID != pool[j].SampleID {
			return pool[i].SampleID < pool[j].SampleID
		}
		if pool[i].Run != pool[j].Run {
			return pool[i].Run < pool[j].Run
		}
		return pool[i].Key.TurnSeq < pool[j].Key.TurnSeq
	})
	n = min(max(n, 0), len(pool))
	perm := rand.New(rand.NewSource(seed)).Perm(len(pool)) //nolint:gosec // deterministic sampling, not security
	items := make([]RatingItem, 0, n)
	for i := range n {
		f := pool[perm[i]]
		items = append(items, RatingItem{ID: i + 1, SampleID: f.SampleID, TurnSeq: f.Key.TurnSeq, Type: f.Key.Type,
			Dimension: f.Key.Dimension, Severity: f.Key.Severity, Original: f.Key.Original, Corrected: f.Key.Corrected})
	}
	return items
}

// PrecisionFromRatings is the fraction of findings the raters judged valid; 0 for no ratings.
func PrecisionFromRatings(valid []bool) float64 {
	n := 0
	for _, v := range valid {
		if v {
			n++
		}
	}
	return Recall(n, len(valid))
}

// WithPrecision returns the report with the manual precision metric filled from the raters' verdicts
// and the verdict recomputed.
func (r Report) WithPrecision(valid []bool) Report {
	out := r
	out.Metrics = make(map[string]MetricResult, len(r.Metrics))
	for k, v := range r.Metrics {
		out.Metrics[k] = v
	}
	if len(valid) > 0 {
		out.Metrics[MetricPrecision] = metricMin(PrecisionFromRatings(valid), thrPrecision)
	}
	out.Verdict = out.verdict()
	return out
}

// Compare lists the dimensions whose MAE got worse than the baseline by more than 2 points.
func Compare(cur, baseline Report) []string {
	var out []string
	for _, d := range evaluation.LLMDimensions {
		key := MetricMAE + "." + string(d)
		c, okC := cur.Metrics[key]
		b, okB := baseline.Metrics[key]
		if okC && okB && c.Value-b.Value > regressionMargin {
			out = append(out, fmt.Sprintf("%s: MAE %.2f vs baseline %.2f (+%.2f > %.0f)", d, c.Value, b.Value, c.Value-b.Value, regressionMargin))
		}
	}
	return out
}

// MainDeps are what Main needs from its caller: Build returns the evaluator under test and a cleanup
// (the cli builds it from DATABASE_URL and PROVIDER_MODE).
type MainDeps struct {
	Build func(ctx context.Context) (Evaluator, func(), error)
}

// Main is MainContext with a background context.
func Main(args []string, stdout, stderr io.Writer, deps MainDeps) int {
	return MainContext(context.Background(), args, stdout, stderr, deps)
}

// MainContext is the body of `kotoki-cli eval-golden`. It returns the exit code: 0 when the report was
// written and the verdict is not FAIL and nothing regressed against the baseline, 1 otherwise, 2 for a
// usage error. ctx bounds the whole run.
func MainContext(ctx context.Context, args []string, stdout, stderr io.Writer, deps MainDeps) int {
	fs := flag.NewFlagSet("eval-golden", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dataset", "", "dataset directory (manifest.json + *.jsonl)")
	runs := fs.Int("runs", 3, "evaluations per sample (stability needs at least 2)")
	out := fs.String("out", "report.json", "report path")
	baseline := fs.String("baseline", "", "report.json of the active configuration, to detect MAE regressions")
	export := fs.String("export-findings", "", "write findings to rate manually (precision) to this JSON file")
	exportN := fs.Int("export-count", defaultExportCount, "number of findings to export")
	seed := fs.Int64("seed", 1, "seed of the findings sample")
	ratings := fs.String("ratings", "", "JSON array of true/false verdicts of the exported findings (fills PRECISION)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 || *dir == "" || *runs < 1 || *exportN < 0 {
		_, _ = fmt.Fprintln(stderr, "eval-golden: --dataset is required, --runs must be >= 1 and no positional arguments are accepted")
		return 2
	}
	fail := func(err error) int {
		_, _ = fmt.Fprintf(stderr, "eval-golden: %v\n", err)
		return 1
	}

	ds, err := LoadDataset(*dir)
	if err != nil {
		return fail(err)
	}
	ev, cleanup, err := deps.Build(ctx)
	if err != nil {
		return fail(err)
	}
	if cleanup != nil {
		defer cleanup()
	}
	rep, err := Run(ctx, ds, ev, *runs)
	if err != nil {
		return fail(err)
	}
	if *ratings != "" {
		var valid []bool
		if err := readJSONFile(*ratings, &valid); err != nil {
			return fail(fmt.Errorf("ratings: %w", err))
		}
		rep = rep.WithPrecision(valid)
	}
	if err := writeJSONFile(*out, rep); err != nil {
		return fail(err)
	}
	if *export != "" {
		if err := writeJSONFile(*export, ExportFindingsForRating(rep, *exportN, *seed)); err != nil {
			return fail(err)
		}
	}

	var regressions []string
	if *baseline != "" {
		var base Report
		if err := readJSONFile(*baseline, &base); err != nil {
			return fail(fmt.Errorf("baseline: %w", err))
		}
		regressions = Compare(rep, base)
	}
	printSummary(stdout, rep, regressions)
	if rep.Verdict == VerdictFail || len(regressions) > 0 {
		return 1
	}
	return 0
}

func printSummary(w io.Writer, rep Report, regressions []string) {
	_, _ = fmt.Fprintf(w, "dataset: %s (%s), %d scored samples, %d insufficient, %d runs\n",
		rep.RubricVersion, rep.DatasetStatus, rep.Samples, rep.InsufficientCount, rep.Runs)
	keys := make([]string, 0, len(rep.Metrics))
	for k := range rep.Metrics {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		m := rep.Metrics[k]
		_, _ = fmt.Fprintf(w, "  %-28s %8.2f  %s\n", k, m.Value, m.Status)
	}
	for _, v := range rep.Violations {
		_, _ = fmt.Fprintf(w, "  dataset: %s\n", v)
	}
	for _, r := range regressions {
		_, _ = fmt.Fprintf(w, "  regression: %s\n", r)
	}
	_, _ = fmt.Fprintf(w, "verdict: %s\n", rep.Verdict)
}

func readJSONFile(path string, v any) error {
	b, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func writeJSONFile(path string, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false) // keep ">=" readable
	if err := enc.Encode(v); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o600)
}
