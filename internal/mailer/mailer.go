// Package mailer holds the auth.Mailer adapters: log, smtp and an in-memory fake.
package mailer

import (
	"crypto/tls"
	"fmt"
	"log/slog"

	"github.com/nabePi/kotoki-backend/internal/auth"
)

// Config selects and configures an adapter (spec 3.9.3).
type Config struct {
	Mode, AppEnv, From, SMTPHost string
	SMTPPort                     int
	SMTPUsername, SMTPPassword   string
}

// New builds the adapter for cfg.Mode. The log adapter prints reset tokens, so
// it is refused when AppEnv is staging or prod.
func New(cfg Config, log *slog.Logger) (auth.Mailer, error) {
	switch cfg.Mode {
	case "log", "":
		if cfg.AppEnv == "staging" || cfg.AppEnv == "prod" {
			return nil, fmt.Errorf("mailer: MAIL_MODE=log is not allowed when APP_ENV is %s", cfg.AppEnv)
		}
		return NewLog(log), nil
	case "smtp":
		if cfg.SMTPHost == "" {
			return nil, fmt.Errorf("mailer: SMTP_HOST is required when MAIL_MODE=smtp")
		}
		return NewSMTP(cfg, &tls.Config{ServerName: cfg.SMTPHost, MinVersion: tls.VersionTLS12}), nil
	default:
		return nil, fmt.Errorf("mailer: unknown MAIL_MODE %q", cfg.Mode)
	}
}
