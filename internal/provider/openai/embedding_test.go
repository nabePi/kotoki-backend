package openai_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/nabePi/kotoki-backend/internal/provider"
	"github.com/nabePi/kotoki-backend/internal/provider/httpx"
	"github.com/nabePi/kotoki-backend/internal/provider/openai"
	"github.com/nabePi/kotoki-backend/internal/testutil/replay"
)

func newEmbed(t *testing.T, srv *replay.Server) *openai.Embedding {
	t.Helper()
	e, err := openai.NewEmbedding(openai.Config{APIKey: "sk-test", BaseURL: srv.URL}, httpx.NewClient(), bufLogger(&logBuf{}))
	require.NoError(t, err)
	return e
}

func TestOpenAIEmbedBodyAndOrder(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "embed_basic"))
	e := newEmbed(t, srv)
	require.Equal(t, provider.OpenAI, e.Name())
	res, err := e.Embed(ctx, provider.EmbeddingRequest{Texts: []string{"a", "b"}, Model: "m", Dimensions: 1536})
	require.NoError(t, err)
	require.JSONEq(t, `{"model":"m","input":["a","b"],"encoding_format":"float"}`, string(srv.Requests()[0].Body))
	require.Len(t, res.Vectors, 2)
	require.Len(t, res.Vectors[0], 1536)
	// the fixture answers index [1,0]: vector i must belong to Texts[i]
	require.InDelta(t, 0.25, res.Vectors[0][0], 1e-6)
	require.InDelta(t, 0.5, res.Vectors[1][0], 1e-6)
	srv.AssertDone()
}

func TestOpenAIEmbedUsage(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "embed_basic"))
	res, err := newEmbed(t, srv).Embed(ctx, provider.EmbeddingRequest{Texts: []string{"a", "b"}, Model: "m"})
	require.NoError(t, err)
	require.Equal(t, 5, res.Usage.InputTokens)
	require.Zero(t, res.Usage.OutputTokens)
	require.Equal(t, provider.OpenAI, res.Provider)
	require.Equal(t, "m", res.Model)
}

func TestOpenAIEmbedWrongDimension(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "embed_wrong_dim"))
	_, err := newEmbed(t, srv).Embed(ctx, provider.EmbeddingRequest{Texts: []string{"a", "b"}, Model: "m"})
	pe := asProviderErr(t, err)
	require.Equal(t, provider.KindInvalidResponse, pe.Kind)
	require.Equal(t, provider.CapEmbedding, pe.Capability)
}

func TestOpenAIEmbedRejectsOver64Texts(t *testing.T) {
	srv := replay.NewHTTPServer(t, recording(t, "embed_basic"))
	e := newEmbed(t, srv)
	texts := make([]string, 65)
	for i := range texts {
		texts[i] = "x"
	}
	_, err := e.Embed(ctx, provider.EmbeddingRequest{Texts: texts, Model: "m"})
	require.Equal(t, provider.KindInvalidRequest, asProviderErr(t, err).Kind)
	_, err = e.Embed(ctx, provider.EmbeddingRequest{Model: "m"})
	require.Equal(t, provider.KindInvalidRequest, asProviderErr(t, err).Kind)
	_, err = e.Embed(ctx, provider.EmbeddingRequest{Texts: []string{"a"}, Model: "m", Dimensions: 512})
	require.Equal(t, provider.KindInvalidRequest, asProviderErr(t, err).Kind)
	require.Empty(t, srv.Requests(), "rejected before any request")
	// exactly 64 is accepted by the guard (the fixture then fails on count, a different kind)
	_, err = e.Embed(ctx, provider.EmbeddingRequest{Texts: texts[:64], Model: "m"})
	require.NotEqual(t, provider.KindInvalidRequest, provider.KindOf(err))
}

func TestOpenAIEmbedBadIndexAndCount(t *testing.T) {
	for name, body := range map[string]string{
		"count":    `{"data":[],"usage":{"prompt_tokens":1}}`,
		"badindex": `{"data":[{"index":5,"embedding":[]},{"index":0,"embedding":[]}],"usage":{"prompt_tokens":1}}`,
		"repeat":   `{"data":[{"index":0,"embedding":[]},{"index":0,"embedding":[]}],"usage":{"prompt_tokens":1}}`,
		"notjson":  `nope`,
	} {
		srv := replay.NewHTTPServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"OPENAI","capability":"EMBEDDING","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/embeddings"},"respond":{"status":200,"chunks":[{"at_ms":0,"text":`+quote(body)+`}]}}}`))
		_, err := newEmbed(t, srv).Embed(ctx, provider.EmbeddingRequest{Texts: []string{"a", "b"}, Model: "m"})
		require.Equal(t, provider.KindInvalidResponse, asProviderErr(t, err).Kind, name)
	}
}

func TestOpenAIEmbedHTTPError(t *testing.T) {
	srv := replay.NewHTTPServer(t, replay.LoadScriptString(t, `{"meta":{"vendor":"OPENAI","capability":"EMBEDDING","source":"x","verified":false}}
{"http":{"expect":{"method":"POST","path":"/embeddings"},"respond":{"status":429,"headers":{"Retry-After":"2"},"chunks":[{"at_ms":0,"text":"{\"error\":{\"message\":\"slow down sk-test\",\"code\":\"rate_limit_exceeded\"}}"}]}}}`))
	_, err := newEmbed(t, srv).Embed(ctx, provider.EmbeddingRequest{Texts: []string{"a"}, Model: "m"})
	pe := asProviderErr(t, err)
	require.Equal(t, provider.KindRateLimited, pe.Kind)
	require.True(t, pe.Retryable)
	require.NotContains(t, pe.Message, "sk-test")
}
