// Package events owns Harbor's typed event bus surface — the single
// pub/sub channel every subsystem (telemetry, audit, governance,
// runtime, planner, tools) Publishes to and Subscribes from. There
// is no parallel observability channel; the unification of telemetry
// + chunked output on one bus is a load-bearing decision that closes
// the predecessor's split-channel sharp edge.
//
// Harbor ships:
//
//   - The exhaustive EventType registry (V1 starter set + the IsValidEventType
//     / EventTypes API; future phases add types by declaring an exported
//     constant + an init() registration in this file).
//   - The sealed EventPayload interface; concrete payload types live in
//     their owning subsystems and embed events.Sealed to satisfy the seal.
//   - The Event record, Filter, Subscription and EventBus interfaces.
//   - Sentinel errors callers compare via errors.Is.
//   - The §4.4 driver-registry seam (registry.go) so future drivers
//     (replay-equipped, durable-log) plug in without
//     changing callers.
//   - Ctx helpers (WithBus / MustFrom / From) mirroring the audit / identity
//     ctx-helper pattern.
//
// What is OUT of scope for this package:
//
//   - Replay-from-cursor / ring-buffered driver.
//   - Durable event-log driver against StateStore.
//   - Cryptographic Admin scope verification — (Protocol auth).
//   - Protocol wire encoding / remote consumers.
//   - Metric label derivation.
package events

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
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

// V1 starter set. A later phase will populate the matching
// emit paths; Harbor emits the bus-internal types itself.
const (
	// EventTypeRuntimeError — emitted by the logger on Error.
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
	// EventTypeGovernanceBudgetExceeded — reserved for emit.
	EventTypeGovernanceBudgetExceeded EventType = "governance.budget_exceeded"
	// EventTypeGovernanceRateLimited — reserved for emit.
	EventTypeGovernanceRateLimited EventType = "governance.rate_limited"
	// EventTypeRuntimeRunCancelled — emitted by Engine.Cancel(runID)
	// when the cancellation was observed for an active run. Payload is
	// RunCancelledPayload (SafePayload).
	EventTypeRuntimeRunCancelled EventType = "runtime.run_cancelled"
	// EventTypeTopologyChanged — emitted by the engine on construction
	// and on every adjacency-set change, carrying the canonical
	// TopologyProjection. Payload is
	// TopologyChangedPayload (SafePayload — the projection has no
	// secret-shaped fields, so the audit redactor bypasses it and
	// subscribers keep typed access). The paired request-side surface
	// is the `topology.snapshot` Protocol method.
	EventTypeTopologyChanged EventType = "topology.changed"
	// EventTypeMCPRawHTMLTrustToggled — emitted by the runtime (the
	// MCPSurface) when a Console admin flips the per-server
	// raw-HTML opt-in flag via `mcp.servers.set_raw_html_trust`. Payload
	// is MCPRawHTMLTrustToggledPayload (SafePayload by construction —
	// server name + boolean + actor identity quadruple; no upstream MCP
	// content). The default-deny posture for raw-HTML / SVG rendering
	// makes this audit event the load-bearing record of
	// the operator's explicit opt-in.
	EventTypeMCPRawHTMLTrustToggled EventType = "mcp.raw_html_trust_toggled"
	// EventTypeRunOverridesSet — emitted by the runtime (the
	// Runs surface) when the Console Playground page records a
	// next-message override via `runs.set_overrides`. Payload is
	// RunOverridesSetPayload (SafePayload by construction — the actor's
	// identity quadruple, the session id, and a bounded set of boolean
	// "which field was set" flags; the override VALUES themselves are
	// NOT in the payload, since a system-prompt override is
	// caller-supplied text that must not reach an audit subscriber
	// unredacted — CLAUDE.md §7).
	EventTypeRunOverridesSet EventType = "runs.overrides_set"
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
		EventTypeTopologyChanged,
		EventTypeMCPRawHTMLTrustToggled,
		EventTypeRunOverridesSet,
	} {
		canonicalTypes[t] = struct{}{}
	}
}

// TopologyChangedPayload is the SafePayload carried by an
// EventTypeTopologyChanged event. The engine
// publishes one on construction and on every adjacency-set change.
//
// It is SafePayload by construction: a TopologyProjection
// carries only node names, edge endpoints, kind tags, and bounded
// integer queue depths — no secret-shaped fields. The bus therefore
// skips the audit.Redactor for this payload and a subscriber keeps
// typed access to Projection (no RedactedMap downgrade).
type TopologyChangedPayload struct {
	SafeSealed
	// Projection is the canonical engine topology at the moment the
	// event was emitted.
	Projection types.TopologyProjection
}

// MCPRawHTMLTrustToggledPayload is the SafePayload carried by an
// EventTypeMCPRawHTMLTrustToggled event. The
// runtime publishes one on every successful
// `mcp.servers.set_raw_html_trust` Protocol call.
//
// It is SafePayload by construction: the payload carries only
// the MCP server name, the new boolean trust value, the actor's
// identity quadruple, and the toggle instant — no upstream MCP content,
// no secret-shaped fields. The bus therefore skips the audit.Redactor
// for this payload and a subscriber keeps typed access.
type MCPRawHTMLTrustToggledPayload struct {
	SafeSealed
	// Actor is the identity quadruple of the admin who toggled the flag.
	Actor identity.Quadruple
	// ServerName is the MCP server the flag was toggled on.
	ServerName string
	// Trusted is the new raw-HTML trust value.
	Trusted bool
	// OccurredAt is the wall-clock instant the toggle was applied.
	OccurredAt time.Time
}

// RunOverridesSetPayload is the SafePayload carried by an
// EventTypeRunOverridesSet event. The runtime
// publishes one on every successful `runs.set_overrides` Protocol call.
//
// It is SafePayload by construction: the payload carries only
// the actor's identity quadruple, the target session id, the recording
// instant, and a bounded set of boolean "which field was set" flags.
// The override VALUES are deliberately NOT in the payload — a
// system-prompt override is caller-supplied free text, and a
// reasoning-effort / temperature / max-tokens value could leak intent;
// the bus must never carry caller-supplied bytes unredacted (CLAUDE.md
// §7). A subscriber that needs the values reads them from the runtime's
// pending-override slot under the same identity scope. The bus
// therefore skips the audit.Redactor for this payload and a subscriber
// keeps typed access.
type RunOverridesSetPayload struct {
	SafeSealed
	// Actor is the identity quadruple of the operator who recorded the
	// override.
	Actor identity.Quadruple
	// SessionID is the session the override applies to.
	SessionID string
	// SetReasoningEffort reports whether the reasoning-effort field was
	// set in this override.
	SetReasoningEffort bool
	// SetTemperature reports whether the temperature field was set.
	SetTemperature bool
	// SetMaxTokens reports whether the max-tokens field was set.
	SetMaxTokens bool
	// SetSystemPrompt reports whether the system-prompt-override field
	// was set.
	SetSystemPrompt bool
	// SetModel reports whether the model-override field was set (the
	// session-level model swap).
	SetModel bool
	// OccurredAt is the wall-clock instant the override was recorded.
	OccurredAt time.Time
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
// Extra is reserved for the bounded low-cardinality metric
// labels. Harbor does not derive metrics; the slot exists so later
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
// The Admin claim is trust-based in a later phase. Cryptographic
// verification arrives with Protocol auth in a later phase; until then
// the audit emit on every Admin-true Subscribe makes any abuse
// retroactively detectable.
type Filter struct {
	Tenant  string
	User    string
	Session string
	// Run, when non-empty, narrows the subscription to a single run
	// inside the (tenant, user, session) scope. An empty Run means
	// "every run in the session" (session-scoped subscription) — the
	// default. The wire transport carries this via the
	// optional `X-Harbor-Run` (stream/HeaderRun) carrier header.
	// PR #91.
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

// MatchesScoped reports whether ev belongs to the identity the filter
// NAMES, scoping by every non-empty triple field (tenant, user, session,
// run) and the Types selector — ALWAYS, even when Admin is set.
//
// It is the BY-ID counterpart to Matches. Matches' Admin mode fans IN
// across identities (the fleet-view Subscribe/Replay tail wants every
// session's events); MatchesScoped never does — a by-id read
// (HistoryReplayer.Bounds/Window backing state.history) names exactly one
// session, and an admin reading session A must never receive session B's
// events from a shared best-effort ring. Admin authority is decided at the
// call edge (it authorises crossing the verified-tenant boundary to read
// the named session); it is not a licence to widen the scan. This closes a
// cross-session/cross-tenant disclosure on the ring-backed drivers, where
// Matches(ev) under Admin returned the whole ring.
//
// The ONE sanctioned exception to "a by-id read never fans in" is
// HistoryReplayer.ListWindow (the `events.list` substrate): it is an
// authority-gated, EXPLICITLY-REQUESTED fleet read on a DISTINCT method,
// carrying a verified `admin` OR `console:fleet` claim plus a per-request
// `audit.admin_scope_used` emit. That is categorically different from the
// silent Admin-widening-of-a-by-id-read this rule closed: there, an admin
// asking for ONE session got the whole ring back with no explicit request
// and no audit. ListWindow honours the wire filter's multi-valued identity
// SETS (MatchWire) rather than one named triple, so its widening is the
// operator's stated intent, gated and observed — not a leak. The by-id
// reads (Bounds/Window) keep the never-fan-in rule verbatim.
func (f Filter) MatchesScoped(ev Event) bool {
	if f.Tenant != "" && ev.Identity.TenantID != f.Tenant {
		return false
	}
	if f.User != "" && ev.Identity.UserID != f.User {
		return false
	}
	if f.Session != "" && ev.Identity.SessionID != f.Session {
		return false
	}
	if f.Run != "" && ev.Identity.RunID != f.Run {
		return false
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

// IsBusInternalNotice reports whether t is a transient, bus-internal
// observability notice — `bus.dropped`, `bus.subscription_idle_closed`,
// `audit.redaction_failed`, `audit.admin_scope_used`. These are emitted BY
// the bus (not by a session's producers), are not part of a session's
// conversation history, and the durable log never persists them. A
// HistoryReplayer windowed read (the `state.history` substrate) excludes
// them so a ring-backed driver matches the durable driver's history shape —
// and, critically, so the `audit.admin_scope_used` notice an admin read
// emits (scoped to the REQUESTED identity) cannot self-match the by-id scan
// and turn a not-found into a page containing only that notice.
func IsBusInternalNotice(t EventType) bool {
	switch t {
	case EventTypeBusDropped,
		EventTypeBusSubscriptionIdleClosed,
		EventTypeAuditRedactionFailed,
		EventTypeAdminScopeUsed:
		return true
	default:
		return false
	}
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
// instance.
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

// HistoryReplayer is the optional capability interface a replay-capable
// driver implements to support a BOUNDED, TAIL-FIRST windowed read of a
// session's retained event history — the substrate the `state.history`
// Protocol method projects. It is a sibling to Replayer
// (forward-from-cursor): where Replayer answers "everything strictly
// newer than this cursor, oldest-first to the tail" (the SSE reconnect
// path), HistoryReplayer answers two reopen-a-long-conversation reads:
//
//  1. Bounds — discover the session's retained head/tail sequence, so a
//     client knows where the window range sits without streaming the log.
//  2. Window — a bounded backward page: the most-recent K events with
//     Sequence < before (before==0 ⇒ from the tail), returned OLDEST-FIRST
//     within the window so the client appends them in order.
//
// Bounds and Window are BY-ID: they answer for exactly the session the
// filter names. Unlike Subscribe / Replay — whose Admin mode fans IN across
// identities for a fleet-view tail — a Bounds/Window read ALWAYS scopes
// to the filter's named session triple, even under Admin. Admin authority
// is decided at the call edge (it authorises reading ANOTHER identity's
// session); it never widens the per-session scan to return other sessions'
// events from a shared ring. Drivers implement these two with MatchesScoped
// (which honours the named triple regardless of Admin), never Matches. An
// empty-triple non-admin filter is still rejected with
// ErrIdentityScopeRequired; a driver that retains no events for the
// filter's session returns ErrNoHistory from Bounds.
//
// ListWindow is the ONE deliberately-amended, cross-session member of this
// seam — the `events.list` substrate. It is a time-ranged, filtered,
// cursor-paged read that, when its query carries Admin (a verified `admin`
// OR `console:fleet` claim, decided at the call edge from the verified
// session — never the request body), FANS IN across sessions in
// global-sequence order, honouring the wire filter's multi-valued identity
// SETS + since/until bounds (MatchWire), and emits one
// `audit.admin_scope_used` per widened request. This does NOT contradict
// the never-fan-in rule the Bounds/Window godoc and MatchesScoped record:
// that rule closed a SILENT Admin-widening of a by-id read (asking for one
// session, getting the whole ring, no audit). ListWindow's widening is the
// EXPLICIT, authority-gated, audited fleet read the operator asked for on a
// distinct method — categorically different. A non-admin ListWindow scopes
// to the caller's own triple exactly like Bounds/Window.
//
// The type assertion bus.(events.HistoryReplayer) is a compile-shaped
// contract; a driver that does not implement it (or whose replay surface
// is configured off) leaves the windowed-read surface unavailable, which
// the handler surfaces loudly rather than as a silent empty page.
type HistoryReplayer interface {
	// Bounds returns the lowest (head) and highest (tail) RETAINED
	// sequence for the filter's session, or ErrNoHistory when the session
	// has no retained events. Scoped to the named session (MatchesScoped),
	// never fanning in under Admin.
	//
	// truncated reports whether older events than the retained head may
	// have been evicted — true once a best-effort ring has overwritten an
	// occupied slot (the head is then the ring's oldest retained, NOT the
	// session's first sequence), false for the gap-free durable log (which
	// never trims the per-session sequence list in V1) and for a ring that
	// is merely at-capacity with nothing yet evicted. It is the honest "history may be incomplete"
	// signal that flows to StateHistoryResponse.Truncated, so a windowed
	// read over a wrapped ring is never silently presented as complete
	// (CLAUDE.md §13 "never silently lossy"). It is conservative for the
	// ring: a wrap evicts SOME session's oldest events, possibly not this
	// one's, so a true value means "older events may be missing", not
	// "definitely are".
	Bounds(ctx context.Context, f Filter) (head, tail uint64, truncated bool, err error)
	// Window returns at most limit events whose Sequence < before
	// (before==0 ⇒ from the tail), the most-recent K selected, returned
	// OLDEST-FIRST within the window, matching f. An empty window (no
	// events below before) returns (nil, nil). Identity-scoped exactly
	// like Subscribe / Replay.
	Window(ctx context.Context, before uint64, limit int, f Filter) ([]Event, error)
	// ListWindow is the cross-session, time-ranged, cursor-paged windowed
	// read backing the `events.list` Protocol method. Unlike Bounds/Window
	// (by-id, single session), it honours the wire filter's multi-valued
	// identity sets + since/until time bounds (MatchWire) and — when
	// q.Admin is set (the verified fleet claim, decided at the call edge)
	// — FANS IN across sessions in global-sequence order. A non-admin
	// query scopes to the caller's own triple (the handler folds it into
	// the filter's elided axes); a non-admin query whose filter still
	// elides an identity axis is rejected with ErrIdentityScopeRequired
	// (fail closed). A widened (Admin) query emits exactly one
	// audit.admin_scope_used before returning — the per-request audit that
	// makes the sanctioned fan-in observable.
	//
	// Paging mirrors Window: q.Before is the exclusive upper sequence
	// bound (0 ⇒ from the tail); the page is the most-recent K matching
	// events with Sequence < Before, returned OLDEST-FIRST. NextCursor is
	// the lowest Sequence in the page (0 when the retained head is
	// reached); HasMore reports whether older matching events remain;
	// Truncated is the honest retention-gap signal (true on a wrapped
	// best-effort ring, false on the gap-free durable log). Bus-internal
	// notices (IsBusInternalNotice) are excluded, exactly like Window.
	ListWindow(ctx context.Context, q EventListQuery) (EventListPage, error)
}

// EventListQuery is the request shape for HistoryReplayer.ListWindow (the
// `events.list` substrate). Filter carries the multi-valued identity sets,
// since/until time bounds, and event-type selector; the handler has
// already folded the caller's triple into any elided axis on the
// non-widened path, or gated the fan-in on the verified scope claim on the
// widened path. Admin authorises the cross-session fan-in (decided at the
// call edge from the verified session, never the request body).
type EventListQuery struct {
	// Filter is the wire predicate (identity sets + since/until + types).
	Filter types.EventFilter
	// Before is the exclusive upper sequence bound (the scroll-up cursor);
	// 0 ⇒ from the tail (the newest retained events).
	Before uint64
	// Limit bounds the page size (the caller clamps to the wire max).
	Limit int
	// Admin authorises the cross-session fan-in. When false the read is
	// scoped to the caller's own triple (folded into Filter by the
	// handler); when true the driver fans in across sessions and emits
	// one audit.admin_scope_used per request.
	Admin bool
}

// EventListPage is the response shape for HistoryReplayer.ListWindow.
type EventListPage struct {
	// Events is the page, oldest-first within the window.
	Events []Event
	// NextCursor is the lowest Sequence in the page (0 when the retained
	// head is reached — no older matching events).
	NextCursor uint64
	// HasMore reports whether older matching events remain below the page.
	HasMore bool
	// Truncated is the honest retention-gap signal (true on a wrapped ring
	// whose oldest retained row may sit above the true first match).
	Truncated bool
}

// Fencer is the optional capability a replay/history-capable bus driver
// implements so a session-erasure cascade can durably FENCE a session
// triple against late, asynchronously-emitted events. It is a sibling to
// Replayer / HistoryReplayer, discovered by type assertion.
//
// The failure it closes: a `sessions.delete` erasure sweeps a session's
// persisted history (the StateStore.DeleteScope step), but a task that was
// still finishing concurrently can emit its lifecycle events AFTER the
// sweep — re-creating retained history under the just-erased triple, so a
// subsequent `state.history` read is non-empty. Fence shuts that window:
// after Fence(id) the bus
//
//   - DROPS (does not retain, does not fan out) every event whose identity
//     triple equals id, so a late event from the defunct task is never
//     persisted; and
//   - reports the fenced triple as having NO retained history (Bounds ⇒
//     ErrNoHistory, Window / Replay ⇒ empty), so any event that slipped
//     into the retention substrate before the fence took hold is invisible
//     to the read surface.
//
// Fence MUST synchronise with the driver's publish path so an in-flight
// Publish either completes before Fence returns (its event is then swept by
// the cascade's DeleteScope, which runs after Fence) or observes the fence
// and is dropped — leaving no event retained past the fence. It is keyed by
// the identity triple (tenant, user, session) — the isolation principal —
// never by agent_id (CLAUDE.md §6).
//
// Unfence(id) lifts the fence: the SessionRegistry calls it when a brand
// new session is opened on the same triple (a freshly-reused conversation
// id) so the new session's events are retained normally. The fence is held
// in-memory — its sole job is to fence events from a now-defunct in-process
// task, which cannot outlive the bus process; a Runtime restart drops the
// task and the (never-persisted) fence together, and the never-retained
// events stay gone.
//
// A bus that does not implement Fencer leaves the cascade's late-event
// window open; the erasure's primary sweep (DeleteScope) still removes all
// already-retained history, so the basic right-to-erasure guarantee holds —
// the fence is the concurrency hardening on top. Both V1 bus drivers (inmem,
// durable) implement it.
type Fencer interface {
	// Fence marks the triple id as erased: subsequent events for it are
	// dropped, and its retained history reads empty. Idempotent. Returns
	// ErrBusClosed after Close.
	Fence(ctx context.Context, id identity.Identity) error
	// Unfence lifts a fence set by Fence (a reused session id is being
	// opened afresh). Idempotent — unfencing a triple that was never fenced
	// is a no-op. Returns ErrBusClosed after Close.
	Unfence(ctx context.Context, id identity.Identity) error
}

// DroppedCounter is the optional capability a bus driver implements to
// report a cumulative count of messages dropped under backpressure. It
// mirrors the Replayer shape: a single driver-specific read surface
// discovered by type assertion, not a mandatory interface method (a
// future durable bus may have no in-memory drop notion). The runtime
// observability wiring asserts the bus for this capability to source
// the harbor_runtime_events_dropped gauge; a bus that does not implement
// it simply leaves that gauge unregistered (the gauge seam skips a nil
// callback).
//
// The count is monotonic for the bus's lifetime and is safe to read
// concurrently with publishing.
type DroppedCounter interface {
	// DroppedTotal returns the cumulative number of events dropped under
	// subscriber-buffer backpressure since the bus was constructed.
	DroppedTotal() int64
}

// RetentionReporter is the optional capability a retention-bearing bus
// driver implements to report the OBSERVED oldest-retained event time —
// the runtime-wide retention horizon for the event log (the substrate
// the conversation-history read projects). It is a sibling to
// DroppedCounter / Replayer: a single read surface discovered by type
// assertion, not a mandatory EventBus method (a future write-through bus
// with no local retention may implement neither).
//
// The value is OBSERVED, never configured: Harbor has no retention knob
// (the durable log is gap-free and untrimmed in V1), so the horizon is
// simply the wall-clock time of the oldest event the bus still holds. It
// is "oldest RETAINED", not "oldest ever": a best-effort ring advances
// its horizon as it evicts, and a consumer reads the advancing edge as
// the honest "expect gaps before X" signal that pairs with the at-read
// truncated flag.
//
// The reported time carries no identity-scoped content — it is a bare
// wall-clock instant over the whole retained set — so it is runtime-wide
// and safe to expose without an identity filter (unlike the by-session
// history reads). Both V1 bus drivers (inmem, durable) implement it.
type RetentionReporter interface {
	// OldestRetainedAt returns the OccurredAt of the oldest event the bus
	// currently retains and present=true; (zero, false, nil) when the bus
	// retains no events (an empty ring / empty log). It returns
	// ErrBusClosed after Close. Bus-internal notices (see
	// IsBusInternalNotice) are excluded so the horizon matches the shape
	// of the history a session read would surface.
	OldestRetainedAt(ctx context.Context) (oldest time.Time, present bool, err error)
}

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrUnknownEventType — Publish was called with an EventType not
	// in the canonical registry.
	ErrUnknownEventType = errors.New("events: unknown EventType")
	// ErrIdentityScopeRequired — Subscribe filter elides the identity
	// triple AND Admin is false.
	ErrIdentityScopeRequired = errors.New("events: filter must specify (tenant, user, session) unless Admin")
	// ErrAdminScopeRequired — reserved; wiring will return
	// this when a caller claims Admin without a verified scope claim.
	// Harbor trusts the caller; the sentinel is exposed now so the
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
	// that fall through to a durable log can interpret the
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
	// ErrNoHistory — HistoryReplayer.Bounds was called for a session that
	// has no retained event history. Distinct from an empty window (which
	// Window returns as (nil, nil)): Bounds cannot report a head/tail for
	// a session that never published.
	ErrNoHistory = errors.New("events: session has no retained event history")
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
