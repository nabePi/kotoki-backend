//go:build integration

package postgres_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/codes"

	"github.com/nabePi/kotoki-backend/internal/platform/config"
	"github.com/nabePi/kotoki-backend/internal/platform/errs"
	"github.com/nabePi/kotoki-backend/internal/platform/metrics"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/postgres/sqlcgen"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func testCfg(url string) config.DB {
	return config.DB{
		URL: config.Secret(url), MaxConns: 4, MinConns: 0,
		MaxConnLifetime: time.Hour, MaxConnIdleTime: time.Minute, ConnectTimeout: 5 * time.Second,
		StatementTimeout: 5 * time.Second, LockTimeout: 3 * time.Second, IdleInTxTimeout: 15 * time.Second,
	}
}

func newPool(t *testing.T) (*pgxpool.Pool, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	var pool *pgxpool.Pool
	m := metrics.NewDB(reg, func() metrics.PoolStat {
		if pool == nil {
			return metrics.PoolStat{}
		}
		return postgres.PoolStat(pool)
	})
	var err error
	pool, err = postgres.NewPool(context.Background(), testCfg(testutil.NewEmptyPostgresDSN(t)),
		postgres.PoolOptions{ServiceName: "kotoki-test", Metrics: m})
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	return pool, reg
}

func TestPoolRuntimeParams(t *testing.T) {
	pool, _ := newPool(t)
	want := map[string]string{
		"statement_timeout": "5s", "lock_timeout": "3s",
		"idle_in_transaction_session_timeout": "15s", "application_name": "kotoki-test",
	}
	for name, v := range want {
		var got string
		require.NoError(t, pool.QueryRow(context.Background(), "SHOW "+name).Scan(&got))
		require.Equal(t, v, got, name)
	}
}

func TestQuerySpanAndMetric(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	pool, reg := newPool(t)
	n, err := sqlcgen.New(pool).Ping(context.Background())
	require.NoError(t, err)
	require.EqualValues(t, 1, n)

	span := rec.Find("db.Ping")
	attrs := map[string]string{}
	for _, kv := range span.Attributes {
		attrs[string(kv.Key)] = kv.Value.String()
		require.NotContains(t, strings.ToUpper(kv.Value.String()), "SELECT", string(kv.Key))
	}
	require.Equal(t, "postgresql", attrs["db.system"])
	require.Equal(t, "Ping", attrs["db.operation.name"])

	fams, err := reg.Gather()
	require.NoError(t, err)
	var count uint64
	for _, f := range fams {
		if f.GetName() != "kotoki_db_query_duration_seconds" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "query" && l.GetValue() == "Ping" {
					count += m.GetHistogram().GetSampleCount()
				}
			}
		}
	}
	require.EqualValues(t, 1, count)
}

func TestQueryErrorSpan(t *testing.T) {
	rec := testutil.NewTraceRecorder(t)
	pool, _ := newPool(t)
	_, err := pool.Exec(context.Background(), "SELEKT 1")
	require.Error(t, err)
	require.Equal(t, codes.Error, rec.Find("db.query").Status.Code)
}

func TestPoolBadURL(t *testing.T) {
	_, err := postgres.NewPool(context.Background(), testCfg("not a url"), postgres.PoolOptions{ServiceName: "x"})
	require.Error(t, err)
	require.Equal(t, errs.CodeServiceUnavailable, errs.CodeOf(err))
}
