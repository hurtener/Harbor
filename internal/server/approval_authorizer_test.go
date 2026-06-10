package server

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/tools/approval"
)

// pendingFixture is the PendingInfo the matrix exercises.
var pendingFixture = approval.PendingInfo{
	Tool:     "demo",
	Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
}

// scopedCtx returns a ctx carrying the given verified Protocol scopes
// (the shape the Phase 61 middleware injects).
func scopedCtx(scopes ...protocolauth.Scope) context.Context {
	return protocolauth.WithScopes(context.Background(), scopes)
}

// TestProtocolScopeAuthorizer_PreservesScopeMatrix pins the pre-111f
// in-gate acceptance byte-for-byte: admin passes, console:fleet
// passes, any other scope set falls through (strict mode: rejected
// with ErrResolveForbidden). This is the wire-behaviour-unchanged
// gate for the D-203 layering move.
func TestProtocolScopeAuthorizer_PreservesScopeMatrix(t *testing.T) {
	strict := NewProtocolScopeAuthorizer(nil)

	cases := []struct {
		name string
		ctx  context.Context
		ok   bool
	}{
		{"admin passes", scopedCtx(protocolauth.ScopeAdmin), true},
		{"console:fleet passes", scopedCtx(protocolauth.ScopeConsoleFleet), true},
		{"both pass", scopedCtx(protocolauth.ScopeAdmin, protocolauth.ScopeConsoleFleet), true},
		{"other scope rejected", scopedCtx(protocolauth.Scope("events:read")), false},
		{"no scopes rejected", context.Background(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := strict.AuthorizeResolve(tc.ctx, pendingFixture)
			if tc.ok && err != nil {
				t.Fatalf("AuthorizeResolve: got %v, want nil", err)
			}
			if !tc.ok && !errors.Is(err, approval.ErrResolveForbidden) {
				t.Fatalf("AuthorizeResolve: got %v, want ErrResolveForbidden", err)
			}
		})
	}
}

// TestProtocolScopeAuthorizer_FallsThroughToNext asserts the
// production composition: a ctx with no Protocol scope delegates to
// Next (the runtime-vocabulary IdentityAuthorizer), so the in-process
// steering bridge resolves with the originating identity and a
// foreign unscoped caller is still rejected.
func TestProtocolScopeAuthorizer_FallsThroughToNext(t *testing.T) {
	a := NewProtocolScopeAuthorizer(approval.NewIdentityAuthorizer())

	ownCtx, err := identity.With(context.Background(), pendingFixture.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if err := a.AuthorizeResolve(ownCtx, pendingFixture); err != nil {
		t.Fatalf("originating identity via Next: got %v, want nil", err)
	}

	foreignCtx, err := identity.With(context.Background(), identity.Identity{
		TenantID: "t2", UserID: "u2", SessionID: "s2",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if err := a.AuthorizeResolve(foreignCtx, pendingFixture); !errors.Is(err, approval.ErrResolveForbidden) {
		t.Fatalf("foreign identity via Next: got %v, want ErrResolveForbidden", err)
	}

	// A Protocol-scoped caller never reaches Next.
	if err := a.AuthorizeResolve(scopedCtx(protocolauth.ScopeAdmin), pendingFixture); err != nil {
		t.Fatalf("admin scope with Next wired: got %v, want nil", err)
	}
}
