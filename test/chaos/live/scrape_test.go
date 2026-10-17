package live_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/test/chaos/live"
	"github.com/nabePi/kotoki-backend/test/load/promtext"
)

var t0 = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

func s(name string, v float64, kv ...string) promtext.Sample {
	l := map[string]string{}
	for i := 0; i < len(kv); i += 2 {
		l[kv[i]] = kv[i+1]
	}
	return promtext.Sample{Name: name, Labels: l, Value: v}
}

// feed records one snapshot per element at t0 + i*10s, all from one source.
func feed(snaps ...promtext.Samples) *live.Sampler {
	sm := live.NewSampler(nil, 0)
	for i, sn := range snaps {
		sm.Observe(t0.Add(time.Duration(i)*10*time.Second), "pod-a", sn)
	}
	return sm
}

func q(t *testing.T, sm *live.Sampler, expr string) (float64, error) {
	t.Helper()
	return sm.Query(t.Context(), expr)
}

func TestQuery_IncreaseSumsSeriesAndStartsAtZeroForANewSeries(t *testing.T) {
	sm := feed(
		promtext.Samples{s("kotoki_provider_errors_total", 5, "capability", "STT", "kind", "TIMEOUT")},
		promtext.Samples{s("kotoki_provider_errors_total", 9, "capability", "STT", "kind", "TIMEOUT")},
		promtext.Samples{s("kotoki_provider_errors_total", 12, "capability", "STT", "kind", "TIMEOUT"),
			s("kotoki_provider_errors_total", 4, "capability", "STT", "kind", "UNAVAILABLE"), // appears mid-run: counts from 0
			s("kotoki_provider_errors_total", 100, "capability", "LLM", "kind", "TIMEOUT")},
	)
	v, err := q(t, sm, `sum(increase(kotoki_provider_errors_total{capability="STT"}[30m]))`)
	require.NoError(t, err)
	require.InDelta(t, (12-5)+4, v, 0)
	v, err = q(t, sm, `sum(increase(kotoki_provider_errors_total{capability!="STT"}[30m]))`)
	require.NoError(t, err)
	require.InDelta(t, 100, v, 0, "a series that first appears after the window start counts in full")
}

func TestQuery_IncreaseHandlesCounterReset(t *testing.T) {
	sm := feed(
		promtext.Samples{s("c_total", 10)}, promtext.Samples{s("c_total", 14)},
		promtext.Samples{s("c_total", 3)}, // restart
		promtext.Samples{s("c_total", 5)},
	)
	// the metric must be a kotoki name only in the table test; the sampler accepts any name
	v, err := q(t, sm, `sum(increase(c_total[30m]))`)
	require.NoError(t, err)
	require.InDelta(t, 4+3+2, v, 0)
}

func TestQuery_IncreaseWindowStartsInsideHistory(t *testing.T) {
	sm := feed(
		promtext.Samples{s("c_total", 0)}, promtext.Samples{s("c_total", 10)},
		promtext.Samples{s("c_total", 30)}, promtext.Samples{s("c_total", 60)},
	)
	// last snapshot at +30s; a 15s window starts at +15s, so the first snapshot inside it is +20s (30)
	v, err := q(t, sm, `sum(increase(c_total[15s]))`)
	require.NoError(t, err)
	require.InDelta(t, 30, v, 0)
}

func TestQuery_MaxOverTimeAndBare(t *testing.T) {
	sm := feed(
		promtext.Samples{s("state", 0, "provider", "A"), s("state", 0, "provider", "B")},
		promtext.Samples{s("state", 1, "provider", "A"), s("state", 0, "provider", "B")},
		promtext.Samples{s("state", 2, "provider", "A"), s("state", 0, "provider", "B")},
		promtext.Samples{s("state", 0, "provider", "A"), s("state", 0, "provider", "B")},
	)
	v, err := q(t, sm, `max(max_over_time(state[30m]))`)
	require.NoError(t, err)
	require.InDelta(t, 2, v, 0)
	v, err = q(t, sm, `min(max_over_time(state[30m]))`)
	require.NoError(t, err)
	require.InDelta(t, 0, v, 0, "per series first, then across series")
	v, err = q(t, sm, `max(state)`)
	require.NoError(t, err)
	require.InDelta(t, 0, v, 0, "a bare selector reads the latest snapshot")
	v, err = q(t, sm, `sum(max_over_time(state{provider="A"}[30m]))`)
	require.NoError(t, err)
	require.InDelta(t, 2, v, 0)
}

func TestQuery_BareSelectorIgnoresASeriesThatIsGone(t *testing.T) {
	sm := feed(
		promtext.Samples{s("state", 2, "provider", "A"), s("state", 0, "provider", "B")},
		promtext.Samples{s("state", 0, "provider", "B")}, // A vanished (its pod went away)
	)
	v, err := q(t, sm, `max(state)`)
	require.NoError(t, err)
	require.InDelta(t, 0, v, 0, "a vanished series is not the current value")
}

func TestQuery_SeriesOfDifferentSourcesStayApart(t *testing.T) {
	sm := live.NewSampler(nil, 0)
	for i, v := range []float64{1, 4} {
		sm.Observe(t0.Add(time.Duration(i)*time.Second), "pod-a", promtext.Samples{s("c_total", v)})
		sm.Observe(t0.Add(time.Duration(i)*time.Second), "pod-b", promtext.Samples{s("c_total", v*10)})
	}
	got, err := sm.Query(t.Context(), `sum(increase(c_total[30m]))`)
	require.NoError(t, err)
	require.InDelta(t, 3+30, got, 0)
}

func TestQuery_NoDataIsAnErrorUnlessOrVector0(t *testing.T) {
	sm := feed(promtext.Samples{s("known", 1)}, promtext.Samples{s("known", 2)})
	_, err := q(t, sm, `sum(increase(missing_total[30m]))`)
	require.ErrorIs(t, err, live.ErrNoData)
	_, err = q(t, sm, `max(missing)`)
	require.ErrorIs(t, err, live.ErrNoData)
	_, err = q(t, sm, `sum(increase(known{a="x"}[30m]))`)
	require.ErrorIs(t, err, live.ErrNoData, "a selector that matches nothing is no data too")

	v, err := q(t, sm, `sum(increase(missing_total[30m])) or vector(0)`)
	require.NoError(t, err)
	require.InDelta(t, 0, v, 0)
	v, err = q(t, sm, `sum(increase(known[30m])) or vector(0)`)
	require.NoError(t, err)
	require.InDelta(t, 1, v, 0, "or vector(0) does not replace a value that exists")

	empty := live.NewSampler(nil, 0)
	_, err = empty.Query(t.Context(), `max(known)`)
	require.ErrorIs(t, err, live.ErrNoData, "nothing scraped yet")
}

func TestQuery_RejectsWhatItCannotEvaluate(t *testing.T) {
	sm := feed(promtext.Samples{s("known", 1)})
	for _, e := range []string{
		``, `known`, `rate(known[1m])`, `histogram_quantile(0.95, sum by (le) (increase(h_bucket[5m])))`,
		`sum(increase(known[30m])) / 2`, `avg(known)`, `sum(known{a=~"x"})`, `max(max_over_time(known[30m])) >= 1`,
	} {
		_, err := q(t, sm, e)
		require.Error(t, err, e)
		require.NotErrorIs(t, err, live.ErrNoData, "%q: an unsupported expression is not 'no data'", e)
	}
}

func TestQuery_NegativeMatcherAndTwoMatchers(t *testing.T) {
	sm := feed(promtext.Samples{
		s("m_total", 1, "a", "x", "b", "1"), s("m_total", 2, "a", "x", "b", "2"), s("m_total", 4, "a", "y", "b", "1"),
	})
	v, err := q(t, sm, `sum(m_total{a="x",b!="1"})`)
	require.NoError(t, err)
	require.InDelta(t, 2, v, 0)
}

func TestSampler_ScrapesURLsAndSurvivesAFailingOne(t *testing.T) {
	var hits atomic.Int32
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = fmt.Fprintf(w, "# TYPE c_total counter\nc_total{a=\"x\"} %d\n", hits.Load())
	}))
	defer ok.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "boom", 500) }))
	defer bad.Close()

	sm := live.NewSampler([]string{ok.URL + "/metrics", bad.URL + "/metrics"}, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(t.Context())
	sm.Start(ctx)
	require.Eventually(t, func() bool {
		v, err := sm.Query(t.Context(), `sum(increase(c_total[30m]))`)
		return err == nil && v >= 2
	}, 5*time.Second, 20*time.Millisecond)
	cancel()
	sm.Wait()
	require.NotEmpty(t, sm.Errors(), "the failing endpoint is reported, not hidden")
	require.True(t, strings.Contains(sm.Errors()[0], "500") || strings.Contains(sm.Errors()[0], "status"), sm.Errors()[0])
}

func TestSampler_FirstSnapshotFailureIsReported(t *testing.T) {
	sm := live.NewSampler([]string{"http://127.0.0.1:1/metrics"}, time.Hour)
	require.Error(t, sm.Prime(t.Context()), "a baseline that cannot be taken must stop the run before it starts")
}
