package fake

import (
	"context"
	"hash/fnv"
	"math"
	"unicode/utf8"

	"github.com/nabePi/kotoki-backend/internal/provider"
)

const (
	embeddingDims     = 1536
	maxEmbeddingTexts = 64
	defaultEmbedModel = "fake-embedding"
)

// Embedding is the fake provider.EmbeddingProvider: deterministic 1536-dim vectors from
// hashed character trigrams, so similar texts have a higher cosine.
type Embedding struct {
	name provider.Name
	cfg  CapConfig
	deps Deps
	dec  *decider
	jit  *jitterSource
}

var _ provider.EmbeddingProvider = (*Embedding)(nil)

// NewEmbedding builds a fake embedding provider.
func NewEmbedding(name provider.Name, c CapConfig, _ Config, d Deps) *Embedding {
	d = d.WithDefaults()
	return &Embedding{name: name, cfg: c, deps: d, dec: newDecider(d.Seed, name, provider.CapEmbedding, c), jit: newJitterSource(d.Seed, 0x454d42)}
}

// Name implements provider.EmbeddingProvider.
func (e *Embedding) Name() provider.Name { return e.name }

func fnv32(s string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(s))
	return h.Sum32()
}

// vectorFor hashes the rune trigrams of "^text$" into 1536 buckets with a ±1 sign, then
// L2-normalises; empty text is the unit vector e_0.
func vectorFor(text string) []float32 {
	v := make([]float32, embeddingDims)
	r := append(append([]rune{'^'}, []rune(text)...), '$')
	for i := 0; i+3 <= len(r); i++ {
		g := string(r[i : i+3])
		sign := float32(1)
		if fnv32("s"+g)%2 == 1 {
			sign = -1
		}
		v[fnv32(g)%embeddingDims] += sign
	}
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	if sum == 0 {
		v[0] = 1
		return v
	}
	norm := float32(math.Sqrt(sum))
	for i := range v {
		v[i] /= norm
	}
	return v
}

// Embed implements provider.EmbeddingProvider.
func (e *Embedding) Embed(ctx context.Context, req provider.EmbeddingRequest) (provider.EmbeddingResult, error) {
	if n := len(req.Texts); n < 1 || n > maxEmbeddingTexts {
		return provider.EmbeddingResult{}, provider.NewError(e.name, provider.CapEmbedding, provider.KindInvalidRequest, "fake: 1..64 texts required")
	}
	if req.Dimensions != 0 && req.Dimensions != embeddingDims {
		return provider.EmbeddingResult{}, provider.NewError(e.name, provider.CapEmbedding, provider.KindInvalidRequest, "fake: Dimensions must be 1536")
	}
	if _, ferr := oneShot(ctx, e.deps, e.jit, e.dec, e.cfg, e.name, provider.CapEmbedding); ferr != nil {
		return provider.EmbeddingResult{}, ferr
	}
	model := req.Model
	if model == "" {
		model = defaultEmbedModel
	}
	res := provider.EmbeddingResult{Provider: e.name, Model: model, Vectors: make([][]float32, len(req.Texts))}
	for i, t := range req.Texts {
		res.Vectors[i] = vectorFor(t)
		res.Usage.InputTokens += utf8.RuneCountInString(t)
	}
	return res, nil
}
