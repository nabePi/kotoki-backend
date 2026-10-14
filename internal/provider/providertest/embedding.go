package providertest

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// EmbeddingSuite checks the provider.EmbeddingProvider contract (spec 8.2.6).
func EmbeddingSuite(t *testing.T, newProvider func(t *testing.T) provider.EmbeddingProvider) {
	t.Helper()
	texts := []string{"日本語の勉強が好きです", "今日の天気は雨です", "映画を見に行きました"}

	t.Run("VectorsDims1536InOrder", func(t *testing.T) {
		p := newProvider(t)
		batch, err := p.Embed(context.Background(), provider.EmbeddingRequest{Texts: texts, Dimensions: 1536})
		require.NoError(t, err)
		require.Len(t, batch.Vectors, len(texts))
		for _, v := range batch.Vectors {
			require.Len(t, v, 1536)
		}
		for i, text := range texts {
			single, err := p.Embed(context.Background(), provider.EmbeddingRequest{Texts: []string{text}, Dimensions: 1536})
			require.NoError(t, err)
			require.Greater(t, cosine(batch.Vectors[i], single.Vectors[0]), 0.99, "vector %d is not in request order", i)
		}
		require.Equal(t, p.Name(), batch.Provider)
	})

	t.Run("UsagePositiveNoOutputTokens", func(t *testing.T) {
		p := newProvider(t)
		res, err := p.Embed(context.Background(), provider.EmbeddingRequest{Texts: texts, Dimensions: 1536})
		require.NoError(t, err)
		require.Positive(t, res.Usage.InputTokens)
		require.Zero(t, res.Usage.OutputTokens)
	})

	t.Run("EmptyInputIsInvalidRequest", func(t *testing.T) {
		p := newProvider(t)
		_, err := p.Embed(context.Background(), provider.EmbeddingRequest{Dimensions: 1536})
		var pe *provider.Error
		require.True(t, errors.As(err, &pe), "got %T", err)
		require.Equal(t, provider.KindInvalidRequest, pe.Kind)
	})
}
