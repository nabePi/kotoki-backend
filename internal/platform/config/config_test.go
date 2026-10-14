package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func TestDefaultsPerBinary(t *testing.T) {
	env := testutil.MinimalEnv(t)

	a, err := loadAPI(env)
	require.NoError(t, err)
	require.Equal(t, ":8080", a.HTTPAddr)
	require.Equal(t, ":9090", a.OpsAddr)
	require.EqualValues(t, 10, a.MaxConns)
	require.EqualValues(t, 2, a.MinConns)
	require.Equal(t, 5*time.Second, a.StatementTimeout)
	require.Equal(t, 3*time.Second, a.LockTimeout)
	require.Equal(t, 20, a.PoolSize)
	require.Equal(t, "kotoki-api", a.ServiceName)
	require.EqualValues(t, 65536, a.HTTPMaxBodyBytes)
	require.Equal(t, 20*time.Second, a.ShutdownTimeout)
	require.Equal(t, []string{"http://localhost:3000"}, a.CORSAllowedOrigins)
	require.True(t, a.MetricsEnabled)
	require.Equal(t, "postgres://u:p@localhost:5432/kotoki?sslmode=disable", a.DB.URL.Reveal())

	r, err := loadRealtime(env)
	require.NoError(t, err)
	require.Equal(t, ":8081", r.HTTPAddr)
	require.Equal(t, ":9091", r.OpsAddr)
	require.EqualValues(t, 6, r.MaxConns)
	require.EqualValues(t, 2, r.MinConns)
	require.Equal(t, 5*time.Second, r.StatementTimeout)
	require.Equal(t, 50, r.PoolSize)
	require.Equal(t, 95, r.ReadyMaxSessions)
	require.Equal(t, 100, r.MaxSessions)
	require.Equal(t, 120, r.HardMaxSessions)
	require.False(t, r.WSAllowEmptyOrigin)

	w, err := loadWorker(env)
	require.NoError(t, err)
	require.Equal(t, ":8081", w.HTTPAddr)
	require.EqualValues(t, 4, w.MaxConns)
	require.EqualValues(t, 1, w.MinConns)
	require.Equal(t, 30*time.Second, w.StatementTimeout)
	require.Equal(t, 10, w.PoolSize)
	require.Equal(t, 25*time.Second, w.ShutdownGrace)
	require.Equal(t, "kotoki-worker", w.ServiceName)
}

func TestLoadFromProcessEnv(t *testing.T) {
	testutil.ApplyEnv(t, testutil.MinimalEnv(t, map[string]string{"HTTP_ADDR": ":1234"}))
	a, err := LoadAPI()
	require.NoError(t, err)
	require.Equal(t, ":1234", a.HTTPAddr)
	_, err = LoadRealtime()
	require.NoError(t, err)
	_, err = LoadWorker()
	require.NoError(t, err)
}

func TestSecretRedacted(t *testing.T) {
	s := Secret("hunter2")
	var buf bytes.Buffer
	slog.New(slog.NewJSONHandler(&buf, nil)).Info("x", "db", s)
	require.Contains(t, buf.String(), "[REDACTED]")
	require.NotContains(t, buf.String(), "hunter2")
	require.Equal(t, "[REDACTED]", fmt.Sprintf("%v", s))
	require.Equal(t, "[REDACTED]", s.String())
	require.Equal(t, "hunter2", s.Reveal())

	a, err := loadAPI(testutil.MinimalEnv(t))
	require.NoError(t, err)
	require.NotContains(t, fmt.Sprintf("%+v", a), "u:p@")
}

func TestValidate(t *testing.T) {
	const (
		apiBin      = "api"
		realtimeBin = "realtime"
	)
	pg := "postgres://u:p@localhost:5432/kotoki?sslmode=disable"
	tests := []struct {
		name string
		bin  string
		env  map[string]string
		want string // "" = passes
	}{
		{"prod transcripts", apiBin, map[string]string{"APP_ENV": "prod", "LOG_TRANSCRIPTS": "true"}, "LOG_TRANSCRIPTS"},
		{"staging text", apiBin, map[string]string{"APP_ENV": "staging", "LOG_FORMAT": "text"}, "LOG_FORMAT"},
		{"local text ok", apiBin, map[string]string{"LOG_FORMAT": "text"}, ""},
		{"staging sslmode", apiBin, map[string]string{"APP_ENV": "staging", "DB_URL": pg}, "DB_URL"},
		{"prod redis", apiBin, map[string]string{"APP_ENV": "prod", "REDIS_URL": "redis://h:6379/0"}, "REDIS_URL"},
		{"prod cors http", apiBin, map[string]string{"APP_ENV": "prod", "CORS_ALLOWED_ORIGINS": "http://x.com"}, "CORS_ALLOWED_ORIGINS"},
		{"prod cors empty", apiBin, map[string]string{"APP_ENV": "prod", "CORS_ALLOWED_ORIGINS": ""}, "CORS_ALLOWED_ORIGINS"},
		{"local cors star", apiBin, map[string]string{"CORS_ALLOWED_ORIGINS": "*"}, "CORS_ALLOWED_ORIGINS"},
		{"local cors localhost ok", apiBin, map[string]string{"CORS_ALLOWED_ORIGINS": "http://localhost:3000"}, ""},
		{"prod ws empty origin", realtimeBin, map[string]string{"APP_ENV": "prod", "WS_ALLOW_EMPTY_ORIGIN": "true"}, "WS_ALLOW_EMPTY_ORIGIN"},
		{"prod ws origin http", realtimeBin, map[string]string{"APP_ENV": "prod", "WS_ALLOWED_ORIGINS": "http://x.com"}, "WS_ALLOWED_ORIGINS"},
		{"ready>max", realtimeBin, map[string]string{"REALTIME_READY_MAX_SESSIONS": "101"}, "REALTIME_READY_MAX_SESSIONS"},
		{"min>max", apiBin, map[string]string{"DB_MIN_CONNS": "11"}, "DB_MIN_CONNS"},
		{"lock zero", apiBin, map[string]string{"DB_LOCK_TIMEOUT": "0s"}, "DB_LOCK_TIMEOUT"},
		{"bad env", apiBin, map[string]string{"APP_ENV": "qa"}, "APP_ENV"},
		{"bad level", apiBin, map[string]string{"LOG_LEVEL": "trace"}, "LOG_LEVEL"},
		{"db url missing", apiBin, map[string]string{"DB_URL": ""}, "DB_URL"},
		{"redis missing", apiBin, map[string]string{"REDIS_URL": ""}, "REDIS_URL"},
		{"sentinel replaces redis url", apiBin, map[string]string{"REDIS_URL": "", "REDIS_SENTINEL_MASTER": "m", "REDIS_SENTINEL_ADDRS": "a:26379"}, ""},
		{"worker bogus handler env", "worker", map[string]string{"WORKER_HANDLERS": "outbox,bogus"}, "bogus"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env := testutil.MinimalEnv(t, tc.env)
			var err error
			switch tc.bin {
			case apiBin:
				_, err = loadAPI(env)
			case realtimeBin:
				_, err = loadRealtime(env)
			default:
				_, err = loadWorker(env)
			}
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestParseWorkerHandlers(t *testing.T) {
	got, err := ParseWorkerHandlers("outbox,sweeper", "")
	require.NoError(t, err)
	require.Equal(t, []string{"outbox", "sweeper"}, got)

	got, err = ParseWorkerHandlers("", "learner,summary")
	require.NoError(t, err)
	require.Equal(t, []string{"learner", "summary"}, got)

	got, err = ParseWorkerHandlers("cleanup", "learner")
	require.NoError(t, err)
	require.Equal(t, []string{"cleanup"}, got)

	got, err = ParseWorkerHandlers(" outbox , memory ", "")
	require.NoError(t, err)
	require.Equal(t, []string{"outbox", "memory"}, got)

	_, err = ParseWorkerHandlers("", "")
	require.Error(t, err)

	_, err = ParseWorkerHandlers("outbox,bogus", "")
	require.ErrorContains(t, err, "bogus")
}
