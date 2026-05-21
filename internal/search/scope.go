package search

import (
	"context"

	"github.com/hurtener/Harbor/internal/protocol/auth"
)

// AdminScopeFromAuth is the production ScopeChecker — it consults the
// Phase 61 auth-attached scope set on ctx via `auth.HasScope`.
//
// The function is exported so the Phase 72c wiring (and tests that
// want the real shape) can inject it directly without re-implementing
// the closed-scope-set predicate.
//
// D-079 pins the closed two-scope set `{admin, console:fleet}` — BOTH
// are cross-tenant fan-in entitlements. The Wave 13 §17.5 checkpoint
// (D-132 / W8) corrected this predicate to honour `console:fleet` too:
// a Console fleet operator carrying only `console:fleet` is entitled to
// a cross-tenant search, exactly as it is entitled to a cross-tenant
// `events.subscribe`.
func AdminScopeFromAuth(ctx context.Context) bool {
	return auth.HasScope(ctx, auth.ScopeAdmin) || auth.HasScope(ctx, auth.ScopeConsoleFleet)
}
