//go:build integration

package jobs_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/jobs"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

// The central retention guarantee (review focus 5), end to end through the job with a real
// database and a fake clock: no turn is deleted unless its session ended at least 180 days ago.
// Each session ended at a different offset around the 180 day line and has a turn created 65
// minutes before ended_at, the earliest a turn can be (spec 9.9.4 step 2).
func TestTranscriptRetentionJob_NoTurnDeletedBefore180DaysAfterEnded(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	now := time.Now().UTC()
	line := now.Add(-180 * 24 * time.Hour)
	content := testutil.SeedContent(t, pool)
	parts := jobs.NewPgPartitionStore(postgres.NewPartitionRepo(pool))

	type seeded struct {
		offset  time.Duration
		ended   time.Time
		session uuid.UUID
		turn    uuid.UUID
	}
	var all []seeded
	have := map[string]bool{}
	for off := -6 * time.Hour; off <= 6*time.Hour; off += 10 * time.Minute {
		ended := line.Add(off)
		created := ended.Add(-65 * time.Minute)
		for _, at := range []time.Time{created, ended} {
			if name := jobs.PartitionName("session_turns", at); !have[name] {
				have[name] = true
				_ = parts.Ensure(ctx, "session_turns", at) // the month of "now" already exists: IF NOT EXISTS
			}
		}
		uid := testutil.SeedUser(t, pool, nil)
		started := ended.Add(-5 * time.Minute)
		sid := testutil.SeedSession(t, pool, uid, content, testutil.Row{
			"status": "COMPLETED", "end_reason": "USER_ENDED", "duration_seconds": 300, "starting_at": started.Add(-time.Second),
			"started_at": started, "expires_at": started.Add(300 * time.Second), "ending_at": ended, "ended_at": ended,
			"created_at": created.Add(-time.Minute),
		})
		tid := uuid.Must(uuid.NewV7())
		testutil.MustInsert(ctx, t, pool, "session_turns", testutil.Row{
			"id": tid, "session_id": sid, "sequence_number": 1, "generation": 1, "created_at": created, "user_text": "テスト",
		})
		all = append(all, seeded{off, ended, sid, tid})
	}

	m := jobs.NewMetrics(prometheus.NewRegistry())
	job := jobs.NewTranscriptRetentionJob(postgres.NewRetentionRepo(pool), jobs.RetentionConfig{TranscriptDays: 180, SessionEventsDays: 365, ExpiredTokensDays: 7},
		m, clock.NewFake(now), nil)
	require.NoError(t, job.Run(ctx))

	deleted := 0
	for _, s := range all {
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM session_turns WHERE id = $1`, s.turn).Scan(&n))
		if n == 0 {
			deleted++
			assert.True(t, s.ended.Before(line), "turn deleted while its session ended only %s after the line (ended_at %s)", s.offset, s.ended)
		}
	}
	assert.Equal(t, float64(deleted), promtest.ToFloat64(m.JobItems.WithLabelValues("transcript_retention", "turns")))
	assert.Greater(t, deleted, 0, "the sweep must include sessions old enough to be purged")
	assert.Less(t, deleted, len(all), "and sessions that must be kept")
	// The margin holds back a few sessions that ended slightly more than 180 days ago: ended - 65 min
	// must be older than line - 2 h, i.e. ended < line - 55 min.
	for _, s := range all {
		var n int
		require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM session_turns WHERE id = $1`, s.turn).Scan(&n))
		wantGone := s.ended.Add(-65 * time.Minute).Before(line.Add(-2 * time.Hour))
		assert.Equal(t, wantGone, n == 0, "offset %s", s.offset)
	}

	// The summaries use the line itself, without margin.
	for _, s := range all {
		var purged *time.Time
		require.NoError(t, pool.QueryRow(ctx, `SELECT transcript_purged_at FROM sessions WHERE id = $1`, s.session).Scan(&purged))
		assert.Equal(t, s.ended.Before(line), purged != nil, fmt.Sprintf("summary purge at offset %s", s.offset))
	}
}

func TestTranscriptRetentionJob_RealDatabase_EventsAndTokens(t *testing.T) {
	pool := testutil.NewPostgres(t)
	ctx := context.Background()
	now := time.Now().UTC()
	content := testutil.SeedContent(t, pool)
	uid := testutil.SeedUser(t, pool, nil)
	sid := testutil.SeedSession(t, pool, uid, content, nil)
	parts := jobs.NewPgPartitionStore(postgres.NewPartitionRepo(pool))
	old, fresh := now.Add(-366*24*time.Hour), now.Add(-364*24*time.Hour)
	require.NoError(t, parts.Ensure(ctx, "session_events", old))
	require.NoError(t, parts.Ensure(ctx, "session_events", fresh))
	for _, at := range []time.Time{old, fresh} {
		testutil.MustInsert(ctx, t, pool, "session_events", testutil.Row{
			"id": uuid.Must(uuid.NewV7()), "session_id": sid, "event_type": "BARGE_IN", "actor": "REALTIME", "created_at": at,
		})
	}
	hash := func(id uuid.UUID) []byte { h := make([]byte, 32); copy(h, id[:]); return h }
	tok := uuid.Must(uuid.NewV7())
	testutil.MustInsert(ctx, t, pool, "refresh_tokens", testutil.Row{
		"id": tok, "user_id": uid, "family_id": tok, "token_hash": hash(tok),
		"created_at": now.Add(-40 * 24 * time.Hour), "expires_at": now.Add(-8 * 24 * time.Hour),
	})
	live := uuid.Must(uuid.NewV7())
	testutil.MustInsert(ctx, t, pool, "refresh_tokens", testutil.Row{
		"id": live, "user_id": uid, "family_id": live, "token_hash": hash(live),
		"created_at": now.Add(-time.Hour), "expires_at": now.Add(30 * 24 * time.Hour),
	})

	m := jobs.NewMetrics(prometheus.NewRegistry())
	job := jobs.NewTranscriptRetentionJob(postgres.NewRetentionRepo(pool), jobs.RetentionConfig{TranscriptDays: 180, SessionEventsDays: 365, ExpiredTokensDays: 7},
		m, clock.NewFake(now), nil)
	require.NoError(t, job.Run(ctx))
	require.NoError(t, job.Run(ctx), "idempotent")

	var n int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM session_events WHERE session_id = $1`, sid).Scan(&n))
	assert.Equal(t, 1, n, "the 366 day old event is gone, the 364 day old one stays")
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM refresh_tokens`).Scan(&n))
	assert.Equal(t, 1, n)
	assert.Equal(t, 1.0, promtest.ToFloat64(m.JobItems.WithLabelValues("transcript_retention", "events")))
	assert.Equal(t, 1.0, promtest.ToFloat64(m.JobItems.WithLabelValues("transcript_retention", "tokens")))
}
