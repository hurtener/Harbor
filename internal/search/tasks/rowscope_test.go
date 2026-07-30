package tasks

// rowScopedCtx is unexported and, in ordinary operation, reachable only
// behind the request-level user gate — so once that gate folds, no
// external test can drive this guard at all. An untested guard is how an
// INERT guard survives, so it is exercised directly here.

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
)

const (
	rsTenant   = "t1"
	rsCaller   = "caller"
	rsForeign  = "somebody-else"
	rsOtherTen = "t2"
)

func verifiedCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), identity.Identity{
		TenantID: rsTenant, UserID: rsCaller, SessionID: "caller-sess",
	})
	if err != nil {
		t.Fatalf("identity.WithVerified: %v", err)
	}
	return ctx
}

// TestTasksSearcher_RowScopedCtx_ForeignUserSameTenantElevates is the
// regression pin for the guard's blind axis: before the user axis became a
// boundary this compared the TENANT alone, so a same-tenant foreign-USER
// session took the UNELEVATED seat and the per-task read went through as
// ordinary narrowing.
func TestTasksSearcher_RowScopedCtx_ForeignUserSameTenantElevates(t *testing.T) {
	t.Parallel()
	s := &Searcher{}
	foreign := identity.Identity{TenantID: rsTenant, UserID: rsForeign, SessionID: "their-sess"}

	// No claim: the crossing is refused rather than seated.
	if _, err := s.rowScopedCtx(verifiedCtx(t), foreign); !errors.Is(err, errRowScopeUnentitled) {
		t.Fatalf("same-tenant foreign user with no claim: got %v, want errRowScopeUnentitled", err)
	}

	// With either admin-tier claim the crossing is seated as an AUDITED
	// elevation, not as plain narrowing — the reason is the record of it.
	for _, scope := range []protocolauth.Scope{protocolauth.ScopeAdmin, protocolauth.ScopeConsoleFleet} {
		ctx := protocolauth.WithScopes(verifiedCtx(t), []protocolauth.Scope{scope})
		sub, err := s.rowScopedCtx(ctx, foreign)
		if err != nil {
			t.Fatalf("same-tenant foreign user under %s: %v", scope, err)
		}
		if !identity.IsElevated(sub) {
			t.Errorf("under %s the crossing must be seated as an audited elevation", scope)
		}
		got, ok := identity.From(sub)
		if !ok || got.UserID != rsForeign {
			t.Errorf("under %s the seated identity is %+v, want user %s", scope, got, rsForeign)
		}
	}
}

// TestTasksSearcher_RowScopedCtx_OwnPrincipalIsPlainNarrowing — reading
// one of the caller's OWN other sessions must NOT require a claim and must
// NOT be minted as an elevation; the session component is deliberately not
// compared.
func TestTasksSearcher_RowScopedCtx_OwnPrincipalIsPlainNarrowing(t *testing.T) {
	t.Parallel()
	s := &Searcher{}
	own := identity.Identity{TenantID: rsTenant, UserID: rsCaller, SessionID: "my-other-sess"}

	sub, err := s.rowScopedCtx(verifiedCtx(t), own)
	if err != nil {
		t.Fatalf("own other session: %v", err)
	}
	if identity.IsElevated(sub) {
		t.Error("reading one of the caller's own sessions must not mint an elevation")
	}
	got, ok := identity.From(sub)
	if !ok || got.SessionID != "my-other-sess" {
		t.Errorf("seated identity %+v, want session my-other-sess", got)
	}
}

// TestTasksSearcher_RowScopedCtx_ForeignTenantStillElevates — the axis
// this guard already covered keeps working; widening it to the user must
// not narrow it on the tenant.
func TestTasksSearcher_RowScopedCtx_ForeignTenantStillElevates(t *testing.T) {
	t.Parallel()
	s := &Searcher{}
	foreign := identity.Identity{TenantID: rsOtherTen, UserID: rsCaller, SessionID: "their-sess"}

	if _, err := s.rowScopedCtx(verifiedCtx(t), foreign); !errors.Is(err, errRowScopeUnentitled) {
		t.Fatalf("foreign tenant with no claim: got %v, want errRowScopeUnentitled", err)
	}
	ctx := protocolauth.WithScopes(verifiedCtx(t), []protocolauth.Scope{protocolauth.ScopeAdmin})
	sub, err := s.rowScopedCtx(ctx, foreign)
	if err != nil {
		t.Fatalf("foreign tenant under admin: %v", err)
	}
	if tenant, ok := identity.ElevatedTenant(sub); !ok || tenant != rsOtherTen {
		t.Errorf("elevated tenant = %q/%v, want %q", tenant, ok, rsOtherTen)
	}
}

// TestTasksSearcher_RowScopedCtx_NoVerifiedAnchorIsUnrestricted — an
// in-process embedder or background worker rooted outside any request has
// no anchor to reconcile against, matching the documented posture of the
// verified-identity read and of the cross-tenant gate beside it.
func TestTasksSearcher_RowScopedCtx_NoVerifiedAnchorIsUnrestricted(t *testing.T) {
	t.Parallel()
	s := &Searcher{}
	any := identity.Identity{TenantID: rsOtherTen, UserID: rsForeign, SessionID: "sess"}
	sub, err := s.rowScopedCtx(context.Background(), any)
	if err != nil {
		t.Fatalf("unanchored ctx: %v", err)
	}
	if got, ok := identity.From(sub); !ok || got.UserID != rsForeign {
		t.Errorf("seated identity %+v, want user %s", got, rsForeign)
	}
}
