package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/prompt"
	"github.com/nabePi/kotoki-backend/internal/provider"
)

// Consumer constants (spec 11.5.1).
const (
	consumerTimeout = 120 * time.Second
	embedTimeout    = 5 * time.Second
	embedDimensions = 1536

	// maxLLMCalls is the per-delivery ceiling: the main call plus one repair (D-017).
	maxLLMCalls = 2
	// repairPreviousMaxRunes bounds the previous output echoed in the repair message.
	repairPreviousMaxRunes = 8000

	// Codes of events.PermanentError raised by the consumer.
	codeInvariantViolation = "INVARIANT_VIOLATION"
	codeEntityNotFound     = "ENTITY_NOT_FOUND"
	codeConfiguration      = "CONFIGURATION_ERROR"
	codeInvalidModelOutput = "INVALID_MODEL_OUTPUT"

	taskMemoryExtraction = "MEMORY_EXTRACTION"
	promptKey            = "MEMORY_EXTRACTION"
)

// ErrPromptNotFound is returned by PromptSource when no PUBLISHED version of the prompt exists.
var ErrPromptNotFound = errors.New("memory: prompt not found")

// Provider is the part of provider.AsyncRouter the extraction uses. GenerateJSON must be one
// attempt on the active evaluation vendor (AsyncRouter.Evaluation()), never the retrying
// AsyncRouter.GenerateJSON: the repair call is the only second attempt (D-017).
type Provider interface {
	GenerateJSON(ctx context.Context, req provider.StructuredRequest) (provider.StructuredResult, error)
	Embed(ctx context.Context, req provider.EmbeddingRequest) (provider.EmbeddingResult, error)
}

// SessionInfo is what the extraction needs of the finished session.
type SessionInfo struct {
	UserID       uuid.UUID
	EndedAt      time.Time // zero when the session has no ended_at
	ScenarioName string
}

// Source reads the transcript side of an extraction outside any transaction.
type Source interface {
	// UserDeleted reports users.status = 'DELETED'; a user without a row counts as deleted.
	UserDeleted(ctx context.Context, userID uuid.UUID) (bool, error)
	// Session returns the session; an error wrapping ErrNotFound when it does not exist.
	Session(ctx context.Context, sessionID uuid.UUID) (SessionInfo, error)
	// Turns returns the COMPLETED and INTERRUPTED turns of the session.
	Turns(ctx context.Context, sessionID uuid.UUID) ([]TurnRow, error)
	// Existing returns at most limit existing non-episodic memories of the user as prompt lines.
	Existing(ctx context.Context, userID uuid.UUID, limit int) ([]ExistingMemoryLine, error)
}

// ProfileSettings reads the memory switch of the profile, creating the profile on first use.
type ProfileSettings interface {
	EnsureMemoryEnabled(ctx context.Context, userID uuid.UUID) (bool, error)
}

// PromptSource returns the PUBLISHED version of a prompt and its id; an error wrapping
// ErrPromptNotFound when there is none.
type PromptSource interface {
	Published(ctx context.Context, key string) (prompt.Template, uuid.UUID, error)
}

// MemVer invalidates the memory context cached for the sessions of the users (redisstore
// MemVerBumper).
type MemVer interface {
	Bump(ctx context.Context, userIDs []uuid.UUID) error
}

// ConsumerDeps are the collaborators of Consumer. Log and Metrics may be nil.
type ConsumerDeps struct {
	Store    Store
	Source   Source
	Settings ProfileSettings
	Prompts  PromptSource
	Provider Provider
	MemVer   MemVer
	Applier  *Applier
	Clock    clock.Clock
	IDs      idgen.Generator
	Log      *slog.Logger
	Metrics  *Metrics
}

// Consumer is the queue handler of session.completed for memory extraction. It satisfies
// amqp.Handler (spec 9.7.1) structurally, so this package imports events only, never amqp.
type Consumer struct{ d ConsumerDeps }

// NewConsumer returns the handler that extracts long-term memory from finished sessions.
func NewConsumer(d ConsumerDeps) *Consumer {
	if d.Log == nil {
		d.Log = slog.New(slog.DiscardHandler)
	}
	return &Consumer{d: d}
}

// Queue is the queue name and the consumer name in processed_events.
func (c *Consumer) Queue() string { return events.QueueMemory }

// SupportedEvents maps each handled event type to the highest version understood.
func (c *Consumer) SupportedEvents() map[string]int {
	return map[string]int{events.SessionCompleted: 1}
}

// Timeout bounds one Handle call.
func (c *Consumer) Timeout() time.Duration { return consumerTimeout }

// OnFailure only logs; the log never carries memory text.
func (c *Consumer) OnFailure(ctx context.Context, d events.Delivery, cause error, final bool) {
	c.d.Log.WarnContext(ctx, "memory extraction failed",
		slog.String("event_id", d.Envelope.EventID.String()), slog.Int("retry_count", d.RetryCount),
		slog.Bool("final", final), slog.String("error", cause.Error()))
}

// Handle runs the extraction flow of spec 11.5.1. The expensive work (LLM, embedding) happens
// outside any transaction; the final transaction is short and holds the idempotency row.
func (c *Consumer) Handle(ctx context.Context, d events.Delivery) error {
	start := c.d.Clock.Now()
	result := "error"
	defer func() { c.d.Metrics.Extraction(result, c.d.Clock.Now().Sub(start)) }()

	env := d.Envelope
	p, err := events.DecodePayload[events.SessionCompletedV1](env)
	if err != nil {
		return err
	}
	userID := env.UserID

	deleted, err := c.d.Source.UserDeleted(ctx, userID)
	if err != nil {
		return err
	}
	if deleted {
		if err := c.markOnly(ctx, env); err != nil {
			return err
		}
		result = "skipped"
		return events.ErrSkippedDeletedUser
	}

	enabled, err := c.d.Settings.EnsureMemoryEnabled(ctx, userID)
	if err != nil {
		return err
	}
	if !enabled {
		result = "skipped"
		return c.markOnly(ctx, env)
	}

	sess, err := c.d.Source.Session(ctx, p.SessionID)
	switch {
	case errors.Is(err, ErrNotFound):
		return events.Permanent(codeEntityNotFound, err)
	case err != nil:
		return err
	}
	if sess.UserID != userID {
		c.d.Log.ErrorContext(ctx, "memory security_mismatch",
			slog.String("event_id", env.EventID.String()), slog.String("reason", "session belongs to another user"))
		return events.Permanent(codeInvariantViolation, errors.New("memory: session belongs to another user"))
	}

	turns, err := c.d.Source.Turns(ctx, p.SessionID)
	if err != nil {
		return err
	}
	transcript, validSeqs, userTurns := BuildTranscript(turns, TranscriptMaxRunes)
	if userTurns < 3 { // skip rule G4
		result = "skipped"
		return c.markOnly(ctx, env)
	}

	existing, err := c.d.Source.Existing(ctx, userID, MaxExistingMemories)
	if err != nil {
		return err
	}
	tpl, pvID, err := c.d.Prompts.Published(ctx, promptKey)
	switch {
	case errors.Is(err, ErrPromptNotFound):
		return events.Permanent(codeConfiguration, err)
	case err != nil:
		return err
	}
	if tpl.User == "" || len(tpl.OutputSchema) == 0 {
		return events.Permanent(codeConfiguration, errors.New("memory: MEMORY_EXTRACTION needs user_template and output_schema"))
	}
	ended := sess.EndedAt
	if ended.IsZero() {
		ended = env.OccurredAt
	}
	rendered, err := prompt.Render(tpl, MemoryExtractionPromptData{
		SessionDate: ended.In(jakarta()).Format("2006-01-02"), ScenarioTitle: sess.ScenarioName,
		ExistingMemories: existing, Transcript: transcript,
	})
	if err != nil {
		return events.Permanent(codeConfiguration, err)
	}

	out, res, err := c.extract(ctx, p.SessionID, tpl, rendered)
	if err != nil {
		return err
	}
	cands := ValidateCandidates(out, validSeqs, p.SessionID, c.d.Metrics)

	if err := c.embed(ctx, p.SessionID, cands, pvID); err != nil {
		return err
	}

	var outcomes []Outcome
	err = c.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		if err := tx.Processed().MarkProcessed(ctx, events.QueueMemory, env); err != nil {
			return err
		}
		var err error
		if outcomes, err = c.d.Applier.ApplyAllOutcomes(ctx, tx, userID, cands); err != nil {
			return err
		}
		ev := events.NewMemoryExtracted(c.d.IDs.New(), c.d.Clock.Now(), events.ProducerWorker, userID,
			extractedPayload(p.SessionID, pvID, res, outcomes, c.d.Clock.Now()))
		return tx.Outbox().Enqueue(ctx, ev)
	})
	if err != nil {
		return err
	}
	result = "ok"
	c.bump(ctx, env, userID, outcomes)
	return nil
}

// markOnly records the event as processed without any domain write.
func (c *Consumer) markOnly(ctx context.Context, env events.Envelope) error {
	return c.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		return tx.Processed().MarkProcessed(ctx, events.QueueMemory, env)
	})
}

// bump invalidates the cached memory of the user. The data is committed, so a failure here only
// delays the effect until the cache entry expires; retrying the delivery could not repair it (the
// retry would find the event processed).
func (c *Consumer) bump(ctx context.Context, env events.Envelope, userID uuid.UUID, outcomes []Outcome) {
	if err := c.d.MemVer.Bump(ctx, []uuid.UUID{userID}); err != nil {
		c.d.Log.WarnContext(ctx, "memory memver bump failed",
			slog.String("event_id", env.EventID.String()), slog.String("error", err.Error()))
		return
	}
	c.d.Log.InfoContext(ctx, "memory extracted",
		slog.String("event_id", env.EventID.String()), slog.Int("candidates_applied", len(outcomes)))
}

// extract runs the call sequence of spec 11.5.1 step 8: call 1; when its output is invalid
// (including INVALID_RESPONSE) one repair call; a second rejection is permanent. Any other
// provider error is returned as it is after one call, so the delivery retry policy decides.
func (c *Consumer) extract(ctx context.Context, sessionID uuid.UUID, tpl prompt.Template, r prompt.Rendered) (ExtractionOutput, provider.StructuredResult, error) {
	base := []provider.Message{
		{Role: provider.RoleSystem, Content: r.System},
		{Role: provider.RoleUser, Content: r.User},
	}
	msgs := base
	var lastErr error
	for call := 1; call <= maxLLMCalls; call++ {
		res, err := c.d.Provider.GenerateJSON(ctx, structuredRequest(sessionID, tpl, msgs))
		previous := ""
		switch {
		case err != nil && provider.KindOf(err) == provider.KindInvalidResponse:
			lastErr = fmt.Errorf("%w: the response was empty, truncated or not valid JSON", ErrInvalidOutput)
		case err != nil:
			return ExtractionOutput{}, provider.StructuredResult{}, err
		default:
			previous = string(res.JSON)
			out, perr := ParseOutput(res.JSON, tpl.OutputSchema)
			if perr == nil {
				return out, res, nil
			}
			if !errors.Is(perr, ErrInvalidOutput) {
				return ExtractionOutput{}, provider.StructuredResult{}, events.Permanent(codeConfiguration, perr)
			}
			lastErr = perr
		}
		msgs = repairMessages(base, previous, lastErr)
	}
	return ExtractionOutput{}, provider.StructuredResult{}, events.Permanent(codeInvalidModelOutput, lastErr)
}

func structuredRequest(sessionID uuid.UUID, tpl prompt.Template, msgs []provider.Message) provider.StructuredRequest {
	req := provider.StructuredRequest{
		Task:            taskMemoryExtraction,
		Messages:        msgs,
		SchemaName:      fmt.Sprintf("kotoki_memory_extraction_v%d", tpl.Version),
		JSONSchema:      tpl.OutputSchema,
		MaxOutputTokens: tpl.Params.MaxOutputTokensOr(2000),
		Temperature:     tpl.Params.TemperatureOr(0),
		RequestID:       taskMemoryExtraction + ":" + sessionID.String(),
	}
	if tpl.Params.Seed != nil {
		seed := *tpl.Params.Seed
		req.Seed = &seed
	}
	return req
}

// repairMessages appends the rejected output (cut to 8,000 runes; omitted when there is none) and
// the validation error to base.
func repairMessages(base []provider.Message, previous string, cause error) []provider.Message {
	out := make([]provider.Message, 0, len(base)+2)
	out = append(out, base...)
	if previous != "" {
		if utf8.RuneCountInString(previous) > repairPreviousMaxRunes {
			previous = string([]rune(previous)[:repairPreviousMaxRunes])
		}
		out = append(out, provider.Message{Role: provider.RoleAssistant, Content: previous})
	}
	return append(out, provider.Message{Role: provider.RoleUser,
		Content: fmt.Sprintf("The previous output was invalid: %s. Return corrected JSON only.", cause.Error())})
}

// embed fills Embedding, EmbeddingModel and PromptVersionID of every candidate with one batch
// call. A failure is returned as it is: the delivery is retried as a whole.
func (c *Consumer) embed(ctx context.Context, sessionID uuid.UUID, cands []Candidate, pvID uuid.UUID) error {
	if len(cands) == 0 {
		return nil
	}
	texts := make([]string, len(cands))
	for i, cd := range cands {
		texts[i] = cd.ContentJA
	}
	ectx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	res, err := c.d.Provider.Embed(ectx, provider.EmbeddingRequest{
		Texts: texts, Dimensions: embedDimensions, RequestID: taskMemoryExtraction + ":" + sessionID.String(),
	})
	if err != nil {
		return err
	}
	if len(res.Vectors) != len(cands) {
		return fmt.Errorf("memory: embedding returned %d vectors for %d texts", len(res.Vectors), len(cands))
	}
	for i := range cands {
		if len(res.Vectors[i]) != embedDimensions {
			return fmt.Errorf("memory: embedding %d has %d dimensions", i, len(res.Vectors[i]))
		}
		cands[i].Embedding = res.Vectors[i]
		cands[i].EmbeddingModel = res.Model
		id := pvID
		cands[i].PromptVersionID = &id
	}
	return nil
}

func extractedPayload(sessionID, pvID uuid.UUID, res provider.StructuredResult, outcomes []Outcome, now time.Time) events.MemoryExtractedV1 {
	p := events.MemoryExtractedV1{
		SessionID: sessionID, PromptVersionID: pvID, ExtractedAt: now,
		Provider: strings.ToUpper(string(res.Provider)), Model: strings.ToUpper(res.Model),
		CategoryCounts: map[string]int{},
	}
	for _, o := range outcomes {
		switch o.Action {
		case ActionCreated:
			p.CreatedMemoryIDs = append(p.CreatedMemoryIDs, o.ID)
			p.CategoryCounts[string(o.Category)]++
		case ActionReinforced:
			p.UpdatedMemoryIDs = append(p.UpdatedMemoryIDs, o.ID)
		case ActionSuperseded:
			p.CreatedMemoryIDs = append(p.CreatedMemoryIDs, o.ID)
			p.SupersededMemoryIDs = append(p.SupersededMemoryIDs, o.SupersededID)
			p.CategoryCounts[string(o.Category)]++
		}
	}
	return p
}

var jakartaLoc = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Jakarta")
	if err != nil {
		return time.FixedZone("WIB", 7*60*60)
	}
	return loc
}()

func jakarta() *time.Location { return jakartaLoc }
