package memory_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/memory"
)

func TestNormalizeKey(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Hobby: Hiking", "hobby:_hiking"},
		{"pref:topic_avoid:Politics", "pref:topic_avoid:politics"},
		{"error:particle-wa.ga", "error:particle-wa_ga"}, // '-' after the last ':' stays, '.' becomes '_'
		{"goal:job-in-japan", "goal:job-in-japan"},
		{"Job Role", "job_role"},
		{"job-role", "job_role"},
		{"field.of-study", "field_of_study"},
		{"  occupation  ", "occupation"},
		{"hobby:ha!ik#ing", "hobby:haiking"},
		{"ｈｏｂｂｙ：ｈｉｋｉｎｇ", "hobby:hiking"}, // NFKC: fullwidth letters and colon
		{"趣味:hiking", ":hiking"},
		{"pref:name_to_call", "pref:name_to_call"},
		{"a-b:c-d:e-f g", "a_b:c_d:e-f_g"},
		{"", ""},
	}
	for _, c := range cases {
		require.Equal(t, c.want, memory.NormalizeKey(c.in), c.in)
	}
}

func TestKeyAllowed_Registry(t *testing.T) {
	pf, pr, lp, ep := memory.CategoryProfileFact, memory.CategoryPreference, memory.CategoryLearningPattern, memory.CategoryEpisodic
	for _, k := range []string{"occupation", "employer", "industry", "job_role", "field_of_study", "student_status", "residence_city",
		"residence_country", "japan_experience", "japanese_study_history", "japanese_study_reason"} {
		require.True(t, memory.KeyAllowed(pf, k), k)
		require.False(t, memory.KeyAllowed(pr, k), k)
		require.False(t, memory.KeyAllowed(lp, k), k)
	}
	for _, k := range []string{"pref:name_to_call", "pref:speech_speed", "pref:formality"} {
		require.True(t, memory.KeyAllowed(pr, k), k)
		require.False(t, memory.KeyAllowed(pf, k), k)
	}
	for _, p := range []string{"hobby", "interest", "goal", "plan", "family", "pet", "skill", "language"} {
		require.True(t, memory.KeyAllowed(pf, p+":x"), p)
		require.False(t, memory.KeyAllowed(pf, p+":"), p+" empty slug")
		require.False(t, memory.KeyAllowed(pf, p), p+" without slot")
		require.False(t, memory.KeyAllowed(lp, p+":x"), p)
	}
	for _, p := range []string{"pref:topic_like", "pref:topic_avoid", "pref:style"} {
		require.True(t, memory.KeyAllowed(pr, p+":anime"), p)
		require.False(t, memory.KeyAllowed(pr, p+":"), p)
		require.False(t, memory.KeyAllowed(pf, p+":anime"), p)
	}
	for _, p := range []string{"error", "strength", "habit", "need"} {
		require.True(t, memory.KeyAllowed(lp, p+":particle_wa_ga"), p)
		require.False(t, memory.KeyAllowed(pf, p+":x"), p)
		require.False(t, memory.KeyAllowed(pr, p+":x"), p)
	}
	require.True(t, memory.KeyAllowed(pf, "hobby:hiking"))
	require.True(t, memory.KeyAllowed(pf, "goal:job-in-japan"))
	require.False(t, memory.KeyAllowed(pf, "occupation:x"))
	require.False(t, memory.KeyAllowed(pf, "hobby:"))
	require.False(t, memory.KeyAllowed(pf, "error:x"))
	require.False(t, memory.KeyAllowed(pf, "foo:bar"))
	require.False(t, memory.KeyAllowed(pf, ""))
	require.False(t, memory.KeyAllowed(pf, "hobby:a:b"), "slug cannot contain ':'")
	require.False(t, memory.KeyAllowed(pf, "hobby:Hiking"), "key must already be normalised")
	require.False(t, memory.KeyAllowed(pf, "hobby:a b"))
	require.True(t, memory.KeyAllowed(pf, "hobby:"+strings.Repeat("a", 40)))
	require.False(t, memory.KeyAllowed(pf, "hobby:"+strings.Repeat("a", 41)))
	require.False(t, memory.KeyAllowed(pf, "pref:name_to_call"))

	id := uuid.NewString()
	require.True(t, memory.KeyAllowed(ep, "episode:"+id))
	require.False(t, memory.KeyAllowed(ep, "episode:not-a-uuid"))
	require.False(t, memory.KeyAllowed(ep, "episode:"+strings.ToUpper(id)))
	require.False(t, memory.KeyAllowed(ep, "episode:"+strings.ReplaceAll(id, "-", "")), "32 hex digits is not the canonical form")
	require.False(t, memory.KeyAllowed(ep, "episode:{"+id+"}"))
	require.False(t, memory.KeyAllowed(ep, "episode:"))
	require.False(t, memory.KeyAllowed(ep, "hobby:x"))
	require.False(t, memory.KeyAllowed(lp, "episode:"+id))
	require.False(t, memory.KeyAllowed(pf, "episode:"+id))
	require.False(t, memory.KeyAllowed(pr, "episode:"+id))
}

func TestKeyAllowed_MaxLength(t *testing.T) {
	// A key longer than 100 characters is rejected even when prefix and slug are fine; the longest
	// registry key (pref:topic_avoid: + 40) is 56 characters, so the cap only guards the DB check.
	require.True(t, memory.KeyAllowed(memory.CategoryPreference, "pref:topic_avoid:"+strings.Repeat("a", 40)))
	require.Equal(t, 100, memory.MaxKeyLen)
}
