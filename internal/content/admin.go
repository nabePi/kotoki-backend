package content

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
)

// AdminDeps are the collaborators of AdminService.
type AdminDeps struct {
	Store       Store
	Validator   ValidatorConfig
	Clock       clock.Clock
	IDs         idgen.Generator
	Invalidator Invalidator                      // cache of this process; may be nil
	RequestID   func(ctx context.Context) string // request id for the audit log; may be nil
}

// AdminService is the authoring lifecycle of scenario and prompt versions, the status of topics
// and scenarios, and the direct edit of characters (spec 6.13.1).
type AdminService struct{ d AdminDeps }

// NewAdminService builds the service.
func NewAdminService(d AdminDeps) *AdminService { return &AdminService{d: d} }

func (s *AdminService) requestID(ctx context.Context) string {
	if s.d.RequestID != nil {
		if id := s.d.RequestID(ctx); id != "" {
			return id
		}
	}
	return "unknown"
}

func (s *AdminService) invalidate() {
	if s.d.Invalidator != nil {
		s.d.Invalidator.Invalidate()
	}
}

// validationError is the 422 with the ERROR issues as details (spec 6.13.1).
func validationError(issues []Issue) error {
	var errsOnly []Issue
	for _, is := range issues {
		if is.Severity == SeverityError {
			errsOnly = append(errsOnly, is)
		}
	}
	if len(errsOnly) == 0 {
		return nil
	}
	return errs.WithDetails(errs.New(errs.CodeContentValidationFailed, "content validation failed"), errsOnly)
}

func warnings(issues []Issue) []Issue {
	out := []Issue{}
	for _, is := range issues {
		if is.Severity == SeverityWarning {
			out = append(out, is)
		}
	}
	return out
}

func badBody(msg string) error { return errs.New(errs.CodeValidationFailed, msg) }

func checkFamily(fam Family) error {
	if fam != FamilyScenarios && fam != FamilyPrompts {
		return notFound("content family")
	}
	return nil
}

func targetType(fam Family) string {
	if fam == FamilyPrompts {
		return TargetPrompt
	}
	return TargetScenario
}

func repoErr(err error, what string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrNotFound) {
		return notFound(what)
	}
	var ae *errs.Error
	if errors.As(err, &ae) {
		return err
	}
	return internal(err, what+" failed")
}

func (s *AdminService) audit(ctx context.Context, tx Tx, actor uuid.UUID, action, target, id string, details map[string]any) error {
	if err := tx.Audit().Record(ctx, AuditEntry{AdminUserID: actor, Action: action, TargetType: target, TargetID: id, Details: details, RequestID: s.requestID(ctx)}); err != nil {
		return internal(err, "write audit log failed")
	}
	return nil
}

func (s *AdminService) validator(tx Tx) Validator { return NewValidator(tx.Content(), s.d.Validator) }

func scenarioViewOf(v ScenarioVersion) VersionView {
	return VersionView{ID: v.ID, ParentID: v.ScenarioID, Version: v.Version, Status: v.Status, CreatedAt: v.CreatedAt,
		PublishedAt: v.PublishedAt, RetiredAt: v.RetiredAt, Scenario: &v}
}

func promptViewOf(v PromptVersion) VersionView {
	return VersionView{ID: v.ID, ParentID: v.PromptConfigID, Version: v.Version, Status: v.Status, CreatedAt: v.CreatedAt,
		PublishedAt: v.PublishedAt, RetiredAt: v.RetiredAt, Prompt: &v}
}

// loadVersion reads a version by id and checks that it belongs to parentID.
func loadVersion(ctx context.Context, r TxRepository, fam Family, parentID, id uuid.UUID) (VersionView, error) {
	switch fam {
	case FamilyScenarios:
		v, err := r.GetScenarioVersion(ctx, id)
		if err != nil || v.ScenarioID != parentID {
			return VersionView{}, notFound("version")
		}
		return scenarioViewOf(v), nil
	default:
		v, err := r.GetPromptVersion(ctx, id)
		if err != nil || v.PromptConfigID != parentID {
			return VersionView{}, notFound("version")
		}
		return promptViewOf(v), nil
	}
}

// ---- reads ---------------------------------------------------------------------------------------

// ListVersions returns every version of the parent, newest first.
func (s *AdminService) ListVersions(ctx context.Context, fam Family, parent string) ([]VersionView, error) {
	if err := checkFamily(fam); err != nil {
		return nil, err
	}
	var out []VersionView
	err := s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		r := tx.Content()
		parentID, err := r.LockParent(ctx, fam, parent)
		if err != nil {
			return repoErr(err, "parent")
		}
		out = []VersionView{}
		if fam == FamilyScenarios {
			list, err := r.ListScenarioVersions(ctx, parentID)
			if err != nil {
				return repoErr(err, "list versions")
			}
			for _, v := range list {
				out = append(out, scenarioViewOf(v))
			}
			return nil
		}
		list, err := r.ListPromptVersions(ctx, PromptKey(strings.ToUpper(parent)))
		if err != nil {
			return repoErr(err, "list versions")
		}
		for _, v := range list {
			out = append(out, promptViewOf(v))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b VersionView) int { return b.Version - a.Version })
	return out, nil
}

// GetVersion returns one version of the parent.
func (s *AdminService) GetVersion(ctx context.Context, fam Family, parent string, id uuid.UUID) (VersionView, error) {
	if err := checkFamily(fam); err != nil {
		return VersionView{}, err
	}
	var out VersionView
	err := s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		r := tx.Content()
		parentID, err := r.LockParent(ctx, fam, parent)
		if err != nil {
			return repoErr(err, "parent")
		}
		out, err = loadVersion(ctx, r, fam, parentID, id)
		return err
	})
	return out, err
}

// ValidationReport validates the whole catalog and splits the issues by severity.
func (s *AdminService) ValidationReport(ctx context.Context) (errsOut, warns []Issue, err error) {
	errsOut, warns = []Issue{}, []Issue{}
	err = s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		for _, is := range s.validator(tx).ValidateCatalog(ctx) {
			if is.Severity == SeverityError {
				errsOut = append(errsOut, is)
			} else {
				warns = append(warns, is)
			}
		}
		return nil
	})
	return errsOut, warns, err
}

// GetCharacter returns the full character (admin view), read fresh from the store.
func (s *AdminService) GetCharacter(ctx context.Context, id uuid.UUID) (Character, error) {
	var out Character
	err := s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		var err error
		out, err = tx.Content().GetCharacter(ctx, id)
		return repoErr(err, "character")
	})
	return out, err
}

// ---- drafts ---------------------------------------------------------------------------------------

type scenarioBody struct {
	CopyFrom            *uuid.UUID       `json:"copy_from_version_id"`
	DifficultyLevel     *int             `json:"difficulty_level"`
	RecommendedLevelMin *float64         `json:"recommended_level_min"`
	RecommendedLevelMax *float64         `json:"recommended_level_max"`
	Situation           *string          `json:"situation"`
	AIRole              *string          `json:"ai_role"`
	UserRole            *string          `json:"user_role"`
	Goal                *string          `json:"goal"`
	Tasks               *[]string        `json:"tasks"`
	Constraints         *[]string        `json:"constraints"`
	GrammarTargets      *[]string        `json:"grammar_targets"`
	VocabHints          *[]VocabHint     `json:"vocab_hints"`
	EvaluationFocus     *[]string        `json:"evaluation_focus"`
	Formality           *Formality       `json:"formality"`
	OpeningLine         *string          `json:"opening_line"`
	VisualPrompt        *VisualPrompt    `json:"visual_prompt"`
	Notes               nullable[string] `json:"notes"`
}

type promptBody struct {
	CopyFrom       *uuid.UUID       `json:"copy_from_version_id"`
	SystemTemplate *string          `json:"system_template"`
	UserTemplate   nullable[string] `json:"user_template"`
	OutputSchema   json.RawMessage  `json:"output_schema"`
	LLMParams      json.RawMessage  `json:"llm_params"`
	Notes          nullable[string] `json:"notes"`
}

// nullable distinguishes an absent key (Set false) from an explicit null (Set true, Val nil).
type nullable[T any] struct {
	Set bool
	Val *T
}

func (n *nullable[T]) UnmarshalJSON(b []byte) error {
	n.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		n.Val = nil
		return nil
	}
	var v T
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	n.Val = &v
	return nil
}

func decodeBody(body json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return badBody("invalid request body: " + err.Error())
	}
	if dec.More() {
		return badBody("invalid request body: trailing data")
	}
	return nil
}

func setIf[T any](dst *T, src *T) {
	if src != nil {
		*dst = *src
	}
}

// CreateDraft creates a DRAFT version under the parent. The body holds the fields of the family
// (snake_case column names); copyFrom (or copy_from_version_id in the body) fills in every field
// the body leaves out from an existing version of the same parent.
func (s *AdminService) CreateDraft(ctx context.Context, actor uuid.UUID, fam Family, parent string, body json.RawMessage, copyFrom *uuid.UUID) (VersionView, error) {
	if err := checkFamily(fam); err != nil {
		return VersionView{}, err
	}
	var out VersionView
	err := s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		r := tx.Content()
		parentID, err := r.LockParent(ctx, fam, parent)
		if err != nil {
			return repoErr(err, "parent")
		}
		n, err := r.NextVersionNumber(ctx, fam, parentID)
		if err != nil {
			return repoErr(err, "next version")
		}
		now := s.d.Clock.Now()
		var issues []Issue
		var details map[string]any
		if fam == FamilyScenarios {
			sv, src, iss, err := s.scenarioDraft(ctx, r, parentID, body, copyFrom)
			if err != nil {
				return err
			}
			sv.ID, sv.ScenarioID, sv.Version, sv.Status, sv.CreatedAt = s.d.IDs.New(), parentID, n, VersionDraft, now
			sv.PublishedAt, sv.RetiredAt = nil, nil
			issues = slices.Concat(iss, s.validator(tx).ValidateDraft(ctx, fam, sv))
			if err := validationError(issues); err != nil {
				return err
			}
			if err := r.InsertScenarioVersion(ctx, sv); err != nil {
				return repoErr(err, "insert version")
			}
			out, details = scenarioViewOf(sv), map[string]any{"parent_id": parentID, "version": n, "copy_from_version_id": src}
		} else {
			pv, src, iss, err := s.promptDraft(ctx, r, parentID, body, copyFrom)
			if err != nil {
				return err
			}
			pv.ID, pv.PromptConfigID, pv.Version, pv.Status, pv.CreatedAt = s.d.IDs.New(), parentID, n, VersionDraft, now
			pv.Key = PromptKey(strings.ToUpper(parent))
			pv.PublishedAt, pv.RetiredAt = nil, nil
			issues = slices.Concat(iss, s.validator(tx).ValidateDraft(ctx, fam, pv))
			if err := validationError(issues); err != nil {
				return err
			}
			if err := r.InsertPromptVersion(ctx, pv); err != nil {
				return repoErr(err, "insert version")
			}
			out, details = promptViewOf(pv), map[string]any{"parent_id": parentID, "key": string(pv.Key), "version": n, "copy_from_version_id": src}
		}
		return s.audit(ctx, tx, actor, ActionVersionCreated, targetType(fam), out.ID.String(), details)
	})
	return out, err
}

func (s *AdminService) scenarioDraft(ctx context.Context, r TxRepository, parentID uuid.UUID, body json.RawMessage, copyFrom *uuid.UUID) (ScenarioVersion, *uuid.UUID, []Issue, error) {
	var b scenarioBody
	if err := decodeBody(body, &b); err != nil {
		return ScenarioVersion{}, nil, nil, err
	}
	if copyFrom == nil {
		copyFrom = b.CopyFrom
	}
	var sv ScenarioVersion
	if copyFrom != nil {
		src, err := r.GetScenarioVersion(ctx, *copyFrom)
		if err != nil || src.ScenarioID != parentID {
			return ScenarioVersion{}, nil, nil, notFound("copy_from_version_id")
		}
		sv = src
	}
	setIf(&sv.DifficultyLevel, b.DifficultyLevel)
	setIf(&sv.RecommendedLevelMin, b.RecommendedLevelMin)
	setIf(&sv.RecommendedLevelMax, b.RecommendedLevelMax)
	setIf(&sv.Situation, b.Situation)
	setIf(&sv.AIRole, b.AIRole)
	setIf(&sv.UserRole, b.UserRole)
	setIf(&sv.Goal, b.Goal)
	setIf(&sv.Tasks, b.Tasks)
	setIf(&sv.Constraints, b.Constraints)
	setIf(&sv.GrammarTargets, b.GrammarTargets)
	setIf(&sv.VocabHints, b.VocabHints)
	setIf(&sv.EvaluationFocus, b.EvaluationFocus)
	setIf(&sv.Formality, b.Formality)
	setIf(&sv.OpeningLine, b.OpeningLine)
	setIf(&sv.VisualPrompt, b.VisualPrompt)
	if b.Notes.Set {
		sv.Notes = b.Notes.Val
	}
	return sv, copyFrom, nil, nil
}

func (s *AdminService) promptDraft(ctx context.Context, r TxRepository, parentID uuid.UUID, body json.RawMessage, copyFrom *uuid.UUID) (PromptVersion, *uuid.UUID, []Issue, error) {
	var b promptBody
	if err := decodeBody(body, &b); err != nil {
		return PromptVersion{}, nil, nil, err
	}
	if copyFrom == nil {
		copyFrom = b.CopyFrom
	}
	var pv PromptVersion
	if copyFrom != nil {
		src, err := r.GetPromptVersion(ctx, *copyFrom)
		if err != nil || src.PromptConfigID != parentID {
			return PromptVersion{}, nil, nil, notFound("copy_from_version_id")
		}
		pv = src
	}
	setIf(&pv.SystemTemplate, b.SystemTemplate)
	if b.UserTemplate.Set {
		pv.UserTemplate = b.UserTemplate.Val
	}
	if b.Notes.Set {
		pv.Notes = b.Notes.Val
	}
	var issues []Issue
	if len(b.OutputSchema) > 0 {
		if string(bytes.TrimSpace(b.OutputSchema)) == "null" {
			pv.OutputSchema = nil
		} else {
			pv.OutputSchema = b.OutputSchema
		}
	}
	if len(b.LLMParams) > 0 {
		p, iss := ParseLLMParams(b.LLMParams)
		pv.LLMParams = p
		issues = append(issues, iss...)
	}
	return pv, copyFrom, issues, nil
}

// ---- publish / retire / delete ---------------------------------------------------------------------

// lockVersion locks the parent and then the version (spec 6.13.1 lock order).
func lockVersion(ctx context.Context, r TxRepository, fam Family, parent string, id uuid.UUID) (uuid.UUID, VersionState, error) {
	parentID, err := r.LockParent(ctx, fam, parent)
	if err != nil {
		return uuid.Nil, VersionState{}, repoErr(err, "parent")
	}
	st, err := r.LockVersion(ctx, fam, parentID, id)
	if err != nil {
		return uuid.Nil, VersionState{}, repoErr(err, "version")
	}
	return parentID, st, nil
}

// Publish makes a DRAFT or RETIRED version the PUBLISHED one of its parent, retiring the previous
// one in the same transaction. Publishing the PUBLISHED version again changes nothing.
func (s *AdminService) Publish(ctx context.Context, actor uuid.UUID, fam Family, parent string, versionID uuid.UUID) (PublishResult, error) {
	if err := checkFamily(fam); err != nil {
		return PublishResult{}, err
	}
	var res PublishResult
	changed := false
	err := s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		res, changed = PublishResult{}, false // the transaction may be retried
		r := tx.Content()
		parentID, st, err := lockVersion(ctx, r, fam, parent, versionID)
		if err != nil {
			return err
		}
		if st.Status == VersionPublished {
			res.Version, err = loadVersion(ctx, r, fam, parentID, versionID)
			res.Warnings = []Issue{}
			return err
		}
		issues := s.validator(tx).ValidatePublish(ctx, fam, versionID)
		if err := validationError(issues); err != nil {
			return err
		}
		now := s.d.Clock.Now()
		retired, err := r.RetirePublished(ctx, fam, parentID, now)
		if err != nil {
			return repoErr(err, "retire previous version")
		}
		if err := r.SetPublished(ctx, fam, versionID, now); err != nil {
			return repoErr(err, "publish version")
		}
		details := map[string]any{"parent_id": parentID, "version": st.Version, "from_status": string(st.Status)}
		if retired != nil {
			details["retired_version_id"] = *retired
		}
		if fam == FamilyPrompts {
			details["key"] = strings.ToUpper(parent)
		}
		if err := s.audit(ctx, tx, actor, ActionVersionPublished, targetType(fam), versionID.String(), details); err != nil {
			return err
		}
		res.RetiredVersionID, res.Warnings = retired, warnings(issues)
		res.Version, err = loadVersion(ctx, r, fam, parentID, versionID)
		changed = true
		return err
	})
	if err != nil {
		return PublishResult{}, err
	}
	if changed {
		s.invalidate()
	}
	return res, nil
}

// Retire turns the PUBLISHED version into RETIRED. A prompt cannot be retired without a PUBLISHED
// replacement (publish the replacement instead, which retires this one); a scenario can, and then
// becomes unavailable to learners, which is reported as a warning.
func (s *AdminService) Retire(ctx context.Context, actor uuid.UUID, fam Family, parent string, versionID uuid.UUID) (RetireResult, error) {
	if err := checkFamily(fam); err != nil {
		return RetireResult{}, err
	}
	var res RetireResult
	err := s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		r := tx.Content()
		parentID, st, err := lockVersion(ctx, r, fam, parent, versionID)
		if err != nil {
			return err
		}
		if st.Status != VersionPublished {
			return errs.New(errs.CodeContentInvalidTransition, fmt.Sprintf("only a PUBLISHED version can be retired, this one is %s", st.Status))
		}
		if fam == FamilyPrompts {
			return errs.New(errs.CodeContentVersionRequired, "publish a replacement version instead of retiring the PUBLISHED prompt")
		}
		if err := r.SetRetired(ctx, fam, versionID, s.d.Clock.Now()); err != nil {
			return repoErr(err, "retire version")
		}
		if err := s.audit(ctx, tx, actor, ActionVersionRetired, targetType(fam), versionID.String(), map[string]any{"parent_id": parentID, "version": st.Version}); err != nil {
			return err
		}
		res.Warnings = []Issue{{Severity: SeverityWarning, Entity: "scenarios/" + parentID.String(), Field: "status", Rule: "no_published_version",
			Message: "the scenario has no PUBLISHED version and is unavailable to learners"}}
		res.Version, err = loadVersion(ctx, r, fam, parentID, versionID)
		return err
	})
	if err != nil {
		return RetireResult{}, err
	}
	s.invalidate()
	return res, nil
}

// DeleteDraft removes a DRAFT version.
func (s *AdminService) DeleteDraft(ctx context.Context, actor uuid.UUID, fam Family, parent string, versionID uuid.UUID) error {
	if err := checkFamily(fam); err != nil {
		return err
	}
	return s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		r := tx.Content()
		parentID, st, err := lockVersion(ctx, r, fam, parent, versionID)
		if err != nil {
			return err
		}
		if st.Status != VersionDraft {
			return errs.New(errs.CodeContentInvalidTransition, fmt.Sprintf("only a DRAFT version can be deleted, this one is %s", st.Status))
		}
		if err := r.DeleteDraft(ctx, fam, versionID); err != nil {
			return repoErr(err, "delete version")
		}
		return s.audit(ctx, tx, actor, ActionVersionDeleted, targetType(fam), versionID.String(), map[string]any{"parent_id": parentID, "version": st.Version})
	})
}

// ---- status of topics and scenarios ------------------------------------------------------------------

// SetStatus changes the status of a topic or scenario; characters are edited with UpdateCharacter.
func (s *AdminService) SetStatus(ctx context.Context, actor uuid.UUID, entity string, id uuid.UUID, to EntityStatus) (EntityView, error) {
	if entity != "topics" && entity != "scenarios" {
		return EntityView{}, notFound("content entity")
	}
	if to != StatusActive && to != StatusDisabled && to != StatusArchived {
		return EntityView{}, badBody("status must be ACTIVE, DISABLED or ARCHIVED")
	}
	var out EntityView
	changed := false
	err := s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		out, changed = EntityView{}, false // the transaction may be retried
		r := tx.Content()
		read := func() (EntityView, error) {
			if entity == "topics" {
				t, err := r.GetTopic(ctx, id)
				return EntityView{ID: t.ID, Code: t.Code, Name: t.Name, Status: t.Status}, repoErr(err, "topic")
			}
			sc, err := r.GetScenario(ctx, id)
			return EntityView{ID: sc.ID, Code: sc.Code, Name: sc.Name, Status: sc.Status}, repoErr(err, "scenario")
		}
		cur, err := read()
		if err != nil {
			return err
		}
		if cur.Status == to {
			out = cur
			return nil
		}
		if err := r.SetEntityStatus(ctx, entity, id, to, s.d.Clock.Now()); err != nil {
			return repoErr(err, entity)
		}
		target := TargetTopic
		if entity == "scenarios" {
			target = TargetScenario
		}
		if err := s.audit(ctx, tx, actor, ActionStatusChanged, target, id.String(), map[string]any{"from": string(cur.Status), "to": string(to)}); err != nil {
			return err
		}
		out, changed = cur, true
		out.Status = to
		return nil
	})
	if err != nil {
		return EntityView{}, err
	}
	if changed {
		s.invalidate()
	}
	return out, nil
}

// ---- characters ----------------------------------------------------------------------------------

// UpdateCharacter applies a partial edit under a row lock: merge, validate (an ERROR rolls the
// edit back with 422), write, audit and drop the cache. Only sessions created afterwards see the
// change, because older sessions carry their own snapshot.
func (s *AdminService) UpdateCharacter(ctx context.Context, actor uuid.UUID, id uuid.UUID, p CharacterPatch) (CharacterUpdateResult, error) {
	var res CharacterUpdateResult
	changed := false
	err := s.d.Store.WithinTx(ctx, func(ctx context.Context, tx Tx) error {
		res, changed = CharacterUpdateResult{}, false // the transaction may be retried
		r := tx.Content()
		cur, err := r.LockCharacter(ctx, id)
		if err != nil {
			return repoErr(err, "character")
		}
		merged, fields := mergeCharacter(cur, p)
		issues := s.validator(tx).ValidateCharacter(ctx, merged)
		if err := validationError(issues); err != nil {
			return err
		}
		res.Warnings = warnings(issues)
		if len(fields) == 0 {
			res.Character = cur
			return nil
		}
		merged.UpdatedAt = s.d.Clock.Now()
		if err := r.UpdateCharacter(ctx, merged, merged.UpdatedAt); err != nil {
			return repoErr(err, "update character")
		}
		details := map[string]any{"changed_fields": fields}
		if merged.ProviderName != cur.ProviderName {
			details["provider_name_before"], details["provider_name_after"] = cur.ProviderName, merged.ProviderName
		}
		if err := s.audit(ctx, tx, actor, ActionCharacterUpdated, TargetCharacter, id.String(), details); err != nil {
			return err
		}
		res.Character, changed = merged, true
		return nil
	})
	if err != nil {
		return CharacterUpdateResult{}, err
	}
	if changed {
		s.invalidate()
	}
	return res, nil
}

// mergeCharacter applies p to cur and returns the names of the fields that actually changed. Vendor
// names are normalized to UPPERCASE before the comparison.
func mergeCharacter(cur Character, p CharacterPatch) (Character, []string) {
	m := cur
	var fields []string
	mark := func(name string, differs bool) {
		if differs {
			fields = append(fields, name)
		}
	}
	if p.Status != nil {
		mark("status", *p.Status != m.Status)
		m.Status = *p.Status
	}
	if p.Name != nil {
		mark("name", *p.Name != m.Name)
		m.Name = *p.Name
	}
	if p.AvatarKey != nil {
		mark("avatar_key", !equalPtr(*p.AvatarKey, m.AvatarKey))
		m.AvatarKey = *p.AvatarKey
	}
	if p.Persona != nil {
		mark("persona", *p.Persona != m.Persona)
		m.Persona = *p.Persona
	}
	if p.GreetingHint != nil {
		mark("greeting_hint", !equalPtr(*p.GreetingHint, m.GreetingHint))
		m.GreetingHint = *p.GreetingHint
	}
	if p.SortOrder != nil {
		mark("sort_order", *p.SortOrder != m.SortOrder)
		m.SortOrder = *p.SortOrder
	}
	if p.ProviderName != nil {
		n := NormalizeProviderName(*p.ProviderName)
		mark("provider_name", n != m.ProviderName)
		m.ProviderName = n
	}
	if p.ModelName != nil {
		mark("model_name", *p.ModelName != m.ModelName)
		m.ModelName = *p.ModelName
	}
	if p.VoiceID != nil {
		mark("voice_id", *p.VoiceID != m.VoiceID)
		m.VoiceID = *p.VoiceID
	}
	if p.Voice != nil {
		v := *p.Voice
		if v.Fallback != nil {
			fb := *v.Fallback
			fb.ProviderName = NormalizeProviderName(fb.ProviderName)
			v.Fallback = &fb
		}
		mark("voice", !sameVoice(v, m.Voice))
		m.Voice = v
	}
	return m, fields
}

func equalPtr(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func sameVoice(a, b Voice) bool {
	if a.Gender != b.Gender || a.Accent != b.Accent || a.SpeakingRate != b.SpeakingRate {
		return false
	}
	if a.Fallback == nil || b.Fallback == nil {
		return a.Fallback == b.Fallback
	}
	return *a.Fallback == *b.Fallback
}
