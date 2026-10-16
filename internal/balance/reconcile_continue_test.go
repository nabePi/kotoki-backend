package balance_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/balance"
)

// One failing query must not blind the other five: R1..R6 all run, the violations found are
// returned together with a joined error that names every failed check.
func TestReconcile_ContinuesAfterCheckError(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("boom")
	f := &fakeChecks{err: map[string]error{"R2": boom}, rows: map[string][]string{"R1": {"x"}, "R3": {"user=a"}}}
	v, err := e.svc.Reconcile(context.Background(), f)
	require.ErrorIs(t, err, boom)
	require.ErrorContains(t, err, "R2")
	require.Equal(t, []string{"R1", "R2", "R3", "R4", "R5", "R6"}, f.calls, "R3..R6 still run after R2 failed")
	require.Equal(t, []balance.Violation{{Check: "R1", Rows: []string{"x"}}, {Check: "R3", Rows: []string{"user=a"}}}, v)
}

func TestReconcile_JoinsEveryCheckError(t *testing.T) {
	e := newEnv(t)
	e2, e5 := errors.New("e2"), errors.New("e5")
	f := &fakeChecks{err: map[string]error{"R2": e2, "R5": e5}}
	v, err := e.svc.Reconcile(context.Background(), f)
	require.ErrorIs(t, err, e2)
	require.ErrorIs(t, err, e5)
	require.ErrorContains(t, err, "R2")
	require.ErrorContains(t, err, "R5")
	require.Empty(t, v)
	require.Len(t, f.calls, 6)
}

func TestReconcile_StopsWhenContextIsCancelled(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	f := &cancelAfter{fakeChecks: fakeChecks{}, at: "R2", cancel: cancel}
	_, err := e.svc.Reconcile(ctx, f)
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, []string{"R1", "R2"}, f.calls, "a cancelled run does not walk through the remaining checks")
}

type cancelAfter struct {
	fakeChecks
	at     string
	cancel func()
}

func (c *cancelAfter) RunCheck(ctx context.Context, check string) ([]string, error) {
	rows, err := c.fakeChecks.RunCheck(ctx, check)
	if check == c.at {
		c.cancel()
	}
	return rows, err
}
