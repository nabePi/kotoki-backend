package mailer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"

	"github.com/nabePi/kotoki-backend/internal/auth"
)

// SMTPMailer sends mail over SMTP; STARTTLS is mandatory.
type SMTPMailer struct {
	cfg Config
	tls *tls.Config
}

var _ auth.Mailer = (*SMTPMailer)(nil)

// NewSMTP returns an SMTPMailer using tlsCfg for the STARTTLS upgrade.
func NewSMTP(cfg Config, tlsCfg *tls.Config) *SMTPMailer { return &SMTPMailer{cfg: cfg, tls: tlsCfg} }

// buildMessage renders RFC 5322 headers (non-ASCII subjects RFC 2047) and a UTF-8 text body.
func buildMessage(from string, msg auth.MailMessage) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + msg.To + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("utf-8", msg.Subject) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(strings.ReplaceAll(strings.ReplaceAll(msg.TextBody, "\r\n", "\n"), "\n", "\r\n"))
	return []byte(b.String())
}

// Send delivers msg. Nothing but the greeting and EHLO crosses the wire before TLS.
func (m *SMTPMailer) Send(ctx context.Context, msg auth.MailMessage) error {
	from, err := mail.ParseAddress(m.cfg.From)
	if err != nil {
		return fmt.Errorf("mailer: invalid MAIL_FROM: %w", err)
	}
	addr := net.JoinHostPort(m.cfg.SMTPHost, strconv.Itoa(m.cfg.SMTPPort))
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("mailer: dial: %w", err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	c, err := smtp.NewClient(conn, m.cfg.SMTPHost)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("mailer: smtp handshake: %w", err)
	}
	defer func() { _ = c.Close() }()

	if ok, _ := c.Extension("STARTTLS"); !ok {
		return errors.New("mailer: server does not offer STARTTLS")
	}
	if err := c.StartTLS(m.tls); err != nil {
		return fmt.Errorf("mailer: starttls: %w", err)
	}
	if m.cfg.SMTPUsername != "" {
		if err := c.Auth(smtp.PlainAuth("", m.cfg.SMTPUsername, m.cfg.SMTPPassword, m.cfg.SMTPHost)); err != nil {
			return fmt.Errorf("mailer: auth: %w", err)
		}
	}
	to, err := mail.ParseAddress(msg.To)
	if err != nil {
		return fmt.Errorf("mailer: invalid recipient: %w", err)
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("mailer: MAIL FROM: %w", err)
	}
	if err := c.Rcpt(to.Address); err != nil {
		return fmt.Errorf("mailer: RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("mailer: DATA: %w", err)
	}
	if _, err := w.Write(buildMessage(m.cfg.From, msg)); err != nil {
		return fmt.Errorf("mailer: write body: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("mailer: finish body: %w", err)
	}
	return c.Quit()
}
