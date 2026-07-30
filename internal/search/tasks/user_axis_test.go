package tasks_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
	tasksearch "github.com/hurtener/Harbor/internal/search/tasks"
	"github.com/hurtener/Harbor/internal/server"
)

// Two users in ONE tenant — the fixture a cross-tenant-only test cannot
// distinguish from a correct one.
const (
	oneTenant = "t1"
	attacker  = "attacker"
	victim    = "victim"
)

func denyingSearcher(t *testing.T, h *harness) *tasksearch.Searcher {
	t.Helper()
	s, err := tasksearch.New(h.sessions, h.tasks, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("tasksearch.New: %v", err)
	}
	return s
}

// claimedSearcher wires the PRODUCTION ScopeChecker so the widening rides
// the same closed claim set the tenant axis already rides.
func claimedSearcher(t *testing.T, h *harness) *tasksearch.Searcher {
	t.Helper()
	s, err := tasksearch.New(h.sessions, h.tasks, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: server.SearchAdminScopeFromAuth,
	})
	if err != nil {
		t.Fatalf("tasksearch.New: %v", err)
	}
	return s
}

func seedTwoUsersOneTenant(t *testing.T, h *harness) {
	t.Helper()
	for _, u := range []string{attacker, victim} {
		ident := identity.Identity{TenantID: oneTenant, UserID: u, SessionID: u + "-sess"}
		openSession(t, h, ident)
		spawnTask(t, h, ident, "task owned by "+u)
	}
}

func attackerCtx(t *testing.T) context.Context {
	t.Helper()
	return callerCtx(t, identity.Identity{TenantID: oneTenant, UserID: attacker, SessionID: attacker + "-sess"})
}

// TestTasksSearcher_ElidedUserFoldsToCaller — the elision arm. This
// searcher walks EVERY session the lister returns and reads every task
// behind it, so an unfolded user axis discloses task descriptions and
// statuses, not just session ids.
func TestTasksSearcher_ElidedUserFoldsToCaller(t *testing.T) {
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
			t.Errorf("CROSS-USER LEAK: task row %s belongs to %q, caller is %q", r.ID, r.UserID, attacker)
		}
	}
}

func TestTasksSearcher_NamedForeignUserRefused(t *testing.T) {
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

func TestTasksSearcher_OwnUserNamedNeedsNoClaim(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)

	s := denyingSearcher(t, h)
	elided, err := s.Search(attackerCtx(t), types.SearchRequest{})
	if err != nil {
		t.Fatalf("elided: %v", err)
	}
	named, err := s.Search(attackerCtx(t), types.SearchRequest{
		Filter: types.SearchFilter{UserIDs: []string{attacker}},
	})
	if err != nil {
		t.Fatalf("own user named: %v", err)
	}
	if len(named.Rows) != len(elided.Rows) || len(named.Rows) == 0 {
		t.Fatalf("own-user-named must equal elided: named=%d elided=%d", len(named.Rows), len(elided.Rows))
	}
}

func TestTasksSearcher_MultiUserFanInRefused(t *testing.T) {
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

func TestTasksSearcher_AdminClaimReopensBothWidenings(t *testing.T) {
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
				t.Fatalf("named foreign user under %s: got %d rows, want 1 owned by %s",
					scope, len(named.Rows), victim)
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
