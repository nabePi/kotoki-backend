package mailer

import (
	"context"
	"sync"

	"github.com/nabePi/kotoki-backend/internal/auth"
)

// FakeMailer keeps sent messages in memory for tests.
type FakeMailer struct {
	mu   sync.Mutex
	msgs []auth.MailMessage
}

var _ auth.Mailer = (*FakeMailer)(nil)

// NewFake returns an empty FakeMailer.
func NewFake() *FakeMailer { return &FakeMailer{} }

// Send records msg.
func (f *FakeMailer) Send(_ context.Context, msg auth.MailMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = append(f.msgs, msg)
	return nil
}

// Messages returns a copy of everything sent so far.
func (f *FakeMailer) Messages() []auth.MailMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]auth.MailMessage(nil), f.msgs...)
}

// Last returns the most recent message, or the zero value.
func (f *FakeMailer) Last() auth.MailMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.msgs) == 0 {
		return auth.MailMessage{}
	}
	return f.msgs[len(f.msgs)-1]
}

// Reset forgets all messages.
func (f *FakeMailer) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.msgs = nil
}
