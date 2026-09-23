// Package memory exposes the identity-scoped administrative surface of Harbor's
// cumulative session memory. Runtime execution and these operations share the
// same StateStore-backed owner in internal/memory/session. There is no pair-only
// history store or background summary engine. Identity is mandatory.
package memory

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
)

// Strategy declares the memory shape the store applies.
type Strategy string

// Strategy values.
const (
	// StrategyNone explicitly disables session memory.
	StrategyNone Strategy = "none"
	// StrategyRollingSummary retains cumulative context and recent execution detail.
	StrategyRollingSummary Strategy = "rolling_summary"
)

// Health enumerates the memory subsystem health states.
//
// only produces `HealthHealthy`. A later phase will drive the
// full FSM (`healthy → retry → degraded → recovering → healthy`)
// for `rolling_summary` failures.
type Health string

// Health values.
const (
	// HealthHealthy — operating normally.
	HealthHealthy Health = "healthy"
	// HealthRetry — last summarisation attempt failed; will retry
	// next opportunity. Reserved for a later phase.
	HealthRetry Health = "retry"
	// HealthDegraded — retry budget exhausted; falling back to
	// truncation semantics and queueing recovery. Reserved for
	HealthDegraded Health = "degraded"
	// HealthRecovering — recovery loop is running; will return to
	// healthy on success. Reserved for a later phase.
	HealthRecovering Health = "recovering"
)

// ConversationTurn is an administrator-supplied conversational note.
// Runtime turns are recorded through the cumulative execution owner, not Put.
type ConversationTurn struct {
	UserMessage       string
	AssistantResponse string
}

// MemoryStore is Harbor's mandatory memory interface. A single
// surface; every V1 driver (inmem here, sqlite + postgres at
// Postgres) implements every method. No `Supports*` ceremony per
// AGENTS.md §4.4.
//
// Identity-mandatory contract:
//
//   - Every method validates the identity `Quadruple` at the
//     boundary. Empty tenant / user / session returns wrapped
//     `ErrIdentityRequired` AND emits one
//     `memory.identity_rejected` event on the bus. Empty `RunID`
//     is accepted (memory is session-scoped).
//
// Concurrent-reuse contract:
//
//   - One instance is safe to share across N concurrent
//     goroutines. Mutable state is internally synchronised; per-
//     call state lives in `ctx` and the supplied `Quadruple`,
//     never on the driver.
type MemoryStore interface {
	// Inspect reads the authoritative committed memory projection. It exposes
	// neither active journals nor a second persisted transcript.
	Inspect(ctx context.Context, id identity.Quadruple) (Inspection, error)
	// Put appends an operator note and returns its actual committed key.
	Put(ctx context.Context, id identity.Quadruple, turn ConversationTurn) (string, error)
	// Delete atomically removes the named item and fences derived context.
	Delete(ctx context.Context, id identity.Quadruple, key string) (int, error)

	// Close releases driver resources. Idempotent. After Close,
	// every method returns `ErrStoreClosed`.
	Close(ctx context.Context) error
}

// Sentinel errors. Callers compare via `errors.Is`.
var (
	// ErrInvalidInspection rejects malformed items returned by a memory driver.
	ErrInvalidInspection = errors.New("memory: invalid inspection")
	// ErrNotFound means no current authorized item has the requested key.
	ErrNotFound = errors.New("memory: record not found")

	// ErrIdentityRequired — a method was called with a
	// `Quadruple` whose tenant, user, or session was empty.
	// The fail-closed gate
	ErrIdentityRequired = errors.New("memory: identity triple incomplete")

	// ErrUnknownDriver — `Open` was asked for a driver name no
	// registered factory handles. The wrapped message lists the
	// registered names.
	ErrUnknownDriver = errors.New("memory: unknown driver")

	// ErrStoreClosed — a method was called after `Close`.
	ErrStoreClosed = errors.New("memory: store is closed")

	// ErrStrategyNotImplemented rejects a removed or unknown strategy.
	ErrStrategyNotImplemented = errors.New("memory: unsupported strategy")

	// ErrInvalidHealthTransition — a strategy executor attempted a
	// `Health` transition outside the documented FSM
	// (`healthy ↔ retry ↔ degraded ↔ recovering`). Fail loudly: an
	// invalid transition is a programming error, not a recoverable
	// state.
	ErrInvalidHealthTransition = errors.New("memory: invalid health transition")
)

// healthTransitions enumerates the legal `Health` FSM edges.
//
//	healthy    → retry      (summariser failed; will retry)
//	retry      → healthy    (retry succeeded)
//	retry      → degraded   (retries exhausted; fall back to truncation)
//	degraded   → recovering (recovery loop draining backlog)
//	recovering → healthy    (backlog drained)
//	recovering → degraded   (recovery batch failed; back to drain)
//
// Self-loops are allowed (no-op transition); any other pair is
// rejected by `ValidateHealthTransition`.
var healthTransitions = map[Health]map[Health]struct{}{
	HealthHealthy: {
		HealthHealthy: {},
		HealthRetry:   {},
	},
	HealthRetry: {
		HealthRetry:    {},
		HealthHealthy:  {},
		HealthDegraded: {},
	},
	HealthDegraded: {
		HealthDegraded:   {},
		HealthRecovering: {},
	},
	HealthRecovering: {
		HealthRecovering: {},
		HealthHealthy:    {},
		HealthDegraded:   {},
	},
}

// ValidateHealthTransition returns nil when `(prior → next)` is a
// legal `Health` FSM edge. Invalid transitions return wrapped
// `ErrInvalidHealthTransition` — fail loudly per AGENTS.md §5; an
// invalid transition is a programming error in the calling
// executor, not a recoverable state.
//
// The empty `Health{}` (zero value) is treated as `HealthHealthy`
// for both sides — a freshly-constructed executor implicitly starts
// healthy.
func ValidateHealthTransition(prior, next Health) error {
	if prior == "" {
		prior = HealthHealthy
	}
	if next == "" {
		next = HealthHealthy
	}
	edges, ok := healthTransitions[prior]
	if !ok {
		return fmt.Errorf("%w: unknown prior health %q", ErrInvalidHealthTransition, prior)
	}
	if _, ok := edges[next]; !ok {
		return fmt.Errorf("%w: %q → %q not allowed",
			ErrInvalidHealthTransition, prior, next)
	}
	return nil
}

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
