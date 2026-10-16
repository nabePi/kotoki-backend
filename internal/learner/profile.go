package learner

import (
	"encoding/json"
	"math"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

// Field is one optional field of a PATCH body: an absent key leaves Set false, `null` sets Set and
// Null, a value sets Set and Value.
type Field[T any] struct {
	Set, Null bool
	Value     T
}

// UnmarshalJSON implements json.Unmarshaler. It is called only for keys present in the body.
func (f *Field[T]) UnmarshalJSON(b []byte) error {
	f.Set = true
	if string(b) == "null" {
		f.Null, f.Value = true, *new(T)
		return nil
	}
	f.Null = false
	return json.Unmarshal(b, &f.Value)
}

// PatchInput is the body of PATCH /profile, with the nested `preferences` object flattened.
type PatchInput struct {
	Nickname, SelfDeclaredLevel, TargetLevel, LearningGoal, LearningGoalNote Field[string]
	SpeechSpeed, Formality, Dialect                                          Field[string]
	MemoryEnabled                                                            Field[bool]
}

func (in PatchInput) empty() bool {
	return !in.Nickname.Set && !in.SelfDeclaredLevel.Set && !in.TargetLevel.Set && !in.LearningGoal.Set &&
		!in.LearningGoalNote.Set && !in.SpeechSpeed.Set && !in.Formality.Set && !in.Dialect.Set && !in.MemoryEnabled.Set
}

// ProfileView is the profile with the figures GET /profile adds (spec 12.9.1).
type ProfileView struct {
	Profile
	OverallLevel          float64
	OverallLevelLabel     string
	CompletedSessionCount int
	TotalPracticeSeconds  int64
	LastSessionAt         *time.Time
}

const (
	maxNicknameRunes = 30
	maxGoalNoteRunes = 300
)

var (
	selfLevels = []string{"BEGINNER", "N5", "N4", "N3", "N2", "N1"}
	targetLvls = []string{"N5", "N4", "N3", "N2", "N1"}
	goals      = []string{"DAILY_CONVERSATION", "JOB_INTERVIEW", "BUSINESS", "TRAVEL", "EXAM_PREP", "LIVING_IN_JAPAN", "OTHER"}
	speeds     = []string{"SLOWER", "NORMAL", "FASTER"}
	formals    = []string{"AUTO", "CASUAL", "POLITE"}
	dialects   = []string{"STANDARD_ONLY", "ALLOW_DIALECT"}
)

const (
	reasonInvalidFormat = "invalid_format"
	reasonOutOfRange    = "out_of_range"
)

type violation struct{ field, reason string }

// validationError is VALIDATION_FAILED with the {fields: [{field, reason}]} details of spec 5.10.
func validationError(vs []violation) error {
	fields := make([]map[string]any, len(vs))
	for i, v := range vs {
		fields[i] = map[string]any{"field": v.field, "reason": v.reason}
	}
	return errs.WithDetails(errs.New(errs.CodeValidationFailed, "invalid profile fields"), map[string]any{"fields": fields})
}

// normalizeNickname is NFKC, then a check that no control or line-separator rune is left (before
// trimming, so a trailing newline is rejected rather than silently dropped), then TrimSpace. The
// result is 1-30 runes without `<`, `>`, `{`, `}`: the value goes into LLM prompts (S-4).
func normalizeNickname(raw string) (string, string) {
	n := norm.NFKC.String(raw)
	for _, r := range n {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return "", reasonInvalidFormat
		}
	}
	n = strings.TrimSpace(n)
	if c := utf8.RuneCountInString(n); c < 1 || c > maxNicknameRunes {
		return "", reasonOutOfRange
	}
	for _, r := range n {
		switch r {
		case '<', '>', '{', '}':
			return "", reasonInvalidFormat
		}
	}
	return n, ""
}

// validatePatch checks every field and reports all violations at once; the returned input carries
// the normalised nickname.
func validatePatch(in PatchInput) (PatchInput, error) {
	var vs []violation
	bad := func(field, reason string) { vs = append(vs, violation{field, reason}) }

	if in.Nickname.Set && !in.Nickname.Null {
		n, reason := normalizeNickname(in.Nickname.Value)
		if reason != "" {
			bad("nickname", reason)
		}
		in.Nickname.Value = n
	}
	enum := func(f Field[string], field string, allowed []string, nullable bool) {
		switch {
		case !f.Set:
		case f.Null && !nullable:
			bad(field, reasonInvalidFormat)
		case !f.Null && !slices.Contains(allowed, f.Value):
			bad(field, reasonInvalidFormat)
		}
	}
	enum(in.SelfDeclaredLevel, "self_declared_level", selfLevels, true)
	enum(in.TargetLevel, "target_level", targetLvls, true)
	enum(in.LearningGoal, "learning_goal", goals, true)
	if in.LearningGoalNote.Set && !in.LearningGoalNote.Null && utf8.RuneCountInString(in.LearningGoalNote.Value) > maxGoalNoteRunes {
		bad("learning_goal_note", reasonOutOfRange)
	}
	enum(in.SpeechSpeed, "preferences.speech_speed", speeds, false)
	enum(in.Formality, "preferences.formality", formals, false)
	enum(in.Dialect, "preferences.dialect", dialects, false)
	if in.MemoryEnabled.Set && in.MemoryEnabled.Null {
		bad("preferences.memory_enabled", reasonInvalidFormat)
	}
	if len(vs) > 0 {
		return PatchInput{}, validationError(vs)
	}
	return in, nil
}

func strField(f Field[string], cur *string) *string {
	switch {
	case !f.Set:
		return cur
	case f.Null:
		return nil
	default:
		v := f.Value
		return &v
	}
}

// apply merges a validated patch into the writable columns.
func (in PatchInput) apply(row ProfileRow) ProfileRow {
	row.Nickname = strField(in.Nickname, row.Nickname)
	row.SelfDeclaredLevel = strField(in.SelfDeclaredLevel, row.SelfDeclaredLevel)
	row.TargetLevel = strField(in.TargetLevel, row.TargetLevel)
	row.LearningGoal = strField(in.LearningGoal, row.LearningGoal)
	row.LearningGoalNote = strField(in.LearningGoalNote, row.LearningGoalNote)
	if in.SpeechSpeed.Set {
		row.PreferredSpeechSpeed = in.SpeechSpeed.Value
	}
	if in.Formality.Set {
		row.PreferredFormality = in.Formality.Value
	}
	if in.Dialect.Set {
		row.DialectPreference = in.Dialect.Value
	}
	if in.MemoryEnabled.Set {
		row.MemoryEnabled = in.MemoryEnabled.Value
	}
	return row
}

// overallLevel is the mean of the estimated levels weighted by max(0.05, EffectiveConfidence),
// rounded to 2 decimals (spec 12.9.1).
func overallLevel(states []StateRow, now time.Time) float64 {
	var sum, weight float64
	for _, s := range states {
		w := math.Max(0.05, EffectiveConfidence(s.Confidence, s.LastEvidenceAt, now))
		sum += s.Level * w
		weight += w
	}
	if weight == 0 {
		return 0
	}
	return round2(sum / weight)
}

func newProfileView(p Profile, states []StateRow, st ProfileStats, now time.Time) ProfileView {
	lvl := overallLevel(states, now)
	return ProfileView{
		Profile: p, OverallLevel: lvl, OverallLevelLabel: LevelLabel(lvl),
		CompletedSessionCount: st.CompletedSessions, TotalPracticeSeconds: st.PracticeSeconds, LastSessionAt: st.LastSessionAt,
	}
}
