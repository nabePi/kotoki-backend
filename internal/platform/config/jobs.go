package config

import "time"

// Jobs holds the scheduled-job settings of the worker: sweeper thresholds (R-9), retention periods
// (R-21) and the account purge delay (spec 9.9, 9.11, 13.2.11, 13.9.3).
type Jobs struct {
	SweeperInterval         time.Duration `env:"SWEEPER_INTERVAL"`
	SweeperExpiryGrace      time.Duration `env:"SWEEPER_EXPIRY_GRACE"`
	SweeperHeartbeatTimeout time.Duration `env:"SWEEPER_HEARTBEAT_TIMEOUT"`

	RetentionTranscriptDays      int `env:"RETENTION_TRANSCRIPT_DAYS"`
	RetentionSessionEventsDays   int `env:"RETENTION_SESSION_EVENTS_DAYS"`
	RetentionOutboxPublishedDays int `env:"RETENTION_OUTBOX_PUBLISHED_DAYS"`
	RetentionProcessedEventsDays int `env:"RETENTION_PROCESSED_EVENTS_DAYS"`
	RetentionExpiredTokensDays   int `env:"RETENTION_EXPIRED_TOKENS_DAYS"`

	AccountPurgeDelay time.Duration `env:"ACCOUNT_PURGE_DELAY"`
}

func defaultJobs() Jobs {
	return Jobs{
		SweeperInterval: 10 * time.Second, SweeperExpiryGrace: 15 * time.Second, SweeperHeartbeatTimeout: 45 * time.Second,
		RetentionTranscriptDays: 180, RetentionSessionEventsDays: 365, RetentionOutboxPublishedDays: 7,
		RetentionProcessedEventsDays: 30, RetentionExpiredTokensDays: 7,
		AccountPurgeDelay: time.Hour,
	}
}

func (j Jobs) validate(v *validator) {
	v.positive("SWEEPER_INTERVAL", j.SweeperInterval)
	v.positive("SWEEPER_EXPIRY_GRACE", j.SweeperExpiryGrace)
	v.positive("SWEEPER_HEARTBEAT_TIMEOUT", j.SweeperHeartbeatTimeout)
	v.positive("ACCOUNT_PURGE_DELAY", j.AccountPurgeDelay)
	for _, d := range []struct {
		name string
		val  int
	}{
		{"RETENTION_TRANSCRIPT_DAYS", j.RetentionTranscriptDays},
		{"RETENTION_SESSION_EVENTS_DAYS", j.RetentionSessionEventsDays},
		{"RETENTION_OUTBOX_PUBLISHED_DAYS", j.RetentionOutboxPublishedDays},
		{"RETENTION_PROCESSED_EVENTS_DAYS", j.RetentionProcessedEventsDays},
		{"RETENTION_EXPIRED_TOKENS_DAYS", j.RetentionExpiredTokensDays},
	} {
		if d.val < 1 || d.val > 3650 {
			v.fail(d.name, "must be within 1–3650, got %d", d.val)
		}
	}
}
