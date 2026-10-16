package fake

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

//go:embed fixtures/*.json
var embeddedFixtures embed.FS

const (
	scorePlaceholder = `"{{score}}"`
	defaultEvalModel = "fake-evaluation"
	minFakeScore     = 50
	fakeScoreSpread  = 41 // 50..90
)

var schemaNameRE = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// Evaluation is the fake provider.EvaluationProvider: it answers with the fixture named
// after StructuredRequest.SchemaName. A fixture may use "{{score}}" for deterministic
// per-request numbers.
type Evaluation struct {
	name provider.Name
	cfg  CapConfig
	conf Config
	deps Deps
	dec  *decider
	jit  *jitterSource
}

var _ provider.EvaluationProvider = (*Evaluation)(nil)

// NewEvaluation builds a fake evaluation provider.
func NewEvaluation(name provider.Name, c CapConfig, cfg Config, d Deps) *Evaluation {
	d = d.WithDefaults()
	return &Evaluation{name: name, cfg: c, conf: cfg, deps: d,
		dec: newDecider(d.Seed, name, provider.CapEvaluation, c), jit: newJitterSource(d.Seed, 0x455641)}
}

// Name implements provider.EvaluationProvider.
func (e *Evaluation) Name() provider.Name { return e.name }

func (e *Evaluation) fixture(schema string) ([]byte, error) {
	if e.conf.EvalFixtureDir != "" {
		b, err := os.ReadFile(filepath.Join(e.conf.EvalFixtureDir, schema+".json")) //nolint:gosec // schema is validated against [a-z0-9_]{1,64}
		if err == nil {
			return b, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
	}
	return embeddedFixtures.ReadFile("fixtures/" + schema + ".json")
}

func fakeScore(requestID string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(requestID))
	return minFakeScore + int(h.Sum32()%fakeScoreSpread)
}

// GenerateJSON implements provider.EvaluationProvider.
func (e *Evaluation) GenerateJSON(ctx context.Context, req provider.StructuredRequest) (provider.StructuredResult, error) {
	if !schemaNameRE.MatchString(req.SchemaName) {
		return provider.StructuredResult{}, provider.NewError(e.name, provider.CapEvaluation, provider.KindInvalidRequest, "fake: SchemaName must match [a-z0-9_]{1,64}")
	}
	latency, ferr := oneShot(ctx, e.deps, e.jit, e.dec, e.cfg, e.name, provider.CapEvaluation)
	if ferr != nil {
		return provider.StructuredResult{}, ferr
	}
	raw, err := e.fixture(req.SchemaName)
	if err != nil {
		return provider.StructuredResult{}, provider.NewError(e.name, provider.CapEvaluation, provider.KindInvalidRequest,
			"fake: no fixture for schema "+req.SchemaName)
	}
	body := strings.ReplaceAll(string(raw), scorePlaceholder, strconv.Itoa(fakeScore(req.RequestID)))
	if req.SchemaName == rubricSchemaName {
		body = string(RubricFindings(req, []byte(body)))
	}
	if !json.Valid([]byte(body)) {
		return provider.StructuredResult{}, provider.NewError(e.name, provider.CapEvaluation, provider.KindInvalidResponse,
			fmt.Sprintf("fake: fixture %s is not valid JSON", req.SchemaName))
	}
	model := req.Model
	if model == "" {
		model = defaultEvalModel
	}
	return provider.StructuredResult{
		JSON: json.RawMessage(body), FinishReason: provider.FinishStop, Provider: e.name, Model: model, Latency: latency,
		Usage: provider.TokenUsage{InputTokens: inputRunes(req.Messages), OutputTokens: utf8.RuneCountInString(body)},
	}, nil
}
