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
func AdminScopeFromAuth(ctx context.Context) bool {
	return auth.HasScope(ctx, auth.ScopeAdmin)
}
