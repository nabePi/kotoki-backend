package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nabePi/kotoki-backend/internal/content"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
)

type contentRepo struct{ q *sqlcgen.Queries }

var (
	_ content.Repository   = (*contentRepo)(nil)
	_ content.TxRepository = (*contentRepo)(nil)
)

// NewContentRepo returns the read-only content repository over the pool.
func NewContentRepo(pool *pgxpool.Pool) content.Repository { return &contentRepo{q: sqlcgen.New(pool)} }

// Content returns the transaction's content repository (reads and writes).
func (t *Tx) Content() content.TxRepository { return &contentRepo{q: t.Queries()} }

func contentErr(err error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return content.ErrNotFound
	}
	return errs.Wrap(err, errs.CodeInternalError, what+" failed")
}

func numToFloat(n pgtype.Numeric) float64 {
	f, err := n.Float64Value()
	if err != nil || !f.Valid {
		return 0
	}
	return f.Float64
}

func floatToNum(v float64) pgtype.Numeric {
	var n pgtype.Numeric
	_ = n.Scan(fmt.Sprintf("%.1f", v)) // numeric(2,1): the string form is always valid
	return n
}

func jsonList[T any](raw []byte) []T {
	var out []T
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out) // the column is a JSON array by CHECK; a bad row yields an empty list and fails validation
	}
	if out == nil {
		out = []T{}
	}
	return out
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // plain strings, numbers and structs of them cannot fail to marshal
	}
	return b
}

func toTopic(r sqlcgen.Topic) content.Topic {
	return content.Topic{ID: r.ID, Code: r.Code, Name: r.Name, NameJA: r.NameJa, Description: r.Description, IconKey: fromText(r.IconKey),
		SortOrder: int(r.SortOrder), Status: content.EntityStatus(r.Status), CreatedAt: fromTsz(r.CreatedAt), UpdatedAt: fromTsz(r.UpdatedAt)}
}

func toScenario(r sqlcgen.Scenario) content.Scenario {
	return content.Scenario{ID: r.ID, TopicID: r.TopicID, Code: r.Code, Name: r.Name, NameJA: r.NameJa, Description: r.Description,
		SortOrder: int(r.SortOrder), Status: content.EntityStatus(r.Status), CreatedAt: fromTsz(r.CreatedAt), UpdatedAt: fromTsz(r.UpdatedAt)}
}

func toScenarioVersion(r sqlcgen.ScenarioVersion) content.ScenarioVersion {
	var vp content.VisualPrompt
	_ = json.Unmarshal(r.VisualPrompt, &vp)
	return content.ScenarioVersion{
		ID: r.ID, ScenarioID: r.ScenarioID, Version: int(r.Version), Status: content.VersionStatus(r.Status), DifficultyLevel: int(r.DifficultyLevel),
		RecommendedLevelMin: numToFloat(r.RecommendedLevelMin), RecommendedLevelMax: numToFloat(r.RecommendedLevelMax),
		Situation: r.Situation, AIRole: r.AiRole, UserRole: r.UserRole, Goal: r.Goal,
		Tasks: jsonList[string](r.Tasks), Constraints: jsonList[string](r.Constraints), GrammarTargets: jsonList[string](r.GrammarTargets),
		VocabHints: jsonList[content.VocabHint](r.VocabHints), EvaluationFocus: append([]string{}, r.EvaluationFocus...),
		Formality: content.Formality(r.Formality), OpeningLine: r.OpeningLine, VisualPrompt: vp, Notes: fromText(r.Notes),
		CreatedAt: fromTsz(r.CreatedAt), PublishedAt: fromTszPtr(r.PublishedAt), RetiredAt: fromTszPtr(r.RetiredAt),
	}
}

type promptRow struct {
	ID             uuid.UUID
	PromptConfigID uuid.UUID
	Version        int32
	Status         string
	SystemTemplate string
	UserTemplate   pgtype.Text
	OutputSchema   []byte
	LlmParams      []byte
	Notes          pgtype.Text
	CreatedAt      pgtype.Timestamptz
	PublishedAt    pgtype.Timestamptz
	RetiredAt      pgtype.Timestamptz
	ConfigKey      string
}

func toPromptVersion(r promptRow) (content.PromptVersion, error) {
	pv := content.PromptVersion{
		ID: r.ID, PromptConfigID: r.PromptConfigID, Key: content.PromptKey(r.ConfigKey), Version: int(r.Version), Status: content.VersionStatus(r.Status),
		SystemTemplate: r.SystemTemplate, UserTemplate: fromText(r.UserTemplate), Notes: fromText(r.Notes),
		CreatedAt: fromTsz(r.CreatedAt), PublishedAt: fromTszPtr(r.PublishedAt), RetiredAt: fromTszPtr(r.RetiredAt),
	}
	if len(r.OutputSchema) > 0 {
		pv.OutputSchema = json.RawMessage(r.OutputSchema)
	}
	if len(r.LlmParams) > 0 {
		if err := json.Unmarshal(r.LlmParams, &pv.LLMParams); err != nil {
			return content.PromptVersion{}, errs.Wrap(err, errs.CodeInternalError, "prompt llm_params is not valid")
		}
	}
	return pv, nil
}

func toCharacter(r sqlcgen.Character) (content.Character, error) {
	ch := content.Character{ID: r.ID, Code: r.Code, Status: content.EntityStatus(r.Status), SortOrder: int(r.SortOrder), Name: r.Name,
		AvatarKey: fromText(r.AvatarKey), Persona: r.Persona, GreetingHint: fromText(r.GreetingHint),
		ProviderName: r.ProviderName, ModelName: r.ModelName, VoiceID: r.VoiceID, CreatedAt: fromTsz(r.CreatedAt), UpdatedAt: fromTsz(r.UpdatedAt)}
	if err := json.Unmarshal(r.Voice, &ch.Voice); err != nil {
		return content.Character{}, errs.Wrap(err, errs.CodeInternalError, "character voice is not valid (characters/"+r.Code+")")
	}
	return ch, nil
}

func (r *contentRepo) GetTopic(ctx context.Context, id uuid.UUID) (content.Topic, error) {
	row, err := r.q.GetTopicRow(ctx, id)
	if err != nil {
		return content.Topic{}, contentErr(err, "read topic")
	}
	return toTopic(row), nil
}

func (r *contentRepo) GetTopicByID(ctx context.Context, id uuid.UUID) (content.Topic, error) {
	return r.GetTopic(ctx, id)
}

func (r *contentRepo) GetScenario(ctx context.Context, id uuid.UUID) (content.Scenario, error) {
	row, err := r.q.GetScenarioRow(ctx, id)
	if err != nil {
		return content.Scenario{}, contentErr(err, "read scenario")
	}
	return toScenario(row), nil
}

func (r *contentRepo) GetScenarioVersion(ctx context.Context, id uuid.UUID) (content.ScenarioVersion, error) {
	row, err := r.q.GetScenarioVersionRow(ctx, id)
	if err != nil {
		return content.ScenarioVersion{}, contentErr(err, "read scenario version")
	}
	return toScenarioVersion(row), nil
}

func (r *contentRepo) GetPublishedScenarioVersion(ctx context.Context, scenarioID uuid.UUID) (content.ScenarioVersion, error) {
	row, err := r.q.GetPublishedScenarioVersionRow(ctx, scenarioID)
	if err != nil {
		return content.ScenarioVersion{}, contentErr(err, "read published scenario version")
	}
	return toScenarioVersion(row), nil
}

func (r *contentRepo) GetCharacter(ctx context.Context, id uuid.UUID) (content.Character, error) {
	row, err := r.q.GetCharacterRow(ctx, id)
	if err != nil {
		return content.Character{}, contentErr(err, "read character")
	}
	return toCharacter(row)
}

func (r *contentRepo) GetPromptVersion(ctx context.Context, id uuid.UUID) (content.PromptVersion, error) {
	row, err := r.q.GetPromptVersionRow(ctx, id)
	if err != nil {
		return content.PromptVersion{}, contentErr(err, "read prompt version")
	}
	return toPromptVersion(promptRow(row))
}

func (r *contentRepo) GetPublishedPromptVersion(ctx context.Context, key content.PromptKey) (content.PromptVersion, error) {
	row, err := r.q.GetPublishedPromptVersionRow(ctx, string(key))
	if err != nil {
		return content.PromptVersion{}, contentErr(err, "read published prompt version")
	}
	return toPromptVersion(promptRow(row))
}

func (r *contentRepo) ListTopics(ctx context.Context) ([]content.Topic, error) {
	rows, err := r.q.ListActiveTopicRows(ctx)
	if err != nil {
		return nil, contentErr(err, "list topics")
	}
	out := make([]content.Topic, len(rows))
	for i, row := range rows {
		out[i] = toTopic(row)
	}
	return out, nil
}

func (r *contentRepo) ListScenarios(ctx context.Context, topicID uuid.UUID) ([]content.Scenario, error) {
	rows, err := r.q.ListActiveScenarioRows(ctx, topicID)
	if err != nil {
		return nil, contentErr(err, "list scenarios")
	}
	out := make([]content.Scenario, len(rows))
	for i, row := range rows {
		out[i] = toScenario(row)
	}
	return out, nil
}

func (r *contentRepo) activeCharacters(ctx context.Context) ([]content.Character, error) {
	rows, err := r.q.ListActiveCharacterRows(ctx)
	if err != nil {
		return nil, contentErr(err, "list characters")
	}
	out := make([]content.Character, 0, len(rows))
	for _, row := range rows {
		ch, err := toCharacter(row)
		if err != nil {
			return nil, err
		}
		out = append(out, ch)
	}
	return out, nil
}

func (r *contentRepo) ListCharacters(ctx context.Context) ([]content.Character, error) {
	return r.activeCharacters(ctx)
}

func (r *contentRepo) ListActiveCharacters(ctx context.Context) ([]content.Character, error) {
	return r.activeCharacters(ctx)
}

func (r *contentRepo) ListPublishedScenarioVersions(ctx context.Context) ([]content.ScenarioVersion, error) {
	rows, err := r.q.ListPublishedScenarioVersionRows(ctx)
	if err != nil {
		return nil, contentErr(err, "list published scenario versions")
	}
	out := make([]content.ScenarioVersion, len(rows))
	for i, row := range rows {
		out[i] = toScenarioVersion(row)
	}
	return out, nil
}

func (r *contentRepo) ListScenarioVersions(ctx context.Context, scenarioID uuid.UUID) ([]content.ScenarioVersion, error) {
	rows, err := r.q.ListScenarioVersionRows(ctx, scenarioID)
	if err != nil {
		return nil, contentErr(err, "list scenario versions")
	}
	out := make([]content.ScenarioVersion, len(rows))
	for i, row := range rows {
		out[i] = toScenarioVersion(row)
	}
	return out, nil
}

func (r *contentRepo) promptList(rows []sqlcgen.ListPromptVersionRowsRow) ([]content.PromptVersion, error) {
	out := make([]content.PromptVersion, 0, len(rows))
	for _, row := range rows {
		pv, err := toPromptVersion(promptRow(row))
		if err != nil {
			return nil, err
		}
		out = append(out, pv)
	}
	return out, nil
}

func (r *contentRepo) ListPromptVersions(ctx context.Context, key content.PromptKey) ([]content.PromptVersion, error) {
	rows, err := r.q.ListPromptVersionRows(ctx, string(key))
	if err != nil {
		return nil, contentErr(err, "list prompt versions")
	}
	return r.promptList(rows)
}

func (r *contentRepo) ListPublishedPromptVersions(ctx context.Context) ([]content.PromptVersion, error) {
	rows, err := r.q.ListPublishedPromptVersionRows(ctx)
	if err != nil {
		return nil, contentErr(err, "list published prompt versions")
	}
	out := make([]content.PromptVersion, 0, len(rows))
	for _, row := range rows {
		pv, err := toPromptVersion(promptRow(row))
		if err != nil {
			return nil, err
		}
		out = append(out, pv)
	}
	return out, nil
}

// ---- writes (inside a transaction) -----------------------------------------------------------

func (r *contentRepo) LockParent(ctx context.Context, fam content.Family, parent string) (uuid.UUID, error) {
	switch fam {
	case content.FamilyScenarios:
		id, err := uuid.Parse(parent)
		if err != nil {
			return uuid.Nil, content.ErrNotFound
		}
		got, err := r.q.LockScenarioParent(ctx, id)
		if err != nil {
			return uuid.Nil, contentErr(err, "lock scenario")
		}
		return got, nil
	case content.FamilyPrompts:
		got, err := r.q.LockPromptConfigParent(ctx, strings.ToUpper(parent))
		if err != nil {
			return uuid.Nil, contentErr(err, "lock prompt config")
		}
		return got, nil
	}
	return uuid.Nil, content.ErrNotFound
}

func (r *contentRepo) LockVersion(ctx context.Context, fam content.Family, parentID, versionID uuid.UUID) (content.VersionState, error) {
	switch fam {
	case content.FamilyScenarios:
		row, err := r.q.LockScenarioVersionRow(ctx, sqlcgen.LockScenarioVersionRowParams{ID: versionID, ScenarioID: parentID})
		if err != nil {
			return content.VersionState{}, contentErr(err, "lock scenario version")
		}
		return content.VersionState{ID: row.ID, Version: int(row.Version), Status: content.VersionStatus(row.Status)}, nil
	case content.FamilyPrompts:
		row, err := r.q.LockPromptVersionRow(ctx, sqlcgen.LockPromptVersionRowParams{ID: versionID, PromptConfigID: parentID})
		if err != nil {
			return content.VersionState{}, contentErr(err, "lock prompt version")
		}
		return content.VersionState{ID: row.ID, Version: int(row.Version), Status: content.VersionStatus(row.Status)}, nil
	}
	return content.VersionState{}, content.ErrNotFound
}

func (r *contentRepo) NextVersionNumber(ctx context.Context, fam content.Family, parentID uuid.UUID) (int, error) {
	var n int32
	var err error
	switch fam {
	case content.FamilyScenarios:
		n, err = r.q.NextScenarioVersionNumber(ctx, parentID)
	case content.FamilyPrompts:
		n, err = r.q.NextPromptVersionNumber(ctx, parentID)
	default:
		return 0, content.ErrNotFound
	}
	if err != nil {
		return 0, contentErr(err, "next version number")
	}
	return int(n), nil
}

func (r *contentRepo) InsertScenarioVersion(ctx context.Context, v content.ScenarioVersion) error {
	err := r.q.InsertScenarioVersionDraft(ctx, sqlcgen.InsertScenarioVersionDraftParams{
		ID: v.ID, ScenarioID: v.ScenarioID, Version: int32(v.Version), DifficultyLevel: int16(v.DifficultyLevel), //nolint:gosec // validated 1–5 / small
		RecommendedLevelMin: floatToNum(v.RecommendedLevelMin), RecommendedLevelMax: floatToNum(v.RecommendedLevelMax),
		Situation: v.Situation, AiRole: v.AIRole, UserRole: v.UserRole, Goal: v.Goal,
		Tasks: mustJSON(nonNil(v.Tasks)), Constraints: mustJSON(nonNil(v.Constraints)), GrammarTargets: mustJSON(nonNil(v.GrammarTargets)),
		VocabHints: mustJSON(nonNilHints(v.VocabHints)), EvaluationFocus: nonNil(v.EvaluationFocus), Formality: string(v.Formality),
		OpeningLine: v.OpeningLine, VisualPrompt: mustJSON(nonNilVisual(v.VisualPrompt)), Notes: textPtr(v.Notes), CreatedAt: tsz(v.CreatedAt),
	})
	if err != nil {
		return errs.Wrap(err, errs.CodeInternalError, "insert scenario version failed")
	}
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilHints(h []content.VocabHint) []content.VocabHint {
	if h == nil {
		return []content.VocabHint{}
	}
	return h
}

func nonNilVisual(v content.VisualPrompt) content.VisualPrompt {
	if v.StarterPhrasesJA == nil {
		v.StarterPhrasesJA = []string{}
	}
	return v
}

func (r *contentRepo) InsertPromptVersion(ctx context.Context, v content.PromptVersion) error {
	var schema []byte
	if len(v.OutputSchema) > 0 {
		schema = v.OutputSchema
	}
	err := r.q.InsertPromptVersionDraft(ctx, sqlcgen.InsertPromptVersionDraftParams{
		ID: v.ID, PromptConfigID: v.PromptConfigID, Version: int32(v.Version), SystemTemplate: v.SystemTemplate, //nolint:gosec // small positive
		UserTemplate: textPtr(v.UserTemplate), OutputSchema: schema, LlmParams: mustJSON(v.LLMParams), Notes: textPtr(v.Notes), CreatedAt: tsz(v.CreatedAt),
	})
	if err != nil {
		return errs.Wrap(err, errs.CodeInternalError, "insert prompt version failed")
	}
	return nil
}

func (r *contentRepo) RetirePublished(ctx context.Context, fam content.Family, parentID uuid.UUID, at time.Time) (*uuid.UUID, error) {
	var id uuid.UUID
	var err error
	switch fam {
	case content.FamilyScenarios:
		id, err = r.q.RetirePublishedScenarioVersion(ctx, sqlcgen.RetirePublishedScenarioVersionParams{ScenarioID: parentID, At: tsz(at)})
	case content.FamilyPrompts:
		id, err = r.q.RetirePublishedPromptVersion(ctx, sqlcgen.RetirePublishedPromptVersionParams{PromptConfigID: parentID, At: tsz(at)})
	default:
		return nil, content.ErrNotFound
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeInternalError, "retire published version failed")
	}
	return &id, nil
}

func (r *contentRepo) execOne(n int64, err error, what string) error {
	if err != nil {
		return errs.Wrap(err, errs.CodeInternalError, what+" failed")
	}
	if n != 1 {
		return content.ErrNotFound
	}
	return nil
}

func (r *contentRepo) SetPublished(ctx context.Context, fam content.Family, versionID uuid.UUID, at time.Time) error {
	switch fam {
	case content.FamilyScenarios:
		n, err := r.q.PublishScenarioVersion(ctx, sqlcgen.PublishScenarioVersionParams{ID: versionID, At: tsz(at)})
		return r.execOne(n, err, "publish scenario version")
	case content.FamilyPrompts:
		n, err := r.q.PublishPromptVersion(ctx, sqlcgen.PublishPromptVersionParams{ID: versionID, At: tsz(at)})
		return r.execOne(n, err, "publish prompt version")
	}
	return content.ErrNotFound
}

func (r *contentRepo) SetRetired(ctx context.Context, fam content.Family, versionID uuid.UUID, at time.Time) error {
	switch fam {
	case content.FamilyScenarios:
		n, err := r.q.RetireScenarioVersion(ctx, sqlcgen.RetireScenarioVersionParams{ID: versionID, At: tsz(at)})
		return r.execOne(n, err, "retire scenario version")
	case content.FamilyPrompts:
		n, err := r.q.RetirePromptVersion(ctx, sqlcgen.RetirePromptVersionParams{ID: versionID, At: tsz(at)})
		return r.execOne(n, err, "retire prompt version")
	}
	return content.ErrNotFound
}

func (r *contentRepo) DeleteDraft(ctx context.Context, fam content.Family, versionID uuid.UUID) error {
	switch fam {
	case content.FamilyScenarios:
		n, err := r.q.DeleteScenarioVersion(ctx, versionID)
		return r.execOne(n, err, "delete scenario version")
	case content.FamilyPrompts:
		n, err := r.q.DeletePromptVersion(ctx, versionID)
		return r.execOne(n, err, "delete prompt version")
	}
	return content.ErrNotFound
}

func (r *contentRepo) SetEntityStatus(ctx context.Context, entity string, id uuid.UUID, to content.EntityStatus, at time.Time) error {
	switch entity {
	case "topics":
		n, err := r.q.SetTopicStatus(ctx, sqlcgen.SetTopicStatusParams{ID: id, Status: string(to), At: tsz(at)})
		return r.execOne(n, err, "set topic status")
	case "scenarios":
		n, err := r.q.SetScenarioStatus(ctx, sqlcgen.SetScenarioStatusParams{ID: id, Status: string(to), At: tsz(at)})
		return r.execOne(n, err, "set scenario status")
	}
	return content.ErrNotFound
}

func (r *contentRepo) LockCharacter(ctx context.Context, id uuid.UUID) (content.Character, error) {
	row, err := r.q.LockCharacterRow(ctx, id)
	if err != nil {
		return content.Character{}, contentErr(err, "lock character")
	}
	return toCharacter(row)
}

func (r *contentRepo) UpdateCharacter(ctx context.Context, c content.Character, at time.Time) error {
	n, err := r.q.UpdateCharacterRow(ctx, sqlcgen.UpdateCharacterRowParams{
		ID: c.ID, Status: string(c.Status), SortOrder: int32(c.SortOrder), Name: c.Name, AvatarKey: textPtr(c.AvatarKey), //nolint:gosec // validated ≥ 0 small
		Persona: c.Persona, GreetingHint: textPtr(c.GreetingHint), ProviderName: c.ProviderName, ModelName: c.ModelName, VoiceID: c.VoiceID,
		Voice: mustJSON(c.Voice), At: tsz(at),
	})
	return r.execOne(n, err, "update character")
}
