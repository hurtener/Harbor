package events

import (
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// WireConversion is the result of converting a wire EventFilter into
// a runtime Filter (see FilterFromWire). The wire shape
// (prototypes.EventFilter) is operator-facing and lives in
// internal/protocol/types; the runtime shape (events.Filter) is
// bus-facing and lives here. This conversion is the one place the two
// namespaces meet — the wire transport produces a WireConversion once
// per request.
//
// Semantics of the conversion:
//
//   - Empty TenantIDs / UserIDs / SessionIDs / RunIDs on the wire are
//     interpreted as "the caller's own component" — the caller's
//     identity quadruple supplies the value. This preserves backward
//     compatibility with the pre-72a wire shape (no filter struct → full
//     triple-scoped subscription).
//   - A single non-caller TenantID OR len(TenantIDs) > 1 signals a
//     cross-tenant request; likewise a single non-caller UserID OR
//     len(UserIDs) > 1 signals a cross-USER request — either sets
//     RequiresAdminScope == true so the wire edge can gate on
//     auth.HasScope before calling Subscribe. The SessionID axis is NOT
//     gated on a foreign single value: a user legitimately reads their
//     own other sessions, so only a multi-session set elevates.
//   - EventTypes are converted 1:1.
//   - Since / Until are passed through unchanged.
//
// The conversion NEVER silently drops a missing identity component: it
// returns the (best-effort) Filter alongside RequiresAdminScope; the
// caller is responsible for the actual rejection if RequiresAdminScope
// is true but the scope claim is absent. (CLAUDE.md §5 "fail loudly.")
//
// WireConversion is a pure value type: zero package-level state, safe
// for concurrent use.
type WireConversion struct {
	// Filter is the bus-facing predicate the caller passes to Subscribe.
	// Triple components are backfilled from the caller's identity.
	Filter Filter
	// RequiresAdminScope is true when the wire EventFilter requested a
	// cross-tenant fan-in (a TenantID set other than the caller's own, or
	// len(TenantIDs) > 1) OR a cross-user read (a UserID set other than
	// the caller's own, or len(UserIDs) > 1) OR a multi-session set. The
	// wire edge gates on auth.HasScope before calling Subscribe; without
	// the scope, the request is rejected with CodeIdentityScopeRequired
	// (HTTP 403). A same-user single foreign SessionID does NOT set this
	// (a user reads their own sessions).
	RequiresAdminScope bool
	// Since / Until carry the optional time-window bounds from the wire
	// filter. They are NOT enforced by Filter.Matches (the bus's
	// per-event match is identity + type only); the aggregator + any
	// post-filtering replay consult them directly.
	Since time.Time
	Until time.Time
}

// FilterFromWire converts a wire EventFilter into a runtime Filter,
// resolving missing identity components from callerTenant / callerUser
// / callerSession (the (tenant, user, session) triple verified at the
// auth middleware edge). callerTenant must be non-empty — a missing
// caller triple is a rejection upstream (CodeIdentityRequired) and this
// helper is not the place to mask it.
//
// The returned WireConversion is a value; the caller passes
// WireConversion.Filter to bus.Subscribe and reads RequiresAdminScope to
// decide whether to gate on auth.HasScope.
func FilterFromWire(
	wire prototypes.EventFilter,
	callerTenant, callerUser, callerSession string,
) WireConversion {
	out := WireConversion{
		Since: wire.Since.UTC(),
		Until: wire.Until.UTC(),
	}

	// Tenants: empty → caller's tenant; single non-caller → cross-tenant;
	// multiple → cross-tenant. RequiresAdminScope is set in the latter
	// two cases; the wire edge gates on auth.HasScope.
	switch len(wire.TenantIDs) {
	case 0:
		out.Filter.Tenant = callerTenant
	case 1:
		out.Filter.Tenant = wire.TenantIDs[0]
		if out.Filter.Tenant != callerTenant {
			out.RequiresAdminScope = true
		}
	default:
		out.RequiresAdminScope = true
		// For the bus-facing Filter we default to the caller's tenant;
		// the actual multi-tenant fan-in happens via Filter.Admin (set
		// by the caller after the scope check passes). The aggregator
		// reads wire.TenantIDs directly for its filtering loop.
		out.Filter.Tenant = callerTenant
	}

	// Users: empty → caller's; a single FOREIGN user is a cross-user
	// read that requires the elevated scope (mirroring the tenant axis
	// above); multiple is preserved for the aggregator (Filter.Matches is
	// single-valued, so a multi-user predicate uses Filter.Admin=true at
	// the bus and the aggregator filters in Go). Gating the single-value
	// case closes a cross-USER disclosure on every row-returning consumer
	// of this helper: without it a non-admin caller naming another user's
	// id kept RequiresAdminScope=false, the wire edge folded only ELIDED
	// axes, and the foreign user survived into the bus predicate — the
	// caller received another user's rows. §6 forbids relying on
	// user/session-id unguessability.
	switch len(wire.UserIDs) {
	case 1:
		out.Filter.User = wire.UserIDs[0]
		if out.Filter.User != callerUser {
			out.RequiresAdminScope = true
		}
	case 0:
		out.Filter.User = callerUser
	default:
		out.Filter.User = callerUser
		out.RequiresAdminScope = true
	}
	// Sessions: empty → caller's; single value used AS-IS (NOT gated on
	// != callerSession). A user legitimately reads their OWN other
	// sessions (the Console Sessions / Playground history flow), so a
	// {same-tenant, same-user, different-session} read must NOT force
	// elevation — that would break a valid same-user flow. The tenant +
	// user gates above already close the cross-USER / cross-TENANT
	// disclosure; the session axis stays same-user-own-sessions. (The
	// broader "cross-session observer needs elevated scope vs a user
	// reading their own sessions" posture question across all event
	// surfaces is tracked at the wave checkpoint, not resolved here.)
	switch len(wire.SessionIDs) {
	case 1:
		out.Filter.Session = wire.SessionIDs[0]
	case 0:
		out.Filter.Session = callerSession
	default:
		out.Filter.Session = callerSession
		out.RequiresAdminScope = true
	}
	if len(wire.RunIDs) == 1 {
		out.Filter.Run = wire.RunIDs[0]
	}

	// EventTypes: copy 1:1 into the bus filter's Types selector.
	if len(wire.EventTypes) > 0 {
		out.Filter.Types = make([]EventType, 0, len(wire.EventTypes))
		for _, t := range wire.EventTypes {
			if t == "" {
				continue
			}
			out.Filter.Types = append(out.Filter.Types, EventType(t))
		}
	}

	return out
}

// MatchWire reports whether ev satisfies the wire EventFilter (header
// fields only — payload bytes are explicitly out of scope per the
// EventFilter godoc). The matcher is identity +
// type + window — exactly the surface the aggregator and any post-
// filtering consumer iterate.
//
// Identity semantics:
//
//   - Empty TenantIDs / UserIDs / SessionIDs / RunIDs means "any" on
//     that axis (the aggregator/consumer has already gated on the
//     scope claim; FilterFromWire returned RequiresAdminScope=true if
//     the request needed it).
//   - A non-empty set means "the event's component must be in the set".
//
// Time semantics:
//
//   - Since.IsZero() means "no lower bound." Otherwise
//     ev.OccurredAt must be >= Since (inclusive).
//   - Until.IsZero() means "no upper bound." Otherwise
//     ev.OccurredAt must be < Until (exclusive).
//
// MatchWire is a pure function — no package-level state, safe for
// concurrent use.
func MatchWire(ev Event, wire prototypes.EventFilter) bool {
	// Type set.
	if len(wire.EventTypes) > 0 {
		matched := false
		for _, t := range wire.EventTypes {
			if string(ev.Type) == t {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}

	// Identity sets — "any" if empty, else membership.
	if !containsOrEmpty(wire.TenantIDs, ev.Identity.TenantID) {
		return false
	}
	if !containsOrEmpty(wire.UserIDs, ev.Identity.UserID) {
		return false
	}
	if !containsOrEmpty(wire.SessionIDs, ev.Identity.SessionID) {
		return false
	}
	if !containsOrEmpty(wire.RunIDs, ev.Identity.RunID) {
		return false
	}

	// Time bounds — Since inclusive, Until exclusive.
	if !wire.Since.IsZero() && ev.OccurredAt.Before(wire.Since) {
		return false
	}
	if !wire.Until.IsZero() && !ev.OccurredAt.Before(wire.Until) {
		return false
	}
	return true
}

// containsOrEmpty reports whether set is empty (interpreted as "any")
// or contains v. Pure helper for MatchWire; not exported.
func containsOrEmpty(set []string, v string) bool {
	if len(set) == 0 {
		return true
	}
	for _, s := range set {
		if s == v {
			return true
		}
	}
	return false
}

// WireFilterHasFullTriple reports whether the wire filter names at least
// one value on each of the tenant / user / session identity axes — the
// fail-closed precondition a NON-admin ListWindow read must satisfy (an
// elided axis on a non-widened read would leak across that axis via
// MatchWire's "empty = any" rule). The handler folds the caller's triple
// into elided axes before the driver call; this predicate is the driver's
// defence-in-depth check that it did (CLAUDE.md §5 "fail loudly").
func WireFilterHasFullTriple(wire prototypes.EventFilter) bool {
	return len(wire.TenantIDs) > 0 && len(wire.UserIDs) > 0 && len(wire.SessionIDs) > 0
}

// WireFilterFirst returns the first element of set, or "" when empty. Used
// to project a representative triple component onto the
// audit.admin_scope_used notice a widened ListWindow read emits (the
// notice records the REQUESTED scope, mirroring the Bounds admin path).
func WireFilterFirst(set []string) string {
	if len(set) == 0 {
		return ""
	}
	return set[0]
}

// WireFilterMatchesTriple reports whether id's tenant/user/session
// satisfy the wire filter's identity sets ("empty = any" per axis). It is
// the CHEAP head-record pre-filter the durable `events.list` scan uses to
// skip sessions the filter excludes before loading any entry record; the
// run + event-type + since/until predicates are applied per event via
// MatchWire (they need the persisted bytes). Pure function.
func WireFilterMatchesTriple(wire prototypes.EventFilter, id identity.Identity) bool {
	return containsOrEmpty(wire.TenantIDs, id.TenantID) &&
		containsOrEmpty(wire.UserIDs, id.UserID) &&
		containsOrEmpty(wire.SessionIDs, id.SessionID)
}

// ListWindowFromSnapshot selects the `events.list` page from a
// sequence-ordered (ascending) event snapshot: the most-recent `limit`
// events matching the wire filter with Sequence < before (before==0 ⇒ from
// the tail), returned OLDEST-FIRST. Bus-internal notices are excluded.
// It fills Events / NextCursor / HasMore; the caller sets Truncated from
// the substrate's honest retention signal.
//
// Paging grammar mirrors the `state.history` window: NextCursor is the
// lowest Sequence in the returned page (0 when no older matching events
// remain), HasMore reports whether older matches sit below the page. The
// scan collects up to limit+1 matches so HasMore is exact without a second
// pass. Pure function — no package state, safe for concurrent use.
func ListWindowFromSnapshot(snapshot []Event, before uint64, limit int, wire prototypes.EventFilter) EventListPage {
	if limit <= 0 || len(snapshot) == 0 {
		return EventListPage{}
	}
	// Walk newest-first, collecting up to limit+1 matches so we can tell
	// whether an older match remains (HasMore) without a second scan.
	matches := make([]Event, 0, limit+1)
	for i := len(snapshot) - 1; i >= 0; i-- {
		ev := snapshot[i]
		if before != 0 && ev.Sequence >= before {
			continue
		}
		if IsBusInternalNotice(ev.Type) || !MatchWire(ev, wire) {
			continue
		}
		matches = append(matches, ev)
		if len(matches) > limit {
			break
		}
	}
	var page EventListPage
	if len(matches) > limit {
		page.HasMore = true
		matches = matches[:limit]
		page.NextCursor = matches[len(matches)-1].Sequence // lowest in page
	}
	// Reverse to oldest-first.
	for i, j := 0, len(matches)-1; i < j; i, j = i+1, j-1 {
		matches[i], matches[j] = matches[j], matches[i]
	}
	page.Events = matches
	return page
}
