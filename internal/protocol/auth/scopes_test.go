package auth_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/auth"
)

func TestScopes_IsValidScope(t *testing.T) {
	cases := []struct {
		s    auth.Scope
		want bool
	}{
		{auth.ScopeAdmin, true},
		{auth.ScopeConsoleFleet, true},
		{auth.Scope("future:scope"), false},
		{auth.Scope(""), false},
	}
	for _, c := range cases {
		if got := auth.IsValidScope(c.s); got != c.want {
			t.Errorf("IsValidScope(%q): got %v, want %v", c.s, got, c.want)
		}
	}
}

func TestScopes_CanonicalScopes_ReturnsClosedSet(t *testing.T) {
	got := auth.CanonicalScopes()
	if len(got) != 2 {
		t.Fatalf("expected 2 canonical scopes, got %d (%v)", len(got), got)
	}
	seen := map[auth.Scope]bool{}
	for _, s := range got {
		seen[s] = true
	}
	if !seen[auth.ScopeAdmin] || !seen[auth.ScopeConsoleFleet] {
		t.Errorf("missing canonical scope: %v", seen)
	}
}

func TestScopes_WithScopes_RoundTrip(t *testing.T) {
	ctx := context.Background()
	ctx = auth.WithScopes(ctx, []auth.Scope{auth.ScopeAdmin})
	scopes, ok := auth.ScopesFrom(ctx)
	if !ok {
		t.Fatalf("ScopesFrom: presence false after WithScopes")
	}
	if len(scopes) != 1 || scopes[0] != auth.ScopeAdmin {
		t.Errorf("scopes: got %v", scopes)
	}
}

func TestScopes_WithScopes_FiltersUnknownScopes(t *testing.T) {
	ctx := context.Background()
	ctx = auth.WithScopes(ctx, []auth.Scope{
		auth.ScopeAdmin,
		auth.Scope("future:scope"), // unknown — must be dropped
		auth.ScopeConsoleFleet,
	})
	scopes, _ := auth.ScopesFrom(ctx)
	if len(scopes) != 2 {
		t.Fatalf("expected 2 valid scopes, got %d (%v)", len(scopes), scopes)
	}
}

func TestScopes_WithScopes_NilSlicePermitted(t *testing.T) {
	ctx := context.Background()
	ctx = auth.WithScopes(ctx, nil)
	scopes, ok := auth.ScopesFrom(ctx)
	if !ok {
		t.Fatalf("ScopesFrom: presence false for empty WithScopes")
	}
	if len(scopes) != 0 {
		t.Errorf("expected empty scope set, got %v", scopes)
	}
}

func TestScopes_HasScope_TrueOnPresentScope(t *testing.T) {
	ctx := context.Background()
	ctx = auth.WithScopes(ctx, []auth.Scope{auth.ScopeAdmin})
	if !auth.HasScope(ctx, auth.ScopeAdmin) {
		t.Errorf("HasScope(admin): false on present scope")
	}
	if auth.HasScope(ctx, auth.ScopeConsoleFleet) {
		t.Errorf("HasScope(console:fleet): true on absent scope")
	}
}

func TestScopes_HasScope_FalseOnContextWithoutMiddleware(t *testing.T) {
	// A request that has not been through the auth middleware has no
	// scope set on ctx — HasScope must return false (the safe default
	// for a privilege check is "absent = denied").
	if auth.HasScope(context.Background(), auth.ScopeAdmin) {
		t.Errorf("HasScope: true on bare context (expected false)")
	}
}

func TestScopes_ScopesFrom_BareContextReportsAbsent(t *testing.T) {
	scopes, ok := auth.ScopesFrom(context.Background())
	if ok {
		t.Errorf("ScopesFrom: presence true on bare context (got %v)", scopes)
	}
	if scopes != nil {
		t.Errorf("ScopesFrom: non-nil scopes on bare context (%v)", scopes)
	}
}

func TestScopes_ScopesFrom_DefensiveCopy(t *testing.T) {
	ctx := auth.WithScopes(context.Background(), []auth.Scope{auth.ScopeAdmin, auth.ScopeConsoleFleet})
	scopes, _ := auth.ScopesFrom(ctx)
	scopes[0] = auth.Scope("mutated")
	// A second read MUST still see the original — ScopesFrom returns a
	// defensive copy.
	scopes2, _ := auth.ScopesFrom(ctx)
	if scopes2[0] != auth.ScopeAdmin {
		t.Errorf("ScopesFrom returned a non-defensive copy: in-context value mutated to %q", scopes2[0])
	}
}
