// Package memory owns Harbor's declared-policy, identity-scoped,
// pluggable memory subsystem.
//
// Phase 23 lands the leaf surface:
//
//   - The single mandatory `MemoryStore` interface every backend
//     (inmem here, sqlite + postgres at Phase 25) implements.
//   - The shared types — `Strategy`, `Health`, `ConversationTurn`,
//     `TrajectoryDigest`, `LLMContextPatch`, `Snapshot`.
//   - Sentinel errors compared via `errors.Is`.
//   - The §4.4 extensibility-seam plumbing (registry + factory).
//   - Ctx helpers (`WithStore` / `MustFrom` / `From`).
//
// The interface owns the typed shape (D-027); drivers persist
// opaque bytes through `state.StateStore` via the typed wrapper
// pattern. Memory records key on `(identity.Quadruple, Kind=
// "memory.state")` — sessions own the wrapper layer of session
// records, memory owns its own.
//
// Identity is mandatory at every method (D-001). The triple
// `(tenant, user, session)` MUST be fully populated; empty `RunID`
// is accepted (memory is session-scoped, not run-scoped, mirroring
// Phase 07's `state.StateStore` rule). Missing-triple operations
// fail closed with `ErrIdentityRequired` AND emit a
// `memory.identity_rejected` event on the configured `events.EventBus`
// so the rejection is observable — never silent (brief 04 §4.2 +
// AGENTS.md §5 "Fail loudly").
//
// Phase 23 ships `Strategy = StrategyNone` only:
//
//   - `AddTurn` is a no-op.
//   - `GetLLMContext` returns an empty patch.
//   - `EstimateTokens` returns 0.
//   - `Flush` is a no-op.
//   - `Health` returns `HealthHealthy`.
//   - `Snapshot` returns an empty snapshot.
//   - `Restore` accepts only an empty snapshot; non-empty is
//     `ErrInvalidSnapshot`.
//
// Phase 24 will activate `StrategyTruncation` and
// `StrategyRollingSummary`; Phase 25 will add the SQLite + Postgres
// drivers under the same conformance suite.
package memory

import (
	"context"
	"errors"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// Strategy declares the memory shape the store applies.
//
// Phase 23 ships `StrategyNone` operational. `StrategyTruncation`
// and `StrategyRollingSummary` are declared so operators can stage
// their config today; the registry's `Open` rejects them with
// `ErrStrategyNotImplemented` until Phase 24 lands.
type Strategy string

// Strategy values.
const (
	// StrategyNone is the no-op memory shape. AddTurn is a no-op;
	// GetLLMContext returns an empty patch. Operationally the
	// "memory disabled" mode.
	StrategyNone Strategy = "none"
	// StrategyTruncation keeps a recent-turn window with budget
	// enforcement. Reserved for Phase 24.
	StrategyTruncation Strategy = "truncation"
	// StrategyRollingSummary keeps a recent-turn window plus a
	// background-summarised long-term context. Reserved for Phase 24.
	StrategyRollingSummary Strategy = "rolling_summary"
)

// Health enumerates the memory subsystem health states.
//
// Phase 23 only produces `HealthHealthy`. Phase 24 will drive the
// full FSM (`healthy → retry → degraded → recovering → healthy`)
// for `rolling_summary` failures.
type Health string

// Health values.
const (
	// HealthHealthy — operating normally.
	HealthHealthy Health = "healthy"
	// HealthRetry — last summarisation attempt failed; will retry
	// next opportunity. Reserved for Phase 24.
	HealthRetry Health = "retry"
	// HealthDegraded — retry budget exhausted; falling back to
	// truncation semantics and queueing recovery. Reserved for
	// Phase 24.
	HealthDegraded Health = "degraded"
	// HealthRecovering — recovery loop is running; will return to
	// healthy on success. Reserved for Phase 24.
	HealthRecovering Health = "recovering"
)

// ConversationTurn is one turn of a memory-tracked conversation.
// Producers (planner runtime, Phase 42+) hand turns to `AddTurn`.
//
// `ArtifactsShown` / `ArtifactsHiddenRefs` carry the model-visible /
// model-hidden artifact references for this turn so memory's
// downstream injection logic can prune or include them per the
// configured strategy (Phase 24+). Phase 23 round-trips both fields
// through the Snapshot bytes but applies no strategy logic.
type ConversationTurn struct {
	UserMessage         string
	AssistantResponse   string
	TrajectoryDigest    *TrajectoryDigest
	ArtifactsShown      map[string]any
	ArtifactsHiddenRefs []string
	Timestamp           time.Time
}

// TrajectoryDigest is the compact planner-side trace snapshot the
// memory subsystem MAY persist alongside the turn. Phase 23 does
// not ingest it (Strategy=none); the type ships now so Phase 24
// and downstream planner phases share one definition.
type TrajectoryDigest struct {
	ToolsInvoked        []string
	ObservationsSummary string
	ReasoningSummary    string
	ArtifactsRefs       []string
}

// LLMContextPatch is the output `GetLLMContext` returns: the patch
// a planner runtime applies to its LLM call.
//
// Strategy=none returns an empty patch; later strategies return a
// rolling summary text, ordered recent turns, and a token estimate
// the planner can compare against its context-window budget.
type LLMContextPatch struct {
	Strategy    Strategy
	Summary     string
	RecentTurns []ConversationTurn
	Tokens      int
}

// Snapshot is the export shape for `Snapshot` / `Restore`. The
// Strategy field round-trips so a `Restore` against a driver
// configured for a different Strategy fails loudly.
//
// `Bytes` is opaque to callers; only a driver of the same Strategy
// can `Restore` them. Crossing driver boundaries (e.g. inmem
// snapshot → sqlite restore) is safe because the bytes are
// JSON-serialised internal records, not driver-private structures.
type Snapshot struct {
	Strategy Strategy
	Bytes    []byte
}

// IsEmpty reports whether the snapshot is operationally empty (no
// strategy + no bytes). Used by `Restore` to accept the
// trivial-snapshot round-trip under Strategy=none.
func (s Snapshot) IsEmpty() bool {
	return s.Strategy == "" && len(s.Bytes) == 0
}

// MemoryStore is Harbor's mandatory memory interface. A single
// surface; every V1 driver (inmem here, sqlite + postgres at
// Phase 25) implements every method. No `Supports*` ceremony per
// AGENTS.md §4.4.
//
// Identity-mandatory contract (D-001):
//
//   - Every method validates the identity `Quadruple` at the
//     boundary. Empty tenant / user / session returns wrapped
//     `ErrIdentityRequired` AND emits one
//     `memory.identity_rejected` event on the bus. Empty `RunID`
//     is accepted (memory is session-scoped).
//
// Concurrent-reuse contract (D-025):
//
//   - One instance is safe to share across N concurrent
//     goroutines. Mutable state is internally synchronised; per-
//     call state lives in `ctx` and the supplied `Quadruple`,
//     never on the driver.
type MemoryStore interface {
	// AddTurn appends a conversation turn to the memory tracked
	// for `id`. Strategy=none is a no-op (returns nil); other
	// strategies will apply their shape logic (Phase 24+).
	AddTurn(ctx context.Context, id identity.Quadruple, turn ConversationTurn) error

	// GetLLMContext returns the patch a planner runtime applies to
	// its LLM call. Strategy=none returns the zero value of
	// `LLMContextPatch`.
	GetLLMContext(ctx context.Context, id identity.Quadruple) (LLMContextPatch, error)

	// EstimateTokens returns the token-estimate for the memory
	// payload `GetLLMContext` would inject right now. Strategy=
	// none returns 0.
	EstimateTokens(ctx context.Context, id identity.Quadruple) (int, error)

	// Flush drops every in-flight turn and resets the memory for
	// `id` to a clean state. Strategy=none is a no-op.
	Flush(ctx context.Context, id identity.Quadruple) error

	// Health reports the current health state for `id`'s memory.
	// Strategy=none always reports `HealthHealthy`.
	Health(ctx context.Context, id identity.Quadruple) (Health, error)

	// Snapshot exports a portable snapshot of `id`'s memory state.
	// Strategy=none returns an empty `Snapshot{Strategy: StrategyNone}`.
	Snapshot(ctx context.Context, id identity.Quadruple) (Snapshot, error)

	// Restore imports a previously-captured `Snapshot`. The
	// Snapshot's Strategy MUST match the driver's configured
	// Strategy; mismatched strategies (e.g. restoring a
	// `truncation` snapshot into a `none` store) returns
	// `ErrInvalidSnapshot`. Strategy=none accepts only empty
	// snapshots; non-empty Bytes returns `ErrInvalidSnapshot`.
	Restore(ctx context.Context, id identity.Quadruple, snap Snapshot) error

	// Close releases driver resources. Idempotent. After Close,
	// every method returns `ErrStoreClosed`.
	Close(ctx context.Context) error
}

// Sentinel errors. Callers compare via `errors.Is`.
var (
	// ErrNotFound — a load-by-key style lookup found nothing.
	// Phase 23 returns this when `Snapshot` is asked for a slot
	// the StateStore wrapper layer has never written.
	ErrNotFound = errors.New("memory: record not found")

	// ErrIdentityRequired — a method was called with a
	// `Quadruple` whose tenant, user, or session was empty.
	// The fail-closed gate per D-001 + brief 04 §4.2.
	ErrIdentityRequired = errors.New("memory: identity triple incomplete")

	// ErrUnknownDriver — `Open` was asked for a driver name no
	// registered factory handles. The wrapped message lists the
	// registered names.
	ErrUnknownDriver = errors.New("memory: unknown driver")

	// ErrStoreClosed — a method was called after `Close`.
	ErrStoreClosed = errors.New("memory: store is closed")

	// ErrStrategyNotImplemented — `Open` was asked for a strategy
	// not yet implemented at this phase (Phase 23 ships
	// `StrategyNone` only). Phase 24 will widen the supported
	// set.
	ErrStrategyNotImplemented = errors.New("memory: strategy not implemented at this phase")

	// ErrInvalidSnapshot — `Restore` was called with a snapshot
	// whose Strategy mismatches the driver's, or with non-empty
	// bytes against a `StrategyNone` driver. Fail loudly; never
	// silently coerce.
	ErrInvalidSnapshot = errors.New("memory: invalid snapshot for this strategy")
)

// ValidateIdentity returns wrapped `ErrIdentityRequired` when any
// of (tenant, user, session) is empty. Empty `RunID` is acceptable.
// Drivers call this at the boundary before any I/O.
func ValidateIdentity(q identity.Quadruple) error {
	if q.TenantID == "" || q.UserID == "" || q.SessionID == "" {
		return ErrIdentityRequired
	}
	return nil
}

// ctxKey is the unexported key under which a `MemoryStore` is
// propagated on a context. Independent from identity / audit /
// events / state ctx keys.
type ctxKey int

const storeCtxKey ctxKey = iota

// WithStore attaches the store to ctx for downstream handlers.
func WithStore(ctx context.Context, store MemoryStore) context.Context {
	return context.WithValue(ctx, storeCtxKey, store)
}

// MustFrom returns the `MemoryStore` in ctx; panics with
// `ErrStoreClosed` (used as the sentinel for "no store
// configured") when none is present. Use in handler/runtime paths
// where a store is mandatory.
func MustFrom(ctx context.Context) MemoryStore {
	s, ok := From(ctx)
	if !ok {
		panic(ErrStoreClosed)
	}
	return s
}

// From returns the `MemoryStore` in ctx and a presence bool. Use
// when absence is recoverable.
func From(ctx context.Context) (MemoryStore, bool) {
	s, ok := ctx.Value(storeCtxKey).(MemoryStore)
	return s, ok
}
