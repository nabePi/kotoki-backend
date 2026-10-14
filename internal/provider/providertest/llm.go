// Package providertest holds the conformance suites every provider implementation (fake
// or real adapter) must pass (spec 8.11). It is imported only by _test.go files.
package providertest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

const eventTimeout = 5 * time.Second

func collectLLM(t *testing.T, s provider.LLMStream) (text string, events []provider.LLMEvent) {
	t.Helper()
	deadline := time.After(eventTimeout)
	var b strings.Builder
	for {
		select {
		case ev, ok := <-s.Events():
			if !ok {
				return b.String(), events
			}
			events = append(events, ev)
			if ev.Type == provider.LLMDelta {
				b.WriteString(ev.Text)
			}
		case <-deadline:
			t.Fatal("providertest: LLM stream did not finish in time")
		}
	}
}

func userMessage(content string) []provider.Message {
	return []provider.Message{{Role: provider.RoleSystem, Content: "あなたは日本語の会話相手です。"}, {Role: provider.RoleUser, Content: content}}
}

// LLMSuite checks the provider.LLMProvider contract (spec 8.2.3). newProvider must return
// a provider that answers a request containing "#long" with more than 5 runes of text.
func LLMSuite(t *testing.T, newProvider func(t *testing.T) provider.LLMProvider) {
	t.Helper()

	t.Run("StreamReturnsBeforeFirstToken", func(t *testing.T) {
		p := newProvider(t)
		start := time.Now()
		s, err := p.Stream(context.Background(), provider.LLMRequest{Messages: userMessage("こんにちは #long")})
		require.NoError(t, err)
		defer func() { _ = s.Close() }()
		require.Less(t, time.Since(start), 50*time.Millisecond, "Stream must not wait for the first token")
	})

	t.Run("EndsWithDoneWithUsage", func(t *testing.T) {
		p := newProvider(t)
		s, err := p.Stream(context.Background(), provider.LLMRequest{Messages: userMessage("こんにちは")})
		require.NoError(t, err)
		text, events := collectLLM(t, s)
		require.NotEmpty(t, text)
		require.NotEmpty(t, events)
		last := events[len(events)-1]
		require.Equal(t, provider.LLMDone, last.Type)
		require.NotNil(t, last.Usage)
		require.Equal(t, p.Name(), last.Provider)
		require.NotZero(t, last.Usage.InputTokens)
		require.NotZero(t, last.Usage.OutputTokens)
	})

	t.Run("MaxTokensTruncatesToFinishMaxTokens", func(t *testing.T) {
		p := newProvider(t)
		s, err := p.Stream(context.Background(), provider.LLMRequest{Messages: userMessage("長く答えて #long"), MaxTokens: 5})
		require.NoError(t, err)
		_, events := collectLLM(t, s)
		last := events[len(events)-1]
		require.Equal(t, provider.LLMDone, last.Type)
		require.Equal(t, provider.FinishMaxTokens, last.FinishReason)
	})

	t.Run("StopSequenceCutsBeforeMatch", func(t *testing.T) {
		p := newProvider(t)
		s, err := p.Stream(context.Background(), provider.LLMRequest{Messages: userMessage("なるほど"), Stop: []string{"。"}})
		require.NoError(t, err)
		text, events := collectLLM(t, s)
		require.NotContains(t, text, "。")
		require.Equal(t, provider.FinishStop, events[len(events)-1].FinishReason)
	})

	t.Run("CancelEmitsCancelledDoneWithin50ms", func(t *testing.T) {
		p := newProvider(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		s, err := p.Stream(ctx, provider.LLMRequest{Messages: userMessage("長く答えて #long")})
		require.NoError(t, err)
		select {
		case ev := <-s.Events():
			require.Equal(t, provider.LLMDelta, ev.Type)
		case <-time.After(eventTimeout):
			t.Fatal("no first token")
		}
		start := time.Now()
		cancel()
		var last provider.LLMEvent
		for ev := range s.Events() {
			last = ev
		}
		require.Less(t, time.Since(start), 50*time.Millisecond)
		require.Equal(t, provider.LLMDone, last.Type)
		require.Equal(t, provider.FinishCancelled, last.FinishReason)
		require.NotNil(t, last.Usage)
		require.NoError(t, s.Close())
		require.NoError(t, s.Close(), "Close is idempotent")
	})

	t.Run("CloseCancels", func(t *testing.T) {
		p := newProvider(t)
		s, err := p.Stream(context.Background(), provider.LLMRequest{Messages: userMessage("長く答えて #long")})
		require.NoError(t, err)
		require.NoError(t, s.Close())
		done := make(chan struct{})
		go func() {
			for range s.Events() {
			}
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(eventTimeout):
			t.Fatal("channel not closed after Close")
		}
	})

	t.Run("SyncErrorIsProviderError", func(t *testing.T) {
		p := newProvider(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := p.Stream(ctx, provider.LLMRequest{Messages: userMessage("こんにちは")})
		require.Error(t, err)
		var pe *provider.Error
		require.True(t, errors.As(err, &pe), "synchronous failures are *provider.Error, got %T", err)
		require.Equal(t, provider.KindCancelled, pe.Kind)
	})
}
