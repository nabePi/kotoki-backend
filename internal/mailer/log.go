package mailer

import (
	"context"
	"log/slog"

	"github.com/nabePi/kotoki-backend/internal/auth"
)

// LogMailer writes mail to the log. Local/dev only: bodies contain reset tokens.
type LogMailer struct{ log *slog.Logger }

var _ auth.Mailer = (*LogMailer)(nil)

// NewLog returns a LogMailer.
func NewLog(log *slog.Logger) *LogMailer { return &LogMailer{log: log} }

// Send logs the message as mail.sent.
func (m *LogMailer) Send(ctx context.Context, msg auth.MailMessage) error {
	m.log.InfoContext(ctx, "mail.sent", "to", msg.To, "subject", msg.Subject, "body", msg.TextBody, "tag", msg.Tag)
	return nil
}
