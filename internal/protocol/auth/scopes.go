package auth

import "context"

// Scope is a verified JWT scope claim — a privilege the Protocol
// consults when granting cross-session / cross-tenant subscriptions or
// fleet-control privileges (RFC §4.2 + §5.5: "Extended scopes (admin,
// console:fleet) gate cross-session and cross-tenant subscriptions").
//
// Scopes are not isolation principals — the (tenant, user, session)
// triple is and stays the isolation key (CLAUDE.md §6 rule 1). A scope
// is an *additional* entitlement carried alongside the triple.
type Scope string

// Canonical scope constants. The set is closed at V1 — adding a new
// scope is a Protocol-surface phase, not an ad-hoc addition.
//
// ScopeAdmin is the cross-tenant fan-in entitlement: a Subscribe call
// with `events.Filter.Admin = true` requires this scope (RFC §6.13
// admin subscriptions). The Phase 05 events.ErrAdminScopeRequired
// sentinel is the corresponding error.
//
// ScopeConsoleFleet is the fleet-observation entitlement (RFC §7
// "Fleet privilege tiers"): a Console managing multiple Runtimes uses
// this scope to subscribe to events from outside its single
// (tenant, user, session) triple. Distinct from a hypothetical
// "fleet:control" scope (deferred per D-066).
const (
	ScopeAdmin        Scope = "admin"
	ScopeConsoleFleet Scope = "console:fleet"
)

// canonicalScopes is the closed set. IsValidScope checks membership.
var canonicalScopes = map[Scope]struct{}{
	ScopeAdmin:        {},
	ScopeConsoleFleet: {},
}

// IsValidScope reports whether s is one of the canonical scopes. An
// unknown scope on a JWT is silently dropped from the verified set —
// a token that claims "future:scope" reads back as having no scopes
// rather than failing. The closed set means an attacker cannot grant
// themselves an undocumented privilege by inventing a scope name.
func IsValidScope(s Scope) bool {
	_, ok := canonicalScopes[s]
	return ok
}

// CanonicalScopes returns a copy of the closed canonical scope set.
// Used by tests to pin the surface and by the audit emitter to render
// the per-request scope set deterministically.
func CanonicalScopes() []Scope {
	out := make([]Scope, 0, len(canonicalScopes))
	for s := range canonicalScopes {
		out = append(out, s)
	}
	return out
}

// scopeKey is the unexported context key under which the middleware
// stashes the verified scope set. Independent from identity's key.
type scopeKey int

const scopesKey scopeKey = iota

// WithScopes attaches the verified scope set to ctx. The middleware
// calls this once per request after Validator.Validate succeeds; the
// SSE handler reads the set back via HasScope to gate cross-tenant
// fan-in.
//
// A nil scopes slice is permitted (a token with no scopes is still
// authenticated) — HasScope will return false for any scope check.
func WithScopes(ctx context.Context, scopes []Scope) context.Context {
	// Filter to the canonical set so an attacker-injected unknown
	// scope cannot reach a downstream consumer that might switch on a
	// raw string.
	clean := make([]Scope, 0, len(scopes))
	for _, s := range scopes {
		if IsValidScope(s) {
			clean = append(clean, s)
		}
	}
	return context.WithValue(ctx, scopesKey, clean)
}

// ScopesFrom returns the verified scope set on ctx, and a presence
// bool. A request that has not been through the auth middleware has no
// scopes attached — ScopesFrom returns (nil, false), which is distinct
// from ScopesFrom returning (nil, true) for a token with no scopes.
func ScopesFrom(ctx context.Context) ([]Scope, bool) {
	v, ok := ctx.Value(scopesKey).([]Scope)
	if !ok {
		return nil, false
	}
	// Defensive copy so callers cannot mutate the in-context slice.
	out := make([]Scope, len(v))
	copy(out, v)
	return out, true
}

// HasScope reports whether ctx carries scope s. A request that has
// not been through the auth middleware (no scope set on ctx) returns
// false — the safe default for a privilege check is "absent = denied".
func HasScope(ctx context.Context, s Scope) bool {
	scopes, ok := ScopesFrom(ctx)
	if !ok {
		return false
	}
	for _, have := range scopes {
		if have == s {
			return true
		}
	}
	return false
}
