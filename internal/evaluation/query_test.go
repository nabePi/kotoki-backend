package evaluation_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/evaluation"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
)

type stubReader struct {
	row         *evaluation.Evaluation
	findings    []evaluation.FindingRow
	getErr      error
	listErr     error
	gets, lists int
}

func (s *stubReader) GetBySession(context.Context, uuid.UUID) (evaluation.Evaluation, bool, error) {
	s.gets++
	if s.getErr != nil {
		return evaluation.Evaluation{}, false, s.getErr
	}
	if s.row == nil {
		return evaluation.Evaluation{}, false, nil
	}
	return *s.row, true, nil
}

func (s *stubReader) ListFindings(context.Context, uuid.UUID) ([]evaluation.FindingRow, error) {
	s.lists++
	return s.findings, s.listErr
}

func ref(status string, started bool) evaluation.SessionRef {
	r := evaluation.SessionRef{ID: uuid.New(), Status: status}
	if started {
		at := time.Date(2026, 10, 12, 6, 0, 0, 0, time.UTC)
		r.StartedAt = &at
	}
	return r
}

func TestQueryNotEnded(t *testing.T) {
	for _, st := range []string{"RESERVED", "STARTING", "ACTIVE", "ENDING"} {
		rd := &stubReader{}
		_, err := evaluation.NewQueryService(rd).Get(t.Context(), ref(st, st != "RESERVED"))
		require.Equal(t, errs.CodeSessionNotEnded, errs.CodeOf(err), st)
		require.Equal(t, map[string]any{"session_status": st}, errs.DetailsOf(err), st)
		require.Zero(t, rd.gets, "the matrix is decided before any read: %s", st)
	}
}

func TestQueryNeverStarted(t *testing.T) {
	for _, st := range []string{"CANCELLED", "ABANDONED", "FAILED"} {
		rd := &stubReader{row: &evaluation.Evaluation{Status: evaluation.StatusCompleted}}
		_, err := evaluation.NewQueryService(rd).Get(t.Context(), ref(st, false))
		require.Equal(t, errs.CodeEvaluationNotAvailable, errs.CodeOf(err), st)
		require.Zero(t, rd.gets, st)
	}
}

func TestQuerySynthesizedPendingWithoutRow(t *testing.T) {
	for _, st := range []string{"COMPLETED", "FAILED"} {
		r := ref(st, true)
		r.TranscriptPurged = true
		v, err := evaluation.NewQueryService(&stubReader{}).Get(t.Context(), r)
		require.NoError(t, err, st)
		require.Equal(t, evaluation.StatusPending, v.Status)
		require.Nil(t, v.Eval)
		require.Equal(t, r.ID, v.SessionID)
		require.True(t, v.TranscriptPurged)
		require.Empty(t, v.Findings)
	}
}

func TestQueryReturnsStoredRowsAsIs(t *testing.T) {
	findings := []evaluation.FindingRow{{ID: uuid.New(), Ordinal: 1}, {ID: uuid.New(), Ordinal: 2}}
	for _, st := range []evaluation.Status{evaluation.StatusPending, evaluation.StatusRunning, evaluation.StatusCompleted, evaluation.StatusFailed} {
		rd := &stubReader{row: &evaluation.Evaluation{ID: uuid.New(), Status: st}, findings: findings}
		v, err := evaluation.NewQueryService(rd).Get(t.Context(), ref("COMPLETED", true))
		require.NoError(t, err, st)
		require.Equal(t, st, v.Status)
		require.Equal(t, rd.row.ID, v.Eval.ID)
		if st == evaluation.StatusCompleted {
			require.Equal(t, findings, v.Findings)
			require.Equal(t, 1, rd.lists)
		} else {
			require.Empty(t, v.Findings, st)
			require.Zero(t, rd.lists, "findings are only read for a COMPLETED evaluation: %s", st)
		}
	}
}

func TestQueryPurgedDetailsSkipFindings(t *testing.T) {
	purged := time.Now()
	rd := &stubReader{row: &evaluation.Evaluation{Status: evaluation.StatusCompleted, DetailsPurgedAt: &purged}, findings: []evaluation.FindingRow{{Ordinal: 1}}}
	v, err := evaluation.NewQueryService(rd).Get(t.Context(), ref("COMPLETED", true))
	require.NoError(t, err)
	require.Empty(t, v.Findings)
	require.Zero(t, rd.lists)
}

func TestQueryPropagatesReaderErrors(t *testing.T) {
	boom := errors.New("db down")
	_, err := evaluation.NewQueryService(&stubReader{getErr: boom}).Get(t.Context(), ref("COMPLETED", true))
	require.ErrorIs(t, err, boom)
	_, err = evaluation.NewQueryService(&stubReader{row: &evaluation.Evaluation{Status: evaluation.StatusCompleted}, listErr: boom}).Get(t.Context(), ref("COMPLETED", true))
	require.ErrorIs(t, err, boom)
}
