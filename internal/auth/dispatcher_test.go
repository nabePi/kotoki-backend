package auth_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/auth"
)

func msg(to string) auth.MailMessage {
	return auth.MailMessage{To: to, Subject: "s", TextBody: "b", Tag: "password_reset"}
}

func logTo(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type failingMailer struct {
	mu       sync.Mutex
	attempts int
}

func (f *failingMailer) Send(context.Context, auth.MailMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	return errors.New("smtp down")
}

type sleepRecorder struct {
	mu sync.Mutex
	d  []time.Duration
}

func (s *sleepRecorder) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d = append(s.d, d)
	return nil
}

type recordingMailer struct {
	mu   sync.Mutex
	msgs []auth.MailMessage
}

func (r *recordingMailer) Send(_ context.Context, m auth.MailMessage) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, m)
	return nil
}

func TestDispatcherDelivers(t *testing.T) {
	fake := &recordingMailer{}
	d := auth.NewMailDispatcher(fake, logTo(&bytes.Buffer{}), auth.DispatcherOptions{})
	for _, to := range []string{"a@x.id", "b@x.id", "c@x.id"} {
		require.True(t, d.Enqueue(msg(to)))
	}
	require.NoError(t, d.Close(context.Background()))
	require.Len(t, fake.msgs, 3)
}

func TestDispatcherRetries(t *testing.T) {
	var buf bytes.Buffer
	f := &failingMailer{}
	rec := &sleepRecorder{}
	d := auth.NewMailDispatcher(f, logTo(&buf), auth.DispatcherOptions{Sleep: rec.sleep})
	require.True(t, d.Enqueue(msg("hana@example.com")))
	require.NoError(t, d.Close(context.Background()))

	require.Equal(t, 4, f.attempts)
	require.Equal(t, []time.Duration{time.Second, 5 * time.Second, 15 * time.Second}, rec.d)
	require.Equal(t, 1, bytes.Count(buf.Bytes(), []byte("mail.dropped")))
	require.Contains(t, buf.String(), "level=ERROR")
	require.NotContains(t, buf.String(), "hana@example.com", "recipient must be masked in logs")
}

type deadlineMailer struct {
	mu       sync.Mutex
	deadline time.Time
	ok       bool
}

func (m *deadlineMailer) Send(ctx context.Context, _ auth.MailMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deadline, m.ok = ctx.Deadline()
	return nil
}

func TestDispatcherSendTimeout(t *testing.T) {
	m := &deadlineMailer{}
	d := auth.NewMailDispatcher(m, logTo(&bytes.Buffer{}), auth.DispatcherOptions{})
	start := time.Now()
	require.True(t, d.Enqueue(msg("a@x.id")))
	require.NoError(t, d.Close(context.Background()))
	require.True(t, m.ok, "Send must get a deadline")
	require.LessOrEqual(t, m.deadline.Sub(start), 10*time.Second+time.Second)
}

type blockingMailer struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingMailer) Send(context.Context, auth.MailMessage) error {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return nil
}

func TestDispatcherBufferFull(t *testing.T) {
	var buf bytes.Buffer
	b := &blockingMailer{started: make(chan struct{}), release: make(chan struct{})}
	d := auth.NewMailDispatcher(b, logTo(&buf), auth.DispatcherOptions{Buffer: 1, Workers: 1})

	require.True(t, d.Enqueue(msg("one@x.id")))
	<-b.started // worker holds message 1
	require.True(t, d.Enqueue(msg("two@x.id")), "message 2 fits the buffer")
	require.False(t, d.Enqueue(msg("three@x.id")), "message 3 is dropped")
	require.Contains(t, buf.String(), "level=ERROR")

	close(b.release)
	require.NoError(t, d.Close(context.Background()))
	require.False(t, d.Enqueue(msg("late@x.id")), "closed dispatcher rejects")
}

func TestRenderPasswordReset(t *testing.T) {
	m, err := auth.RenderPasswordReset("https://app.example/reset-password", "abc", "hana@example.com")
	require.NoError(t, err)
	require.Equal(t, "[Kotoki] パスワード再設定 / Reset your password", m.Subject)
	require.Equal(t, "password_reset", m.Tag)
	require.Equal(t, "hana@example.com", m.To)
	require.Contains(t, m.TextBody, "https://app.example/reset-password?token=abc")
	require.Contains(t, m.TextBody, "15 menit")
}
