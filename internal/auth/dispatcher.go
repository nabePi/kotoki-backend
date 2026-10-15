package auth

import (
	"bytes"
	"context"
	"embed"
	"log/slog"
	"sync"
	"text/template"
	"time"
)

//go:embed templates/password_reset.txt
var templatesFS embed.FS

var passwordResetTmpl = template.Must(template.ParseFS(templatesFS, "templates/password_reset.txt"))

// MailQueue accepts mail for asynchronous delivery.
type MailQueue interface {
	// Enqueue reports whether msg was accepted; false means it was dropped.
	Enqueue(msg MailMessage) bool
}

// DispatcherOptions tunes MailDispatcher (spec 3.9.1 defaults: 1000/4/10s, 1s/5s/15s).
type DispatcherOptions struct {
	Buffer, Workers int
	Timeout         time.Duration
	Backoffs        []time.Duration
	Sleep           func(ctx context.Context, d time.Duration) error
}

// MailDispatcher delivers mail from an in-process queue with retries. It never
// blocks the caller, so response latency does not reveal whether an email exists.
type MailDispatcher struct {
	mailer  Mailer
	log     *slog.Logger
	opts    DispatcherOptions
	mu      sync.RWMutex
	closed  bool
	queue   chan MailMessage
	wg      sync.WaitGroup
	baseCtx context.Context
	cancel  context.CancelFunc
}

var _ MailQueue = (*MailDispatcher)(nil)

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// NewMailDispatcher starts the worker pool.
func NewMailDispatcher(m Mailer, log *slog.Logger, o DispatcherOptions) *MailDispatcher {
	if o.Buffer <= 0 {
		o.Buffer = 1000
	}
	if o.Workers <= 0 {
		o.Workers = 4
	}
	if o.Timeout <= 0 {
		o.Timeout = 10 * time.Second
	}
	if o.Backoffs == nil {
		o.Backoffs = []time.Duration{time.Second, 5 * time.Second, 15 * time.Second}
	}
	if o.Sleep == nil {
		o.Sleep = sleepCtx
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &MailDispatcher{mailer: m, log: log, opts: o, queue: make(chan MailMessage, o.Buffer), baseCtx: ctx, cancel: cancel}
	for i := 0; i < o.Workers; i++ {
		d.wg.Add(1)
		go d.worker()
	}
	return d
}

// Enqueue queues msg without blocking; a full buffer or a closed dispatcher drops it.
func (d *MailDispatcher) Enqueue(msg MailMessage) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if d.closed {
		return false
	}
	select {
	case d.queue <- msg:
		return true
	default:
		d.log.Error("mail.queue_full", "tag", msg.Tag, "to", MaskEmail(msg.To))
		return false
	}
}

func (d *MailDispatcher) worker() {
	defer d.wg.Done()
	for msg := range d.queue {
		d.deliver(msg)
	}
}

func (d *MailDispatcher) deliver(msg MailMessage) {
	var err error
	attempts := 0
	for i := 0; ; i++ {
		attempts++
		ctx, cancel := context.WithTimeout(d.baseCtx, d.opts.Timeout)
		err = d.mailer.Send(ctx, msg)
		cancel()
		if err == nil {
			return
		}
		if i >= len(d.opts.Backoffs) {
			break
		}
		if serr := d.opts.Sleep(d.baseCtx, d.opts.Backoffs[i]); serr != nil {
			break
		}
	}
	d.log.Error("mail.dropped", "tag", msg.Tag, "to", MaskEmail(msg.To), "attempts", attempts, "error", err.Error())
}

// Close stops accepting mail and waits for the queue to drain or ctx to end.
func (d *MailDispatcher) Close(ctx context.Context) error {
	d.mu.Lock()
	if !d.closed {
		d.closed = true
		close(d.queue)
	}
	d.mu.Unlock()
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
		d.cancel()
		return nil
	case <-ctx.Done():
		d.cancel()
		<-done
		return ctx.Err()
	}
}

// RenderPasswordReset builds the reset mail for rawToken (spec 3.9.1).
func RenderPasswordReset(urlBase, rawToken, to string) (MailMessage, error) {
	var body bytes.Buffer
	if err := passwordResetTmpl.Execute(&body, map[string]string{"URL": urlBase + "?token=" + rawToken}); err != nil {
		return MailMessage{}, err
	}
	return MailMessage{
		To:       to,
		Subject:  "[Kotoki] パスワード再設定 / Reset your password",
		TextBody: body.String(),
		Tag:      "password_reset",
	}, nil
}
