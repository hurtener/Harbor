package server

import (
	"context"
	"testing"

	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/search"
)

// TestSearchAdminScopeFromAuth_ClosedScopeSet pins the D-079 closed
// two-scope set `{admin, console:fleet}` byte-for-byte across the
// relocation out of `internal/search` (the D-203 addendum follow-up):
// either cross-tenant fan-in entitlement passes, anything else —
// including an unrelated scope and a bare ctx — is rejected.
func TestSearchAdminScopeFromAuth_ClosedScopeSet(t *testing.T) {
	cases := []struct {
		name string
		ctx  context.Context
		want bool
	}{
		{"admin passes", scopedCtx(protocolauth.ScopeAdmin), true},
		{"console:fleet passes", scopedCtx(protocolauth.ScopeConsoleFleet), true},
		{"both pass", scopedCtx(protocolauth.ScopeAdmin, protocolauth.ScopeConsoleFleet), true},
		{"other scope rejected", scopedCtx(protocolauth.Scope("events:read")), false},
		{"no scopes rejected", context.Background(), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := SearchAdminScopeFromAuth(tc.ctx); got != tc.want {
				t.Fatalf("SearchAdminScopeFromAuth: got %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSearchAdminScopeFromAuth_SatisfiesScopeChecker asserts the
// relocated predicate still satisfies the injected `search.ScopeChecker`
// seam — the shape `cmd/harbor` wires at the Phase 72c construction
// sites.
func TestSearchAdminScopeFromAuth_SatisfiesScopeChecker(t *testing.T) {
	var checker search.ScopeChecker = SearchAdminScopeFromAuth
	if !checker(scopedCtx(protocolauth.ScopeAdmin)) {
		t.Fatal("ScopeChecker(admin ctx): got false, want true")
	}
	if checker(context.Background()) {
		t.Fatal("ScopeChecker(bare ctx): got true, want false")
	}
}
