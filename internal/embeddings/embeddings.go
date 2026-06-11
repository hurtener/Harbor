// Package embeddings owns Harbor's embedding client — the seam that
// turns text into vectors (RFC §6.5).
//
// The `Embedder` is a standalone primitive, deliberately separate
// from the one-method chat `LLMClient`: an embeddings-only consumer
// must not inherit the chat client's dependency surface (artifact
// store + event bus). The package ships the §4.4 extensibility-seam
// plumbing — interface, sentinel errors, registry + factory
// (`Open`) — and the cosine-similarity helper its retrieval
// consumers share.
//
// Consumers:
//
//   - The memory subsystem's opt-in semantic-retrieval mode
//     (`internal/memory`, RFC §6.6) injects an `Embedder` via
//     `memory.Deps.Embedder`.
//   - The skills subsystem's opt-in semantic skill retrieval
//     (`internal/skills`, RFC §6.7) injects one via
//     `skills.Deps.Embedder`.
//   - À-la-carte consumers call `Open` + `Embed` directly and rank
//     with `Cosine` over their own corpus — no memory subsystem, no
//     config file, no Protocol (see
//     `docs/recipes/embed-and-retrieve.md`).
//
// Identity is mandatory at the `Embed` edge, mirroring the chat
// edge: embedding calls are billable provider traffic, so the
// runtime fails closed on a missing identity. The seam stays
// Protocol-free — `identity.With` / `identity.WithRun` is the
// library consumer's path.
//
// Concurrent reuse: one `Embedder` instance is safe to share across
// N concurrent goroutines. Drivers hold no per-call state; per-call
// data lives on `ctx` and the argument slice.
package embeddings

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/hurtener/Harbor/internal/identity"
)

// Embedder is Harbor's embedding-client interface. A single
// mandatory surface; every driver implements every method — no
// `Supports*` ceremony per AGENTS.md §4.4.
//
// `Embed` returns one vector per input text, in input order. The
// vectors of one call share a dimension (the configured model's);
// callers persist the model name alongside derived vectors when
// they need to detect model drift (vectors from different models
// are not comparable).
//
// Identity-mandatory contract: `Embed` fails closed with
// `ErrIdentityMissing` when `ctx` carries no Harbor identity. The
// registry path (`Open`) enforces this by construction; drivers
// re-check defensively at their own edge.
//
// `Close` releases driver resources (provider worker pools). After
// `Close`, `Embed` returns `ErrClientClosed`. Close is idempotent.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
	Close(ctx context.Context) error
}

// Sentinel errors. Callers compare via `errors.Is`.
var (
	// ErrInvalidConfig — the `ConfigSnapshot` (or `Deps`) failed
	// construction-time validation. The wrapped message names the
	// offending field.
	ErrInvalidConfig = errors.New("embeddings: invalid config")

	// ErrUnknownDriver — `Open` was asked for a driver name no
	// registered factory handles. The wrapped message lists the
	// registered names.
	ErrUnknownDriver = errors.New("embeddings: unknown driver")

	// ErrIdentityMissing — `Embed` was called with a `ctx` carrying
	// no Harbor identity. The embedding edge fails closed, mirroring
	// the chat edge (AGENTS.md §6 rule 9).
	ErrIdentityMissing = errors.New("embeddings: identity missing from context")

	// ErrEmptyInput — `Embed` was called with no texts, or with an
	// empty-string element. Providers reject empty inputs; the edge
	// fails loudly instead of forwarding a doomed request.
	ErrEmptyInput = errors.New("embeddings: empty input")

	// ErrClientClosed — a method was called after `Close`.
	ErrClientClosed = errors.New("embeddings: client is closed")

	// ErrDimensionMismatch — `Cosine` was handed vectors of
	// different lengths. Almost always model drift: vectors derived
	// under one embedding model are not comparable with another's.
	ErrDimensionMismatch = errors.New("embeddings: vector dimension mismatch")
)

// HasIdentity reports whether `ctx` carries a Harbor identity
// (either the triple or the run-scoped quadruple). The embedding
// edge MUST validate this before invoking any driver — the runtime
// fails closed on missing identity. Mirrors the chat edge's check;
// presence implies validity because `identity.With` / `WithRun`
// validate at write time.
func HasIdentity(ctx context.Context) bool {
	if _, ok := identity.From(ctx); ok {
		return true
	}
	_, ok := identity.QuadrupleFrom(ctx)
	return ok
}

// Cosine returns the cosine similarity of two vectors in [-1, 1].
// Vectors of different lengths return `ErrDimensionMismatch`;
// zero-length vectors return `ErrEmptyInput`. A zero-norm vector
// yields similarity 0 (no direction to compare).
//
// This is the one shared ranking primitive the semantic-retrieval
// consumers (memory turns, skill descriptions) and à-la-carte
// callers all use — a second cosine implementation elsewhere in the
// tree is a bug.
func Cosine(a, b []float32) (float64, error) {
	if len(a) == 0 || len(b) == 0 {
		return 0, fmt.Errorf("%w: vectors must be non-empty", ErrEmptyInput)
	}
	if len(a) != len(b) {
		return 0, fmt.Errorf("%w: len(a)=%d len(b)=%d", ErrDimensionMismatch, len(a), len(b))
	}
	var dot, normA, normB float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		normA += float64(a[i]) * float64(a[i])
		normB += float64(b[i]) * float64(b[i])
	}
	if normA == 0 || normB == 0 {
		return 0, nil
	}
	return dot / (math.Sqrt(normA) * math.Sqrt(normB)), nil
}
