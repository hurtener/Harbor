package protocol

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/runtime/steering"
)

// scopeCtx builds the verified-identity context deriveSteeringScope reads
// — the shape the auth middleware produces: the caller identity under the
// identity key, the verified JWT scope set under the scope key.
func scopeCtx(t *testing.T, id identity.Identity, scopes ...auth.Scope) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return auth.WithScopes(ctx, scopes)
}

// derivedRun is a documented dummy run quadruple owned by
// (tenant-a, user-A, session-x) — no secrets.
func derivedRun() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  "tenant-a",
			UserID:    "user-A",
			SessionID: "session-x",
		},
		RunID: "run-1",
	}
}

// TestDeriveSteeringScope_Matrix pins the exact tier each verified caller
// earns over a target run — the load-bearing authorization contract a
// non-admin token is judged against. The session-scoped tier is reserved
// (a verified session participant IS the owner in Harbor's
// single-participant session model, so the full-triple match earns the
// strictly-higher owner tier); the distinct session_user grant is never
// minted, and a session-id collision across users confers nothing.
func TestDeriveSteeringScope_Matrix(t *testing.T) {
	run := derivedRun()

	cases := []struct {
		name      string
		caller    identity.Identity
		scopes    []auth.Scope
		wantScope steering.Scope
		wantOK    bool
	}{
		{
			name:      "admin scope grants admin tier",
			caller:    identity.Identity{TenantID: "tenant-a", UserID: "user-A", SessionID: "session-x"},
			scopes:    []auth.Scope{auth.ScopeAdmin},
			wantScope: steering.ScopeAdmin,
			wantOK:    true,
		},
		{
			name:      "admin from a foreign tenant still grants admin (cross-tenant)",
			caller:    identity.Identity{TenantID: "tenant-b", UserID: "ops", SessionID: "session-ops"},
			scopes:    []auth.Scope{auth.ScopeAdmin},
			wantScope: steering.ScopeAdmin,
			wantOK:    true,
		},
		{
			name:      "verified owner (full-triple match) earns owner_user",
			caller:    identity.Identity{TenantID: "tenant-a", UserID: "user-A", SessionID: "session-x"},
			wantScope: steering.ScopeOwnerUser,
			wantOK:    true,
		},
		{
			name:      "owner from a different session of their own still earns owner_user",
			caller:    identity.Identity{TenantID: "tenant-a", UserID: "user-A", SessionID: "session-other"},
			wantScope: steering.ScopeOwnerUser,
			wantOK:    true,
		},
		{
			name:   "session-id collision (same tenant+session, DIFFERENT user) earns nothing",
			caller: identity.Identity{TenantID: "tenant-a", UserID: "user-B", SessionID: "session-x"},
			wantOK: false,
		},
		{
			name:   "cross-tenant non-admin earns nothing",
			caller: identity.Identity{TenantID: "tenant-b", UserID: "user-A", SessionID: "session-x"},
			wantOK: false,
		},
		{
			name:   "console:fleet alone (non-owner) earns no steering authority",
			caller: identity.Identity{TenantID: "tenant-a", UserID: "user-B", SessionID: "session-x"},
			scopes: []auth.Scope{auth.ScopeConsoleFleet},
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := scopeCtx(t, tc.caller, tc.scopes...)
			scope, ok := deriveSteeringScope(ctx, tc.caller, run)
			if ok != tc.wantOK {
				t.Fatalf("deriveSteeringScope ok = %v, want %v (scope=%q)", ok, tc.wantOK, scope)
			}
			if tc.wantOK && scope != tc.wantScope {
				t.Errorf("deriveSteeringScope scope = %q, want %q", scope, tc.wantScope)
			}
		})
	}
}

// TestDeriveSteeringScope_NeverMintsSessionUserTier guards the reserved
// status of the distinct session-scoped tier: no verified caller — owner,
// collision, cross-tenant, or scoped — is ever handed
// steering.ScopeSessionUser by the derivation. A future multi-participant
// session capability is the only thing that would change this, and it is
// not a V1 feature.
func TestDeriveSteeringScope_NeverMintsSessionUserTier(t *testing.T) {
	run := derivedRun()
	callers := []struct {
		id     identity.Identity
		scopes []auth.Scope
	}{
		{id: run.Identity}, // the owner / session participant
		{id: identity.Identity{TenantID: "tenant-a", UserID: "user-B", SessionID: "session-x"}},                                        // collision
		{id: identity.Identity{TenantID: "tenant-b", UserID: "user-A", SessionID: "session-x"}},                                        // cross-tenant
		{id: identity.Identity{TenantID: "tenant-a", UserID: "user-A", SessionID: "session-x"}, scopes: []auth.Scope{auth.ScopeAdmin}}, // admin
	}
	for _, c := range callers {
		ctx := scopeCtx(t, c.id, c.scopes...)
		if scope, _ := deriveSteeringScope(ctx, c.id, run); scope == steering.ScopeSessionUser {
			t.Errorf("deriveSteeringScope minted the reserved session_user tier for caller %+v", c.id)
		}
	}
}
