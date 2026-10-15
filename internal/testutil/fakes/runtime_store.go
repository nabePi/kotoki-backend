package fakes

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/conversation/rtstate"
)

var _ rtstate.Store = (*RuntimeStore)(nil)

// streamMaxLen is the MAXLEN of the events stream (spec 7.9.1); the fake trims exactly, Redis
// approximately (~), so tests may only rely on "at least 500 kept".
const streamMaxLen = 500

type ownerEntry struct {
	pod   string
	fence int64
}

// RuntimeStore is an in-memory rtstate.Store. TTLs are accepted and ignored: nothing expires.
// FailNext("<Method>", err) fails the next call of a method. Safe for concurrent use.
type RuntimeStore struct {
	FailInjector
	mu      sync.Mutex
	owner   map[uuid.UUID]ownerEntry
	beat    map[uuid.UUID]time.Time
	rt      map[uuid.UUID]rtstate.RT
	turn    map[uuid.UUID]rtstate.TurnSnap
	events  map[uuid.UUID][]rtstate.EventEntry
	summary map[uuid.UUID]rtstate.Summary
	memctx  map[uuid.UUID][]byte
	subs    map[string]map[int]chan rtstate.Takeover
	nextSub int
	pods    map[string]rtstate.PodInfo
}

// NewRuntimeStore returns an empty store.
func NewRuntimeStore() *RuntimeStore {
	return &RuntimeStore{
		owner: map[uuid.UUID]ownerEntry{}, beat: map[uuid.UUID]time.Time{}, rt: map[uuid.UUID]rtstate.RT{},
		turn: map[uuid.UUID]rtstate.TurnSnap{}, events: map[uuid.UUID][]rtstate.EventEntry{},
		summary: map[uuid.UUID]rtstate.Summary{}, memctx: map[uuid.UUID][]byte{},
		subs: map[string]map[int]chan rtstate.Takeover{}, pods: map[string]rtstate.PodInfo{},
	}
}

// SetMemCtx stores the memctx JSON ReadMemCtx returns (memory, M8, writes it in production).
func (s *RuntimeStore) SetMemCtx(sid uuid.UUID, raw []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.memctx[sid] = slices.Clone(raw)
}

// Owner returns the current owner of a session, for assertions.
func (s *RuntimeStore) Owner(sid uuid.UUID) (pod string, fence int64, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	o, ok := s.owner[sid]
	return o.pod, o.fence, ok
}

// Subscribers is how many subscriptions to the channel of pod are open.
func (s *RuntimeStore) Subscribers(pod string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs[pod])
}

// PodInfo returns the last PodInfo written for pod, for assertions.
func (s *RuntimeStore) PodInfo(pod string) (rtstate.PodInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.pods[pod]
	return p, ok
}

// AcquireOwner implements rtstate.Store with the semantics of the Lua script of spec 7.9.2.
func (s *RuntimeStore) AcquireOwner(_ context.Context, sid uuid.UUID, pod string, fence int64, _ time.Duration) (bool, error) {
	if err := s.Take("AcquireOwner"); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.owner[sid]; ok && cur.fence > fence {
		return false, nil
	}
	s.owner[sid] = ownerEntry{pod: pod, fence: fence}
	return true, nil
}

// ReleaseOwner implements rtstate.Store: only the matching owner is removed.
func (s *RuntimeStore) ReleaseOwner(_ context.Context, sid uuid.UUID, pod string, fence int64) error {
	if err := s.Take("ReleaseOwner"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.owner[sid]; ok && cur.pod == pod && cur.fence == fence {
		delete(s.owner, sid)
	}
	return nil
}

// WriteHeartbeat implements rtstate.Store; like Redis it keeps millisecond precision.
func (s *RuntimeStore) WriteHeartbeat(_ context.Context, sid uuid.UUID, at time.Time, _ time.Duration) error {
	if err := s.Take("WriteHeartbeat"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beat[sid] = time.UnixMilli(at.UnixMilli()).UTC()
	return nil
}

// ReadHeartbeat implements rtstate.Store.
func (s *RuntimeStore) ReadHeartbeat(_ context.Context, sid uuid.UUID) (time.Time, bool, error) {
	if err := s.Take("ReadHeartbeat"); err != nil {
		return time.Time{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	at, ok := s.beat[sid]
	return at, ok, nil
}

// WriteRT implements rtstate.Store.
func (s *RuntimeStore) WriteRT(_ context.Context, sid uuid.UUID, rt rtstate.RT, _ time.Duration) error {
	if err := s.Take("WriteRT"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rt[sid] = rt
	return nil
}

// ReadRT implements rtstate.Store.
func (s *RuntimeStore) ReadRT(_ context.Context, sid uuid.UUID) (rtstate.RT, bool, error) {
	if err := s.Take("ReadRT"); err != nil {
		return rtstate.RT{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rt, ok := s.rt[sid]
	return rt, ok, nil
}

// WriteTurn implements rtstate.Store.
func (s *RuntimeStore) WriteTurn(_ context.Context, sid uuid.UUID, t rtstate.TurnSnap, _ time.Duration) error {
	if err := s.Take("WriteTurn"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.turn[sid] = t
	return nil
}

// ReadTurn implements rtstate.Store.
func (s *RuntimeStore) ReadTurn(_ context.Context, sid uuid.UUID) (rtstate.TurnSnap, bool, error) {
	if err := s.Take("ReadTurn"); err != nil {
		return rtstate.TurnSnap{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.turn[sid]
	return t, ok, nil
}

// DeleteTurn implements rtstate.Store.
func (s *RuntimeStore) DeleteTurn(_ context.Context, sid uuid.UUID) error {
	if err := s.Take("DeleteTurn"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.turn, sid)
	return nil
}

// AppendEvent implements rtstate.Store: the stream keeps the newest 500 entries.
func (s *RuntimeStore) AppendEvent(_ context.Context, sid uuid.UUID, e rtstate.EventEntry) error {
	if err := s.Take("AppendEvent"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e.Env = slices.Clone(e.Env)
	evs := append(slices.Clone(s.events[sid]), e)
	if len(evs) > streamMaxLen {
		evs = slices.Clone(evs[len(evs)-streamMaxLen:])
	}
	s.events[sid] = evs
	return nil
}

// ReadEvents implements rtstate.Store; entries come back in Seq order.
func (s *RuntimeStore) ReadEvents(_ context.Context, sid uuid.UUID) ([]rtstate.EventEntry, error) {
	if err := s.Take("ReadEvents"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := slices.Clone(s.events[sid])
	slices.SortStableFunc(out, func(a, b rtstate.EventEntry) int {
		switch {
		case a.Seq < b.Seq:
			return -1
		case a.Seq > b.Seq:
			return 1
		}
		return 0
	})
	return out, nil
}

// WriteSummary implements rtstate.Store.
func (s *RuntimeStore) WriteSummary(_ context.Context, sid uuid.UUID, sum rtstate.Summary, _ time.Duration) error {
	if err := s.Take("WriteSummary"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.summary[sid] = sum
	return nil
}

// ReadSummary implements rtstate.Store.
func (s *RuntimeStore) ReadSummary(_ context.Context, sid uuid.UUID) (rtstate.Summary, bool, error) {
	if err := s.Take("ReadSummary"); err != nil {
		return rtstate.Summary{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sum, ok := s.summary[sid]
	return sum, ok, nil
}

// ReadMemCtx implements rtstate.Store.
func (s *RuntimeStore) ReadMemCtx(_ context.Context, sid uuid.UUID) ([]byte, bool, error) {
	if err := s.Take("ReadMemCtx"); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, ok := s.memctx[sid]
	return slices.Clone(raw), ok, nil
}

// PublishTakeover implements rtstate.Store with Pub/Sub semantics: only current subscribers
// receive it, and a subscriber whose buffer (16) is full misses it.
func (s *RuntimeStore) PublishTakeover(_ context.Context, pod string, t rtstate.Takeover) error {
	if err := s.Take("PublishTakeover"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ch := range s.subs[pod] {
		select {
		case ch <- t:
		default:
		}
	}
	return nil
}

// SubscribePod implements rtstate.Store; the channel closes when ctx ends.
func (s *RuntimeStore) SubscribePod(ctx context.Context, pod string) (<-chan rtstate.Takeover, error) {
	if err := s.Take("SubscribePod"); err != nil {
		return nil, err
	}
	ch := make(chan rtstate.Takeover, 16)
	s.mu.Lock()
	id := s.nextSub
	s.nextSub++
	if s.subs[pod] == nil {
		s.subs[pod] = map[int]chan rtstate.Takeover{}
	}
	s.subs[pod][id] = ch
	s.mu.Unlock()
	context.AfterFunc(ctx, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.subs[pod], id)
		close(ch)
	})
	return ch, nil
}

// WritePodInfo implements rtstate.Store.
func (s *RuntimeStore) WritePodInfo(_ context.Context, pod string, info rtstate.PodInfo, _ time.Duration) error {
	if err := s.Take("WritePodInfo"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pods[pod] = info
	return nil
}
