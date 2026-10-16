package evaluation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/platform/otel"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

const (
	// consumerName is the consumer column of processed_events: the queue name (spec 10.5 step 10).
	consumerName = events.QueueEvaluation
	// turnWindowPad widens the created_at window around the session for partition pruning.
	turnWindowPad = time.Minute
	// lastErrorMax bounds evaluations.last_error.
	lastErrorMax = 2000
)

// Permanent error codes the service raises (spec 9.7.3).
const (
	codeEntityNotFound     = "ENTITY_NOT_FOUND"
	codeInvariantViolation = "INVARIANT_VIOLATION"
)

// ContentReader is the content read port the evaluation needs; content.Repository satisfies it.
type ContentReader interface {
	GetScenarioVersion(ctx context.Context, id uuid.UUID) (content.ScenarioVersion, error)
	GetTopicByID(ctx context.Context, id uuid.UUID) (content.Topic, error)
	GetScenario(ctx context.Context, id uuid.UUID) (content.Scenario, error)
	GetPublishedPromptVersion(ctx context.Context, key content.PromptKey) (content.PromptVersion, error)
}

// Router yields the active evaluation provider; provider.AsyncRouter satisfies it.
type Router interface {
	Evaluation() provider.EvaluationProvider
}

// Deps are the collaborators of Service. Metrics and Log may be nil; Schemas defaults to a new cache.
type Deps struct {
	Store   Store
	Source  Source
	Content ContentReader
	Router  Router
	Cfg     Config
	Clock   clock.Clock
	IDs     idgen.Generator
	Metrics *Metrics
	Schemas *SchemaCache
	Log     *slog.Logger
}

// Service orchestrates one evaluation per session.completed delivery (spec 10.5).
type Service struct {
	d Deps
}

// NewService returns a Service over d.
func NewService(d Deps) *Service {
	if d.Schemas == nil {
		d.Schemas = NewSchemaCache()
	}
	if d.Log == nil {
		d.Log = slog.Default()
	}
	return &Service{d: d}
}

// Process runs spec 10.5 steps 2-12 for one session.completed delivery. Permanent failures are
// *events.PermanentError; ErrAlreadyProcessed and ErrSkippedDeletedUser are acknowledged by the
// framework; any other error is retried.
func (s *Service) Process(ctx context.Context, env events.Envelope, p events.SessionCompletedV1) (err error) {
	ctx, span := otel.Start(ctx, "evaluation.Process")
	defer otel.End(span, &err)

	info, err := s.d.Source.LoadSession(ctx, p.SessionID)
	if errors.Is(err, ErrSessionNotFound) {
		return events.Permanent(codeEntityNotFound, err)
	}
	if err != nil {
		return err
	}
	if info.UserID != env.UserID {
		return events.Permanent(codeInvariantViolation, errors.New("evaluation: session user differs from event user"))
	}
	if info.UserStatus == "DELETED" {
		if err := s.markProcessedOnly(ctx, env); err != nil {
			return err
		}
		return events.ErrSkippedDeletedUser
	}
	if (info.Status != "COMPLETED" && info.Status != "FAILED") || info.StartedAt == nil {
		return s.markProcessedOnly(ctx, env)
	}

	pv, err := s.loadPrompt(ctx)
	if err != nil {
		return err
	}

	claim, err := s.claim(ctx, env, info, pv)
	if err != nil || !claim.Claimed {
		return err
	}

	sv, topic, scenario, err := s.loadContent(ctx, info)
	if err != nil {
		return err
	}

	startedAt := *info.StartedAt
	endedAt := p.EndedAt
	if info.EndedAt != nil {
		endedAt = *info.EndedAt
	}
	from := startedAt
	if info.StartingAt != nil {
		from = *info.StartingAt
	}
	turns, err := s.d.Source.LoadTurns(ctx, info.ID, from.Add(-turnWindowPad), endedAt.Add(turnWindowPad))
	if err != nil {
		return err
	}
	if len(turns) == 0 && p.UserTurnCount > 0 {
		return events.Permanent(string(FailTranscriptUnavailable), errors.New("evaluation: no turns although the session had user turns"))
	}
	stats := ComputeStats(turns, startedAt, endedAt, info.DisconnectedSeconds)

	upd := CompletedUpdate{
		UserTurnCount: stats.UserTurns, UserSpeechSeconds: stats.UserSpeechSeconds, ActiveSeconds: stats.ActiveSeconds,
		ScenarioDifficultyLevel: sv.DifficultyLevel,
	}
	var findings []FindingRow
	if stats.Insufficient(s.d.Cfg.Thresholds()) {
		upd.InsufficientEvidence = true
		upd.SummaryFeedback = InsufficientSummary
	} else {
		res, err := s.evaluate(ctx, info, pv, sv, topic, scenario, turns, stats)
		if err != nil {
			return err
		}
		upd.Scores = Scores{
			Grammar: &res.Output.Scores.Grammar.Score, Vocabulary: &res.Output.Scores.Vocabulary.Score,
			Fluency: &res.Output.Scores.Fluency.Score, Naturalness: &res.Output.Scores.Naturalness.Score,
			TaskAchievement: &res.Output.Scores.TaskAchievement.Score,
		}
		upd.OverallScore = ComputeOverall(upd.Scores)
		lr, ic := res.Output.Signals.ListeningResponse, res.Output.Signals.InteractionComplexity
		upd.ListeningResponseSignal, upd.InteractionComplexitySignal = &lr.Score, &ic.Score
		upd.DimensionConfidence = map[string]float64{
			string(DimGrammar): res.Output.Scores.Grammar.Confidence, string(DimVocabulary): res.Output.Scores.Vocabulary.Confidence,
			string(DimFluency): res.Output.Scores.Fluency.Confidence, string(DimNaturalness): res.Output.Scores.Naturalness.Confidence,
			string(DimTaskAchievement): res.Output.Scores.TaskAchievement.Confidence,
			"LISTENING_RESPONSE":       lr.Confidence, "INTERACTION_COMPLEXITY": ic.Confidence,
		}
		upd.DimensionComments = map[string]string{}
		for d, ds := range res.Output.Dimensions() {
			upd.DimensionComments[string(d)] = ds.Comment
		}
		upd.SummaryFeedback = res.Output.Summary
		upd.Strengths, upd.FocusAreas, upd.Recommendations = res.Output.Strengths, res.Output.FocusAreas, res.Output.Recommendations
		prov, model := string(res.Provider), res.Model
		in, out := res.InputTokens, res.OutputTokens
		upd.Provider, upd.Model, upd.LLMCallCount = &prov, &model, res.CallCount
		upd.InputTokens, upd.OutputTokens = &in, &out
		findings = s.rowsOf(res.Findings)
	}

	now := s.d.Clock.Now().UTC()
	upd.CompletedAt = now
	ev := events.NewEvaluationCompleted(s.d.IDs.New(), now, events.ProducerWorker, info.UserID, completedPayload(claim.ID, info.ID, RubricVersionOf(pv), endedAt, upd, findings))

	err = s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		if err := tx.Processed().MarkProcessed(ctx, consumerName, env); err != nil {
			return err
		}
		ok, err := tx.Evaluations().Complete(ctx, claim.ID, upd)
		if err != nil {
			return err
		}
		if !ok {
			return events.ErrAlreadyProcessed
		}
		if err := tx.Evaluations().ReplaceFindings(ctx, claim.ID, info.UserID, info.ID, findings); err != nil {
			return err
		}
		return tx.Outbox().Enqueue(ctx, ev)
	})
	if err != nil {
		return err
	}
	s.d.Metrics.ObserveLatency(now.Sub(endedAt).Seconds())
	return nil
}

// markProcessedOnly records the event as processed without any other effect.
func (s *Service) markProcessedOnly(ctx context.Context, env events.Envelope) error {
	return s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		return tx.Processed().MarkProcessed(ctx, consumerName, env)
	})
}

// loadPrompt reads the published EVALUATION prompt version and checks what the run needs from it;
// any gap is a configuration error raised before the claim.
func (s *Service) loadPrompt(ctx context.Context) (content.PromptVersion, error) {
	pv, err := s.d.Content.GetPublishedPromptVersion(ctx, content.PromptEvaluation)
	if errors.Is(err, content.ErrNotFound) {
		return pv, events.Permanent(string(FailConfigurationError), fmt.Errorf("evaluation: no published EVALUATION prompt: %w", err))
	}
	if err != nil {
		return pv, err
	}
	if pv.UserTemplate == nil || len(pv.OutputSchema) == 0 || string(pv.OutputSchema) == "null" {
		return pv, events.Permanent(string(FailConfigurationError), errors.New("evaluation: EVALUATION prompt lacks user_template or output_schema"))
	}
	if _, err := s.d.Schemas.Get(pv.ID, pv.OutputSchema); err != nil {
		return pv, events.Permanent(string(FailConfigurationError), fmt.Errorf("evaluation: output_schema does not compile: %w", err))
	}
	return pv, nil
}

// claim is the short transaction of spec 10.5 step 4. An evaluation that is already COMPLETED
// only gets its event recorded as processed (Claimed false; ErrAlreadyProcessed when it already was).
func (s *Service) claim(ctx context.Context, env events.Envelope, info SessionInfo, pv content.PromptVersion) (ClaimResult, error) {
	var res ClaimResult
	err := s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		res, err = tx.Evaluations().Claim(ctx, ClaimInput{
			ID: s.d.IDs.New(), SessionID: info.ID, UserID: info.UserID, PromptVersionID: pv.ID,
			SourceEventID: env.EventID, RubricVersion: RubricVersionOf(pv),
		})
		if err != nil {
			return err
		}
		if !res.Claimed {
			return tx.Processed().MarkProcessed(ctx, consumerName, env)
		}
		return nil
	})
	if err != nil {
		return ClaimResult{}, err
	}
	return res, nil
}

// loadContent reads the scenario content; a missing row is a permanent failure.
func (s *Service) loadContent(ctx context.Context, info SessionInfo) (content.ScenarioVersion, content.Topic, content.Scenario, error) {
	notFound := func(err error) error {
		if errors.Is(err, content.ErrNotFound) {
			return events.Permanent(codeEntityNotFound, err)
		}
		return err
	}
	sv, err := s.d.Content.GetScenarioVersion(ctx, info.ScenarioVersionID)
	if err != nil {
		return sv, content.Topic{}, content.Scenario{}, notFound(err)
	}
	topic, err := s.d.Content.GetTopicByID(ctx, info.TopicID)
	if err != nil {
		return sv, topic, content.Scenario{}, notFound(err)
	}
	sc, err := s.d.Content.GetScenario(ctx, info.ScenarioID)
	if err != nil {
		return sv, topic, sc, notFound(err)
	}
	return sv, topic, sc, nil
}

// evaluate renders the prompt and runs the LLM sequence (spec 10.5 steps 5-7).
func (s *Service) evaluate(ctx context.Context, info SessionInfo, pv content.PromptVersion, sv content.ScenarioVersion,
	topic content.Topic, sc content.Scenario, turns []TurnRow, stats Stats) (LLMResult, error) {
	data := BuildPromptData(PromptInput{
		TopicName: topic.Name, ScenarioName: sc.Name, CharacterName: characterName(info.ConfigSnapshot),
		SV: sv, SessionAdaptive: info.AdaptiveDifficulty, RubricVersion: RubricVersionOf(pv),
		Stats: stats, Transcript: BuildTranscript(turns, s.d.Cfg.TranscriptMaxChars),
	})
	rendered, err := prompt.Render(pv.Template(), data)
	if err != nil {
		return LLMResult{}, events.Permanent(string(FailConfigurationError), err)
	}
	refs := make([]TurnRef, 0, len(turns))
	for _, t := range turns {
		if t.UserText != nil {
			refs = append(refs, TurnRef{ID: t.ID, Seq: t.Seq, UserText: *t.UserText})
		}
	}
	ev := NewLLMEvaluator(s.d.Router.Evaluation(), s.d.Schemas, s.d.Metrics, s.d.Cfg.MinFindingConfidence)
	return ev.Evaluate(ctx, LLMInput{SessionID: info.ID, PV: pv, Rendered: rendered, Turns: refs})
}

// characterName reads config_snapshot.character.name (D-014); empty when absent.
func characterName(snapshot json.RawMessage) string {
	var c struct {
		Character struct {
			Name string `json:"name"`
		} `json:"character"`
	}
	if len(snapshot) == 0 || json.Unmarshal(snapshot, &c) != nil {
		return ""
	}
	return c.Character.Name
}

// rowsOf orders the findings (MAJOR, MINOR, INFO; STRENGTH last; then turn_seq, then dimension) and
// numbers them from 1 (spec 10.5 step 8).
func (s *Service) rowsOf(fs []Finding) []FindingRow {
	sorted := slices.Clone(fs)
	slices.SortStableFunc(sorted, func(a, b Finding) int {
		if sa, sb := a.Type == FindingStrength, b.Type == FindingStrength; sa != sb {
			if sa {
				return 1
			}
			return -1
		}
		if d := severityRank(b.Severity) - severityRank(a.Severity); d != 0 {
			return d
		}
		switch {
		case a.TurnSeq == nil && b.TurnSeq != nil:
			return 1
		case a.TurnSeq != nil && b.TurnSeq == nil:
			return -1
		case a.TurnSeq != nil && b.TurnSeq != nil && *a.TurnSeq != *b.TurnSeq:
			return *a.TurnSeq - *b.TurnSeq
		}
		switch {
		case a.Dimension < b.Dimension:
			return -1
		case a.Dimension > b.Dimension:
			return 1
		}
		return 0
	})
	rows := make([]FindingRow, len(sorted))
	for i, f := range sorted {
		rows[i] = FindingRow{ID: s.d.IDs.New(), Ordinal: i + 1, Finding: f}
	}
	return rows
}

// completedPayload builds the evaluation.completed v1 payload (spec 9.5.2, 10.9).
func completedPayload(evalID, sessionID uuid.UUID, rubric string, endedAt time.Time, u CompletedUpdate, fs []FindingRow) events.EvaluationCompletedV1 {
	counts := map[string]events.SeverityCounts{}
	for _, f := range fs {
		if f.Type == FindingStrength {
			continue
		}
		c := counts[string(f.Dimension)]
		switch f.Severity {
		case SeverityMajor:
			c.Major++
		case SeverityMinor:
			c.Minor++
		default:
			c.Info++
		}
		counts[string(f.Dimension)] = c
	}
	var lr, ic *events.EvalSignal
	if u.ListeningResponseSignal != nil {
		lr = &events.EvalSignal{Score: *u.ListeningResponseSignal, Confidence: u.DimensionConfidence["LISTENING_RESPONSE"]}
	}
	if u.InteractionComplexitySignal != nil {
		ic = &events.EvalSignal{Score: *u.InteractionComplexitySignal, Confidence: u.DimensionConfidence["INTERACTION_COMPLEXITY"]}
	}
	dimConf := map[string]float64{}
	for _, d := range LLMDimensions {
		if c, ok := u.DimensionConfidence[string(d)]; ok {
			dimConf[string(d)] = c
		}
	}
	return events.EvaluationCompletedV1{
		EvaluationID: evalID, SessionID: sessionID, RubricVersion: rubric, InsufficientEvidence: u.InsufficientEvidence,
		Scores: events.EvalScores{
			Grammar: u.Scores.Grammar, Vocabulary: u.Scores.Vocabulary, Fluency: u.Scores.Fluency,
			Naturalness: u.Scores.Naturalness, TaskAchievement: u.Scores.TaskAchievement,
		},
		DimensionConfidence: dimConf,
		Signals:             events.EvalSignals{ListeningResponse: lr, InteractionComplexity: ic},
		OverallScore:        u.OverallScore, SessionEndedAt: endedAt, ScenarioDifficultyLevel: u.ScenarioDifficultyLevel,
		UserTurnCount: u.UserTurnCount, UserSpeechSeconds: u.UserSpeechSeconds,
		FindingCounts: counts, CompletedAt: u.CompletedAt,
	}
}

// RecordFailure is the OnFailure side (spec 10.5, 10.10): a retryable failure moves the row to
// PENDING; a final one to FAILED with exactly one evaluation.failed event and no MarkProcessed (so a
// DLQ replay can retry). It is best effort and only logs its own errors.
func (s *Service) RecordFailure(ctx context.Context, env events.Envelope, cause error, final bool) {
	p, err := events.DecodePayload[events.SessionCompletedV1](env)
	if err != nil {
		s.d.Log.WarnContext(ctx, "evaluation failure not recorded: undecodable payload", "event_id", env.EventID)
		return
	}
	msg := truncateRunes(cause.Error(), lastErrorMax)
	err = s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		if !final {
			return tx.Evaluations().MarkPending(ctx, p.SessionID, msg)
		}
		code := FailureCodeOf(cause)
		row, changed, err := tx.Evaluations().MarkFailed(ctx, p.SessionID, code, msg)
		if err != nil || !changed {
			return err
		}
		now := s.d.Clock.Now().UTC()
		return tx.Outbox().Enqueue(ctx, events.NewEvaluationFailed(s.d.IDs.New(), now, events.ProducerWorker, row.UserID, events.EvaluationFailedV1{
			EvaluationID: row.ID, SessionID: row.SessionID, FailureCode: string(code), AttemptCount: row.AttemptCount, FailedAt: now,
		}))
	})
	if err != nil {
		s.d.Log.ErrorContext(ctx, "evaluation failure not recorded", "session_id", p.SessionID, "final", final, "error", err)
	}
}

func truncateRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// FailureCodeOf maps the error that ended an evaluation to evaluations.failure_code.
func FailureCodeOf(err error) FailureCode {
	var perm *events.PermanentError
	if errors.As(err, &perm) {
		switch c := FailureCode(perm.Code); c {
		case FailTranscriptUnavailable, FailConfigurationError:
			return c
		}
		return FailInternalError
	}
	var pe *provider.Error
	if errors.As(err, &pe) {
		return FailProviderUnavailable
	}
	if errors.Is(err, ErrInvalidModelOutput) {
		return FailInvalidModelOutput
	}
	return FailInternalError
}
