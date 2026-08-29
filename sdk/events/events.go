// Package events is the public SDK facade over Harbor's
// internal/events package — the typed event bus every subsystem
// publishes to and subscribes from (RFC §3.6, §6.13).
// Alias-based re-exports only: no behavior lives here. Driver
// registration seams, Protocol wire conversion, the aggregator, and
// per-subsystem payload structs are deliberately private.
package events

import (
	internal "github.com/hurtener/Harbor/internal/events"
)

// Core bus vocabulary — aliases of the internal types.
type (
	// EventBus is the typed pub/sub bus interface.
	EventBus = internal.EventBus
	// LivePublisher is the additive present-tense animation capability.
	// Legacy EventBus implementations may omit it; chunk publishers disable
	// animation (with one per-run warning) when it is absent rather than
	// turning chunks into durable writes.
	LivePublisher = internal.LivePublisher
	// Event is the canonical event record.
	Event = internal.Event
	// EventID uniquely identifies one published Event.
	EventID = internal.EventID
	// EventType names a registered event type ("task.spawned", ...).
	EventType = internal.EventType
	// EventPayload is the sealed payload interface; concrete payloads
	// embed Sealed (or SafeSealed) to satisfy it.
	EventPayload = internal.EventPayload
	// Sealed is embedded by redactor-visible payload structs.
	Sealed = internal.Sealed
	// SafePayload marks a payload as redaction-exempt by construction.
	SafePayload = internal.SafePayload
	// SafeSealed is embedded by redaction-exempt payload structs.
	SafeSealed = internal.SafeSealed
	// Filter scopes a subscription by (tenant, user, session) — the
	// identity-mandatory subscription boundary (CLAUDE.md §6).
	Filter = internal.Filter
	// Subscription is one live, cancellable event subscription.
	Subscription = internal.Subscription
	// Cursor addresses a replay position on replay-capable drivers.
	Cursor = internal.Cursor
	// Replayer is the optional replay-from-cursor driver surface.
	Replayer = internal.Replayer
	// Deps carries shared dependencies for deps-aware drivers
	// (e.g. the durable driver sharing the runtime's StateStore).
	Deps = internal.Deps
)

// DefaultDriver is the driver name Open resolves when the config
// names none.
const DefaultDriver = internal.DefaultDriver

// Canonical cross-subsystem event-type constants.
const (
	// EventTypeRuntimeError — a runtime operation failed.
	EventTypeRuntimeError = internal.EventTypeRuntimeError
	// EventTypeRuntimeWarning — unexpected but recovered.
	EventTypeRuntimeWarning = internal.EventTypeRuntimeWarning
	// EventTypeBusDropped — backpressure dropped events.
	EventTypeBusDropped = internal.EventTypeBusDropped
	// EventTypeBusSubscriptionIdleClosed — an idle subscription was closed.
	EventTypeBusSubscriptionIdleClosed = internal.EventTypeBusSubscriptionIdleClosed
	// EventTypeAuditRedactionFailed — the audit redactor failed loudly.
	EventTypeAuditRedactionFailed = internal.EventTypeAuditRedactionFailed
	// EventTypeAdminScopeUsed — an elevated-scope subscription was used.
	EventTypeAdminScopeUsed = internal.EventTypeAdminScopeUsed
	// EventTypeGovernanceBudgetExceeded — a cost ceiling rejected a call.
	EventTypeGovernanceBudgetExceeded = internal.EventTypeGovernanceBudgetExceeded
	// EventTypeGovernanceRateLimited — a rate limit rejected a call.
	EventTypeGovernanceRateLimited = internal.EventTypeGovernanceRateLimited
	// EventTypeRuntimeRunCancelled — a run was cancelled.
	EventTypeRuntimeRunCancelled = internal.EventTypeRuntimeRunCancelled
	// EventTypeTopologyChanged — the live topology snapshot changed.
	EventTypeTopologyChanged = internal.EventTypeTopologyChanged
)

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrUnknownEventType — Publish with an unregistered EventType.
	ErrUnknownEventType = internal.ErrUnknownEventType
	// ErrIdentityScopeRequired — a non-admin filter missing the triple.
	ErrIdentityScopeRequired = internal.ErrIdentityScopeRequired
	// ErrAdminScopeRequired — cross-session subscription without admin scope.
	ErrAdminScopeRequired = internal.ErrAdminScopeRequired
	// ErrSubscriberLimitReached — the per-session subscriber cap hit.
	ErrSubscriberLimitReached = internal.ErrSubscriberLimitReached
	// ErrBusClosed — the bus has been closed.
	ErrBusClosed = internal.ErrBusClosed
	// ErrInvalidEvent — the event failed validation.
	ErrInvalidEvent = internal.ErrInvalidEvent
	// ErrIdentityRequired — the event's identity misses a component.
	ErrIdentityRequired = internal.ErrIdentityRequired
	// ErrCursorTooOld — the replay cursor predates the ring tail.
	ErrCursorTooOld = internal.ErrCursorTooOld
	// ErrReplayUnavailable — the driver does not support replay.
	ErrReplayUnavailable = internal.ErrReplayUnavailable
	// ErrUnknownDriver — the named bus driver is not registered.
	ErrUnknownDriver = internal.ErrUnknownDriver
)

// Open resolves the configured bus driver and opens it.
var Open = internal.Open

// OpenWith opens the configured bus driver with shared dependencies
// (the deps-aware factory path; e.g. StateStore sharing for the
// durable driver).
var OpenWith = internal.OpenWith

// OpenDriver opens a bus driver by explicit name.
var OpenDriver = internal.OpenDriver

// RegisteredDrivers lists the seated bus driver names (blank-import
// sdk/drivers/prod to seat the production set).
var RegisteredDrivers = internal.RegisteredDrivers

// RegisterEventType registers a custom EventType so ValidateEvent
// (and Publish) accept it. Embedders publishing their own event
// vocabulary call this from an init().
var RegisterEventType = internal.RegisterEventType

// IsValidEventType reports whether t is a registered EventType.
var IsValidEventType = internal.IsValidEventType

// EventTypes lists every registered EventType.
var EventTypes = internal.EventTypes

// ValidateEvent validates an Event record (type registered, identity
// complete, sequencing untouched).
var ValidateEvent = internal.ValidateEvent

// WithBus returns a child context carrying the bus.
var WithBus = internal.WithBus

// From extracts the bus from ctx, reporting presence.
var From = internal.From

// MustFrom extracts the bus from ctx, panicking when absent.
var MustFrom = internal.MustFrom
