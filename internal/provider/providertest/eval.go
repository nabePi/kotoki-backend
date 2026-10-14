package providertest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

// EvaluationSuite checks the provider.EvaluationProvider contract (spec 8.2.5). req is a
// valid request for a schema the provider can answer.
func EvaluationSuite(t *testing.T, newProvider func(t *testing.T) provider.EvaluationProvider, req provider.StructuredRequest) {
	t.Helper()

	t.Run("JSONValidAndUsagePositive", func(t *testing.T) {
		p := newProvider(t)
		res, err := p.GenerateJSON(context.Background(), req)
		require.NoError(t, err)
		require.True(t, json.Valid(res.JSON))
		require.Positive(t, res.Usage.InputTokens)
		require.Positive(t, res.Usage.OutputTokens)
		require.Equal(t, provider.FinishStop, res.FinishReason)
		require.Equal(t, p.Name(), res.Provider)
		require.NotEmpty(t, res.Model)
	})

	t.Run("InvalidSchemaNameIsInvalidRequest", func(t *testing.T) {
		p := newProvider(t)
		bad := req
		bad.SchemaName = "NOT A VALID NAME!"
		_, err := p.GenerateJSON(context.Background(), bad)
		var pe *provider.Error
		require.True(t, errors.As(err, &pe), "got %T", err)
		require.Equal(t, provider.KindInvalidRequest, pe.Kind)
	})

	t.Run("CancelledContextIsAnError", func(t *testing.T) {
		p := newProvider(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := p.GenerateJSON(ctx, req)
		require.Error(t, err)
		require.Equal(t, provider.KindCancelled, provider.KindOf(err))
	})
}
