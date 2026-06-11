// Package memory is the public SDK facade over Harbor's
// internal/memory package — the session-scoped memory store, its
// strategies, and the LLM-context patch vocabulary (RFC §3.6, §6.6;
// D-204). Alias-based re-exports only: no behavior lives here. Driver
// factories, event emission helpers, and Protocol wire projections
// are deliberately private.
package memory

import (
	internal "github.com/hurtener/Harbor/internal/memory"
)

// Store vocabulary — aliases of the internal types.
type (
	// MemoryStore is the identity-mandatory memory store interface.
	MemoryStore = internal.MemoryStore
	// ConfigSnapshot is the resolved memory configuration.
	ConfigSnapshot = internal.ConfigSnapshot
	// Deps carries shared dependencies (bus, summarizer, logger).
	Deps = internal.Deps
	// Record is one stored memory record.
	Record = internal.Record
	// Snapshot is a strategy-shaped memory snapshot.
	Snapshot = internal.Snapshot
	// ConversationTurn is one conversational turn in a snapshot.
	ConversationTurn = internal.ConversationTurn
	// LLMContextPatch is the planner-visible memory projection.
	LLMContextPatch = internal.LLMContextPatch
	// TrajectoryDigest is a compacted prior-trajectory digest.
	TrajectoryDigest = internal.TrajectoryDigest
	// Summarizer produces rolling summaries for the strategy.
	Summarizer = internal.Summarizer
	// SummarizeRequest is the summarizer input.
	SummarizeRequest = internal.SummarizeRequest
	// SummarizeResponse is the summarizer output.
	SummarizeResponse = internal.SummarizeResponse
	// Health is the store's degradation state.
	Health = internal.Health
	// Strategy names the memory compaction strategy.
	Strategy = internal.Strategy
	// OverflowPolicy names the bounded-buffer overflow behavior.
	OverflowPolicy = internal.OverflowPolicy
	// RetrievalMode names the opt-in retrieval shape layered on the
	// strategy.
	RetrievalMode = internal.RetrievalMode
	// Embedder is the injectable text→vector callable the semantic
	// retrieval mode consumes (any embeddings.Embedder satisfies it).
	Embedder = internal.Embedder
	// ScoredTurn is one SearchTurns result with its similarity score.
	ScoredTurn = internal.ScoredTurn
)

// DefaultDriver is the driver name Open resolves when the config
// names none.
const DefaultDriver = internal.DefaultDriver

// Health values.
const (
	// HealthHealthy — the store is fully operational.
	HealthHealthy = internal.HealthHealthy
	// HealthRetry — transient failures; retrying.
	HealthRetry = internal.HealthRetry
	// HealthDegraded — the store is degraded.
	HealthDegraded = internal.HealthDegraded
	// HealthRecovering — the store is recovering.
	HealthRecovering = internal.HealthRecovering
)

// Strategy values.
const (
	// StrategyNone — no compaction.
	StrategyNone = internal.StrategyNone
	// StrategyTruncation — drop-oldest truncation.
	StrategyTruncation = internal.StrategyTruncation
	// StrategyRollingSummary — LLM-backed rolling summarisation.
	StrategyRollingSummary = internal.StrategyRollingSummary
)

// OverflowDropOldest is the default bounded-buffer overflow policy.
const OverflowDropOldest = internal.OverflowDropOldest

// RetrievalMode values.
const (
	// RetrievalDefault — strategy-shaped retrieval only.
	RetrievalDefault = internal.RetrievalDefault
	// RetrievalSemantic — embedding-similarity SearchTurns (requires
	// Deps.Embedder).
	RetrievalSemantic = internal.RetrievalSemantic
)

// DefaultSemanticTopK is the SearchTurns result cap when neither the
// caller nor the config supplies one.
const DefaultSemanticTopK = internal.DefaultSemanticTopK

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrNotFound — no record under that key.
	ErrNotFound = internal.ErrNotFound
	// ErrIdentityRequired — the identity triple is incomplete.
	ErrIdentityRequired = internal.ErrIdentityRequired
	// ErrUnknownDriver — the named memory driver is not registered.
	ErrUnknownDriver = internal.ErrUnknownDriver
	// ErrStoreClosed — the store has been closed.
	ErrStoreClosed = internal.ErrStoreClosed
	// ErrInvalidSnapshot — the snapshot does not fit the strategy.
	ErrInvalidSnapshot = internal.ErrInvalidSnapshot
	// ErrSemanticDisabled — SearchTurns on a store whose retrieval
	// mode is not semantic.
	ErrSemanticDisabled = internal.ErrSemanticDisabled
)

// Open resolves the configured memory driver and opens it.
var Open = internal.Open

// OpenDriver opens a memory driver by explicit name.
var OpenDriver = internal.OpenDriver

// SnapshotFromConfig projects the operator config block into the
// resolved ConfigSnapshot Open consumes.
var SnapshotFromConfig = internal.SnapshotFromConfig

// RegisteredDrivers lists the seated memory driver names
// (blank-import sdk/drivers/prod to seat the production set).
var RegisteredDrivers = internal.RegisteredDrivers

// WithStore returns a child context carrying the store.
var WithStore = internal.WithStore

// From extracts the store from ctx, reporting presence.
var From = internal.From

// MustFrom extracts the store from ctx, panicking when absent.
var MustFrom = internal.MustFrom
