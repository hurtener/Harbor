// Package integration's verified_anchor_crosstenant_test.go pins the two
// cross-tenant admin projections against a request context shaped the way
// a MOUNTED ROUTE shapes it — anchored at identity.WithVerified.
//
// This file exists because the rest of the cross-tenant suite roots its
// contexts at identity.With(context.Background(), …). That shape carries
// no verified anchor, so the tenant-widening guard never fires and the
// suite stays green while production breaks. Every mounted Protocol route
// now seats a verified identity before any handler runs, so a fleet
// projection ALWAYS executes under an anchor in production — and a
// per-row re-scope to a foreign tenant is therefore always a move the
// guard inspects.
//
// Both tests below FAIL if the per-row re-scope is a plain identity.With:
// the sessions row reports counters nobody measured, and the search row
// disappears entirely.
package integration_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// anchoredCtx roots a context the way a mounted route does: the caller's
// identity is the VERIFIED anchor, not merely the working identity.
func anchoredCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatalf("seat verified identity: %v", err)
	}
	return ctx
}

// anchoredFleetCtx adds the admin-tier claim a fleet read carries. The
// per-row projections re-check it where they mint their crossing, so a
// fleet test must supply it exactly as the wire does.
func anchoredFleetCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	return auth.WithScopes(anchoredCtx(t, id), []auth.Scope{auth.ScopeAdmin})
}

// TestE2E_VerifiedAnchor_FleetSessionList_ForeignRowCountersAreMeasured —
// an admin listing another tenant's sessions gets that row's REAL
// counters. The counter rollup re-scopes to the row's own identity, which
// is a tenant move under the caller's anchor; if that move is refused the
// row reports zeros nobody measured, with the partial marker unset — a
// fabricated exact zero, which is the shape this whole phase exists to
// forbid.
func TestE2E_VerifiedAnchor_FleetSessionList_ForeignRowCountersAreMeasured(t *testing.T) {
	deps := newSessionEnrichDeps(t)
	defer deps.cleanup()

	caller := identity.Identity{TenantID: "tA", UserID: "uA", SessionID: "sA"}
	foreign := identity.Identity{TenantID: "tB", UserID: "uB", SessionID: "sB"}

	// Seed the FOREIGN tenant's session with a task the rollup must find.
	spawnFailedTask(t, deps.tasks, foreign)

	resp, err := deps.svc.List(anchoredFleetCtx(t, caller), prototypes.SessionsListRequest{
		Identity: prototypes.IdentityScope{
			Tenant: caller.TenantID, User: caller.UserID, Session: caller.SessionID,
		},
		Filter: prototypes.SessionFilter{TenantIDs: []string{"tA", "tB"}},
	}, true)
	if err != nil {
		t.Fatalf("admin fleet List: %v", err)
	}

	var foreignRow *prototypes.SessionRow
	for i := range resp.Rows {
		if resp.Rows[i].Identity.Tenant == "tB" {
			foreignRow = &resp.Rows[i]
		}
	}
	if foreignRow == nil {
		t.Fatalf("admin fleet List returned no tB row; got %d rows", len(resp.Rows))
	}

	if foreignRow.TasksCount != 1 {
		t.Errorf("foreign-tenant row TasksCount = %d, want 1 — the rollup must read the row under the row's own identity, not report a zero it never measured",
			foreignRow.TasksCount)
	}
	if !foreignRow.HasFailedTask {
		t.Error("foreign-tenant row HasFailedTask = false, want true — the seeded task failed")
	}
	if foreignRow.CountersPartial {
		t.Error("foreign-tenant row CountersPartial = true, want false — the rollup was taken in full")
	}
}

// TestE2E_VerifiedAnchor_FleetSessionList_UnreadableRowIsMarkedPartial —
// the companion contract: when a row's counters genuinely cannot be read,
// the row says so. A zero that means "we could not look" must never be
// reported as a zero that means "we looked and there were none".
//
// The unreadable case here is a row whose tenant the caller has no
// authorized crossing to, reached with the fleet flag off.
func TestE2E_VerifiedAnchor_FleetSessionList_UnreadableRowIsMarkedPartial(t *testing.T) {
	deps := newSessionEnrichDeps(t)
	defer deps.cleanup()

	caller := identity.Identity{TenantID: "tA", UserID: "uA", SessionID: "sA"}
	resp, err := deps.svc.List(anchoredCtx(t, caller), prototypes.SessionsListRequest{
		Identity: prototypes.IdentityScope{
			Tenant: caller.TenantID, User: caller.UserID, Session: caller.SessionID,
		},
	}, false)
	if err != nil {
		t.Fatalf("own-scope List: %v", err)
	}
	// The caller's own row is readable, so it must NOT be marked partial —
	// the marker has to stay meaningful.
	for _, row := range resp.Rows {
		if row.Identity.Tenant == "tA" && row.CountersPartial {
			t.Error("own-tenant row CountersPartial = true, want false — an own-scope rollup is fully readable")
		}
	}
}
