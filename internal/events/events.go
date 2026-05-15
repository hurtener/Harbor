// Package events owns Harbor's typed event bus surface — the single
// pub/sub channel every subsystem (telemetry, audit, governance,
// runtime, planner, tools) Publishes to and Subscribes from. There
// is no parallel observability channel; the unification of telemetry
// + chunked output on one bus is a load-bearing decision that closes
// the predecessor's split-channel sharp edge (brief 06 §1).
//
// Phase 05 ships:
//
//   - The exhaustive EventType registry (V1 starter set + the IsValidEventType
//     / EventTypes API; future phases add types by declaring an exported
//     constant + an init() registration in this file).
//   - The sealed EventPayload interface; concrete payload types live in
//     their owning subsystems and embed events.Sealed to satisfy the seal.
//   - The Event record, Filter, Subscription and EventBus interfaces.
//   - Sentinel errors callers compare via errors.Is.
//   - The §4.4 driver-registry seam (registry.go) so future drivers
//     (replay-equipped Phase 06, durable-log Phase 57) plug in without
//     changing callers.
//   - Ctx helpers (WithBus / MustFrom / From) mirroring the audit / identity
//     ctx-helper pattern.
//
// What is OUT of scope for Phase 05:
//
//   - Replay-from-cursor / ring-buffered driver — Phase 06.
//   - Durable event-log driver against StateStore — Phase 57.
//   - Cryptographic Admin scope verification — Phase 61 (Protocol auth).
//   - Protocol wire encoding / remote consumers — Phase 60.
//   - Metric label derivation — Phase 56.
package events

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// wrap formats a sentinel error with %w plus contextual key=value
// pairs. Keeps the call sites compact.
func wrap(sentinel error, format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{sentinel}, args...)...)
}

// EventID is a per-event idempotency key. ULID-shaped at construction
// time; the convention is to generate via `state.NewEventID` (or any
// caller-side ULID source) at the publish site. Used by
// `internal/distributed.BusEnvelope.EventID` to dedupe at-least-once
// deliveries on `(TaskID, Edge, EventID)`.
//
// The type lives here (and not in `internal/state`) so distributed
// callers can reference it without importing state's persistence
// surface. The two namespaces are intentionally parallel: state's
// `EventID` is the persistence-layer idempotency key; events'
// `EventID` is the bus-layer correlation key. Both ULID-shaped, both
// caller-supplied; consumers MAY pass the same value across both
// layers when convenient.
type EventID string

// EventType is a string-typed exhaustive enum. Each canonical type
// is declared as an exported constant plus registered in init() so
// the registry stays the single source of truth.
//
// Adding a new type in a later phase: declare an exported constant,
// extend canonicalTypes, and update the master plan / glossary if the
// new type is reader-facing.
type EventType string

// V1 starter set. Phase 03 / 04 / 36b will populate the matching
// emit paths; Phase 05 emits the bus-internal types itself.
const (
	// EventTypeRuntimeError — emitted by the Phase 04 logger on Error.
	EventTypeRuntimeError EventType = "runtime.error"
	// EventTypeRuntimeWarning — reserved for future runtime-warn emits.
	EventTypeRuntimeWarning EventType = "runtime.warning"
	// EventTypeBusDropped — emitted by the bus into a subscriber's
	// stream when its buffer overflowed; carries the dropped sequence
	// range. At most one per DropWindow per subscriber.
	EventTypeBusDropped EventType = "bus.dropped"
	// EventTypeBusSubscriptionIdleClosed — emitted by the reaper when
	// a subscription is cancelled for not draining within IdleTimeout.
	EventTypeBusSubscriptionIdleClosed EventType = "bus.subscription_idle_closed"
	// EventTypeAuditRedactionFailed — emitted when audit.Redactor.Redact
	// returns an error during Publish. Carries the failing event's
	// type + identity but NO payload bytes.
	EventTypeAuditRedactionFailed EventType = "audit.redaction_failed"
	// EventTypeAdminScopeUsed — emitted when a Subscribe call passes
	// Admin: true (cross-session/cross-tenant) so admin-scope use is
	// retroactively detectable.
	EventTypeAdminScopeUsed EventType = "audit.admin_scope_used"
	// EventTypeGovernanceBudgetExceeded — reserved for Phase 36b emit.
	EventTypeGovernanceBudgetExceeded EventType = "governance.budget_exceeded"
	// EventTypeGovernanceRateLimited — reserved for Phase 36b emit.
	EventTypeGovernanceRateLimited EventType = "governance.rate_limited"
	// EventTypeRuntimeRunCancelled — emitted by Engine.Cancel(runID)
	// when the cancellation was observed for an active run. Payload is
	// RunCancelledPayload (SafePayload). Phase 13.
	EventTypeRuntimeRunCancelled EventType = "runtime.run_cancelled"
)

// canonicalTypes is the registered set. Build via init() so the file
// is the single source of truth — adding a new constant above without
// extending this list fails IsValidEventType, which the
// TestEventTypes_Exhaustiveness test pins as a phase contract.
var (
	canonicalMu    sync.RWMutex
	canonicalTypes = map[EventType]struct{}{}
)

func init() {
	for _, t := range []EventType{
		EventTypeRuntimeError,
		EventTypeRuntimeWarning,
		EventTypeBusDropped,
		EventTypeBusSubscriptionIdleClosed,
		EventTypeAuditRedactionFailed,
		EventTypeAdminScopeUsed,
		EventTypeGovernanceBudgetExceeded,
		EventTypeGovernanceRateLimited,
		EventTypeRuntimeRunCancelled,
	} {
		canonicalTypes[t] = struct{}{}
	}
}

// IsValidEventType reports whether t is in the canonical registry.
func IsValidEventType(t EventType) bool {
	canonicalMu.RLock()
	defer canonicalMu.RUnlock()
	_, ok := canonicalTypes[t]
	return ok
}

// EventTypes returns a deterministic snapshot of every registered
// type, lexicographically sorted.
func EventTypes() []EventType {
	canonicalMu.RLock()
	out := make([]EventType, 0, len(canonicalTypes))
	for t := range canonicalTypes {
		out = append(out, t)
	}
	canonicalMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// RegisterEventType installs a new canonical EventType into the
// registry. Call from a subsystem-side init() so the type is in the
// registry before any Publish runs (Publish rejects unregistered
// types with ErrUnknownEventType).
//
// Re-registering the same value is a no-op; registering an empty
// EventType panics — silent acceptance would defeat the exhaustive-enum
// invariant the registry exists to enforce.
func RegisterEventType(t EventType) {
	if t == "" {
		panic("events: RegisterEventType called with empty EventType")
	}
	canonicalMu.Lock()
	canonicalTypes[t] = struct{}{}
	canonicalMu.Unlock()
}

// EventPayload is the sealed payload interface. Concrete payload
// types live alongside their owning subsystems and embed Sealed to
// satisfy the seal. The unexported method on Sealed is the seal.
type EventPayload interface {
	isEventPayload()
}

// Sealed is embedded in concrete payload structs to satisfy
// EventPayload from any package. The interface stays sealed in
// spirit — to declare a payload, you have to import this package.
//
// Standard Go pattern (mirrors net/netip.Addr's seal, encoding/gob
// stdlib types, etc.). Compile-time enforcement.
type Sealed struct{}

func (Sealed) isEventPayload() {}

// SafePayload marks payload types whose contents are known not to
// carry secrets. Bus implementations skip the audit.Redactor for
// these — typed access is preserved on the subscriber side.
//
// Bus-internal payloads (BusDropped, IdleClosed, AuditRedactionFailed,
// AdminScopeUsed) are SafePayload by construction. External payloads
// MAY implement SafePayload when their declarer is confident the
// type carries no secret-shaped data; declarers in doubt should NOT
// implement SafePayload — the bus will run the value through the
// redactor and the subscriber-side payload becomes a RedactedMap.
type SafePayload interface {
	EventPayload
	isSafePayload()
}

// SafeSealed is embedded in payload structs that are both EventPayload
// AND SafePayload. Composes Sealed.
type SafeSealed struct{ Sealed }

func (SafeSealed) isSafePayload() {}

// RedactedMap is the post-redaction payload form for payloads that
// did NOT implement SafePayload and whose Redact() output became a
// generic map[string]any (the audit redactor's normalised shape for
// reflective struct walks). Subscribers can extract redacted fields
// via Data lookup.
type RedactedMap struct {
	Sealed
	Data map[string]any
}

// Event is the canonical bus record.
//
// Sequence is per-bus monotonic and gap-free; assigned by Publish.
// Callers MUST NOT pre-fill Sequence (Publish rejects with
// ErrSequenceProvided). OccurredAt defaults to time.Now() when zero.
//
// Extra is reserved for Phase 56's bounded low-cardinality metric
// labels. Phase 05 does not derive metrics; the slot exists so later
// phases can populate it without changing the Event shape.
type Event struct {
	Type       EventType
	Identity   identity.Quadruple
	OccurredAt time.Time
	Sequence   uint64
	Payload    EventPayload
	Extra      map[string]string
}

// Filter is the server-enforced subscription predicate. Subscribe
// rejects filters that elide the identity triple (Tenant + User +
// Session) unless Admin is set. When Admin is set with a partial
// triple, the bus emits an audit.admin_scope_used event before
// returning the subscription.
//
// The Admin claim is trust-based in Phase 05. Cryptographic
// verification arrives with Protocol auth in Phase 61; until then
// the audit emit on every Admin-true Subscribe makes any abuse
// retroactively detectable.
type Filter struct {
	Tenant  string
	User    string
	Session string
	// Run, when non-empty, narrows the subscription to a single run
	// inside the (tenant, user, session) scope. An empty Run means
	// "every run in the session" (session-scoped subscription) — the
	// Phase 60 default. The wire transport carries this via the
	// optional `X-Harbor-Run` (stream/HeaderRun) carrier header.
	// PR #91 / D-082 (Wave 10 audit WARN-5).
	Run   string
	Types []EventType
	Admin bool
}

// HasFullTriple reports whether the filter specifies all three
// identity components.
func (f Filter) HasFullTriple() bool {
	return f.Tenant != "" && f.User != "" && f.Session != ""
}

// Matches reports whether ev satisfies the filter's identity gates
// and event-type selector. Admin filters bypass the identity match
// (cross-tenant fan-in); Types empty matches every type. A non-empty
// Run additionally narrows to events whose RunID matches.
func (f Filter) Matches(ev Event) bool {
	if !f.Admin {
		if ev.Identity.TenantID != f.Tenant {
			return false
		}
		if ev.Identity.UserID != f.User {
			return false
		}
		if ev.Identity.SessionID != f.Session {
			return false
		}
		if f.Run != "" && ev.Identity.RunID != f.Run {
			return false
		}
	}
	if len(f.Types) == 0 {
		return true
	}
	for _, t := range f.Types {
		if ev.Type == t {
			return true
		}
	}
	return false
}

// Subscription delivers events to one consumer.
//
// Events() returns a receive-only channel. The channel is closed by
// the bus on Cancel or Close — consumers can use the close as the
// termination signal.
//
// Cancel is idempotent and safe to call from any goroutine.
type Subscription interface {
	Events() <-chan Event
	Cancel()
}

// EventBus is the canonical pub/sub surface. Implementations MUST be
// safe for concurrent use by N goroutines against a single shared
// instance (D-025).
type EventBus interface {
	Publish(ctx context.Context, ev Event) error
	Subscribe(ctx context.Context, f Filter) (Subscription, error)
	Close(ctx context.Context) error
}

// Cursor identifies the last event a subscriber has consumed for a
// session. Sequence is the per-bus monotonic value assigned by
// Publish; SessionID scopes the cursor so two subscribers on
// different sessions can use the same numeric Sequence without
// collision. The bus that issued the Sequence is the only bus the
// Cursor is meaningful against; cross-bus replay is not supported.
//
// Cursor{Sequence: 0} has the special meaning "from the beginning"
// for Replayer.Replay — equivalent to "the caller has seen nothing
// yet" — and bypasses the ErrCursorTooOld check so a fresh client
// can read whatever the ring still retains without coordinating on
// its tail.
type Cursor struct {
	SessionID string
	Sequence  uint64
}

// Replayer is the optional capability interface drivers may implement
// to support replay-from-cursor. EventBus.Subscribe + Replayer.Replay
// together give the caller a "resume cleanly after disconnect" pattern:
//
//  1. Open a fresh Subscribe with the desired filter — let the live
//     stream begin queuing into the subscriber's buffer.
//  2. Call Replay(lastSeenCursor, filter) — drain the historical
//     snapshot strictly newer than the cursor.
//  3. Live-tail the Subscribe channel; dedupe against the snapshot's
//     last sequence so a Publish landing between Subscribe and Replay
//     is not double-counted.
//
// A driver that does not implement Replayer (or whose
// EventsConfig.ReplayBufferSize is 0) returns ErrReplayUnavailable.
// The type assertion bus.(events.Replayer) still succeeds in the
// configured-off case — the assertion is a compile-shaped contract;
// the runtime decision lives in the call.
type Replayer interface {
	// Replay returns events whose Sequence > from.Sequence and that
	// match f, in Sequence order (strictly increasing). The returned
	// slice is owned by the caller. (nil, nil) is the "nothing newer
	// to replay" case (cursor at or past the bus head). See
	// ErrCursorTooOld and ErrReplayUnavailable for failure modes.
	//
	// The same filter rules as Subscribe apply: empty-triple
	// non-admin filters are rejected with ErrIdentityScopeRequired,
	// and Admin-scope replay emits an audit.admin_scope_used event
	// before returning the snapshot so admin-scope use is observable.
	Replay(ctx context.Context, from Cursor, f Filter) ([]Event, error)
}

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrUnknownEventType — Publish was called with an EventType not
	// in the canonical registry.
	ErrUnknownEventType = errors.New("events: unknown EventType")
	// ErrIdentityScopeRequired — Subscribe filter elides the identity
	// triple AND Admin is false.
	ErrIdentityScopeRequired = errors.New("events: filter must specify (tenant, user, session) unless Admin")
	// ErrAdminScopeRequired — reserved; Phase 61 wiring will return
	// this when a caller claims Admin without a verified scope claim.
	// Phase 05 trusts the caller; the sentinel is exposed now so the
	// API surface is stable across the auth wiring.
	ErrAdminScopeRequired = errors.New("events: admin scope required for cross-session/cross-tenant subscription")
	// ErrSubscriberLimitReached — per-session subscriber cap hit.
	ErrSubscriberLimitReached = errors.New("events: per-session subscriber limit reached")
	// ErrBusClosed — Publish or Subscribe called after Close.
	ErrBusClosed = errors.New("events: bus is closed")
	// ErrSequenceProvided — caller pre-filled Event.Sequence; Publish
	// owns sequence numbering.
	ErrSequenceProvided = errors.New("events: caller pre-filled Sequence; bus owns sequencing")
	// ErrInvalidEvent — Event failed structural validation (empty
	// identity triple, missing Payload, etc.).
	ErrInvalidEvent = errors.New("events: invalid event")
	// ErrIdentityRequired — Publish event identity is missing the
	// triple. Wraps identity.ErrIdentityIncomplete in spirit; bus-side
	// rejection happens before any redaction or queueing.
	ErrIdentityRequired = errors.New("events: event identity missing one or more components")
	// ErrCursorTooOld — Replay was called with a Cursor whose Sequence
	// is older than the ring's oldest retained entry. Wraps a
	// "(oldest, requested)" detail in the formatted message so callers
	// that fall through to a durable log (Phase 57) can interpret the
	// gap. errors.Is(err, ErrCursorTooOld) is the comparison.
	ErrCursorTooOld = errors.New("events: cursor older than ring tail")
	// ErrReplayUnavailable — replay is disabled on this driver
	// (EventsConfig.ReplayBufferSize=0) or the driver does not
	// implement Replayer at all. The type assertion
	// bus.(events.Replayer) succeeds even when the configured ring
	// size is zero — callers learn at call time, not at assertion
	// time, so the same call sites work whether replay is enabled or
	// not.
	ErrReplayUnavailable = errors.New("events: replay not available on this driver")
)

// ValidateEvent does structural validation: the EventType is in the
// registry; the identity quadruple has at least the triple; Sequence
// is zero (assigned by Publish); Payload is non-nil. Returns wrapped
// sentinels. Callers can call this directly to validate before
// Publish if they want compile-shaped check; Publish calls it
// internally.
func ValidateEvent(ev Event) error {
	if !IsValidEventType(ev.Type) {
		return wrap(ErrUnknownEventType, "type=%q", string(ev.Type))
	}
	if ev.Identity.TenantID == "" || ev.Identity.UserID == "" || ev.Identity.SessionID == "" {
		return wrap(ErrIdentityRequired, "type=%q", string(ev.Type))
	}
	if ev.Sequence != 0 {
		return wrap(ErrSequenceProvided, "type=%q sequence=%d", string(ev.Type), ev.Sequence)
	}
	if ev.Payload == nil {
		return wrap(ErrInvalidEvent, "type=%q: nil payload", string(ev.Type))
	}
	return nil
}

// ctxKey is the unexported key under which an EventBus is propagated
// on a context. Independent from identity / audit ctx keys.
type ctxKey int

const busCtxKey ctxKey = iota

// WithBus attaches bus to ctx so downstream handlers can recover it
// via MustFrom or From.
func WithBus(ctx context.Context, bus EventBus) context.Context {
	return context.WithValue(ctx, busCtxKey, bus)
}

// MustFrom returns the EventBus in ctx; panics with ErrBusClosed
// (used as the sentinel for "no bus configured") when none is
// present. Use in handler/runtime paths where a bus is mandatory.
func MustFrom(ctx context.Context) EventBus {
	b, ok := From(ctx)
	if !ok {
		panic(ErrBusClosed)
	}
	return b
}

// From returns the EventBus in ctx and a presence bool. Use when
// absence is recoverable.
func From(ctx context.Context) (EventBus, bool) {
	b, ok := ctx.Value(busCtxKey).(EventBus)
	return b, ok
}
