package session_test

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/session"
)

var every = []session.Status{
	session.StatusReserved, session.StatusStarting, session.StatusActive, session.StatusEnding,
	session.StatusCompleted, session.StatusCancelled, session.StatusAbandoned, session.StatusFailed,
}

func TestCanTransitionMatrix(t *testing.T) {
	allowed := map[[2]session.Status]bool{
		{session.StatusReserved, session.StatusStarting}:  true,
		{session.StatusReserved, session.StatusCancelled}: true,
		{session.StatusReserved, session.StatusAbandoned}: true,
		{session.StatusStarting, session.StatusActive}:    true,
		{session.StatusStarting, session.StatusFailed}:    true,
		{session.StatusActive, session.StatusEnding}:      true,
		{session.StatusActive, session.StatusFailed}:      true,
		{session.StatusEnding, session.StatusCompleted}:   true,
		{session.StatusEnding, session.StatusFailed}:      true,
	}
	n := 0
	for _, from := range every {
		for _, to := range every {
			require.Equal(t, allowed[[2]session.Status{from, to}], session.CanTransition(from, to), "%s → %s", from, to)
			n++
		}
	}
	require.Equal(t, 64, n)
}

func TestTerminalStatusFor(t *testing.T) {
	for r, want := range map[session.EndReason]session.Status{
		session.EndUserEnded: session.StatusCompleted, session.EndDurationElapsed: session.StatusCompleted, session.EndReconnectTimeout: session.StatusCompleted,
		session.EndProviderFailure: session.StatusFailed, session.EndServerError: session.StatusFailed,
		session.EndUserCancelled: session.StatusCancelled, session.EndNotStarted: session.StatusAbandoned,
	} {
		require.Equal(t, want, session.TerminalStatusFor(r), r)
		require.True(t, want.Terminal())
	}
	require.Empty(t, session.TerminalStatusFor("BOGUS"))
}

func TestStatusFromStringRejectsUnknown(t *testing.T) {
	for _, st := range every {
		got, err := session.StatusFromString(string(st))
		require.NoError(t, err)
		require.Equal(t, st, got)
	}
	_, err := session.StatusFromString("PAUSED")
	require.Error(t, err)
	_, err = session.StatusFromString("")
	require.Error(t, err)
}

func TestOpenAndTerminalPartitionTheStatuses(t *testing.T) {
	open := 0
	for _, st := range every {
		require.NotEqual(t, st.Open(), st.Terminal(), st)
		if st.Open() {
			open++
		}
	}
	require.Equal(t, 4, open, "RESERVED, STARTING, ACTIVE and ENDING are open (ux_sessions_one_open_per_user)")
	for _, st := range every {
		if st.Terminal() {
			for _, to := range every {
				require.False(t, session.CanTransition(st, to), "nothing leaves %s", st)
			}
		}
	}
}

func fieldNames(v any) map[string]bool {
	out := map[string]bool{}
	t := reflect.TypeOf(v)
	for i := 0; i < t.NumField(); i++ {
		out[t.Field(i).Name] = true
	}
	return out
}

func TestSessionHasSnapshotNoLegacyVersionFields(t *testing.T) {
	banned := []string{"CharacterVersionID", "VoiceProfileID", "ConversationStyleVersionID", "ExpressionProfileVersionID", "Expressions"}
	for _, v := range []any{session.Session{}, session.RuntimeRecord{}, session.Turn{}} {
		f := fieldNames(v)
		for _, b := range banned {
			require.False(t, f[b], "%T must not have %s (D-014)", v, b)
		}
	}
	require.True(t, fieldNames(session.Session{})["ConfigSnapshot"])
	require.True(t, fieldNames(session.RuntimeRecord{})["ConfigSnapshot"])
	require.True(t, fieldNames(session.RuntimeRecord{})["RuntimeFence"])
	require.True(t, fieldNames(session.RuntimeRecord{})["OpeningPromptVersionID"])
}

func TestConstants(t *testing.T) {
	require.Equal(t, 5*60, int(session.StartDeadline.Seconds()))
	require.Equal(t, 10, int(session.StartTimeout.Seconds()))
}
