package fakes

import (
	"context"
	"strconv"
	"sync"

	"github.com/google/uuid"

	"github.com/nabePi/kotoki-backend/internal/conversation"
	"github.com/nabePi/kotoki-backend/internal/learner"
	"github.com/nabePi/kotoki-backend/internal/prompt"
)

var _ conversation.LearnerContextProvider = (*LearnerContext)(nil)

// LearnerContext is a conversation.LearnerContextProvider answering Ctx with an Adaptation line
// derived from the difficulty (so tests can tell renders apart). FailNext("PromptContext", err)
// fails the next PromptContext.
type LearnerContext struct {
	FailInjector
	mu sync.Mutex
	// Ctx is the base context; DisplayName and GoalLine come from it.
	Ctx       prompt.LearnerContext
	users     []uuid.UUID
	rerenders int
}

// adaptation is the stand-in for the finished <conversation_difficulty> block.
func adaptation(ad learner.AdaptiveDifficulty) string {
	return "difficulty speed=" + strconv.Itoa(ad.SpeechSpeed) + " adjustments=" + strconv.Itoa(ad.InSessionAdjustments)
}

// PromptContext implements conversation.LearnerContextProvider.
func (l *LearnerContext) PromptContext(_ context.Context, userID uuid.UUID, ad learner.AdaptiveDifficulty) (prompt.LearnerContext, error) {
	if err := l.Take("PromptContext"); err != nil {
		return prompt.LearnerContext{}, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.users = append(l.users, userID)
	out := l.Ctx
	out.Adaptation = adaptation(ad)
	return out, nil
}

// Rerender implements conversation.LearnerContextProvider.
func (l *LearnerContext) Rerender(prev prompt.LearnerContext, ad learner.AdaptiveDifficulty) prompt.LearnerContext {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rerenders++
	prev.Adaptation = adaptation(ad)
	return prev
}

// Users returns the user ids PromptContext was asked for.
func (l *LearnerContext) Users() []uuid.UUID {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]uuid.UUID(nil), l.users...)
}

// Rerenders returns the number of Rerender calls.
func (l *LearnerContext) Rerenders() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rerenders
}
