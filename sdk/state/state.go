// Package state is the public SDK facade over Harbor's
// internal/state package — the identity-scoped StateStore (RFC §3.6,
// §9). Alias-based re-exports only: no behavior lives here.
// Driver factories and validation internals are deliberately private.
package state

import (
	internal "github.com/hurtener/Harbor/internal/state"
)

// Store vocabulary — aliases of the internal types.
type (
	// StateStore is the identity-mandatory durable state interface.
	StateStore = internal.StateStore
	// StateRecord is one stored state record.
	StateRecord = internal.StateRecord
	// EventID identifies one appended state event.
	EventID = internal.EventID
	// ListScope is the explicit scope claim StateStore.ListKind
	// requires (the cross-identity maintenance scan).
	ListScope = internal.ListScope
)

// DefaultDriver is the driver name Open resolves when the config
// names none.
const DefaultDriver = internal.DefaultDriver

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrNotFound — no record under that key.
	ErrNotFound = internal.ErrNotFound
	// ErrIdempotencyConflict — an idempotency key was reused divergently.
	ErrIdempotencyConflict = internal.ErrIdempotencyConflict
	// ErrIdentityRequired — the identity triple is incomplete.
	ErrIdentityRequired = internal.ErrIdentityRequired
	// ErrStoreClosed — the store has been closed.
	ErrStoreClosed = internal.ErrStoreClosed
	// ErrInvalidRecord — the record failed validation.
	ErrInvalidRecord = internal.ErrInvalidRecord
	// ErrReservedKind — an ordinary caller targeted Harbor coordination state.
	ErrReservedKind = internal.ErrReservedKind
	// ErrReservedIdentity — an ordinary caller targeted Harbor's coordination principal.
	ErrReservedIdentity = internal.ErrReservedIdentity
	// ErrCommitOutcomeUnknown — a transaction commit acknowledgement was ambiguous.
	ErrCommitOutcomeUnknown = internal.ErrCommitOutcomeUnknown
	// ErrUnknownDriver — the named state driver is not registered.
	ErrUnknownDriver = internal.ErrUnknownDriver
	// ErrMaintenanceScopeRequired — ListKind called without the
	// explicit maintenance scope claim.
	ErrMaintenanceScopeRequired = internal.ErrMaintenanceScopeRequired
)

// Open resolves the configured state driver and opens it.
var Open = internal.Open

// OpenDriver opens a state driver by explicit name.
var OpenDriver = internal.OpenDriver

// NewEventID mints a fresh state EventID.
var NewEventID = internal.NewEventID

// RegisteredDrivers lists the seated state driver names (blank-import
// sdk/drivers/prod to seat the production set).
var RegisteredDrivers = internal.RegisteredDrivers

// WithStore returns a child context carrying the store.
var WithStore = internal.WithStore

// From extracts the store from ctx, reporting presence.
var From = internal.From

// MustFrom extracts the store from ctx, panicking when absent.
var MustFrom = internal.MustFrom
