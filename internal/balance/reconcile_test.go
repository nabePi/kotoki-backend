package balance_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/balance"
)

type fakeChecks struct {
	rows  map[string][]string
	err   map[string]error
	calls []string
}

func (f *fakeChecks) RunCheck(_ context.Context, check string) ([]string, error) {
	f.calls = append(f.calls, check)
	if e := f.err[check]; e != nil {
		return nil, e
	}
	return f.rows[check], nil
}

func TestReconcile_ReportsRowsPerCheck(t *testing.T) {
	e := newEnv(t)
	f := &fakeChecks{rows: map[string][]string{"R3": {"user=a", "user=b"}}}
	v, err := e.svc.Reconcile(context.Background(), f)
	require.NoError(t, err)
	require.Equal(t, []balance.Violation{{Check: "R3", Rows: []string{"user=a", "user=b"}}}, v)
}

func TestReconcile_CleanIsEmpty(t *testing.T) {
	e := newEnv(t)
	v, err := e.svc.Reconcile(context.Background(), &fakeChecks{})
	require.NoError(t, err)
	require.Empty(t, v)
}

func TestReconcile_RunsR1ToR6InOrder(t *testing.T) {
	e := newEnv(t)
	f := &fakeChecks{}
	_, err := e.svc.Reconcile(context.Background(), f)
	require.NoError(t, err)
	require.Equal(t, []string{"R1", "R2", "R3", "R4", "R5", "R6"}, f.calls)
}

func TestReconcile_ErrorStopsAndWraps(t *testing.T) {
	e := newEnv(t)
	boom := errors.New("boom")
	f := &fakeChecks{err: map[string]error{"R2": boom}, rows: map[string][]string{"R1": {"x"}}}
	v, err := e.svc.Reconcile(context.Background(), f)
	require.ErrorIs(t, err, boom)
	require.ErrorContains(t, err, "R2")
	require.Equal(t, []string{"R1", "R2"}, f.calls, "R3..R6 are not run after a failure")
	require.Nil(t, v)
}
