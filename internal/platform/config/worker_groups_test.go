package config

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/testutil"
)

func TestWorkerGroupDefaults(t *testing.T) {
	w, err := loadWorker(testutil.MinimalEnv(t))
	require.NoError(t, err)

	require.Equal(t, "amqp://guest:guest@localhost:5672/", w.Rabbit.URL.Reveal())

	require.Equal(t, 500*time.Millisecond, w.PollInterval)
	require.Equal(t, 100, w.BatchSize)
	require.Equal(t, 5*time.Second, w.ConfirmTimeout)
	require.Equal(t, 15*time.Second, w.TxTimeout)
	require.Equal(t, 10, w.MaxAttempts)

	require.Equal(t, 10*time.Second, w.SweeperInterval)
	require.Equal(t, 15*time.Second, w.SweeperExpiryGrace)
	require.Equal(t, 45*time.Second, w.SweeperHeartbeatTimeout)
	require.Equal(t, 180, w.RetentionTranscriptDays)
	require.Equal(t, 365, w.RetentionSessionEventsDays)
	require.Equal(t, 7, w.RetentionOutboxPublishedDays)
	require.Equal(t, 30, w.RetentionProcessedEventsDays)
	require.Equal(t, 7, w.RetentionExpiredTokensDays)
	require.Equal(t, time.Hour, w.AccountPurgeDelay)
}

func TestWorkerGroupOverrides(t *testing.T) {
	w, err := loadWorker(testutil.MinimalEnv(t, map[string]string{
		"RABBITMQ_URL": "amqp://u:p@mq:5672/%2Fkotoki", "OUTBOX_POLL_INTERVAL": "2s", "OUTBOX_BATCH_SIZE": "7",
		"OUTBOX_MAX_ATTEMPTS": "3", "SWEEPER_HEARTBEAT_TIMEOUT": "1m", "RETENTION_TRANSCRIPT_DAYS": "30",
		"RETENTION_EXPIRED_TOKENS_DAYS": "9", "ACCOUNT_PURGE_DELAY": "2h",
	}))
	require.NoError(t, err)
	require.Equal(t, "amqp://u:p@mq:5672/%2Fkotoki", w.Rabbit.URL.Reveal())
	require.Equal(t, 2*time.Second, w.PollInterval)
	require.Equal(t, 7, w.BatchSize)
	require.Equal(t, 3, w.MaxAttempts)
	require.Equal(t, time.Minute, w.SweeperHeartbeatTimeout)
	require.Equal(t, 30, w.RetentionTranscriptDays)
	require.Equal(t, 9, w.RetentionExpiredTokensDays)
	require.Equal(t, 2*time.Hour, w.AccountPurgeDelay)
}

func TestValidateRabbit(t *testing.T) {
	const strictPG = "postgres://u:p@db:5432/kotoki?sslmode=verify-full"
	strict := map[string]string{
		"APP_ENV": "staging", "DB_URL": strictPG, "REDIS_URL": "rediss://h:6379/0",
	}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range strict {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	tests := []struct {
		name string
		env  map[string]string
		want string // "" = passes
	}{
		{"staging amqp", with(map[string]string{"RABBITMQ_URL": "amqp://u:p@mq:5672/%2Fkotoki"}), "RABBITMQ_URL"},
		{"prod amqp", with(map[string]string{"APP_ENV": "prod", "RABBITMQ_URL": "amqp://u:p@mq:5672/%2Fkotoki"}), "RABBITMQ_URL"},
		{"staging amqps ok", with(map[string]string{"RABBITMQ_URL": "amqps://u:p@mq:5671/%2Fkotoki"}), ""},
		{"local amqp ok", map[string]string{"RABBITMQ_URL": "amqp://u:p@localhost:5672/"}, ""},
		{"dev amqp ok", map[string]string{"APP_ENV": "dev", "RABBITMQ_URL": "amqp://u:p@mq:5672/"}, ""},
		{"local wrong scheme", map[string]string{"RABBITMQ_URL": "http://localhost:5672/"}, "RABBITMQ_URL"},
		{"poll zero", map[string]string{"OUTBOX_POLL_INTERVAL": "0s"}, "OUTBOX_POLL_INTERVAL"},
		{"confirm negative", map[string]string{"OUTBOX_CONFIRM_TIMEOUT": "-1s"}, "OUTBOX_CONFIRM_TIMEOUT"},
		{"tx zero", map[string]string{"OUTBOX_TX_TIMEOUT": "0s"}, "OUTBOX_TX_TIMEOUT"},
		{"batch zero", map[string]string{"OUTBOX_BATCH_SIZE": "0"}, "OUTBOX_BATCH_SIZE"},
		{"attempts zero", map[string]string{"OUTBOX_MAX_ATTEMPTS": "0"}, "OUTBOX_MAX_ATTEMPTS"},
		{"sweeper interval zero", map[string]string{"SWEEPER_INTERVAL": "0s"}, "SWEEPER_INTERVAL"},
		{"sweeper grace zero", map[string]string{"SWEEPER_EXPIRY_GRACE": "0s"}, "SWEEPER_EXPIRY_GRACE"},
		{"sweeper hb zero", map[string]string{"SWEEPER_HEARTBEAT_TIMEOUT": "0s"}, "SWEEPER_HEARTBEAT_TIMEOUT"},
		{"purge delay zero", map[string]string{"ACCOUNT_PURGE_DELAY": "0s"}, "ACCOUNT_PURGE_DELAY"},
		{"transcript days zero", map[string]string{"RETENTION_TRANSCRIPT_DAYS": "0"}, "RETENTION_TRANSCRIPT_DAYS"},
		{"events days zero", map[string]string{"RETENTION_SESSION_EVENTS_DAYS": "0"}, "RETENTION_SESSION_EVENTS_DAYS"},
		{"outbox days zero", map[string]string{"RETENTION_OUTBOX_PUBLISHED_DAYS": "0"}, "RETENTION_OUTBOX_PUBLISHED_DAYS"},
		{"processed days zero", map[string]string{"RETENTION_PROCESSED_EVENTS_DAYS": "0"}, "RETENTION_PROCESSED_EVENTS_DAYS"},
		{"tokens days zero", map[string]string{"RETENTION_EXPIRED_TOKENS_DAYS": "0"}, "RETENTION_EXPIRED_TOKENS_DAYS"},
		{"tokens days too big", map[string]string{"RETENTION_EXPIRED_TOKENS_DAYS": "3651"}, "RETENTION_EXPIRED_TOKENS_DAYS"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadWorker(testutil.MinimalEnv(t, tc.env))
			if tc.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.want)
		})
	}
}

func TestRabbitURLRedacted(t *testing.T) {
	w, err := loadWorker(testutil.MinimalEnv(t, map[string]string{"RABBITMQ_URL": "amqp://secretuser:secretpass@mq:5672/"}))
	require.NoError(t, err)
	for _, f := range []string{"%v", "%+v"} {
		require.NotContains(t, fmt.Sprintf(f, w), "secretpass", f)
		require.NotContains(t, fmt.Sprintf(f, w.Rabbit), "secretpass", f)
	}
}
