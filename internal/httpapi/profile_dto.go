package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

type preferencesResponse struct {
	SpeechSpeed   string `json:"speech_speed"`
	Formality     string `json:"formality"`
	Dialect       string `json:"dialect"`
	MemoryEnabled bool   `json:"memory_enabled"`
}

// profileResponse is the body of GET and PATCH /profile (spec 5.9.7). The shape of 5.9.7 wins over
// 12.9.1: it adds completed_session_count, total_practice_seconds and last_session_at.
type profileResponse struct {
	UserID                uuid.UUID           `json:"user_id"`
	Nickname              *string             `json:"nickname"`
	SelfDeclaredLevel     *string             `json:"self_declared_level"`
	TargetLevel           *string             `json:"target_level"`
	LearningGoal          *string             `json:"learning_goal"`
	LearningGoalNote      *string             `json:"learning_goal_note"`
	Preferences           preferencesResponse `json:"preferences"`
	OverallLevel          float64             `json:"overall_level"`
	OverallLevelLabel     string              `json:"overall_level_label"`
	CompletedSessionCount int                 `json:"completed_session_count"`
	TotalPracticeSeconds  int64               `json:"total_practice_seconds"`
	LastSessionAt         *string             `json:"last_session_at"`
	OnboardingCompletedAt *string             `json:"onboarding_completed_at"`
	CreatedAt             string              `json:"created_at"`
	UpdatedAt             string              `json:"updated_at"`
}

func newProfileResponse(v learner.ProfileView) profileResponse {
	return profileResponse{
		UserID: v.UserID, Nickname: v.Nickname, SelfDeclaredLevel: v.SelfDeclaredLevel, TargetLevel: v.TargetLevel,
		LearningGoal: v.LearningGoal, LearningGoalNote: v.LearningGoalNote,
		Preferences: preferencesResponse{
			SpeechSpeed: v.PreferredSpeechSpeed, Formality: v.PreferredFormality, Dialect: v.DialectPreference, MemoryEnabled: v.MemoryEnabled,
		},
		OverallLevel: round(v.OverallLevel, 2), OverallLevelLabel: v.OverallLevelLabel,
		CompletedSessionCount: v.CompletedSessionCount, TotalPracticeSeconds: v.TotalPracticeSeconds,
		LastSessionAt: formatTimePtr(v.LastSessionAt), OnboardingCompletedAt: formatTimePtr(v.OnboardingCompletedAt),
		CreatedAt: formatTime(v.CreatedAt), UpdatedAt: formatTime(v.UpdatedAt),
	}
}

// decodeField reads one key of a PATCH body into a Field. The decoder of Handle cannot name the
// field of a type error that comes out of a custom unmarshaler, so the DTOs decode key by key.
func decodeField[T any](name string, raw json.RawMessage, dst *learner.Field[T]) error {
	if raw == nil { // the key is absent
		return nil
	}
	if err := json.Unmarshal(raw, dst); err != nil {
		return FieldError(name, "invalid_type")
	}
	return nil
}

// strictObject decodes b into v (a struct of raw keys) and refuses unknown keys, which the decoder
// of Handle cannot see inside a custom unmarshaler.
func strictObject(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// preferencesPatch is the nested `preferences` object of PATCH /profile.
type preferencesPatch struct {
	SpeechSpeed   learner.Field[string]
	Formality     learner.Field[string]
	Dialect       learner.Field[string]
	MemoryEnabled learner.Field[bool]
}

func (p *preferencesPatch) UnmarshalJSON(b []byte) error {
	var raw struct {
		SpeechSpeed   json.RawMessage `json:"speech_speed"`
		Formality     json.RawMessage `json:"formality"`
		Dialect       json.RawMessage `json:"dialect"`
		MemoryEnabled json.RawMessage `json:"memory_enabled"`
	}
	if err := strictObject(b, &raw); err != nil {
		return err
	}
	return errors.Join(
		decodeField("preferences.speech_speed", raw.SpeechSpeed, &p.SpeechSpeed),
		decodeField("preferences.formality", raw.Formality, &p.Formality),
		decodeField("preferences.dialect", raw.Dialect, &p.Dialect),
		decodeField("preferences.memory_enabled", raw.MemoryEnabled, &p.MemoryEnabled),
	)
}

// patchProfileRequest is the body of PATCH /profile: every field optional, `null` clears a
// nullable one.
type patchProfileRequest struct {
	Nickname          learner.Field[string]
	SelfDeclaredLevel learner.Field[string]
	TargetLevel       learner.Field[string]
	LearningGoal      learner.Field[string]
	LearningGoalNote  learner.Field[string]
	Preferences       learner.Field[preferencesPatch]
}

func (r *patchProfileRequest) UnmarshalJSON(b []byte) error {
	var raw struct {
		Nickname          json.RawMessage `json:"nickname"`
		SelfDeclaredLevel json.RawMessage `json:"self_declared_level"`
		TargetLevel       json.RawMessage `json:"target_level"`
		LearningGoal      json.RawMessage `json:"learning_goal"`
		LearningGoalNote  json.RawMessage `json:"learning_goal_note"`
		Preferences       json.RawMessage `json:"preferences"`
	}
	if err := strictObject(b, &raw); err != nil {
		return err
	}
	var prefsErr error
	if raw.Preferences != nil {
		r.Preferences.Set = true
		if string(raw.Preferences) == "null" {
			r.Preferences.Null = true
		} else if err := json.Unmarshal(raw.Preferences, &r.Preferences.Value); err != nil {
			prefsErr = err
		}
	}
	if prefsErr != nil {
		var e *errs.Error
		if errors.As(prefsErr, &e) {
			return prefsErr
		}
		return FieldError("preferences", "invalid_type")
	}
	return errors.Join(
		decodeField("nickname", raw.Nickname, &r.Nickname),
		decodeField("self_declared_level", raw.SelfDeclaredLevel, &r.SelfDeclaredLevel),
		decodeField("target_level", raw.TargetLevel, &r.TargetLevel),
		decodeField("learning_goal", raw.LearningGoal, &r.LearningGoal),
		decodeField("learning_goal_note", raw.LearningGoalNote, &r.LearningGoalNote),
	)
}

// Validate refuses `"preferences": null`: the object is not nullable.
func (r patchProfileRequest) Validate() error {
	if r.Preferences.Null {
		return FieldError("preferences", "invalid_type")
	}
	return nil
}

func (r patchProfileRequest) input() learner.PatchInput {
	p := r.Preferences.Value
	return learner.PatchInput{
		Nickname: r.Nickname, SelfDeclaredLevel: r.SelfDeclaredLevel, TargetLevel: r.TargetLevel,
		LearningGoal: r.LearningGoal, LearningGoalNote: r.LearningGoalNote,
		SpeechSpeed: p.SpeechSpeed, Formality: p.Formality, Dialect: p.Dialect, MemoryEnabled: p.MemoryEnabled,
	}
}

type skillResponse struct {
	Skill               string   `json:"skill"`
	EstimatedLevel      float64  `json:"estimated_level"`
	LevelLabel          string   `json:"level_label"`
	DemonstratedCeiling *float64 `json:"demonstrated_ceiling"`
	Confidence          float64  `json:"confidence"`
	ConfidenceLabel     string   `json:"confidence_label"`
	Trend               string   `json:"trend"`
	EvidenceCount       int      `json:"evidence_count"`
	IsProvisional       bool     `json:"is_provisional"`
	LastAssessedAt      *string  `json:"last_assessed_at"`
}

type exposureItemResponse struct {
	Gender   string  `json:"gender"`
	Accent   string  `json:"accent"`
	Sessions int     `json:"sessions"`
	Seconds  int64   `json:"seconds"`
	Share    float64 `json:"share"`
}

type voiceExposureResponse struct {
	WindowDays         int                    `json:"window_days"`
	Items              []exposureItemResponse `json:"items"`
	ExposureSuggestion *string                `json:"exposure_suggestion"`
}

// skillsResponse is the composite body of GET /profile/skills (no `data` envelope).
type skillsResponse struct {
	Skills           []skillResponse       `json:"skills"`
	FocusSkills      []string              `json:"focus_skills"`
	VoiceExposure    voiceExposureResponse `json:"voice_exposure"`
	AlgorithmVersion int                   `json:"algorithm_version"`
}

func newSkillsResponse(v learner.SkillsView) skillsResponse {
	out := skillsResponse{
		Skills: make([]skillResponse, 0, len(v.Skills)), FocusSkills: make([]string, 0, len(v.FocusSkills)),
		VoiceExposure: voiceExposureResponse{
			WindowDays: v.VoiceExposure.WindowDays, Items: make([]exposureItemResponse, 0, len(v.VoiceExposure.Items)),
			ExposureSuggestion: v.VoiceExposure.ExposureSuggestion,
		},
		AlgorithmVersion: v.AlgorithmVersion,
	}
	for _, s := range v.Skills {
		out.Skills = append(out.Skills, skillResponse{
			Skill: string(s.Skill), EstimatedLevel: round(s.EstimatedLevel, 2), LevelLabel: s.LevelLabel,
			DemonstratedCeiling: roundPtr(s.DemonstratedCeiling, 2), Confidence: round(s.Confidence, 2),
			ConfidenceLabel: s.ConfidenceLabel, Trend: string(s.Trend), EvidenceCount: s.EvidenceCount,
			IsProvisional: s.IsProvisional, LastAssessedAt: formatTimePtr(s.LastAssessedAt),
		})
	}
	for _, k := range v.FocusSkills {
		out.FocusSkills = append(out.FocusSkills, string(k))
	}
	for _, it := range v.VoiceExposure.Items {
		out.VoiceExposure.Items = append(out.VoiceExposure.Items, exposureItemResponse{
			Gender: it.Gender, Accent: it.Accent, Sessions: it.Sessions, Seconds: it.Seconds, Share: round(it.Share, 2),
		})
	}
	return out
}

type historyItemResponse struct {
	AssessedAt          string    `json:"assessed_at"`
	SessionID           uuid.UUID `json:"session_id"`
	EvaluationID        uuid.UUID `json:"evaluation_id"`
	RawScore            float64   `json:"raw_score"`
	EffectiveDifficulty *int      `json:"effective_difficulty"`
	EvidenceLevel       float64   `json:"evidence_level"`
	EstimatedLevel      float64   `json:"estimated_level"`
	Confidence          float64   `json:"confidence"`
	Trend               string    `json:"trend"`
}

// historyResponse is the body of GET /profile/skills/{skill}/history.
type historyResponse struct {
	Skill      string                `json:"skill"`
	Data       []historyItemResponse `json:"data"`
	NextCursor *string               `json:"next_cursor"`
}

func newHistoryResponse(skill learner.Skill, rows []learner.EstimateRow, next *string) historyResponse {
	out := historyResponse{Skill: string(skill), Data: make([]historyItemResponse, 0, len(rows)), NextCursor: next}
	for _, r := range rows {
		out.Data = append(out.Data, historyItemResponse{
			AssessedAt: formatTime(r.AssessedAt), SessionID: r.SessionID, EvaluationID: r.EvaluationID,
			RawScore: round(r.RawScore, 2), EffectiveDifficulty: r.DEff, EvidenceLevel: round(r.LObs, 2),
			EstimatedLevel: round(r.Level, 2), Confidence: round(r.Confidence, 3), Trend: string(r.Trend),
		})
	}
	return out
}

func round(x float64, places int) float64 {
	p := math.Pow10(places)
	return math.Round(x*p) / p
}

func roundPtr(p *float64, places int) *float64 {
	if p == nil {
		return nil
	}
	r := round(*p, places)
	return &r
}
