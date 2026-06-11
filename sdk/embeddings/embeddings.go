// Package embeddings is the public SDK facade over Harbor's
// internal/embeddings package — the embedding client that turns text
// into vectors (RFC §3.6, §6.5; D-204). Alias-based re-exports only:
// no behavior lives here.
//
// The Embedder is a standalone, à-la-carte-usable primitive: open it
// with a ConfigSnapshot + Deps (blank-import sdk/drivers/prod to seat
// the production driver), stamp an identity on the context, call
// Embed, and rank with Cosine — no memory subsystem, no config file,
// no Protocol. See docs/recipes/embed-and-retrieve.md.
package embeddings

import (
	internal "github.com/hurtener/Harbor/internal/embeddings"
)

// Embedding vocabulary — aliases of the internal types.
type (
	// Embedder is the identity-mandatory embedding client interface.
	Embedder = internal.Embedder
	// ConfigSnapshot is the resolved embeddings configuration.
	ConfigSnapshot = internal.ConfigSnapshot
	// Deps carries the (currently empty) runtime dependencies.
	Deps = internal.Deps
)

// DefaultDriver is the driver name Open resolves when the config
// names none.
const DefaultDriver = internal.DefaultDriver

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrInvalidConfig — the snapshot failed validation.
	ErrInvalidConfig = internal.ErrInvalidConfig
	// ErrUnknownDriver — the named embeddings driver is not registered.
	ErrUnknownDriver = internal.ErrUnknownDriver
	// ErrIdentityMissing — Embed was called without an identity in ctx.
	ErrIdentityMissing = internal.ErrIdentityMissing
	// ErrEmptyInput — Embed was called with no (or empty) texts.
	ErrEmptyInput = internal.ErrEmptyInput
	// ErrClientClosed — the client has been closed.
	ErrClientClosed = internal.ErrClientClosed
	// ErrDimensionMismatch — Cosine was handed vectors of different
	// lengths (usually embedding-model drift).
	ErrDimensionMismatch = internal.ErrDimensionMismatch
)

// Open resolves the configured embeddings driver and opens it.
var Open = internal.Open

// SnapshotFromConfig projects the operator config block into the
// resolved ConfigSnapshot Open consumes.
var SnapshotFromConfig = internal.SnapshotFromConfig

// RegisteredDrivers lists the seated embeddings driver names
// (blank-import sdk/drivers/prod to seat the production set).
var RegisteredDrivers = internal.RegisteredDrivers

// Cosine returns the cosine similarity of two vectors — the shared
// ranking primitive for retrieval over embedded corpora.
var Cosine = internal.Cosine

// HasIdentity reports whether ctx carries a Harbor identity (the
// embedding edge fails closed without one).
var HasIdentity = internal.HasIdentity
