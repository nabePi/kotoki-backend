package fakes

import (
	"context"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/balance"
	"github.com/nabePi/kotoki-backend/internal/events"
	"github.com/nabePi/kotoki-backend/internal/session"
)

var _ session.Store = (*SessionStore)(nil)

type sessionState struct {
	sessions map[uuid.UUID]session.Session
	turns    []session.Turn
	events   []session.Event
}

func (s sessionState) clone() sessionState {
	c := sessionState{sessions: maps.Clone(s.sessions), turns: slices.Clone(s.turns), events: slices.Clone(s.events)}
	if c.sessions == nil {
		c.sessions = map[uuid.UUID]session.Session{}
	}
	return c
}

// SessionStore is an in-memory session.Store that follows the semantics of the PostgreSQL
// repositories (unique indexes, CAS, fences, partition-free turns). A transaction runs under the
// balance store's lock, so balance and session state commit or roll back together.
type SessionStore struct {
	FailInjector
	bal    *BalanceStore
	Outbox *Outbox
	// BeforeInsert, when set, runs once at the start of the next Sessions().Insert: tests use it to
	// commit a competing session "between" the checks of a transaction and its INSERT.
	BeforeInsert func()
	mu           sync.Mutex
	st           sessionState
	puts         []session.Session // PutSession calls model rows committed by someone else: a rollback keeps them
}

// NewSessionStore returns an empty store whose transactions also cover bal.
func NewSessionStore(bal *BalanceStore) *SessionStore {
	return &SessionStore{bal: bal, Outbox: &Outbox{}, st: sessionState{sessions: map[uuid.UUID]session.Session{}}}
}

type sessionTx struct {
	balance.Tx
	s *SessionStore
}

func (t sessionTx) Sessions() session.Repository    { return &sessionRepoFake{s: t.s} }
func (t sessionTx) Turns() session.TurnRepository   { return &turnRepoFake{s: t.s} }
func (t sessionTx) Events() session.EventRepository { return &eventRepoFake{s: t.s} }
func (t sessionTx) Outbox() events.OutboxWriter     { return t.s.Outbox }

// WithinTx implements session.Store.
func (s *SessionStore) WithinTx(ctx context.Context, fn func(ctx context.Context, tx session.Tx) error) error {
	return s.bal.WithinTx(ctx, func(ctx context.Context, btx balance.Tx) error {
		s.mu.Lock()
		snap := s.st.clone()
		queued, putCount := s.Outbox.Len(), len(s.puts)
		s.mu.Unlock()
		if err := fn(ctx, sessionTx{Tx: btx, s: s}); err != nil {
			s.mu.Lock()
			s.st = snap
			for _, v := range s.puts[putCount:] {
				s.st.sessions[v.ID] = v
			}
			s.mu.Unlock()
			s.Outbox.Truncate(queued)
			return err
		}
		return nil
	})
}

// Reader returns the repository for reads outside a transaction (Deps.Reader).
func (s *SessionStore) Reader() session.Repository { return &sessionRepoFake{s: s} }

// Session returns the stored session (zero value when unknown).
func (s *SessionStore) Session(id uuid.UUID) session.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.st.sessions[id]
}

// PutSession stores a session as it is, bypassing the unique indexes (test setup).
func (s *SessionStore) PutSession(v session.Session) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.st.sessions[v.ID] = v
	s.puts = append(s.puts, v)
}

// Events returns the session_events rows of a session in insertion order.
func (s *SessionStore) Events(sessionID uuid.UUID) []session.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []session.Event
	for _, e := range s.st.events {
		if e.SessionID == sessionID {
			out = append(out, e)
		}
	}
	return out
}

// Turns returns the turns of a session ordered by sequence number.
func (s *SessionStore) Turns(sessionID uuid.UUID) []session.Turn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnsOf(sessionID)
}

func (s *SessionStore) turnsOf(sessionID uuid.UUID) []session.Turn {
	var out []session.Turn
	for _, t := range s.st.turns {
		if t.SessionID == sessionID {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out
}

func (s *SessionStore) fenceOK(sessionID uuid.UUID, fence int64) bool {
	cur, ok := s.st.sessions[sessionID]
	if !ok || cur.RuntimeFence != fence {
		return false
	}
	return cur.Status == session.StatusStarting || cur.Status == session.StatusActive || cur.Status == session.StatusEnding
}

type sessionRepoFake struct{ s *SessionStore }

var _ session.Repository = (*sessionRepoFake)(nil)

func (r *sessionRepoFake) Insert(_ context.Context, v session.Session) error {
	if err := r.s.Take("Sessions.Insert"); err != nil {
		return err
	}
	if hook := r.s.BeforeInsert; hook != nil {
		r.s.BeforeInsert = nil
		hook()
	}
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	for _, o := range r.s.st.sessions {
		if o.UserID == v.UserID && o.CreateIdempotencyKey == v.CreateIdempotencyKey {
			return session.ErrDuplicateIdempotency
		}
		if o.UserID == v.UserID && o.Status.Open() && v.Status.Open() {
			return session.ErrAlreadyOpen
		}
	}
	v.Status, v.Version = session.StatusReserved, 1
	v.UpdatedAt = v.CreatedAt
	r.s.st.sessions[v.ID] = v
	return nil
}

func (r *sessionRepoFake) get(id uuid.UUID) (session.Session, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	v, ok := r.s.st.sessions[id]
	if !ok {
		return session.Session{}, session.ErrNotFound
	}
	return v, nil
}

func (r *sessionRepoFake) GetByID(_ context.Context, id uuid.UUID) (session.Session, error) {
	return r.get(id)
}

func (r *sessionRepoFake) GetForUpdate(_ context.Context, id uuid.UUID) (session.Session, error) {
	return r.get(id)
}

func (r *sessionRepoFake) OwnerOf(_ context.Context, id uuid.UUID) (uuid.UUID, error) {
	v, err := r.get(id)
	return v.UserID, err
}

func (r *sessionRepoFake) FindByIdempotencyKey(_ context.Context, userID uuid.UUID, key string) (session.Session, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	for _, o := range r.s.st.sessions {
		if o.UserID == userID && o.CreateIdempotencyKey == key {
			return o, nil
		}
	}
	return session.Session{}, session.ErrNotFound
}

func (r *sessionRepoFake) FindOpenByUser(_ context.Context, userID uuid.UUID) (session.Session, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	for _, o := range r.s.st.sessions {
		if o.UserID == userID && o.Status.Open() {
			return o, nil
		}
	}
	return session.Session{}, session.ErrNotFound
}

func (r *sessionRepoFake) CAS(_ context.Context, id uuid.UUID, from session.Status, fence session.Fence, p session.Patch) (session.Session, bool, error) {
	if err := r.s.Take("Sessions.CAS"); err != nil {
		return session.Session{}, false, err
	}
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	v, ok := r.s.st.sessions[id]
	if !ok || v.Status != from || (fence != nil && v.RuntimeFence != *fence) {
		return session.Session{}, false, nil
	}
	v.Status = p.To
	if p.EndReason != nil {
		v.EndReason = p.EndReason
	}
	if p.FailureDetail != nil {
		v.FailureDetail = p.FailureDetail
	}
	setTime := func(dst **time.Time, src *time.Time) {
		if src != nil {
			t := *src
			*dst = &t
		}
	}
	setTime(&v.StartingAt, p.StartingAt)
	setTime(&v.StartedAt, p.StartedAt)
	setTime(&v.ExpiresAt, p.ExpiresAt)
	setTime(&v.EndingAt, p.EndingAt)
	setTime(&v.EndedAt, p.EndedAt)
	if p.ChargedSeconds != nil {
		v.ChargedSeconds = *p.ChargedSeconds
	}
	if p.OpeningPromptVersionID != nil {
		id := *p.OpeningPromptVersionID
		v.OpeningPromptVersionID = &id
	}
	if p.BumpFence {
		v.RuntimeFence++
	}
	v.Version++
	v.UpdatedAt = p.At
	r.s.st.sessions[id] = v
	return v, true, nil
}

func (r *sessionRepoFake) Claim(_ context.Context, id uuid.UUID, allowed []session.Status) (session.Session, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	v, ok := r.s.st.sessions[id]
	if !ok || !slices.Contains(allowed, v.Status) {
		return session.Session{}, session.ErrInvalidTransition
	}
	v.RuntimeFence++
	v.HandoffAt = nil
	r.s.st.sessions[id] = v
	return v, nil
}

func (r *sessionRepoFake) MarkHandoff(_ context.Context, id uuid.UUID, fence int64, at time.Time) (bool, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	v, ok := r.s.st.sessions[id]
	if !ok || v.Status != session.StatusActive || v.RuntimeFence != fence {
		return false, nil
	}
	v.HandoffAt, v.UpdatedAt = &at, at
	r.s.st.sessions[id] = v
	return true, nil
}

func (r *sessionRepoFake) AddDisconnected(_ context.Context, id uuid.UUID, fence int64, n int, at time.Time) (session.Session, bool, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	v, ok := r.s.st.sessions[id]
	if !ok || (v.Status != session.StatusActive && v.Status != session.StatusEnding) || v.RuntimeFence != fence {
		return session.Session{}, false, nil
	}
	over := 0
	if v.ExpiresAt != nil {
		over = max(0, int(math.Ceil(at.Sub(*v.ExpiresAt).Seconds())))
	}
	v.DisconnectedSeconds = min(v.DurationSeconds, v.DisconnectedSeconds+max(0, n-over))
	v.Version++
	v.UpdatedAt = at
	r.s.st.sessions[id] = v
	return v, true, nil
}

func (r *sessionRepoFake) ListByUser(_ context.Context, userID uuid.UUID, f session.ListFilter, after *uuid.UUID, limit int) ([]session.Session, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	var out []session.Session
	for _, o := range r.s.st.sessions {
		if o.UserID != userID || (len(f.Statuses) > 0 && !slices.Contains(f.Statuses, o.Status)) {
			continue
		}
		if after != nil && o.ID.String() >= after.String() {
			continue
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID.String() > out[j].ID.String() })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *sessionRepoFake) GetRuntimeRecord(_ context.Context, id uuid.UUID) (session.RuntimeRecord, error) {
	v, err := r.get(id)
	if err != nil {
		return session.RuntimeRecord{}, err
	}
	return session.RuntimeRecordOf(v), nil
}

func (r *sessionRepoFake) SetSummary(_ context.Context, id uuid.UUID, text string, at time.Time) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	v, ok := r.s.st.sessions[id]
	if !ok {
		return session.ErrNotFound
	}
	v.SummaryText, v.UpdatedAt = &text, at
	r.s.st.sessions[id] = v
	return nil
}

// ids returns up to limit ids of the sessions that match, ordered by key.
func (r *sessionRepoFake) ids(limit int, match func(session.Session) (time.Time, bool)) []uuid.UUID {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	type kv struct {
		id uuid.UUID
		at time.Time
	}
	var hits []kv
	for _, v := range r.s.st.sessions {
		if at, ok := match(v); ok {
			hits = append(hits, kv{v.ID, at})
		}
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].at.Before(hits[j].at) })
	out := make([]uuid.UUID, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.id)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func before(t *time.Time, cutoff time.Time) (time.Time, bool) {
	if t == nil || !t.Before(cutoff) {
		return time.Time{}, false
	}
	return *t, true
}

func (r *sessionRepoFake) ListExpired(_ context.Context, cutoff time.Time, limit int) ([]uuid.UUID, error) {
	return r.ids(limit, func(v session.Session) (time.Time, bool) {
		if v.Status != session.StatusActive {
			return time.Time{}, false
		}
		return before(v.ExpiresAt, cutoff)
	}), nil
}

func (r *sessionRepoFake) ListStuckEnding(_ context.Context, cutoff time.Time, limit int) ([]uuid.UUID, error) {
	return r.ids(limit, func(v session.Session) (time.Time, bool) {
		if v.Status != session.StatusEnding {
			return time.Time{}, false
		}
		return before(v.EndingAt, cutoff)
	}), nil
}

func (r *sessionRepoFake) ListStuckStarting(_ context.Context, cutoff time.Time, limit int) ([]uuid.UUID, error) {
	return r.ids(limit, func(v session.Session) (time.Time, bool) {
		if v.Status != session.StatusStarting {
			return time.Time{}, false
		}
		return before(v.StartingAt, cutoff)
	}), nil
}

func (r *sessionRepoFake) ListHandoffStale(_ context.Context, cutoff time.Time, limit int) ([]uuid.UUID, error) {
	return r.ids(limit, func(v session.Session) (time.Time, bool) {
		if v.Status != session.StatusActive {
			return time.Time{}, false
		}
		return before(v.HandoffAt, cutoff)
	}), nil
}

func (r *sessionRepoFake) ListActiveForHeartbeat(_ context.Context, cutoff time.Time, limit int) ([]session.ActiveRef, error) {
	ids := r.ids(limit, func(v session.Session) (time.Time, bool) {
		if v.Status != session.StatusActive || v.HandoffAt != nil {
			return time.Time{}, false
		}
		return before(v.StartedAt, cutoff)
	})
	out := make([]session.ActiveRef, len(ids))
	for i, id := range ids {
		v, _ := r.get(id)
		out[i] = session.ActiveRef{ID: id, StartedAt: *v.StartedAt, HandoffAt: v.HandoffAt}
	}
	return out, nil
}

func (r *sessionRepoFake) CountActiveNonHandoff(context.Context) (int, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	n := 0
	for _, v := range r.s.st.sessions {
		if v.Status == session.StatusActive && v.HandoffAt == nil {
			n++
		}
	}
	return n, nil
}

func (r *sessionRepoFake) ListAbandonCandidates(_ context.Context, cutoff time.Time, limit int) ([]uuid.UUID, error) {
	return r.ids(limit, func(v session.Session) (time.Time, bool) {
		if v.Status != session.StatusReserved {
			return time.Time{}, false
		}
		res := r.s.bal.Reservation(v.ID)
		if res.Status != balance.ReservationHeld || !res.StartDeadlineAt.Before(cutoff) {
			return time.Time{}, false
		}
		return res.StartDeadlineAt, true
	}), nil
}

type turnRepoFake struct{ s *SessionStore }

var _ session.TurnRepository = (*turnRepoFake)(nil)

func (r *turnRepoFake) find(turnID uuid.UUID) int {
	for i, t := range r.s.st.turns {
		if t.ID == turnID {
			return i
		}
	}
	return -1
}

func (r *turnRepoFake) Insert(_ context.Context, fence int64, t session.TurnStart) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if !r.s.fenceOK(t.SessionID, fence) {
		return session.ErrFenceLost
	}
	r.s.st.turns = append(r.s.st.turns, session.Turn{ID: t.ID, SessionID: t.SessionID, Sequence: t.Sequence, Generation: t.Generation, CreatedAt: t.CreatedAt})
	return nil
}

// open returns the index of the in-progress turn when the fence holds, else -1.
func (r *turnRepoFake) open(fence int64, turnID uuid.UUID) int {
	i := r.find(turnID)
	if i < 0 || r.s.st.turns[i].Status != nil || !r.s.fenceOK(r.s.st.turns[i].SessionID, fence) {
		return -1
	}
	return i
}

func (r *turnRepoFake) UpdateUser(_ context.Context, fence int64, turnID uuid.UUID, u session.TurnUser) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	i := r.open(fence, turnID)
	if i < 0 {
		return session.ErrFenceLost
	}
	t := &r.s.st.turns[i]
	text := u.Text
	t.UserText, t.UserStartedAt, t.UserEndedAt, t.STTProvider, t.STTFinalMS, t.STTAudioMS = &text, u.StartedAt, u.EndedAt, u.STTProvider, u.STTFinalMS, u.STTAudioMS
	return nil
}

func applyFinal(t *session.Turn, f session.TurnFinal) {
	st := f.Status
	t.Status, t.CompletedAt = &st, &f.CompletedAt
	t.AssistantText, t.AssistantSpokenText = f.AssistantText, f.AssistantSpokenText
	t.LLMProvider, t.LLMModel, t.TTSProvider, t.TTSModel, t.TTSVoiceID = f.LLMProvider, f.LLMModel, f.TTSProvider, f.TTSModel, f.TTSVoiceID
	t.UsedFallback = f.UsedFallback
	t.TurnEndToFirstAudioMS, t.LLMFirstTokenMS, t.TTSFirstAudioMS = f.TurnEndToFirstAudioMS, f.LLMFirstTokenMS, f.TTSFirstAudioMS
	t.InputTokens, t.OutputTokens, t.TTSCharacters, t.ErrorCode = f.InputTokens, f.OutputTokens, f.TTSCharacters, f.ErrorCode
}

func (r *turnRepoFake) Finalize(_ context.Context, fence int64, turnID uuid.UUID, f session.TurnFinal) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	i := r.open(fence, turnID)
	if i < 0 {
		return session.ErrFenceLost
	}
	applyFinal(&r.s.st.turns[i], f)
	return nil
}

func (r *turnRepoFake) FailInProgress(_ context.Context, fence int64, sessionID uuid.UUID, partial *session.TurnFinal) (int, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if !r.s.fenceOK(sessionID, fence) {
		return 0, session.ErrFenceLost
	}
	n := 0
	for i := range r.s.st.turns {
		t := &r.s.st.turns[i]
		if t.SessionID != sessionID || t.Status != nil {
			continue
		}
		code := "TAKEOVER"
		now := time.Now().UTC()
		st := session.TurnFailed
		t.Status, t.CompletedAt, t.ErrorCode = &st, &now, &code
		if partial != nil {
			if partial.ErrorCode != nil {
				t.ErrorCode = partial.ErrorCode
			}
			if partial.AssistantText != nil {
				t.AssistantText = partial.AssistantText
			}
			if partial.AssistantSpokenText != nil {
				t.AssistantSpokenText = partial.AssistantSpokenText
			}
		}
		n++
	}
	return n, nil
}

func (r *turnRepoFake) ListRecent(_ context.Context, sessionID uuid.UUID, limit int) ([]session.TurnRecord, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	all := r.s.turnsOf(sessionID)
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	return all, nil
}

func (r *turnRepoFake) MaxSeqAndGeneration(_ context.Context, sessionID uuid.UUID) (int, int, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	maxSeq, maxGen := -1, 0
	for _, t := range r.s.turnsOf(sessionID) {
		maxSeq, maxGen = max(maxSeq, t.Sequence), max(maxGen, t.Generation)
	}
	return maxSeq, maxGen, nil
}

func (r *turnRepoFake) FailDangling(_ context.Context, sessionID uuid.UUID, _ time.Time, at time.Time) (int, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	n := 0
	for i := range r.s.st.turns {
		t := &r.s.st.turns[i]
		if t.SessionID != sessionID || t.Status != nil {
			continue
		}
		st, code := session.TurnFailed, "SESSION_ENDED"
		t.Status, t.CompletedAt = &st, &at
		if t.ErrorCode == nil {
			t.ErrorCode = &code
		}
		n++
	}
	return n, nil
}

func (r *turnRepoFake) Stats(_ context.Context, sessionID uuid.UUID, _ time.Time) (int, int, bool, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	turns, users, audio := 0, 0, false
	for _, t := range r.s.turnsOf(sessionID) {
		if t.Status != nil && *t.Status == session.TurnDiscarded {
			continue
		}
		turns++
		if t.UserText != nil && len(trimSpace(*t.UserText)) > 0 {
			users++
		}
		if t.STTAudioMS != nil || t.TTSFirstAudioMS != nil {
			audio = true
		}
	}
	return turns, users, audio, nil
}

func trimSpace(s string) string { return strings.TrimSpace(s) }

func (r *turnRepoFake) ListForUser(_ context.Context, sessionID uuid.UUID, _ time.Time, _ *time.Time, afterSeq *int, limit int) ([]session.TurnRecord, error) {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	var out []session.Turn
	for _, t := range r.s.turnsOf(sessionID) {
		if (t.Status != nil && *t.Status == session.TurnDiscarded) || (afterSeq != nil && t.Sequence <= *afterSeq) {
			continue
		}
		out = append(out, t)
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

type eventRepoFake struct{ s *SessionStore }

var _ session.EventRepository = (*eventRepoFake)(nil)

func (r *eventRepoFake) Append(_ context.Context, e session.Event) error {
	if err := r.s.Take("Events.Append"); err != nil {
		return err
	}
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	r.s.st.events = append(r.s.st.events, e)
	return nil
}

func (r *eventRepoFake) AppendFenced(_ context.Context, fence int64, e session.Event) error {
	r.s.mu.Lock()
	defer r.s.mu.Unlock()
	if !r.s.fenceOK(e.SessionID, fence) {
		return session.ErrFenceLost
	}
	r.s.st.events = append(r.s.st.events, e)
	return nil
}
