// Package embeddingstest provides a deterministic, test-grade
// `embeddings.Embedder` for conformance suites and integration
// tests that need meaningful similarity without provider traffic.
//
// The embedder is NEVER registered with the embeddings driver
// registry and is never a production default (AGENTS.md §13 — test
// stubs stay out of operator-facing seams). It lives in its own
// clearly-named subpackage, mirroring the conformance-suite
// precedent, so test authors across subsystems (memory, skills,
// integration) share one implementation instead of growing private
// fakes.
//
// Vector model: a bag-of-words hash projection. Each lowercase
// whitespace-separated token hashes (FNV-1a) onto one of `Dim`
// buckets; the bucket counts are L2-normalised. Texts sharing more
// tokens therefore land closer under cosine similarity — enough
// signal for retrieval-ranking assertions, fully deterministic, and
// identity-respecting (the embedder itself holds no per-call
// state).
package embeddingstest

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"strings"
	"sync/atomic"

	"github.com/hurtener/Harbor/internal/embeddings"
)

// Dim is the fixed vector dimension the deterministic embedder
// produces.
const Dim = 64

// Deterministic is the test-grade embedder. Construct with `New`.
// Safe for concurrent use: the only mutable field is the atomic
// closed flag; `Calls` is an atomic counter tests may assert on.
type Deterministic struct {
	closed atomic.Bool

	// Calls counts Embed invocations (not texts). Tests assert on it
	// to prove a consumer actually drove the embedder.
	Calls atomic.Int64
}

// New constructs a deterministic test embedder.
func New() *Deterministic {
	return &Deterministic{}
}

// Compile-time assertion.
var _ embeddings.Embedder = (*Deterministic)(nil)

// Embed implements embeddings.Embedder. Identity-mandatory like the
// production edge: tests that forget to stamp an identity fail the
// same way production would.
func (d *Deterministic) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if d.closed.Load() {
		return nil, embeddings.ErrClientClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !embeddings.HasIdentity(ctx) {
		return nil, embeddings.ErrIdentityMissing
	}
	if len(texts) == 0 {
		return nil, fmt.Errorf("%w: no texts supplied", embeddings.ErrEmptyInput)
	}
	d.Calls.Add(1)
	out := make([][]float32, len(texts))
	for i, t := range texts {
		if t == "" {
			return nil, fmt.Errorf("%w: texts[%d] is empty", embeddings.ErrEmptyInput, i)
		}
		out[i] = vectorFor(t)
	}
	return out, nil
}

// Close implements embeddings.Embedder. Idempotent.
func (d *Deterministic) Close(_ context.Context) error {
	d.closed.Store(true)
	return nil
}

// vectorFor projects one text onto the bag-of-words hash space.
func vectorFor(text string) []float32 {
	vec := make([]float32, Dim)
	for _, tok := range strings.Fields(strings.ToLower(text)) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(tok))
		vec[h.Sum32()%Dim]++
	}
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if norm == 0 {
		// Whitespace-only text: a stable unit vector so cosine stays
		// defined.
		vec[0] = 1
		return vec
	}
	n := float32(math.Sqrt(norm))
	for i := range vec {
		vec[i] /= n
	}
	return vec
}
