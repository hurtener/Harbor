// search_scope.go — the Protocol-side search ScopeChecker (the D-203
// addendum follow-up).
//
// Pre-relocation the production `search.ScopeChecker` lived INSIDE
// `internal/search` as `AdminScopeFromAuth`, with a hard
// `internal/protocol/auth` import — the named standing violation of
// the D-203 direction rule (runtime packages import protocol TYPES
// only, never protocol auth / methods / transports): the area package
// itself, not an `<area>/protocol` adapter subpackage. The check moves
// here, one-way (the `ProtocolScopeAuthorizer` precedent): the server
// package — the Runtime's network surface — adapts the Protocol's
// verified scope claims onto the search package's injected
// `ScopeChecker` seam. `internal/search` no longer knows the
// Protocol's auth vocabulary exists.

package server

import (
	"context"

	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
)

// SearchAdminScopeFromAuth is the production `search.ScopeChecker` —
// it consults the Phase 61 auth-attached scope set on ctx via
// `protocolauth.HasScope`.
//
// The function is exported so the Phase 72c wiring (and tests that
// want the real shape) can inject it directly without re-implementing
// the closed-scope-set predicate.
//
// D-079 pins the closed two-scope set `{admin, console:fleet}` — BOTH
// are cross-tenant fan-in entitlements. The Wave 13 §17.5 checkpoint
// (D-132 / W8) corrected this predicate to honour `console:fleet` too:
// a Console fleet operator carrying only `console:fleet` is entitled
// to a cross-tenant search, exactly as it is entitled to a
// cross-tenant `events.subscribe`.
func SearchAdminScopeFromAuth(ctx context.Context) bool {
	return protocolauth.HasScope(ctx, protocolauth.ScopeAdmin) ||
		protocolauth.HasScope(ctx, protocolauth.ScopeConsoleFleet)
}
