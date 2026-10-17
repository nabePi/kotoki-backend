//go:build integration

package amqp_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	amqp091 "github.com/rabbitmq/amqp091-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/amqp"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/postgres"
	"github.com/nabePi/kotoki-backend/internal/testutil"
)

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))

// ---- fake broker side ------------------------------------------------------------------------

type verdict int

const (
	vAck verdict = iota
	vNack
	vTimeout
	vChannelDied
)

type sent struct {
	exchange, key, id string
	msg               amqp091.Publishing
}

// fakePub records Sends and answers each confirmation with a scripted verdict.
type fakePub struct {
	mu       sync.Mutex
	open     atomic.Bool
	sent     []sent
	verdicts map[string]verdict // by event id, default ack
	sendErr  func(n int) error  // n = 1-based Send count
	onWait   func()             // called at the start of every Wait
	started  *sync.WaitGroup    // optional rendezvous: Done on first Send of this pub
	once     sync.Once
}

func newFakePub() *fakePub {
	f := &fakePub{verdicts: map[string]verdict{}}
	f.open.Store(true)
	return f
}

func (f *fakePub) IsOpen() bool { return f.open.Load() }

func (f *fakePub) Send(_ context.Context, exchange, key string, msg amqp091.Publishing) (amqp.Confirmation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		if err := f.sendErr(len(f.sent) + 1); err != nil {
			return nil, err
		}
	}
	f.sent = append(f.sent, sent{exchange: exchange, key: key, id: msg.MessageId, msg: msg})
	if f.started != nil {
		f.once.Do(f.started.Done)
	}
	return &fakeConf{f: f, id: msg.MessageId}, nil
}

func (f *fakePub) ids() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.sent))
	for i, s := range f.sent {
		out[i] = s.id
	}
	return out
}

type fakeConf struct {
	f  *fakePub
	id string
}

func (c *fakeConf) Wait(context.Context) (bool, error) {
	if c.f.onWait != nil {
		c.f.onWait()
	}
	c.f.mu.Lock()
	v := c.f.verdicts[c.id]
	c.f.mu.Unlock()
	switch v {
	case vNack:
		return false, nil
	case vTimeout:
		return false, amqp.ErrConfirmTimeout
	case vChannelDied:
		return false, amqp.ErrBrokerUnavailable
	}
	return true, nil
}

// ---- postgres helpers ------------------------------------------------------------------------

func cfg() amqp.PublisherConfig {
	return amqp.PublisherConfig{
		PollInterval: 500 * time.Millisecond, ConfirmTimeout: 5 * time.Second, TxTimeout: 15 * time.Second,
		BatchSize: 100, MaxAttempts: 10,
	}
}

type outboxSeed struct {
	id        uuid.UUID
	offset    time.Duration // available_at = now() + offset
	status    string
	attempts  int
	createdAt time.Duration // created_at = now() + createdAt (negative = past)
}

func seedOutbox(t *testing.T, pool *pgxpool.Pool, s outboxSeed) uuid.UUID {
	t.Helper()
	if s.id == uuid.Nil {
		s.id = uuid.Must(uuid.NewV7())
	}
	if s.status == "" {
		s.status = "PENDING"
	}
	_, err := pool.Exec(context.Background(), `
		INSERT INTO event_outbox (event_id, event_type, aggregate_type, aggregate_id, user_id, producer, payload,
		                          occurred_at, status, attempts, available_at, created_at, published_at)
		VALUES ($1, 'session.completed', 'SESSION', $2, $3, 'REALTIME', '{"k": 1}', now(), $4, $5,
		        now() + make_interval(secs => $6), now() + make_interval(secs => $7),
		        CASE WHEN $4 = 'PUBLISHED' THEN now() END)`,
		s.id, uuid.New(), uuid.New(), s.status, s.attempts, s.offset.Seconds(), s.createdAt.Seconds())
	require.NoError(t, err)
	return s.id
}

type outboxState struct {
	status      string
	attempts    int
	publishedAt *time.Time
	lastError   *string
	backoffSecs float64 // available_at - now()
}

func rowState(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) outboxState {
	t.Helper()
	var s outboxState
	require.NoError(t, pool.QueryRow(context.Background(), `
		SELECT status, attempts, published_at, last_error, EXTRACT(EPOCH FROM available_at - now())::float8
		FROM event_outbox WHERE event_id = $1`, id).Scan(&s.status, &s.attempts, &s.publishedAt, &s.lastError, &s.backoffSecs))
	return s
}

func makeAvailable(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `UPDATE event_outbox SET available_at = now() WHERE event_id = $1`, id)
	require.NoError(t, err)
}

func newPublisher(pool *pgxpool.Pool, pub amqp.BatchPublisher, c amqp.PublisherConfig) (*amqp.OutboxPublisher, *amqp.Metrics) {
	m := amqp.NewMetrics(prometheus.NewRegistry())
	return amqp.NewOutboxPublisher(pool, pub, c, m, discard), m
}

func counter(c prometheus.Collector) float64 { return promtest.ToFloat64(c) }

// ---- PublishBatch ----------------------------------------------------------------------------

func TestPublishBatch_AckedRowsPublished(t *testing.T) {
	pool := testutil.NewPostgres(t)
	// event_id order (a < b < c) is the reverse of available_at order (b, c, a), so ordering by
	// event_id alone or by available_at descending cannot pass.
	a := seedOutbox(t, pool, outboxSeed{id: uuid.MustParse("00000000-0000-7000-8000-00000000000a"), offset: -1 * time.Second})
	b := seedOutbox(t, pool, outboxSeed{id: uuid.MustParse("00000000-0000-7000-8000-00000000000b"), offset: -3 * time.Second})
	c := seedOutbox(t, pool, outboxSeed{id: uuid.MustParse("00000000-0000-7000-8000-00000000000c"), offset: -2 * time.Second})
	fp := newFakePub()
	p, m := newPublisher(pool, fp, cfg())

	n, err := p.PublishBatch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Equal(t, []string{b.String(), c.String(), a.String()}, fp.ids(), "oldest available_at first")
	for _, s := range fp.sent {
		assert.Equal(t, "kotoki.events", s.exchange)
		assert.Equal(t, events.SessionCompleted, s.key)
	}
	for _, id := range []uuid.UUID{a, b, c} {
		st := rowState(t, pool, id)
		assert.Equal(t, "PUBLISHED", st.status)
		assert.NotNil(t, st.publishedAt)
		assert.Zero(t, st.attempts)
	}
	assert.EqualValues(t, 3, counter(m.OutboxPublished.WithLabelValues(events.SessionCompleted)))
}

func TestPublishBatch_EqualAvailableAt_OrdersByEventID(t *testing.T) {
	pool := testutil.NewPostgres(t)
	lo := uuid.MustParse("00000000-0000-7000-8000-000000000001")
	hi := uuid.MustParse("00000000-0000-7000-8000-000000000002")
	for _, id := range []uuid.UUID{hi, lo} { // insert out of order
		_, err := pool.Exec(context.Background(), `
			INSERT INTO event_outbox (event_id, event_type, aggregate_type, aggregate_id, user_id, producer, payload, occurred_at, available_at)
			VALUES ($1, 'session.completed', 'SESSION', $2, $3, 'REALTIME', '{}', now(), '2026-01-01T00:00:00Z')`,
			id, uuid.New(), uuid.New())
		require.NoError(t, err)
	}
	fp := newFakePub()
	p, _ := newPublisher(pool, fp, cfg())
	_, err := p.PublishBatch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{lo.String(), hi.String()}, fp.ids())
}

func TestPublishBatch_NackBackoffAndAttempts(t *testing.T) {
	pool := testutil.NewPostgres(t)
	id := seedOutbox(t, pool, outboxSeed{})
	fp := newFakePub()
	fp.verdicts[id.String()] = vNack
	p, m := newPublisher(pool, fp, cfg())

	n, err := p.PublishBatch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	st := rowState(t, pool, id)
	assert.Equal(t, 1, st.attempts)
	assert.Equal(t, "PENDING", st.status)
	assert.Nil(t, st.publishedAt)
	assert.Greater(t, st.backoffSecs, 1.0)
	assert.LessOrEqual(t, st.backoffSecs, 3.0, "first failure backs off 2 s")
	require.NotNil(t, st.lastError)
	assert.Equal(t, "broker nack or confirm timeout", *st.lastError)
	assert.EqualValues(t, 1, counter(m.OutboxFailures.WithLabelValues(events.SessionCompleted, "nack")))
	assert.EqualValues(t, 0, counter(m.OutboxPublished.WithLabelValues(events.SessionCompleted)))

	// A row inside its backoff window is not picked up again.
	n, err = p.PublishBatch(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)

	makeAvailable(t, pool, id)
	_, err = p.PublishBatch(context.Background())
	require.NoError(t, err)
	st = rowState(t, pool, id)
	assert.Equal(t, 2, st.attempts)
	assert.Greater(t, st.backoffSecs, 3.0)
	assert.LessOrEqual(t, st.backoffSecs, 5.0, "second failure backs off 4 s")
}

func TestPublishBatch_ConfirmTimeoutCountsAsAttempt(t *testing.T) {
	pool := testutil.NewPostgres(t)
	id := seedOutbox(t, pool, outboxSeed{})
	fp := newFakePub()
	fp.verdicts[id.String()] = vTimeout
	p, m := newPublisher(pool, fp, cfg())

	_, err := p.PublishBatch(context.Background())
	require.NoError(t, err)
	st := rowState(t, pool, id)
	assert.Equal(t, 1, st.attempts)
	assert.Equal(t, "PENDING", st.status)
	assert.EqualValues(t, 1, counter(m.OutboxFailures.WithLabelValues(events.SessionCompleted, "timeout")))
}

func TestPublishBatch_BackoffIsCappedAt300s(t *testing.T) {
	pool := testutil.NewPostgres(t)
	id := seedOutbox(t, pool, outboxSeed{attempts: 8}) // power(2, 9) = 512 -> capped
	fp := newFakePub()
	fp.verdicts[id.String()] = vNack
	p, _ := newPublisher(pool, fp, cfg())

	_, err := p.PublishBatch(context.Background())
	require.NoError(t, err)
	st := rowState(t, pool, id)
	assert.Equal(t, "PENDING", st.status, "9th failure of 10 is not the last")
	assert.Equal(t, 9, st.attempts)
	assert.Greater(t, st.backoffSecs, 295.0)
	assert.LessOrEqual(t, st.backoffSecs, 300.0)
}

func TestPublishBatch_TenthFailureMarksFailed(t *testing.T) {
	pool := testutil.NewPostgres(t)
	id := seedOutbox(t, pool, outboxSeed{attempts: 9})
	fp := newFakePub()
	fp.verdicts[id.String()] = vNack
	p, _ := newPublisher(pool, fp, cfg())

	_, err := p.PublishBatch(context.Background())
	require.NoError(t, err)
	st := rowState(t, pool, id)
	assert.Equal(t, "FAILED", st.status)
	assert.Equal(t, 10, st.attempts)

	// A FAILED row is never picked up again.
	makeAvailable(t, pool, id)
	n, err := p.PublishBatch(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)
}

func TestPublishBatch_MaxAttemptsFollowsConfig(t *testing.T) {
	pool := testutil.NewPostgres(t)
	id := seedOutbox(t, pool, outboxSeed{attempts: 2})
	fp := newFakePub()
	fp.verdicts[id.String()] = vNack
	c := cfg()
	c.MaxAttempts = 3
	p, _ := newPublisher(pool, fp, c)

	_, err := p.PublishBatch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "FAILED", rowState(t, pool, id).status)
}

func TestPublishBatch_MixedVerdicts_OnlyFailedRowsGetBackoff(t *testing.T) {
	pool := testutil.NewPostgres(t)
	okID := seedOutbox(t, pool, outboxSeed{offset: -3 * time.Second})
	nackID := seedOutbox(t, pool, outboxSeed{offset: -2 * time.Second})
	okID2 := seedOutbox(t, pool, outboxSeed{offset: -1 * time.Second})
	fp := newFakePub()
	fp.verdicts[nackID.String()] = vNack
	p, _ := newPublisher(pool, fp, cfg())

	n, err := p.PublishBatch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Equal(t, "PUBLISHED", rowState(t, pool, okID).status)
	assert.Equal(t, "PUBLISHED", rowState(t, pool, okID2).status)
	st := rowState(t, pool, nackID)
	assert.Equal(t, "PENDING", st.status)
	assert.Equal(t, 1, st.attempts)
	assert.Zero(t, rowState(t, pool, okID).attempts)
}

func TestPublishBatch_BrokerDown_DoesNotIncrementAttempts(t *testing.T) {
	pool := testutil.NewPostgres(t)
	id := seedOutbox(t, pool, outboxSeed{offset: -time.Second, attempts: 3})
	before := rowState(t, pool, id)
	fp := newFakePub()
	fp.open.Store(false)
	p, m := newPublisher(pool, fp, cfg())

	n, err := p.PublishBatch(context.Background())
	require.ErrorIs(t, err, amqp.ErrBrokerUnavailable)
	assert.Zero(t, n)
	assert.Empty(t, fp.ids(), "nothing is sent while the channel is closed")
	after := rowState(t, pool, id)
	assert.Equal(t, 3, after.attempts, "a dead broker is not the event's fault")
	assert.Equal(t, "PENDING", after.status)
	assert.Nil(t, after.lastError)
	assert.InDelta(t, before.backoffSecs, after.backoffSecs, 2.0)
	assert.EqualValues(t, 0, counter(m.OutboxFailures.WithLabelValues(events.SessionCompleted, "nack")))
}

func TestPublishBatch_SendUnavailableMidBatch_RollsBackWholeBatchAndRepublishes(t *testing.T) {
	pool := testutil.NewPostgres(t)
	var ids []uuid.UUID
	for i := range 3 {
		ids = append(ids, seedOutbox(t, pool, outboxSeed{offset: -time.Duration(10-i) * time.Second}))
	}
	fp := newFakePub()
	fp.sendErr = func(n int) error {
		if n == 2 {
			return amqp.ErrBrokerUnavailable
		}
		return nil
	}
	p, m := newPublisher(pool, fp, cfg())

	n, err := p.PublishBatch(context.Background())
	require.ErrorIs(t, err, amqp.ErrBrokerUnavailable)
	assert.Zero(t, n)
	assert.Equal(t, []string{ids[0].String()}, fp.ids(), "row 1 already went out before the failure")
	for _, id := range ids {
		st := rowState(t, pool, id)
		assert.Equal(t, "PENDING", st.status, "whole batch rolled back, including the row that was sent")
		assert.Zero(t, st.attempts)
		assert.Nil(t, st.publishedAt)
	}
	assert.EqualValues(t, 0, counter(m.OutboxPublished.WithLabelValues(events.SessionCompleted)))

	// Broker back: the batch is published again (a duplicate of row 1 is safe, consumers are idempotent).
	fp.sendErr = nil
	n, err = p.PublishBatch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	for _, id := range ids {
		assert.Equal(t, "PUBLISHED", rowState(t, pool, id).status)
	}
	sentCount := map[string]int{}
	for _, id := range fp.ids() {
		sentCount[id]++
	}
	assert.Equal(t, 2, sentCount[ids[0].String()], "row 1 is the duplicate")
	assert.Equal(t, 1, sentCount[ids[1].String()])
	assert.Equal(t, 1, sentCount[ids[2].String()])
}

func TestPublishBatch_SendGenericErrorMidBatch_RollsBackWholeBatch(t *testing.T) {
	pool := testutil.NewPostgres(t)
	a := seedOutbox(t, pool, outboxSeed{offset: -2 * time.Second})
	b := seedOutbox(t, pool, outboxSeed{offset: -1 * time.Second})
	fp := newFakePub()
	boom := errors.New("boom")
	fp.sendErr = func(n int) error {
		if n == 2 {
			return boom
		}
		return nil
	}
	p, _ := newPublisher(pool, fp, cfg())

	_, err := p.PublishBatch(context.Background())
	require.ErrorIs(t, err, boom)
	for _, id := range []uuid.UUID{a, b} {
		st := rowState(t, pool, id)
		assert.Equal(t, "PENDING", st.status)
		assert.Zero(t, st.attempts)
	}
}

func TestPublishBatch_ChannelDiesWhileAwaitingConfirms_RollsBackWholeBatchAndRepublishes(t *testing.T) {
	pool := testutil.NewPostgres(t)
	a := seedOutbox(t, pool, outboxSeed{offset: -3 * time.Second})
	b := seedOutbox(t, pool, outboxSeed{offset: -2 * time.Second})
	c := seedOutbox(t, pool, outboxSeed{offset: -1 * time.Second})
	fp := newFakePub()
	fp.verdicts[b.String()] = vChannelDied // a is acked, b dies in the middle
	p, m := newPublisher(pool, fp, cfg())

	n, err := p.PublishBatch(context.Background())
	require.ErrorIs(t, err, amqp.ErrBrokerUnavailable)
	assert.Zero(t, n)
	for _, id := range []uuid.UUID{a, b, c} {
		st := rowState(t, pool, id)
		assert.Equal(t, "PENDING", st.status, "an acked row is not marked PUBLISHED when the batch fails midway")
		assert.Zero(t, st.attempts, "channel death is not a publish failure")
		assert.Nil(t, st.publishedAt)
	}
	assert.EqualValues(t, 0, counter(m.OutboxPublished.WithLabelValues(events.SessionCompleted)))
	assert.EqualValues(t, 0, counter(m.OutboxFailures.WithLabelValues(events.SessionCompleted, "nack")))

	fp.mu.Lock()
	delete(fp.verdicts, b.String())
	fp.mu.Unlock()
	n, err = p.PublishBatch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 3, n)
	for _, id := range []uuid.UUID{a, b, c} {
		assert.Equal(t, "PUBLISHED", rowState(t, pool, id).status)
	}
	assert.Len(t, fp.ids(), 6, "all three were sent twice: duplicates are the price of atomicity")
}

func TestPublishBatch_ShutdownDuringConfirmWait_StillCommitsTheBatch(t *testing.T) {
	// Spec 9.7.7 step 6: the running batch finishes (bounded by the tx timeout) even after SIGTERM.
	pool := testutil.NewPostgres(t)
	id := seedOutbox(t, pool, outboxSeed{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fp := newFakePub()
	fp.onWait = cancel
	p, _ := newPublisher(pool, fp, cfg())

	n, err := p.PublishBatch(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, "PUBLISHED", rowState(t, pool, id).status)
}

func TestPublishBatch_TxTimeoutBoundsTheBatch(t *testing.T) {
	pool := testutil.NewPostgres(t)
	id := seedOutbox(t, pool, outboxSeed{})
	fp := newFakePub()
	fp.onWait = func() { time.Sleep(400 * time.Millisecond) }
	c := cfg()
	c.TxTimeout = 150 * time.Millisecond
	p, _ := newPublisher(pool, fp, c)

	_, err := p.PublishBatch(context.Background())
	require.Error(t, err, "commit after the tx deadline must fail")
	st := rowState(t, pool, id)
	assert.Equal(t, "PENDING", st.status)
	assert.Zero(t, st.attempts)
}

func TestPublishBatch_SkipLocked_TwoPublishersNoDuplicate(t *testing.T) {
	pool := testutil.NewPostgres(t)
	const total = 200
	for i := range total {
		seedOutbox(t, pool, outboxSeed{offset: -time.Duration(total-i) * time.Second})
	}

	// Rendezvous: neither publisher confirms before both have sent, so the two batches are
	// provably in flight (and their row locks held) at the same time.
	var started sync.WaitGroup
	started.Add(2)
	both := make(chan struct{})
	go func() { started.Wait(); close(both) }()
	// Only the first confirmation each publisher awaits counts: it is awaited while that publisher
	// holds its first batch, so seeing the rendezvous complete there proves the batches overlapped.
	var sawOverlap atomic.Int32
	pubs := []*fakePub{newFakePub(), newFakePub()}
	for _, fp := range pubs {
		var first sync.Once
		fp.started = &started
		fp.onWait = func() {
			first.Do(func() {
				select {
				case <-both:
					sawOverlap.Add(1)
				case <-time.After(5 * time.Second):
				}
			})
		}
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, fp := range pubs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, _ := newPublisher(pool, fp, cfg())
			for {
				n, err := p.PublishBatch(context.Background())
				if err != nil {
					errs[i] = err
					return
				}
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	require.NoError(t, errs[0])
	require.NoError(t, errs[1])
	require.EqualValues(t, 2, sawOverlap.Load(), "both publishers must have held a batch at the same time")

	count := map[string]int{}
	for _, fp := range pubs {
		assert.NotEmpty(t, fp.ids(), "each publisher got a share")
		for _, id := range fp.ids() {
			count[id]++
		}
	}
	assert.Len(t, count, total)
	for id, n := range count {
		assert.Equal(t, 1, n, "event %s published by more than one publisher", id)
	}
	var published int
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT count(*) FROM event_outbox WHERE status = 'PUBLISHED'`).Scan(&published))
	assert.Equal(t, total, published)
}

func TestPublishBatch_IgnoresFutureAndNonPending(t *testing.T) {
	pool := testutil.NewPostgres(t)
	seedOutbox(t, pool, outboxSeed{offset: time.Hour})
	seedOutbox(t, pool, outboxSeed{status: "PUBLISHED", offset: -time.Minute})
	seedOutbox(t, pool, outboxSeed{status: "FAILED", offset: -time.Minute, attempts: 10})
	due := seedOutbox(t, pool, outboxSeed{offset: -time.Second})
	fp := newFakePub()
	p, _ := newPublisher(pool, fp, cfg())

	n, err := p.PublishBatch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, []string{due.String()}, fp.ids())
}

func TestPublishBatch_RespectsBatchSize(t *testing.T) {
	pool := testutil.NewPostgres(t)
	for i := range 5 {
		seedOutbox(t, pool, outboxSeed{offset: -time.Duration(10-i) * time.Second})
	}
	fp := newFakePub()
	c := cfg()
	c.BatchSize = 2
	p, _ := newPublisher(pool, fp, c)
	n, err := p.PublishBatch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.Len(t, fp.ids(), 2)
}

func TestPublishBatch_EmptyDoesNotTouchBroker(t *testing.T) {
	pool := testutil.NewPostgres(t)
	fp := newFakePub()
	fp.open.Store(false) // an idle publisher with a dead broker is not an error
	p, _ := newPublisher(pool, fp, cfg())
	n, err := p.PublishBatch(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)
}

// ---- Run -------------------------------------------------------------------------------------

type sleeper struct {
	mu    sync.Mutex
	calls []time.Duration
	after func(n int) // called after recording; n = number of calls so far
}

func (s *sleeper) Sleep(_ context.Context, d time.Duration) {
	s.mu.Lock()
	s.calls = append(s.calls, d)
	n := len(s.calls)
	s.mu.Unlock()
	if s.after != nil {
		s.after(n)
	}
}

func (s *sleeper) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func TestRun_BacklogContinuesThenSleeps(t *testing.T) {
	pool := testutil.NewPostgres(t)
	var ids []uuid.UUID
	for i := range 5 {
		ids = append(ids, seedOutbox(t, pool, outboxSeed{offset: -time.Duration(10-i) * time.Second}))
	}
	fp := newFakePub()
	c := cfg()
	c.BatchSize = 2
	p, _ := newPublisher(pool, fp, c)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sl := &sleeper{after: func(int) { cancel() }}
	p.Sleep = sl.Sleep

	require.NoError(t, p.Run(ctx))
	assert.Equal(t, 1, sl.count(), "batches of 2, 2, 1: sleep only after the short batch")
	assert.Equal(t, []time.Duration{c.PollInterval}, sl.calls)
	assert.Len(t, fp.ids(), 5)
	for _, id := range ids {
		assert.Equal(t, "PUBLISHED", rowState(t, pool, id).status)
	}
}

func TestRun_BrokerDown_WaitsReconnect(t *testing.T) {
	pool := testutil.NewPostgres(t)
	id := seedOutbox(t, pool, outboxSeed{offset: -time.Second})
	fp := newFakePub()
	fp.open.Store(false)
	p, _ := newPublisher(pool, fp, cfg())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sl := &sleeper{after: func(int) { cancel() }} // the first Sleep follows the successful batch
	p.Sleep = sl.Sleep
	var waits atomic.Int32
	p.WaitReconnect = func(context.Context) {
		if waits.Add(1) == 2 {
			fp.open.Store(true) // broker back after the second wait
		}
	}

	require.NoError(t, p.Run(ctx))
	assert.Equal(t, int32(2), waits.Load(), "retried after each reconnect wait, then published")
	assert.Equal(t, "PUBLISHED", rowState(t, pool, id).status, "published once the broker is back")
	assert.Zero(t, rowState(t, pool, id).attempts, "outage did not burn attempts")
	assert.Equal(t, []string{id.String()}, fp.ids())
}

// WaitReconnect returns at once while the connection is healthy. If the publisher nevertheless keeps
// reporting the broker unavailable, Run must back off (not spin and flood the log).
func TestRun_BrokerDown_ImmediateReconnectStillBacksOff(t *testing.T) {
	pool := testutil.NewPostgres(t)
	id := seedOutbox(t, pool, outboxSeed{offset: -time.Second})
	fp := newFakePub()
	fp.open.Store(false)
	p, _ := newPublisher(pool, fp, cfg())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sl := &sleeper{after: func(n int) {
		if n == 2 {
			cancel()
		}
	}}
	p.Sleep = sl.Sleep
	var waits atomic.Int32
	p.WaitReconnect = func(context.Context) { waits.Add(1) } // healthy connection: returns at once

	require.NoError(t, p.Run(ctx))
	assert.Equal(t, int32(4), waits.Load(), "two rounds without sleep, then one backoff sleep per round")
	require.Equal(t, 2, sl.count())
	// unavailable = 3 -> Backoff(0) = 1 s, unavailable = 4 -> Backoff(1) = 2 s, each within +-20%
	for i, base := range []time.Duration{time.Second, 2 * time.Second} {
		assert.GreaterOrEqual(t, sl.calls[i], time.Duration(float64(base)*0.8), "sleep %d", i)
		assert.LessOrEqual(t, sl.calls[i], time.Duration(float64(base)*1.2), "sleep %d", i)
	}
	assert.Zero(t, rowState(t, pool, id).attempts, "outage did not burn attempts")
}

func TestRun_BrokerDown_DefaultWaitIsBackoffNotBusyLoop(t *testing.T) {
	pool := testutil.NewPostgres(t)
	seedOutbox(t, pool, outboxSeed{offset: -time.Second})
	fp := newFakePub()
	fp.open.Store(false)
	p, _ := newPublisher(pool, fp, cfg())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sl := &sleeper{after: func(n int) {
		if n == 3 {
			cancel()
		}
	}}
	p.Sleep = sl.Sleep

	require.NoError(t, p.Run(ctx))
	require.Equal(t, 3, sl.count())
	// attempt 0,1,2 -> 1s,2s,4s, each within the +-20% jitter
	for i, base := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		assert.GreaterOrEqual(t, sl.calls[i], time.Duration(float64(base)*0.8), "wait %d", i)
		assert.LessOrEqual(t, sl.calls[i], time.Duration(float64(base)*1.2), "wait %d", i)
	}
}

func TestRun_GenericErrorSleepsPollInterval(t *testing.T) {
	pool := testutil.NewPostgres(t)
	id := seedOutbox(t, pool, outboxSeed{offset: -time.Second})
	fp := newFakePub()
	fp.sendErr = func(int) error { return errors.New("boom") }
	p, _ := newPublisher(pool, fp, cfg())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sl := &sleeper{after: func(int) { cancel() }}
	p.Sleep = sl.Sleep

	require.NoError(t, p.Run(ctx))
	assert.Equal(t, []time.Duration{cfg().PollInterval}, sl.calls)
	assert.Zero(t, rowState(t, pool, id).attempts)
}

func TestRun_StopsCleanlyWhenContextCancelled(t *testing.T) {
	pool := testutil.NewPostgres(t)
	p, _ := newPublisher(pool, newFakePub(), cfg()) // real Sleep seam
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// ---- gauges ----------------------------------------------------------------------------------

func TestRefreshOutboxGauges(t *testing.T) {
	pool := testutil.NewPostgres(t)
	seedOutbox(t, pool, outboxSeed{createdAt: -100 * time.Second})
	seedOutbox(t, pool, outboxSeed{createdAt: -10 * time.Second})
	seedOutbox(t, pool, outboxSeed{status: "FAILED", attempts: 10, createdAt: -500 * time.Second})
	seedOutbox(t, pool, outboxSeed{status: "PUBLISHED", createdAt: -900 * time.Second})
	m := amqp.NewMetrics(prometheus.NewRegistry())

	require.NoError(t, amqp.RefreshOutboxGauges(context.Background(), pool, m))
	assert.EqualValues(t, 2, counter(m.OutboxPending))
	assert.EqualValues(t, 1, counter(m.OutboxFailed))
	assert.InDelta(t, 100, counter(m.OutboxOldestPendingAge), 5, "oldest PENDING only: not the FAILED/PUBLISHED rows")
}

func TestRefreshOutboxGauges_Empty(t *testing.T) {
	pool := testutil.NewPostgres(t)
	m := amqp.NewMetrics(prometheus.NewRegistry())
	m.OutboxPending.Set(7)
	m.OutboxOldestPendingAge.Set(7)
	m.OutboxFailed.Set(7)
	require.NoError(t, amqp.RefreshOutboxGauges(context.Background(), pool, m))
	assert.Zero(t, counter(m.OutboxPending))
	assert.Zero(t, counter(m.OutboxOldestPendingAge))
	assert.Zero(t, counter(m.OutboxFailed))
}

func TestRefreshQueueDepth(t *testing.T) {
	url := testutil.NewRabbitMQ(t)
	conn := dial(t, url)
	cp := amqp.NewConfirmPublisher(conn, 5*time.Second)
	require.NoError(t, cp.PublishConfirmed(context.Background(), amqp.ExchangeEvents, events.SessionCompleted, msg()))
	ch, err := conn.Channel()
	require.NoError(t, err)
	defer func() { _ = ch.Close() }()
	m := amqp.NewMetrics(prometheus.NewRegistry())

	queues := []string{events.QueueEvaluation, events.QueueLearner, amqp.DLQName(events.QueueEvaluation)}
	require.NoError(t, amqp.RefreshQueueDepth(context.Background(), ch, queues, m))
	assert.EqualValues(t, 1, counter(m.QueueMessages.WithLabelValues(events.QueueEvaluation)))
	assert.EqualValues(t, 0, counter(m.QueueMessages.WithLabelValues(events.QueueLearner)))
	assert.EqualValues(t, 0, counter(m.QueueMessages.WithLabelValues(amqp.DLQName(events.QueueEvaluation))))

	require.Error(t, amqp.RefreshQueueDepth(context.Background(), ch, []string{"no.such.queue"}, m))
}

// ---- real PG + real RabbitMQ -----------------------------------------------------------------

func sessionCompleted(userID uuid.UUID) events.Event {
	t0 := time.Date(2026, 10, 12, 6, 0, 0, 0, time.UTC)
	p := events.SessionCompletedPayload{
		SessionID: uuid.New(), FinalStatus: "COMPLETED", EndReason: "USER_ENDED", ConversationStartMode: "USER_FIRST",
		TopicID: uuid.New(), ScenarioID: uuid.New(), ScenarioVersionID: uuid.New(), CharacterID: uuid.New(), PromptVersionID: uuid.New(),
		DurationSeconds: 300, StartedAt: t0, EndedAt: t0.Add(5 * time.Minute), ActiveSeconds: 295, ChargedSeconds: 295, TurnCount: 6, UserTurnCount: 3,
	}
	return events.NewSessionCompleted(uuid.Must(uuid.NewV7()), t0.Add(5*time.Minute), events.ProducerRealtime, userID, p)
}

func TestOutbox_EndToEnd_SessionCompleted(t *testing.T) {
	testutil.NewTraceRecorder(t)
	pool := testutil.NewPostgres(t)
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	conn := dial(t, url)
	store := postgres.NewStore(pool)
	ev := sessionCompleted(uuid.New())

	ctx, span := tracerForTest().Start(context.Background(), "business")
	require.NoError(t, store.WithinTx(ctx, func(ctx context.Context, tx *postgres.Tx) error {
		return tx.Outbox().Enqueue(ctx, ev)
	}))
	span.End()
	var stored string
	require.NoError(t, pool.QueryRow(context.Background(), `SELECT traceparent FROM event_outbox WHERE event_id = $1`, ev.EventID).Scan(&stored))

	p, _ := newPublisher(pool, amqp.NewConfirmPublisher(conn, 5*time.Second), cfg())
	n, err := p.PublishBatch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, "PUBLISHED", rowState(t, pool, ev.EventID).status)

	for _, q := range []string{events.QueueEvaluation, events.QueueMemory, events.QueueSummary} {
		assert.Equal(t, 1, adm.QueueMessages(q), q)
	}
	assert.Equal(t, 0, adm.QueueMessages(events.QueueLearner))

	ch, err := conn.Channel()
	require.NoError(t, err)
	defer func() { _ = ch.Close() }()
	d, ok, err := ch.Get(events.QueueEvaluation, true)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, ev.EventID.String(), d.MessageId)
	assert.Equal(t, uint8(2), d.DeliveryMode)
	assert.Equal(t, "kotoki", d.AppId)
	tp, _ := d.Headers["traceparent"].(string)
	require.NotEmpty(t, tp)
	assert.Equal(t, stored[3:35], tp[3:35], "same trace id as the business transaction's outbox row")
	assert.NotEqual(t, stored, tp, "but a new span (the publish span)")
	env, err := events.DecodeEnvelope(d.Body)
	require.NoError(t, err)
	assert.Equal(t, tp, env.Traceparent)
	assert.Equal(t, events.SessionCompleted, env.EventType)
}

func TestOutbox_BusinessRollback_NoEventReachesTheBroker(t *testing.T) {
	pool := testutil.NewPostgres(t)
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	store := postgres.NewStore(pool)
	boom := errors.New("business failure after enqueue")

	err := store.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) error {
		if err := tx.Outbox().Enqueue(ctx, sessionCompleted(uuid.New())); err != nil {
			return err
		}
		return boom
	})
	require.ErrorIs(t, err, boom)

	p, _ := newPublisher(pool, amqp.NewConfirmPublisher(dial(t, url), 5*time.Second), cfg())
	n, err := p.PublishBatch(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n, "nothing was committed, so nothing to publish")
	for _, q := range []string{events.QueueEvaluation, events.QueueMemory, events.QueueSummary, amqp.QueueUnrouted} {
		assert.Zero(t, adm.QueueMessages(q), q)
	}
}

func TestOutbox_RealBrokerOutage_DoesNotBurnAttemptsAndRecovers(t *testing.T) {
	pool := testutil.NewPostgres(t)
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	conn := dial(t, url)
	store := postgres.NewStore(pool)
	ev := sessionCompleted(uuid.New())
	require.NoError(t, store.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) error {
		return tx.Outbox().Enqueue(ctx, ev)
	}))
	pub := amqp.NewConfirmPublisher(conn, 5*time.Second)
	p, _ := newPublisher(pool, pub, cfg())

	adm.CloseAllConnections()
	require.Eventually(t, func() bool { return !conn.Healthy() }, 5*time.Second, 20*time.Millisecond)

	for range 3 {
		n, err := p.PublishBatch(context.Background())
		require.ErrorIs(t, err, amqp.ErrBrokerUnavailable)
		assert.Zero(t, n)
	}
	st := rowState(t, pool, ev.EventID)
	assert.Equal(t, "PENDING", st.status)
	assert.Zero(t, st.attempts, "three failed polls against a dead broker, zero attempts burned")
	assert.Less(t, st.backoffSecs, 1.0, "and no backoff was applied")

	require.NoError(t, conn.Reconnect(context.Background()))
	n, err := p.PublishBatch(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, "PUBLISHED", rowState(t, pool, ev.EventID).status)
	assert.Equal(t, 1, adm.QueueMessages(events.QueueEvaluation))
}

func TestOutbox_Run_DrainsBacklogThroughRealBroker(t *testing.T) {
	pool := testutil.NewPostgres(t)
	url := testutil.NewRabbitMQ(t)
	adm := testutil.RabbitMQAdmin(t, url)
	store := postgres.NewStore(pool)
	const total = 7
	require.NoError(t, store.WithinTx(context.Background(), func(ctx context.Context, tx *postgres.Tx) error {
		for range total {
			if err := tx.Outbox().Enqueue(ctx, sessionCompleted(uuid.New())); err != nil {
				return err
			}
		}
		return nil
	}))
	c := cfg()
	c.PollInterval, c.BatchSize = 50*time.Millisecond, 3
	p, m := newPublisher(pool, amqp.NewConfirmPublisher(dial(t, url), c.ConfirmTimeout), c)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	require.Eventually(t, func() bool {
		return counter(m.OutboxPublished.WithLabelValues(events.SessionCompleted)) == total
	}, 10*time.Second, 50*time.Millisecond)
	cancel()
	require.NoError(t, <-done)
	assert.Equal(t, total, adm.QueueMessages(events.QueueEvaluation), fmt.Sprintf("%d events, one copy each", total))
}
