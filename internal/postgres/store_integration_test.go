//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/idgen"
	"github.com/nabePi/kotoki-backend/internal/platform/otel"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func insertTopic(ctx context.Context, tx *postgres.Tx) (uuid.UUID, error) {
	r := testutil.TopicRow(nil)
	return r["id"].(uuid.UUID), testutil.TryInsert(ctx, tx.DBTX(), "topics", r)
}

func topicExists(t *testing.T, s *postgres.Store, id uuid.UUID) bool {
	var n int
	require.NoError(t, s.Pool().QueryRow(context.Background(), `SELECT count(*) FROM topics WHERE id=$1`, id).Scan(&n))
	return n == 1
}

func TestCommit(t *testing.T) {
	s := postgres.NewStore(testutil.NewPostgres(t))
	var id uuid.UUID
	require.NoError(t, s.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) (err error) {
		id, err = insertTopic(ctx, tx)
		return err
	}))
	require.True(t, topicExists(t, s, id))
}

func TestStoreOptions(t *testing.T) {
	pool := testutil.NewPostgres(t)
	def := postgres.NewStore(pool)
	require.Equal(t, uuid.Version(7), def.IDs().New().Version())
	require.IsType(t, clock.System{}, def.Clock())

	t0 := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ids, clk := idgen.NewSequence(t), clock.NewFake(t0)
	s := postgres.NewStore(pool, postgres.WithIDs(ids), postgres.WithClock(clk))
	require.Same(t, ids, s.IDs())
	require.Same(t, clk, s.Clock())
	require.NoError(t, s.WithinTx(context.Background(), func(_ context.Context, tx *postgres.Tx) error {
		require.Same(t, ids, tx.IDs())
		require.Same(t, clk, tx.Clock())
		return nil
	}))
}

func TestRollbackOnFnError(t *testing.T) {
	s := postgres.NewStore(testutil.NewPostgres(t))
	boom := errors.New("boom")
	var id uuid.UUID
	err := s.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) (err error) {
		id, err = insertTopic(ctx, tx)
		require.NoError(t, err)
		return boom
	})
	require.Same(t, boom, err)
	require.False(t, topicExists(t, s, id))
}

func TestRollbackOnPanic(t *testing.T) {
	s := postgres.NewStore(testutil.NewPostgres(t))
	var id uuid.UUID
	require.PanicsWithValue(t, "kaboom", func() {
		_ = s.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) (err error) {
			id, err = insertTopic(ctx, tx)
			require.NoError(t, err)
			panic("kaboom")
		})
	})
	require.False(t, topicExists(t, s, id))
	require.Zero(t, s.Pool().Stat().AcquiredConns())
}

func TestSetLocalTimeouts(t *testing.T) {
	s := postgres.NewStore(testutil.NewPostgres(t))
	require.NoError(t, s.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) error {
		for show, want := range map[string]string{"lock_timeout": "3s", "statement_timeout": "5s", "transaction_isolation": "read committed"} {
			var got string
			require.NoError(t, tx.DBTX().QueryRow(ctx, "SHOW "+show).Scan(&got))
			require.Equal(t, want, got, show)
		}
		return nil
	}))
}

type sleeps struct{ got []time.Duration }

func (s *sleeps) sleep(_ context.Context, d time.Duration) error {
	s.got = append(s.got, d)
	return nil
}

func within(d, base time.Duration) bool { return d >= base/2 && d <= base*3/2 }

func TestRetryOn40001(t *testing.T) {
	sl := &sleeps{}
	s := postgres.NewStore(testutil.NewPostgres(t), postgres.WithSleep(sl.sleep))
	calls := 0
	err := s.WithinTx(context.Background(), func(context.Context, *postgres.Tx) error {
		calls++
		if calls <= 2 {
			return &pgconn.PgError{Code: "40001"}
		}
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, 3, calls)
	require.Len(t, sl.got, 2)
	require.True(t, within(sl.got[0], 20*time.Millisecond), sl.got[0])
	require.True(t, within(sl.got[1], 50*time.Millisecond), sl.got[1])
}

func TestRetryOn40P01Exhausted(t *testing.T) {
	sl := &sleeps{}
	s := postgres.NewStore(testutil.NewPostgres(t), postgres.WithSleep(sl.sleep))
	calls := 0
	err := s.WithinTx(context.Background(), func(context.Context, *postgres.Tx) error {
		calls++
		return &pgconn.PgError{Code: "40P01"}
	})
	require.Equal(t, 4, calls)
	require.Equal(t, errs.CodeServiceUnavailable, errs.CodeOf(err), "contention outlasting the retries is transient, not a bug")
	var pe *pgconn.PgError
	require.ErrorAs(t, err, &pe)
	require.Equal(t, "40P01", pe.Code)
}

func TestNoRetryOnOtherError(t *testing.T) {
	s := postgres.NewStore(testutil.NewPostgres(t))
	calls := 0
	err := s.WithinTx(context.Background(), func(context.Context, *postgres.Tx) error {
		calls++
		return &pgconn.PgError{Code: "23505"}
	})
	require.Error(t, err)
	require.Equal(t, 1, calls)
}

func TestLockTimeout(t *testing.T) {
	pool := testutil.NewPostgres(t)
	row := testutil.TopicRow(nil)
	testutil.MustInsert(context.Background(), t, pool, "topics", row)

	holder, err := pool.Begin(context.Background())
	require.NoError(t, err)
	defer func() { _ = holder.Rollback(context.Background()) }()
	_, err = holder.Exec(context.Background(), `SELECT id FROM topics WHERE id=$1 FOR UPDATE`, row["id"])
	require.NoError(t, err)

	s := postgres.NewStore(pool, postgres.WithTimeouts(200*time.Millisecond, 5*time.Second))
	calls := 0
	err = s.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) error {
		calls++
		_, err := tx.DBTX().Exec(ctx, `SELECT id FROM topics WHERE id=$1 FOR UPDATE`, row["id"])
		return err
	})
	require.Equal(t, 1, calls)
	require.Equal(t, errs.CodeServiceUnavailable, errs.CodeOf(err))
	var pe *pgconn.PgError
	require.ErrorAs(t, err, &pe)
	require.Equal(t, "55P03", pe.Code)
}

func TestTxSpan(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	s := postgres.NewStore(testutil.NewPostgres(t))
	ctx, parent := otel.Start(context.Background(), "parent")
	err := s.WithinTx(ctx, func(context.Context, *postgres.Tx) error { return errors.New("internal failure") })
	require.Error(t, err)
	parent.End()
	span := rec.Find("db.tx")
	require.Equal(t, parent.SpanContext().SpanID(), span.Parent.SpanID())
	require.Equal(t, codes.Error, span.Status.Code)
}

func TestContextCancel(t *testing.T) {
	s := postgres.NewStore(testutil.NewPostgres(t))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := s.WithinTx(ctx, func(context.Context, *postgres.Tx) error { return nil })
	require.Error(t, err)
	require.Less(t, time.Since(start), time.Second)
	require.Zero(t, s.Pool().Stat().AcquiredConns())
}

func TestStatementTimeoutIsUnavailable(t *testing.T) {
	s := postgres.NewStore(testutil.NewPostgres(t), postgres.WithTimeouts(3*time.Second, 100*time.Millisecond))
	err := s.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) error {
		_, err := tx.DBTX().Exec(ctx, "SELECT pg_sleep(2)")
		return err
	})
	require.Equal(t, errs.CodeServiceUnavailable, errs.CodeOf(err))
}
