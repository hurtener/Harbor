package events

import (
	"time"

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
//     cross-tenant request — the result has RequiresAdminScope == true
//     so the wire edge can gate on auth.HasScope before calling
//     Subscribe.
//   - EventTypes are converted 1:1.
//   - Since / Until are passed through unchanged.
//
// The conversion NEVER silently drops a missing identity component: it
// returns the (best-effort) Filter alongside RequiresAdminScope; the
// caller is responsible for the actual rejection if RequiresAdminScope
// is true but the scope claim is absent. (CLAUDE.md §5 "fail loudly.")
//
// WireConversion is a pure value type: zero package-level state, safe
// for concurrent use (D-025).
type WireConversion struct {
	// Filter is the bus-facing predicate the caller passes to Subscribe.
	// Triple components are backfilled from the caller's identity.
	Filter Filter
	// RequiresAdminScope is true when the wire EventFilter requested a
	// cross-tenant fan-in (a TenantID set other than the caller's own,
	// or len(TenantIDs) > 1). The wire edge gates on auth.HasScope
	// before calling Subscribe; without the scope, the request is
	// rejected with CodeIdentityScopeRequired (HTTP 403).
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

	// Users / Sessions / Runs: empty → caller's; single value used;
	// multiple is preserved for the aggregator (Filter.Matches is
	// single-valued, so a multi-user predicate uses Filter.Admin=true at
	// the bus and the aggregator filters in Go).
	switch len(wire.UserIDs) {
	case 1:
		out.Filter.User = wire.UserIDs[0]
	case 0:
		out.Filter.User = callerUser
	default:
		out.Filter.User = callerUser
		out.RequiresAdminScope = true
	}
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
// EventFilter godoc + Brief 11 §CC-4). The matcher is identity +
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
// concurrent use (D-025).
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
