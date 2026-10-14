package fake

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/platform/clock"
	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/providertest"
)

func newTestEmbedding(cfg CapConfig) *Embedding {
	return NewEmbedding(provider.FakePrimary, cfg, Config{}, Deps{Clock: clock.NewFake(t0), Seed: 42})
}

func embedAll(t *testing.T, texts ...string) provider.EmbeddingResult {
	t.Helper()
	r, err := newTestEmbedding(CapConfig{}).Embed(context.Background(), provider.EmbeddingRequest{Texts: texts, Dimensions: 1536})
	require.NoError(t, err)
	return r
}

func cosine(a, b []float32) float64 {
	var dot float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
	}
	return dot / (norm(a) * norm(b))
}

func norm(v []float32) float64 {
	var s float64
	for _, x := range v {
		s += float64(x) * float64(x)
	}
	return math.Sqrt(s)
}

func TestFakeEmbeddingConformance(t *testing.T) {
	providertest.EmbeddingSuite(t, func(_ *testing.T) provider.EmbeddingProvider { return newTestEmbedding(CapConfig{}) })
}

func TestEmbeddingSimilarTextsHaveHigherCosine(t *testing.T) {
	r := embedAll(t, "日本語の勉強が好きです", "日本語の勉強が楽しいです", "今日の天気は雨です")
	require.Len(t, r.Vectors[0], 1536)
	require.Greater(t, cosine(r.Vectors[0], r.Vectors[1]), cosine(r.Vectors[0], r.Vectors[2]))
	require.InDelta(t, 1.0, norm(r.Vectors[0]), 1e-6)
}

func TestEmbeddingDeterministic(t *testing.T) {
	a := embedAll(t, "こんにちは")
	b := embedAll(t, "こんにちは")
	require.Equal(t, a.Vectors[0], b.Vectors[0])
}

func TestEmbeddingEmptyTextIsUnitE0(t *testing.T) {
	r := embedAll(t, "")
	require.EqualValues(t, 1, r.Vectors[0][0])
	for _, x := range r.Vectors[0][1:] {
		require.Zero(t, x)
	}
}

func TestEmbeddingBatchLimits(t *testing.T) {
	e := newTestEmbedding(CapConfig{})
	texts := make([]string, 64)
	for i := range texts {
		texts[i] = "あ"
	}
	_, err := e.Embed(context.Background(), provider.EmbeddingRequest{Texts: texts})
	require.NoError(t, err)
	_, err = e.Embed(context.Background(), provider.EmbeddingRequest{Texts: append(texts, "い")})
	require.Equal(t, provider.KindInvalidRequest, provider.KindOf(err))
	_, err = e.Embed(context.Background(), provider.EmbeddingRequest{Texts: texts[:1], Dimensions: 1024})
	require.Equal(t, provider.KindInvalidRequest, provider.KindOf(err))
}

func TestEmbeddingUsage(t *testing.T) {
	r := embedAll(t, "あい", "うえお")
	require.Equal(t, 5, r.Usage.InputTokens)
	require.Zero(t, r.Usage.OutputTokens)
	require.Equal(t, "fake-embedding", r.Model)
}

func TestEmbeddingFault(t *testing.T) {
	e := newTestEmbedding(CapConfig{FailEveryN: 1, FailureKind: provider.KindUnavailable})
	_, err := e.Embed(context.Background(), provider.EmbeddingRequest{Texts: []string{"あ"}})
	require.Equal(t, provider.KindUnavailable, provider.KindOf(err))
}
