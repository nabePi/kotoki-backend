package config

import "time"

// Outbox holds the outbox publisher settings (spec 9.2.4, 9.11, 13.2.6).
type Outbox struct {
	PollInterval   time.Duration `env:"OUTBOX_POLL_INTERVAL"`
	ConfirmTimeout time.Duration `env:"OUTBOX_CONFIRM_TIMEOUT"`
	TxTimeout      time.Duration `env:"OUTBOX_TX_TIMEOUT"`
	BatchSize      int           `env:"OUTBOX_BATCH_SIZE"`
	MaxAttempts    int           `env:"OUTBOX_MAX_ATTEMPTS"`
}

func defaultOutbox() Outbox {
	return Outbox{
		PollInterval: 500 * time.Millisecond, ConfirmTimeout: 5 * time.Second, TxTimeout: 15 * time.Second,
		BatchSize: 100, MaxAttempts: 10,
	}
}

func (o Outbox) validate(v *validator) {
	v.positive("OUTBOX_POLL_INTERVAL", o.PollInterval)
	v.positive("OUTBOX_CONFIRM_TIMEOUT", o.ConfirmTimeout)
	v.positive("OUTBOX_TX_TIMEOUT", o.TxTimeout)
	if o.BatchSize <= 0 {
		v.fail("OUTBOX_BATCH_SIZE", "must be > 0, got %d", o.BatchSize)
	}
	if o.MaxAttempts <= 0 {
		v.fail("OUTBOX_MAX_ATTEMPTS", "must be > 0, got %d", o.MaxAttempts)
	}
}
