package mailer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
)

func TestNewFactory(t *testing.T) {
	log := slog.New(slog.DiscardHandler)

	m, err := New(Config{Mode: "log", AppEnv: "local"}, log)
	require.NoError(t, err)
	require.IsType(t, &LogMailer{}, m)

	_, err = New(Config{Mode: "smtp", AppEnv: "prod"}, log)
	require.Error(t, err, "smtp without host")
	_, err = New(Config{Mode: "x", AppEnv: "local"}, log)
	require.Error(t, err)

	for _, env := range []string{"prod", "staging"} {
		_, err = New(Config{Mode: "log", AppEnv: env}, log)
		require.Error(t, err, env)
	}

	m, err = New(Config{Mode: "smtp", AppEnv: "prod", SMTPHost: "smtp.example", SMTPPort: 587, From: "Kotoki <no-reply@kotoki.id>"}, log)
	require.NoError(t, err)
	require.IsType(t, &SMTPMailer{}, m)
}

type capture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *capture) Enabled(context.Context, slog.Level) bool { return true }
func (c *capture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
	return nil
}
func (c *capture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *capture) WithGroup(string) slog.Handler      { return c }

func TestLogMailerLogsMailSent(t *testing.T) {
	rec := &capture{}
	m := NewLog(slog.New(rec))
	require.NoError(t, m.Send(context.Background(), auth.MailMessage{
		To: "hana@example.com", Subject: "Hi", TextBody: "token=abc", Tag: "password_reset"}))

	require.Len(t, rec.records, 1)
	r := rec.records[0]
	require.Equal(t, "mail.sent", r.Message)
	got := map[string]string{}
	r.Attrs(func(a slog.Attr) bool { got[a.Key] = a.Value.String(); return true })
	require.Equal(t, map[string]string{"to": "hana@example.com", "subject": "Hi", "body": "token=abc", "tag": "password_reset"}, got)
}

func TestFakeMailerStoresMessagesConcurrent(t *testing.T) {
	f := NewFake()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = f.Send(context.Background(), auth.MailMessage{To: fmt.Sprintf("u%d@example.com", i)})
		}()
	}
	wg.Wait()
	require.Len(t, f.Messages(), 50)
	require.NotEmpty(t, f.Last().To)
	f.Reset()
	require.Empty(t, f.Messages())
	require.Equal(t, auth.MailMessage{}, f.Last())
}

func TestSMTPBuildMessage(t *testing.T) {
	raw := string(buildMessage("Kotoki <no-reply@kotoki.id>", auth.MailMessage{
		To: "hana@example.com", Subject: "パスワード再設定", TextBody: "halo\nbody"}))

	require.Contains(t, raw, "From: Kotoki <no-reply@kotoki.id>\r\n")
	require.Contains(t, raw, "To: hana@example.com\r\n")
	require.Contains(t, raw, "Subject: =?utf-8?q?")
	require.NotContains(t, raw, "パスワード")
	require.Contains(t, raw, "Content-Type: text/plain; charset=UTF-8\r\n")
	require.Contains(t, raw, "MIME-Version: 1.0\r\n")
	require.True(t, strings.HasSuffix(raw, "halo\r\nbody"), raw)
}
