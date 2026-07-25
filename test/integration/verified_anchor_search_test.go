// Package integration's verified_anchor_search_test.go is the in-package
// half of the verified-anchor cross-tenant pins; see
// verified_anchor_crosstenant_test.go for why the anchor shape matters.
// The two halves are split only because the search and sessions fixtures
// live in different test packages.
package integration

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	protoauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// anchoredSearchCtx roots a context the way a mounted route does: the
// caller's identity is the VERIFIED anchor, not merely the working
// identity. Every cross-tenant re-scope below is therefore a move the
// tenant guard inspects, exactly as it is in production.
func anchoredSearchCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatalf("seat verified identity: %v", err)
	}
	return ctx
}

// anchoredFleetSearchCtx adds the admin-tier claim a fleet fan-in carries.
// The per-hit read re-checks it where it mints its crossing, so a fleet
// test must supply it exactly as the wire does.
func anchoredFleetSearchCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	return protoauth.WithScopes(anchoredSearchCtx(t, id), []protoauth.Scope{protoauth.ScopeAdmin})
}

// TestE2E_VerifiedAnchor_FleetTaskSearch_ForeignRowSurvives — an admin
// searching tasks across tenants gets the foreign tenant's hits. The
// per-hit read re-scopes to the matched session's identity; if that move
// is refused the hit is dropped and the search reports a smaller result
// set with no error — a silent narrowing of an authorized fan-in.
func TestE2E_VerifiedAnchor_FleetTaskSearch_ForeignRowSurvives(t *testing.T) {
	t.Parallel()
	st := newSearchStack(t, true)
	defer st.close()

	own := identity.Identity{TenantID: "t1", UserID: "u", SessionID: "s1"}
	foreign := identity.Identity{TenantID: "t2", UserID: "u", SessionID: "s2"}
	st.openSession(t, own)
	st.openSession(t, foreign)
	st.spawnTask(t, own, "deploy production")
	st.spawnTask(t, foreign, "deploy production")

	resp, err := st.surface.Dispatch(anchoredFleetSearchCtx(t, own), methods.MethodSearchTasks, &types.SearchRequest{
		Query:  "deploy",
		Filter: types.SearchFilter{TenantIDs: []string{"t1", "t2"}},
	})
	if err != nil {
		t.Fatalf("admin fleet task search: %v", err)
	}
	if len(resp.Rows) != 2 {
		t.Fatalf("admin cross-tenant task search returned %d rows, want 2 — a foreign-tenant hit must not be silently dropped by the per-hit re-scope",
			len(resp.Rows))
	}

	tenants := map[string]bool{}
	for _, row := range resp.Rows {
		tenants[row.TenantID] = true
	}
	if !tenants["t2"] {
		t.Error("the foreign tenant's row is missing from an authorized fleet search")
	}
}
