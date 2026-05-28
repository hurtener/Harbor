package builtin

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
)

// ErrIdentityRequired is the sentinel meta-tools surface when ctx
// arrives without a complete (tenant, user, session) triple. Returned
// instead of silently zero-quadruple-falling-through to a SkillStore
// call that would either error or — worse — leak across tenants
// (CLAUDE.md §6 rule 9 + §13).
var ErrIdentityRequired = errors.New("builtin: identity (tenant/user/session) is mandatory")

// requireIdentity reads the calling identity from ctx and fails loud
// when any component of the (tenant, user, session) triple is missing.
// Phase 107c / D-167 — the discovery meta-tools (`tool_search`,
// `tool_get`, `skill_search`, `skill_get`) refuse to operate without
// identity so cross-tenant discovery leaks are structurally impossible.
func requireIdentity(ctx context.Context) (identity.Quadruple, error) {
	q, ok := identity.QuadrupleFrom(ctx)
	if !ok {
		return identity.Quadruple{}, ErrIdentityRequired
	}
	if q.TenantID == "" || q.UserID == "" || q.SessionID == "" {
		return identity.Quadruple{}, fmt.Errorf("%w: triple=%q/%q/%q",
			ErrIdentityRequired, q.TenantID, q.UserID, q.SessionID)
	}
	return q, nil
}
