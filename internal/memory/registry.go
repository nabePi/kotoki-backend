package memory

import (
	"regexp"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"
)

// MaxKeyLen is the longest memory_key the database accepts (chk_memories_key_fmt).
const MaxKeyLen = 100

// slugRE is the slug of a multi-slot key (spec 11.4).
var slugRE = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)

var singleSlotKeys = map[Category]map[string]struct{}{
	CategoryProfileFact: set("occupation", "employer", "industry", "job_role", "field_of_study", "student_status",
		"residence_city", "residence_country", "japan_experience", "japanese_study_history", "japanese_study_reason"),
	CategoryPreference: set("pref:name_to_call", "pref:speech_speed", "pref:formality"),
}

var multiSlotPrefixes = map[Category][]string{
	CategoryProfileFact:     {"hobby:", "interest:", "goal:", "plan:", "family:", "pet:", "skill:", "language:"},
	CategoryPreference:      {"pref:topic_like:", "pref:topic_avoid:", "pref:style:"},
	CategoryLearningPattern: {"error:", "strength:", "habit:", "need:"},
}

func set(v ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(v))
	for _, s := range v {
		m[s] = struct{}{}
	}
	return m
}

// NormalizeKey normalises a key proposed by the LLM (spec 11.4): NFKC, lowercase, space, '-' and
// '.' become '_' except that '-' stays after the last ':'; every other character outside
// [a-z0-9_:] is dropped. Surrounding whitespace is trimmed first.
func NormalizeKey(k string) string {
	k = strings.ToLower(norm.NFKC.String(strings.TrimSpace(k)))
	lastColon := strings.LastIndexByte(k, ':')
	var b strings.Builder
	for i := 0; i < len(k); i++ {
		c := k[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '_', c == ':':
			b.WriteByte(c)
		case c == '-' && lastColon >= 0 && i > lastColon:
			b.WriteByte('-')
		case c == ' ', c == '-', c == '.':
			b.WriteByte('_')
		}
	}
	return b.String()
}

// KeyAllowed reports whether key (already normalised) is in the registry of category c (spec
// 11.4): an exact single-slot key, or a multi-slot prefix followed by a slug [a-z0-9_-]{1,40}.
// EPISODIC accepts only episode:{uuid}, which the server assigns.
func KeyAllowed(c Category, key string) bool {
	if len(key) > MaxKeyLen { // unreachable with today's registry (longest key: 56); guards the DB check
		return false
	}
	if c == CategoryEpisodic {
		id, ok := strings.CutPrefix(key, "episode:")
		if !ok {
			return false
		}
		_, err := uuid.Parse(id)
		return err == nil && len(id) == 36 && strings.ToLower(id) == id
	}
	if _, ok := singleSlotKeys[c][key]; ok {
		return true
	}
	for _, p := range multiSlotPrefixes[c] {
		if slug, ok := strings.CutPrefix(key, p); ok {
			return slugRE.MatchString(slug)
		}
	}
	return false
}
