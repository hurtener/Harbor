package sessions_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	eventsubsys "github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
	sessionsearch "github.com/hurtener/Harbor/internal/search/sessions"
	"github.com/hurtener/Harbor/internal/server"
)

// The two-users-in-ONE-tenant fixture. A cross-tenant test passes today
// against code that leaks across users, which is how this defect survived
// from the cluster's first phase — so every arm below stays inside one
// tenant.
const (
	oneTenant = "t1"
	attacker  = "attacker"
	victim    = "victim"
)

func denyingSearcher(t *testing.T, h *harness) *sessionsearch.Searcher {
	t.Helper()
	s, err := sessionsearch.New(h.registry, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
		Audit:      testAudit,
	})
	if err != nil {
		t.Fatalf("sessionsearch.New: %v", err)
	}
	return s
}

// claimedSearcher wires the PRODUCTION ScopeChecker, so the arm proves the
// widening rides the same closed claim set the tenant axis already rides —
// not a test-local always-true predicate.
func claimedSearcher(t *testing.T, h *harness) *sessionsearch.Searcher {
	t.Helper()
	s, err := sessionsearch.New(h.registry, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: server.SearchAdminScopeFromAuth,
		Audit:      testAudit,
	})
	if err != nil {
		t.Fatalf("sessionsearch.New: %v", err)
	}
	return s
}

func seedTwoUsersOneTenant(t *testing.T, h *harness) {
	t.Helper()
	openSession(t, h, identity.Identity{TenantID: oneTenant, UserID: attacker, SessionID: "attacker-sess"})
	openSession(t, h, identity.Identity{TenantID: oneTenant, UserID: victim, SessionID: "victim-sess"})
}

func attackerCtx(t *testing.T) context.Context {
	t.Helper()
	return callerCtx(t, identity.Identity{TenantID: oneTenant, UserID: attacker, SessionID: "attacker-sess"})
}

// TestSessionsSearcher_ElidedUserFoldsToCaller is the ELISION arm — the
// wider of the two defect shapes, because it fires on the DEFAULT request
// with no attacker input at all. Asserts the OWNING USER of every row, not
// a row count: a count passes for the wrong reason as soon as the fixture
// changes.
func TestSessionsSearcher_ElidedUserFoldsToCaller(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)

	resp, err := denyingSearcher(t, h).Search(attackerCtx(t), types.SearchRequest{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Rows) == 0 {
		t.Fatal("the fold returned nothing — a fix that empties a working surface is not a fix")
	}
	for _, r := range resp.Rows {
		if r.UserID != attacker {
			t.Errorf("CROSS-USER LEAK: row %s belongs to %q, caller is %q", r.ID, r.UserID, attacker)
		}
		if r.SessionID == "victim-sess" {
			t.Errorf("CROSS-USER LEAK: the victim's session id reached the caller (%s)", r.SessionID)
		}
	}
}

// TestSessionsSearcher_NamedForeignUserRefused — the NAMED arm. The
// refusal is loud; an empty page would be indistinguishable from "that
// user has no sessions" and would leak nothing while also telling the
// operator nothing.
func TestSessionsSearcher_NamedForeignUserRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)

	_, err := denyingSearcher(t, h).Search(attackerCtx(t), types.SearchRequest{
		Filter: types.SearchFilter{UserIDs: []string{victim}},
	})
	if !errors.Is(err, search.ErrCrossUserRequiresAdmin) {
		t.Fatalf("named foreign user: got %v, want ErrCrossUserRequiresAdmin", err)
	}
}

func TestSessionsSearcher_GrantedWideningsEmitCanonicalAuditBeforeRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		req  types.SearchRequest
	}{
		{name: "tenant axis", req: types.SearchRequest{Filter: types.SearchFilter{
			TenantIDs: []string{"tenant-target"}, UserIDs: []string{attacker},
		}}},
		{name: "user axis", req: types.SearchRequest{Filter: types.SearchFilter{UserIDs: []string{victim}}}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			defer h.cleanup()
			seedTwoUsersOneTenant(t, h)
			var got []eventsubsys.Event
			s, err := sessionsearch.New(h.registry, search.Deps{
				Redactor: patterns.New(), AdminScope: server.SearchAdminScopeFromAuth,
				Audit: func(_ context.Context, ev eventsubsys.Event) error {
					got = append(got, ev)
					return nil
				},
			})
			if err != nil {
				t.Fatalf("sessionsearch.New: %v", err)
			}
			ctx := protocolauth.WithScopes(attackerCtx(t), []protocolauth.Scope{protocolauth.ScopeAdmin})
			if _, err := s.Search(ctx, tc.req); err != nil {
				t.Fatalf("Search: %v", err)
			}
			if len(got) != 1 || got[0].Type != eventsubsys.EventTypeAdminScopeUsed {
				t.Fatalf("audit events = %+v, want one audit.admin_scope_used", got)
			}
		})
	}
}

// TestSessionsSearcher_OwnUserNamedNeedsNoClaim — naming yourself is
// indistinguishable from naming nobody.
func TestSessionsSearcher_OwnUserNamedNeedsNoClaim(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)

	s := denyingSearcher(t, h)
	ctx := attackerCtx(t)
	elided, err := s.Search(ctx, types.SearchRequest{})
	if err != nil {
		t.Fatalf("elided: %v", err)
	}
	named, err := s.Search(ctx, types.SearchRequest{
		Filter: types.SearchFilter{UserIDs: []string{attacker}},
	})
	if err != nil {
		t.Fatalf("own user named: %v", err)
	}
	if len(named.Rows) != len(elided.Rows) || len(named.Rows) == 0 {
		t.Fatalf("own-user-named must equal elided: named=%d elided=%d", len(named.Rows), len(elided.Rows))
	}
	for _, r := range named.Rows {
		if r.UserID != attacker {
			t.Errorf("row %s belongs to %q", r.ID, r.UserID)
		}
	}
}

// TestSessionsSearcher_MultiUserFanInRefused — the fan-in trigger fires
// even when every named user IS the caller: asking for many principals'
// rows in one read is the gate, not "is one of them foreign".
func TestSessionsSearcher_MultiUserFanInRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)

	s := denyingSearcher(t, h)
	for _, users := range [][]string{{attacker, victim}, {attacker, attacker}} {
		_, err := s.Search(attackerCtx(t), types.SearchRequest{
			Filter: types.SearchFilter{UserIDs: users},
		})
		if !errors.Is(err, search.ErrCrossUserRequiresAdmin) {
			t.Errorf("multi-user %v: got %v, want ErrCrossUserRequiresAdmin", users, err)
		}
	}
}

// TestSessionsSearcher_AdminClaimReopensBothWidenings — under EACH claim
// of the closed admin-tier set, a named foreign user reads that user and
// an ELIDED user fans across the tenant rather than folding.
func TestSessionsSearcher_AdminClaimReopensBothWidenings(t *testing.T) {
	t.Parallel()
	for _, scope := range []protocolauth.Scope{protocolauth.ScopeAdmin, protocolauth.ScopeConsoleFleet} {
		t.Run(string(scope), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			defer h.cleanup()
			seedTwoUsersOneTenant(t, h)

			s := claimedSearcher(t, h)
			ctx := protocolauth.WithScopes(attackerCtx(t), []protocolauth.Scope{scope})

			named, err := s.Search(ctx, types.SearchRequest{
				Filter: types.SearchFilter{UserIDs: []string{victim}},
			})
			if err != nil {
				t.Fatalf("named foreign user under %s: %v", scope, err)
			}
			if len(named.Rows) != 1 || named.Rows[0].UserID != victim {
				t.Fatalf("named foreign user under %s: got %d rows %v, want 1 owned by %s",
					scope, len(named.Rows), named.Rows, victim)
			}

			elided, err := s.Search(ctx, types.SearchRequest{})
			if err != nil {
				t.Fatalf("elided under %s: %v", scope, err)
			}
			if len(elided.Rows) != 2 {
				t.Fatalf("a widened read must NOT fold its elided user axis: got %d rows, want 2", len(elided.Rows))
			}
		})
	}
}

// TestSessionsSearcher_OwnOtherSessionNeedsNoClaim — the session axis
// stays a filter within one user. Breaking this would break the everyday
// "open one of my past sessions" flow for nothing: under the folded user
// filter a foreign session already resolves to no rows.
func TestSessionsSearcher_OwnOtherSessionNeedsNoClaim(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)
	openSession(t, h, identity.Identity{TenantID: oneTenant, UserID: attacker, SessionID: "attacker-older"})

	resp, err := denyingSearcher(t, h).Search(attackerCtx(t), types.SearchRequest{
		Filter: types.SearchFilter{SessionIDs: []string{"attacker-older"}},
	})
	if err != nil {
		t.Fatalf("own other session: %v", err)
	}
	if len(resp.Rows) != 1 || resp.Rows[0].ID != "attacker-older" {
		t.Fatalf("own other session: got %v, want [attacker-older]", resp.Rows)
	}

	// A FOREIGN session under the folded user filter resolves to nothing
	// rather than being refused — no claim is consulted on that axis.
	foreign, err := denyingSearcher(t, h).Search(attackerCtx(t), types.SearchRequest{
		Filter: types.SearchFilter{SessionIDs: []string{"victim-sess"}},
	})
	if err != nil {
		t.Fatalf("foreign session: %v", err)
	}
	if len(foreign.Rows) != 0 {
		t.Fatalf("foreign session under the folded user filter: got %v, want no rows", foreign.Rows)
	}
}

// TestSessionsSearcher_MultiSessionFanInRefused — a multi-value session
// set is a fan-in, elevated by the same rule that governs the tenant and
// user axes.
func TestSessionsSearcher_MultiSessionFanInRefused(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)
	openSession(t, h, identity.Identity{TenantID: oneTenant, UserID: attacker, SessionID: "attacker-older"})

	_, err := denyingSearcher(t, h).Search(attackerCtx(t), types.SearchRequest{
		Filter: types.SearchFilter{SessionIDs: []string{"attacker-sess", "attacker-older"}},
	})
	if !errors.Is(err, search.ErrCrossSessionRequiresAdmin) {
		t.Fatalf("multi-session fan-in: got %v, want ErrCrossSessionRequiresAdmin", err)
	}

	// Under the claim it passes.
	ctx := protocolauth.WithScopes(attackerCtx(t), []protocolauth.Scope{protocolauth.ScopeAdmin})
	resp, err := claimedSearcher(t, h).Search(ctx, types.SearchRequest{
		Filter: types.SearchFilter{SessionIDs: []string{"attacker-sess", "attacker-older"}},
	})
	if err != nil {
		t.Fatalf("multi-session fan-in under the claim: %v", err)
	}
	if len(resp.Rows) != 2 {
		t.Fatalf("multi-session fan-in under the claim: got %d rows, want 2", len(resp.Rows))
	}
}
