package jobs_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/jobs"
	"github.com/nabePi/kotoki-backend/internal/platform/clock"
)

func utc(y int, m time.Month, d, h, mi int) time.Time {
	return time.Date(y, m, d, h, mi, 0, 0, time.UTC)
}

func monthPart(table string, y int, m time.Month) jobs.Partition {
	lo := utc(y, m, 1, 0, 0)
	return jobs.Partition{Name: jobs.PartitionName(table, lo), Lower: lo, Upper: lo.AddDate(0, 1, 0)}
}

func TestPartitionName(t *testing.T) {
	assert.Equal(t, "session_turns_p202701", jobs.PartitionName("session_turns", utc(2027, 1, 1, 0, 0)))
	assert.Equal(t, "session_events_p202612", jobs.PartitionName("session_events", utc(2026, 12, 31, 23, 59)))
	// A time zone in which it is already January must still name the UTC month.
	wib := time.FixedZone("WIB", 7*3600)
	assert.Equal(t, "session_turns_p202612", jobs.PartitionName("session_turns", time.Date(2027, 1, 1, 6, 0, 0, 0, wib)))
}

func TestPlanCreate_FourMonthsUTC(t *testing.T) {
	names := func(now time.Time) []string {
		var out []string
		for _, m := range jobs.PlanCreate(now) {
			out = append(out, jobs.PartitionName("t", m))
		}
		return out
	}
	assert.Equal(t, []string{"t_p202612", "t_p202701", "t_p202702", "t_p202703"}, names(utc(2026, 12, 15, 12, 0)))
	assert.Equal(t, []string{"t_p202612", "t_p202701", "t_p202702", "t_p202703"}, names(utc(2026, 12, 31, 23, 30)),
		"23:30 UTC on Dec 31 is still December")
	// 00:30 on Jan 1 in WIB is Dec 31 17:30 UTC: the month is the UTC one.
	wib := time.FixedZone("WIB", 7*3600)
	assert.Equal(t, []string{"t_p202612", "t_p202701", "t_p202702", "t_p202703"}, names(time.Date(2027, 1, 1, 0, 30, 0, 0, wib)))
	for _, m := range jobs.PlanCreate(utc(2026, 12, 15, 12, 0)) {
		assert.Equal(t, time.UTC, m.Location())
		assert.Equal(t, 1, m.Day())
	}
}

func TestParseBound(t *testing.T) {
	lo, up, err := jobs.ParseBound("FOR VALUES FROM ('2026-10-01 00:00:00+00') TO ('2026-11-01 00:00:00+00')")
	require.NoError(t, err)
	assert.True(t, lo.Equal(utc(2026, 10, 1, 0, 0)))
	assert.True(t, up.Equal(utc(2026, 11, 1, 0, 0)))
	assert.Equal(t, time.UTC, lo.Location())

	// Session time zone other than UTC: pg_get_expr prints the offset.
	lo, up, err = jobs.ParseBound("FOR VALUES FROM ('2026-10-01 07:00:00+07') TO ('2026-11-01 07:00:00+07')")
	require.NoError(t, err)
	assert.True(t, lo.Equal(utc(2026, 10, 1, 0, 0)))
	assert.True(t, up.Equal(utc(2026, 11, 1, 0, 0)))

	lo, _, err = jobs.ParseBound("FOR VALUES FROM ('2026-10-01 05:30:00.5+05:30') TO ('2026-11-01 00:00:00+00')")
	require.NoError(t, err)
	assert.True(t, lo.Equal(utc(2026, 10, 1, 0, 0).Add(500*time.Millisecond)))

	for _, bad := range []string{"DEFAULT", "", "FOR VALUES FROM (MINVALUE) TO ('2026-11-01 00:00:00+00')",
		"FOR VALUES IN ('a')", "FOR VALUES FROM ('garbage') TO ('2026-11-01 00:00:00+00')"} {
		_, _, err := jobs.ParseBound(bad)
		assert.Error(t, err, bad)
	}
}

func names(ps []jobs.Partition) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

func TestExpiredPartitions_Turns(t *testing.T) {
	now := utc(2027, 6, 15, 0, 0) // cutoff = now-180d-2h = 2026-12-17 22:00Z
	parts := []jobs.Partition{
		monthPart("session_turns", 2026, time.November), // upper 2026-12-01
		monthPart("session_turns", 2026, time.December), // upper 2027-01-01
	}
	got := jobs.ExpiredPartitions("session_turns", parts, now, 180)
	assert.Equal(t, []string{"session_turns_p202611"}, names(got))

	// The 2 hour margin decides at the boundary: upper 2027-01-01 vs cutoff.
	assert.Empty(t, jobs.ExpiredPartitions("session_turns", parts[1:], utc(2027, 6, 29, 22, 0), 180),
		"upper == now-180d-2h is not older than the cutoff")
	assert.Empty(t, jobs.ExpiredPartitions("session_turns", parts[1:], utc(2027, 6, 30, 1, 59), 180),
		"upper is 180d+1h59m old: inside the margin")
	assert.Len(t, jobs.ExpiredPartitions("session_turns", parts[1:], utc(2027, 7, 1, 2, 1), 180), 1)
}

func TestExpiredPartitions_Events365(t *testing.T) {
	now := utc(2027, 12, 15, 0, 0) // cutoff = now-365d = 2026-12-15
	parts := []jobs.Partition{
		monthPart("session_events", 2026, time.November), // upper 2026-12-01 -> drop
		monthPart("session_events", 2026, time.December), // upper 2027-01-01 -> keep
	}
	assert.Equal(t, []string{"session_events_p202611"}, names(jobs.ExpiredPartitions("session_events", parts, now, 365)))
	// events have no 2 hour margin: upper exactly at the cutoff is kept, 1 s older is dropped.
	assert.Empty(t, jobs.ExpiredPartitions("session_events", parts[1:], utc(2028, 1, 1, 0, 0), 365))
	assert.Len(t, jobs.ExpiredPartitions("session_events", parts[1:], utc(2028, 1, 1, 0, 1), 365), 1)
}

func TestExpiredPartitions_NeverDropsCurrentOrFutureMonth(t *testing.T) {
	now := utc(2027, 6, 15, 12, 0)
	parts := []jobs.Partition{
		monthPart("session_turns", 2027, time.May),    // upper == start of current month
		monthPart("session_turns", 2027, time.June),   // current
		monthPart("session_turns", 2027, time.July),   // future
		monthPart("session_turns", 2027, time.August), // future
	}
	for _, table := range []string{"session_turns", "session_events"} {
		assert.Empty(t, jobs.ExpiredPartitions(table, parts, now, 0), table+" with retention 0")
	}
	// Right after midnight on the 1st the previous month is older than the margin, still not dropped.
	assert.Empty(t, jobs.ExpiredPartitions("session_turns", parts, utc(2027, 6, 1, 5, 0), 0))
	// Old partitions are dropped while the guard keeps the rest.
	old := append([]jobs.Partition{monthPart("session_turns", 2026, time.January)}, parts...)
	assert.Equal(t, []string{"session_turns_p202601"}, names(jobs.ExpiredPartitions("session_turns", old, now, 0)))
}

func TestExpiredPartitions_UnknownTable(t *testing.T) {
	assert.Empty(t, jobs.ExpiredPartitions("sessions", []jobs.Partition{monthPart("sessions", 2020, time.January)}, utc(2027, 6, 15, 0, 0), 1))
}

type fakePartStore struct {
	parts   map[string][]jobs.Partition
	listErr error
	ensured []string
	dropped []string
	ensErr  map[string]error
	dropErr map[string]error
}

func newFakePartStore() *fakePartStore {
	return &fakePartStore{parts: map[string][]jobs.Partition{}, ensErr: map[string]error{}, dropErr: map[string]error{}}
}

func (f *fakePartStore) List(_ context.Context, table string) ([]jobs.Partition, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]jobs.Partition(nil), f.parts[table]...), nil
}

func (f *fakePartStore) Ensure(_ context.Context, table string, month time.Time) error {
	name := jobs.PartitionName(table, month)
	if err := f.ensErr[name]; err != nil {
		return err
	}
	f.ensured = append(f.ensured, name)
	lo := time.Date(month.Year(), month.Month(), 1, 0, 0, 0, 0, time.UTC)
	f.parts[table] = append(f.parts[table], jobs.Partition{Name: name, Lower: lo, Upper: lo.AddDate(0, 1, 0)})
	return nil
}

func (f *fakePartStore) Drop(_ context.Context, table string, p jobs.Partition) error {
	if err := f.dropErr[p.Name]; err != nil {
		return err
	}
	f.dropped = append(f.dropped, p.Name)
	var keep []jobs.Partition
	for _, q := range f.parts[table] {
		if q.Name != p.Name {
			keep = append(keep, q)
		}
	}
	f.parts[table] = keep
	return nil
}

var partCfg = jobs.PartitionConfig{RetentionTranscriptDays: 180, RetentionSessionEventsDays: 365}

func newPartJob(s jobs.PartitionStore, now time.Time) (*jobs.PartitionMaintenanceJob, *jobs.Metrics, *bytes.Buffer) {
	m := jobs.NewMetrics(prometheus.NewRegistry())
	logs := &bytes.Buffer{}
	return jobs.NewPartitionMaintenanceJob(s, partCfg, m, clock.NewFake(now), slog.New(slog.NewJSONHandler(logs, nil))), m, logs
}

func TestJob_CreatesMissing_IgnoresExisting(t *testing.T) {
	now := utc(2026, 12, 15, 12, 0)
	s := newFakePartStore()
	s.parts["session_turns"] = []jobs.Partition{monthPart("session_turns", 2026, time.December), monthPart("session_turns", 2027, time.January)}
	job, m, _ := newPartJob(s, now)
	require.NoError(t, job.Run(context.Background()))

	assert.ElementsMatch(t, []string{
		"session_turns_p202702", "session_turns_p202703",
		"session_events_p202612", "session_events_p202701", "session_events_p202702", "session_events_p202703",
	}, s.ensured)
	assert.Equal(t, 2.0, promtest.ToFloat64(m.PartitionsCreated.WithLabelValues("session_turns")))
	assert.Equal(t, 4.0, promtest.ToFloat64(m.PartitionsCreated.WithLabelValues("session_events")))

	// A second run has nothing to do.
	s.ensured = nil
	require.NoError(t, job.Run(context.Background()))
	assert.Empty(t, s.ensured)
	assert.Empty(t, s.dropped)
}

func TestJob_CreateErrorLogged_ContinuesToDrop(t *testing.T) {
	now := utc(2027, 6, 15, 12, 0)
	s := newFakePartStore()
	s.parts["session_turns"] = []jobs.Partition{monthPart("session_turns", 2026, time.January), monthPart("session_turns", 2027, time.June)}
	s.ensErr["session_turns_p202707"] = errors.New("lock timeout")
	job, m, logs := newPartJob(s, now)
	err := job.Run(context.Background())

	require.Error(t, err, "the run is reported as failed")
	assert.Contains(t, err.Error(), "lock timeout")
	assert.Contains(t, logs.String(), `"level":"ERROR"`)
	assert.Contains(t, logs.String(), "session_turns_p202707")
	assert.Equal(t, []string{"session_turns_p202601"}, s.dropped, "the drop step still ran")
	assert.Contains(t, s.ensured, "session_turns_p202708", "later months were still created")
	assert.Contains(t, s.ensured, "session_events_p202706", "the other table was still handled")
	assert.Equal(t, 1.0, promtest.ToFloat64(m.PartitionsDropped.WithLabelValues("session_turns")))
}

func TestJob_DropErrorLogged_ContinuesWithOthers(t *testing.T) {
	now := utc(2027, 6, 15, 12, 0)
	s := newFakePartStore()
	s.parts["session_turns"] = []jobs.Partition{monthPart("session_turns", 2026, time.January), monthPart("session_turns", 2026, time.February)}
	s.dropErr["session_turns_p202601"] = errors.New("detach failed")
	job, _, logs := newPartJob(s, now)
	require.Error(t, job.Run(context.Background()))
	assert.Equal(t, []string{"session_turns_p202602"}, s.dropped)
	assert.Contains(t, logs.String(), "detach failed")
}

func TestJob_ListError_StillCreatesButDoesNotDrop(t *testing.T) {
	s := newFakePartStore()
	s.listErr = errors.New("catalog down")
	job, m, _ := newPartJob(s, utc(2027, 6, 15, 12, 0))
	require.Error(t, job.Run(context.Background()))
	assert.Len(t, s.ensured, 8, "creation does not depend on the listing")
	assert.Empty(t, s.dropped)
	assert.Equal(t, 0.0, promtest.ToFloat64(m.PartitionsCreated.WithLabelValues("session_turns")), "nothing is counted without a listing")
}

func TestJob_GaugesAndCounters(t *testing.T) {
	now := utc(2027, 6, 15, 12, 0)
	s := newFakePartStore()
	s.parts["session_turns"] = []jobs.Partition{
		monthPart("session_turns", 2026, time.January),
		monthPart("session_turns", 2027, time.June),
		monthPart("session_turns", 2027, time.July),
	}
	s.parts["session_events"] = []jobs.Partition{
		monthPart("session_events", 2025, time.January),
		monthPart("session_events", 2025, time.February),
	}
	job, m, _ := newPartJob(s, now)
	require.NoError(t, job.Run(context.Background()))

	// Lower > now: July, August, September (June starts before now).
	assert.Equal(t, 3.0, promtest.ToFloat64(m.PartitionMonthsAhead.WithLabelValues("session_turns")))
	assert.Equal(t, 3.0, promtest.ToFloat64(m.PartitionMonthsAhead.WithLabelValues("session_events")))
	assert.Equal(t, 2.0, promtest.ToFloat64(m.PartitionsCreated.WithLabelValues("session_turns")), "August, September")
	assert.Equal(t, 4.0, promtest.ToFloat64(m.PartitionsCreated.WithLabelValues("session_events")))
	assert.Equal(t, 1.0, promtest.ToFloat64(m.PartitionsDropped.WithLabelValues("session_turns")))
	assert.Equal(t, 2.0, promtest.ToFloat64(m.PartitionsDropped.WithLabelValues("session_events")))
	assert.Equal(t, fmt.Sprint([]string{"session_turns_p202601", "session_events_p202501", "session_events_p202502"}), fmt.Sprint(s.dropped))
}

func TestJob_UsesConfiguredRetention(t *testing.T) {
	now := utc(2027, 6, 15, 12, 0)
	s := newFakePartStore()
	s.parts["session_turns"] = []jobs.Partition{monthPart("session_turns", 2027, time.March)}
	s.parts["session_events"] = []jobs.Partition{monthPart("session_events", 2027, time.March)}
	m := jobs.NewMetrics(prometheus.NewRegistry())
	job := jobs.NewPartitionMaintenanceJob(s, jobs.PartitionConfig{RetentionTranscriptDays: 30, RetentionSessionEventsDays: 400}, m, clock.NewFake(now), nil)
	require.NoError(t, job.Run(context.Background()))
	assert.Equal(t, []string{"session_turns_p202703"}, s.dropped, "transcript retention applies to turns, event retention to events")
}
