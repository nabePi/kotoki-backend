package fakes

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/session"
)

var _ conversation.TurnRepository = (*TurnRepo)(nil)

// TurnRepo is an in-memory conversation.TurnRepository. SetFence arms the fence guard of spec
// 7.9.3: while it is non-zero, a write with another fence is session.ErrFenceLost. FailNext
// ("<Method>", err) fails the next call of a method. Safe for concurrent use.
type TurnRepo struct {
	FailInjector
	mu    sync.Mutex
	fence int64
	// FenceOf, when set, is the fence of the session in the database (SessionRuntime.Fence): a write
	// with another fence is session.ErrFenceLost, as the fenced SQL of spec 7.9.3 would answer.
	FenceOf  func(sid uuid.UUID) int64
	turns    []session.Turn
	attempts []WriteAttempt
	holds    map[string]chan struct{}
}

// Hold makes every call of method (Insert, UpdateUser, Finalize, FailInProgress) wait, before it is
// counted as an attempt or checked against the fence, until the returned release is called: a
// database that is slow, so the persister's queue fills behind the job in flight.
func (r *TurnRepo) Hold(method string) (release func()) {
	ch := make(chan struct{})
	r.mu.Lock()
	if r.holds == nil {
		r.holds = map[string]chan struct{}{}
	}
	r.holds[method] = ch
	r.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

// wait blocks while method is held; the caller does not hold r.mu.
func (r *TurnRepo) wait(method string) {
	r.mu.Lock()
	ch := r.holds[method]
	r.mu.Unlock()
	if ch != nil {
		<-ch
	}
}

// WriteAttempt is one write call, accepted or not, with the fence it carried.
type WriteAttempt struct {
	Method string
	Fence  int64
	OK     bool
}

// Attempts returns every write call so far in order, the refused ones included.
func (r *TurnRepo) Attempts() []WriteAttempt {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.attempts)
}

// NewTurnRepo returns an empty repository without a fence guard.
func NewTurnRepo() *TurnRepo { return &TurnRepo{} }

// SetFence makes writes with any other fence fail (0 disables the guard).
func (r *TurnRepo) SetFence(f int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fence = f
}

// Turns returns a copy of the turns of a session in sequence order.
func (r *TurnRepo) Turns(sid uuid.UUID) []session.Turn {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []session.Turn
	for _, t := range r.turns {
		if t.SessionID == sid {
			out = append(out, t)
		}
	}
	slices.SortFunc(out, func(a, b session.Turn) int { return a.Sequence - b.Sequence })
	return out
}

// guard is the fence check of a write; the caller holds r.mu. sid is the session the write is for.
func (r *TurnRepo) guard(method string, fence int64, sid uuid.UUID) error {
	err := r.Take(method)
	if err == nil && r.fence != 0 && r.fence != fence {
		err = session.ErrFenceLost
	}
	if err == nil && r.FenceOf != nil {
		if cur := r.FenceOf(sid); cur != 0 && cur != fence {
			err = session.ErrFenceLost
		}
	}
	r.attempts = append(r.attempts, WriteAttempt{Method: method, Fence: fence, OK: err == nil})
	return err
}

// sessionOf is the session of turn id (uuid.Nil when unknown); the caller holds r.mu.
func (r *TurnRepo) sessionOf(id uuid.UUID) uuid.UUID {
	if t := r.find(id); t != nil {
		return t.SessionID
	}
	return uuid.Nil
}

func (r *TurnRepo) find(id uuid.UUID) *session.Turn {
	for i := range r.turns {
		if r.turns[i].ID == id {
			return &r.turns[i]
		}
	}
	return nil
}

// Insert implements conversation.TurnRepository.
func (r *TurnRepo) Insert(_ context.Context, fence int64, t conversation.TurnStart) error {
	r.wait("Insert")
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.guard("Insert", fence, t.SessionID); err != nil {
		return err
	}
	if r.find(t.ID) != nil {
		return nil // idempotent
	}
	r.turns = append(r.turns, session.Turn{ID: t.ID, SessionID: t.SessionID, Sequence: t.Sequence, Generation: t.Generation, CreatedAt: t.CreatedAt})
	return nil
}

// UpdateUser implements conversation.TurnRepository.
func (r *TurnRepo) UpdateUser(_ context.Context, fence int64, id uuid.UUID, u conversation.TurnUser) error {
	r.wait("UpdateUser")
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.guard("UpdateUser", fence, r.sessionOf(id)); err != nil {
		return err
	}
	t := r.find(id)
	if t == nil {
		return session.ErrNotFound
	}
	text := u.Text
	t.UserText, t.UserStartedAt, t.UserEndedAt = &text, u.StartedAt, u.EndedAt
	t.STTProvider, t.STTFinalMS, t.STTAudioMS = u.STTProvider, u.STTFinalMS, u.STTAudioMS
	return nil
}

func apply(t *session.Turn, f conversation.TurnFinal) {
	st := f.Status
	done := f.CompletedAt
	t.Status, t.CompletedAt = &st, &done
	t.AssistantText, t.AssistantSpokenText = f.AssistantText, f.AssistantSpokenText
	t.LLMProvider, t.LLMModel, t.TTSProvider, t.TTSModel, t.TTSVoiceID = f.LLMProvider, f.LLMModel, f.TTSProvider, f.TTSModel, f.TTSVoiceID
	t.UsedFallback = f.UsedFallback
	t.TurnEndToFirstAudioMS, t.LLMFirstTokenMS, t.TTSFirstAudioMS = f.TurnEndToFirstAudioMS, f.LLMFirstTokenMS, f.TTSFirstAudioMS
	t.InputTokens, t.OutputTokens, t.TTSCharacters, t.ErrorCode = f.InputTokens, f.OutputTokens, f.TTSCharacters, f.ErrorCode
	if f.Generation != nil {
		t.Generation = *f.Generation
	}
}

// Finalize implements conversation.TurnRepository; a turn already final is left as it is.
func (r *TurnRepo) Finalize(_ context.Context, fence int64, id uuid.UUID, f conversation.TurnFinal) error {
	r.wait("Finalize")
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.guard("Finalize", fence, r.sessionOf(id)); err != nil {
		return err
	}
	t := r.find(id)
	if t == nil {
		return session.ErrNotFound
	}
	if t.Status == nil {
		apply(t, f)
	}
	return nil
}

// FailInProgress implements conversation.TurnRepository: every turn without a status becomes FAILED.
func (r *TurnRepo) FailInProgress(_ context.Context, fence int64, sid uuid.UUID, partial *conversation.TurnFinal) (int, error) {
	r.wait("FailInProgress")
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.guard("FailInProgress", fence, sid); err != nil {
		return 0, err
	}
	n := 0
	for i := range r.turns {
		t := &r.turns[i]
		if t.SessionID != sid || t.Status != nil {
			continue
		}
		f := conversation.TurnFinal{CompletedAt: time.Now().UTC()}
		if partial != nil {
			f = *partial
		}
		f.Status = session.TurnFailed
		apply(t, f)
		n++
	}
	return n, nil
}

// ListRecent implements conversation.TurnRepository: the newest limit non-DISCARDED turns, oldest first.
func (r *TurnRepo) ListRecent(_ context.Context, sid uuid.UUID, limit int) ([]conversation.TurnRecord, error) {
	if err := r.Take("ListRecent"); err != nil {
		return nil, err
	}
	var out []conversation.TurnRecord
	for _, t := range r.Turns(sid) {
		if t.Status == nil || *t.Status != session.TurnDiscarded {
			out = append(out, t)
		}
	}
	if limit >= 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

// MaxSeqAndGeneration implements conversation.TurnRepository: (-1, 0) when there is no turn.
func (r *TurnRepo) MaxSeqAndGeneration(_ context.Context, sid uuid.UUID) (int, int, error) {
	if err := r.Take("MaxSeqAndGeneration"); err != nil {
		return 0, 0, err
	}
	maxSeq, maxGen := -1, 0
	for _, t := range r.Turns(sid) {
		maxSeq, maxGen = max(maxSeq, t.Sequence), max(maxGen, t.Generation)
	}
	return maxSeq, maxGen, nil
}
