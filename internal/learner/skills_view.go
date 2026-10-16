package learner

import (
	"cmp"
	"slices"
	"time"
)

// Display thresholds of GET /profile/skills (spec 12.9.3).
const (
	provisionalMinEvidence   = 3
	provisionalMinConfidence = 0.30
	exposureWindowDays       = 90
	exposureMinSessions      = 5
	exposureDominantPercent  = 80
	// ExposureTryOtherVoice is the exposure_suggestion value.
	ExposureTryOtherVoice = "TRY_OTHER_VOICE"
)

// SkillView is one skill of GET /profile/skills.
type SkillView struct {
	Skill               Skill
	EstimatedLevel      float64
	LevelLabel          string
	DemonstratedCeiling *float64
	Confidence          float64 // EffectiveConfidence, 2 decimals
	ConfidenceLabel     string
	Trend               Trend
	EvidenceCount       int
	IsProvisional       bool
	LastAssessedAt      *time.Time
}

// ExposureItem is one (gender, accent) voice of the exposure of the last 90 days.
type ExposureItem struct {
	Gender, Accent string
	Sessions       int
	Seconds        int64
	Share          float64 // seconds / total seconds, 2 decimals
}

// VoiceExposureView is the voice_exposure object (spec 12.8.6, 12.9.3).
type VoiceExposureView struct {
	WindowDays         int
	Items              []ExposureItem
	ExposureSuggestion *string
}

// SkillsView is the body of GET /profile/skills.
type SkillsView struct {
	Skills           []SkillView
	FocusSkills      []Skill
	VoiceExposure    VoiceExposureView
	AlgorithmVersion int
}

// newSkillView renders a state. The confidence is rounded first, and the label and the provisional
// flag read the rounded value, so what the learner sees never contradicts them.
func newSkillView(s StateRow, now time.Time) SkillView {
	conf := round2(EffectiveConfidence(s.Confidence, s.LastEvidenceAt, now))
	return SkillView{
		Skill: s.Skill, EstimatedLevel: s.Level, LevelLabel: LevelLabel(s.Level), DemonstratedCeiling: s.Ceiling,
		Confidence: conf, ConfidenceLabel: ConfidenceLabel(conf), Trend: s.Trend, EvidenceCount: s.EvidenceCount,
		IsProvisional:  s.EvidenceCount < provisionalMinEvidence || conf < provisionalMinConfidence,
		LastAssessedAt: s.LastEvidenceAt,
	}
}

// newVoiceExposure shares the seconds out, orders the items by seconds (then gender, accent) and
// suggests another voice when at least 5 sessions are over 80% one combination.
func newVoiceExposure(rows []ExposureRow) VoiceExposureView {
	v := VoiceExposureView{WindowDays: exposureWindowDays, Items: []ExposureItem{}}
	var totalSeconds int64
	var totalSessions int
	for _, r := range rows {
		totalSeconds += r.Seconds
		totalSessions += r.Sessions
	}
	sorted := slices.Clone(rows)
	slices.SortFunc(sorted, func(a, b ExposureRow) int {
		return cmp.Or(cmp.Compare(b.Seconds, a.Seconds), cmp.Compare(a.Gender, b.Gender), cmp.Compare(a.Accent, b.Accent))
	})
	dominant := false
	for _, r := range sorted {
		var share float64
		if totalSeconds > 0 {
			share = round2(float64(r.Seconds) / float64(totalSeconds))
			if r.Seconds*100 > totalSeconds*exposureDominantPercent {
				dominant = true
			}
		}
		v.Items = append(v.Items, ExposureItem{Gender: r.Gender, Accent: r.Accent, Sessions: r.Sessions, Seconds: r.Seconds, Share: share})
	}
	if dominant && totalSessions >= exposureMinSessions {
		s := ExposureTryOtherVoice
		v.ExposureSuggestion = &s
	}
	return v
}

// focusInput builds the FocusSkills input of a user's states with no scenario focus (spec 12.9.3).
func focusInput(states []StateRow, findings map[Skill]FindingStat, evaluations int, now time.Time) FocusInput {
	in := FocusInput{
		Levels: map[Skill]float64{}, Confidences: map[Skill]float64{}, Trends: map[Skill]Trend{},
		Findings: findings, Evaluations: evaluations,
	}
	for _, s := range states {
		in.Levels[s.Skill] = EffectiveLevel(s.Level, s.Ceiling)
		in.Confidences[s.Skill] = EffectiveConfidence(s.Confidence, s.LastEvidenceAt, now)
		in.Trends[s.Skill] = s.Trend
	}
	return in
}
