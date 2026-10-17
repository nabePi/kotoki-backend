package probe_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/test/chaos/probe"
)

func rec(typ, reason string, at time.Duration) probe.Record {
	r := probe.Record{Type: typ, Reason: reason, At: at}
	if typ == "session.ending" {
		r.Stage = "ENDING"
	}
	return r
}

func goodExpiry() []probe.Record {
	// the answer to an utterance at 296 s starts at 298 s, the session ends at 300 s and the answer
	// finishes within the grace of the ending
	return []probe.Record{
		rec("session.started", "", time.Second),
		rec("turn.started", "", 296*time.Second),
		rec("ai.generation_started", "", 298*time.Second),
		rec("session.ending", "DURATION_ELAPSED", 300*time.Second),
		rec("turn.completed", "", 301*time.Second),
		{Type: "session.completed", Reason: "DURATION_ELAPSED", At: 301 * time.Second, Charged: 299},
	}
}

func TestCheckExpiry_Passes(t *testing.T) {
	require.NoError(t, probe.CheckExpiry(goodExpiry(), 300))
}

func TestCheckExpiry_AnswerCutAtTheEndOfTheGraceAlsoCounts(t *testing.T) {
	rs := goodExpiry()
	rs[4] = rec("ai.interrupted", "SESSION_ENDING", 310*time.Second)
	require.NoError(t, probe.CheckExpiry(rs, 300))
}

func TestCheckExpiry_Failures(t *testing.T) {
	cases := map[string]func([]probe.Record) []probe.Record{
		"only the warnings":         func(r []probe.Record) []probe.Record { r[3].Stage = "WARNING"; return r },
		"no ending":                 func(r []probe.Record) []probe.Record { return append(r[:3:3], r[4:]...) },
		"ending for another reason": func(r []probe.Record) []probe.Record { r[3].Reason = "USER_ENDED"; return r },
		"the AI had not started answering": func(r []probe.Record) []probe.Record {
			r[2].At = 300 * time.Second // the generation starts at the ending, not before it
			return r
		},
		"the AI answered before the ending": func(r []probe.Record) []probe.Record { r[4].At = 299 * time.Second; return r },
		"no answer at all":                  func(r []probe.Record) []probe.Record { return append(r[:2:2], r[3], r[5]) },
		"interrupted for another reason": func(r []probe.Record) []probe.Record {
			r[4] = rec("ai.interrupted", "BARGE_IN", 301*time.Second)
			return r
		},
		"never completed":              func(r []probe.Record) []probe.Record { return r[:5] },
		"completed for another reason": func(r []probe.Record) []probe.Record { r[5].Reason = "SERVER_ERROR"; return r },
		"completed before ending":      func(r []probe.Record) []probe.Record { r[5].At = 299 * time.Second; return r },
		"charged more than reserved":   func(r []probe.Record) []probe.Record { r[5].Charged = 301; return r },
		"session failed":               func(r []probe.Record) []probe.Record { return append(r, rec("session.failed", "", 302*time.Second)) },
	}
	for name, mod := range cases {
		require.Error(t, probe.CheckExpiry(mod(goodExpiry()), 300), name)
	}
	require.Error(t, probe.CheckExpiry(nil, 300))
}
