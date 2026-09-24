// Package memory is the public SDK facade over Harbor's
// internal/memory package — the cumulative session-memory administrative
// surface and its committed source projections (RFC §3.6,
// §6.6). Alias-based re-exports only: no behavior lives here. Driver
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
	// Inspection is the bounded administrative view of execution memory.
	Inspection = internal.Inspection
	// Item is one source-keyed committed memory projection.
	Item = internal.Item
	// ConfigSnapshot is the resolved memory configuration.
	ConfigSnapshot = internal.ConfigSnapshot
	// Deps carries the shared StateStore, event bus and redactor.
	Deps = internal.Deps
	// ConversationTurn is an administrator-supplied conversational note.
	ConversationTurn = internal.ConversationTurn
	// Health is the store's degradation state.
	Health = internal.Health
	// Strategy names the memory compaction strategy.
	Strategy = internal.Strategy
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
	// StrategyNone explicitly disables session memory.
	StrategyNone = internal.StrategyNone
	// StrategyRollingSummary — LLM-backed rolling summarisation.
	StrategyRollingSummary = internal.StrategyRollingSummary
)

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrInvalidInspection rejects malformed committed memory projections.
	ErrInvalidInspection = internal.ErrInvalidInspection
	// ErrNotFound — no record under that key.
	ErrNotFound = internal.ErrNotFound
	// ErrIdentityRequired — the identity triple is incomplete.
	ErrIdentityRequired = internal.ErrIdentityRequired
	// ErrUnknownDriver — the named memory driver is not registered.
	ErrUnknownDriver = internal.ErrUnknownDriver
	// ErrStoreClosed — the store has been closed.
	ErrStoreClosed = internal.ErrStoreClosed
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
