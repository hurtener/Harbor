package events_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	eventsubsys "github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
	eventsearch "github.com/hurtener/Harbor/internal/search/events"
	"github.com/hurtener/Harbor/internal/server"
)

// Two users in ONE tenant. This index was the one that already defaulted
// `scopeUser` to the caller, so it never leaked on ELISION — but it
// overwrote that default from the raw filter and still handed the bus
// `Admin: false`, so a named foreign user was honoured as an ordinary
// scoped read.
const (
	oneTenant = "t1"
	attacker  = "attacker"
	victim    = "victim"
)

func denyingSearcher(t *testing.T, h *busHarness) *eventsearch.Searcher {
	t.Helper()
	s, err := eventsearch.New(h.bus.(eventsubsys.Replayer), search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
		Audit:      testAudit,
	})
	if err != nil {
		t.Fatalf("eventsearch.New: %v", err)
	}
	return s
}

func claimedSearcher(t *testing.T, h *busHarness) *eventsearch.Searcher {
	t.Helper()
	s, err := eventsearch.New(h.bus.(eventsubsys.Replayer), search.Deps{
		Redactor:   patterns.New(),
		AdminScope: server.SearchAdminScopeFromAuth,
		Audit:      testAudit,
	})
	if err != nil {
		t.Fatalf("eventsearch.New: %v", err)
	}
	return s
}

func seedTwoUsersOneTenant(t *testing.T, h *busHarness) {
	t.Helper()
	for _, u := range []string{attacker, victim} {
		publishRuntimeError(t, h.bus, identity.Quadruple{Identity: identity.Identity{
			TenantID: oneTenant, UserID: u, SessionID: u + "-sess",
		}})
	}
}

func attackerCtx() context.Context {
	ctx, _ := identity.With(context.Background(), identity.Identity{
		TenantID: oneTenant, UserID: attacker, SessionID: attacker + "-sess",
	})
	return ctx
}

// TestEventsSearcher_ElidedUserFoldsToCaller — this index was already
// SAFE on elision; the arm pins that as a decision rather than leaving it
// a coincidence of the default assignment.
func TestEventsSearcher_ElidedUserFoldsToCaller(t *testing.T) {
	t.Parallel()
	h := newBusHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)

	resp, err := denyingSearcher(t, h).Search(attackerCtx(), types.SearchRequest{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Rows) == 0 {
		t.Fatal("the fold returned nothing — a fix that empties a working surface is not a fix")
	}
	for _, r := range resp.Rows {
		if r.UserID != attacker {
			t.Errorf("CROSS-USER LEAK: event row %s belongs to %q", r.ID, r.UserID)
		}
	}
}

// TestEventsSearcher_NamedForeignUserRefused — this index's actual leak
// shape: the named value overwrote the caller default while the bus
// filter still said `Admin: false`, so the foreign user was read as if it
// were the caller's own scope.
func TestEventsSearcher_NamedForeignUserRefused(t *testing.T) {
	t.Parallel()
	h := newBusHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)

	_, err := denyingSearcher(t, h).Search(attackerCtx(), types.SearchRequest{
		Filter: types.SearchFilter{
			UserIDs:    []string{victim},
			SessionIDs: []string{victim + "-sess"},
		},
	})
	if !errors.Is(err, search.ErrCrossUserRequiresAdmin) {
		t.Fatalf("named foreign user: got %v, want ErrCrossUserRequiresAdmin", err)
	}
}

func TestEventsSearcher_OwnUserNamedNeedsNoClaim(t *testing.T) {
	t.Parallel()
	h := newBusHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)

	s := denyingSearcher(t, h)
	elided, err := s.Search(attackerCtx(), types.SearchRequest{})
	if err != nil {
		t.Fatalf("elided: %v", err)
	}
	named, err := s.Search(attackerCtx(), types.SearchRequest{
		Filter: types.SearchFilter{UserIDs: []string{attacker}},
	})
	if err != nil {
		t.Fatalf("own user named: %v", err)
	}
	if len(named.Rows) != len(elided.Rows) || len(named.Rows) == 0 {
		t.Fatalf("own-user-named must equal elided: named=%d elided=%d", len(named.Rows), len(elided.Rows))
	}
}

func TestEventsSearcher_MultiUserFanInRefused(t *testing.T) {
	t.Parallel()
	h := newBusHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)

	s := denyingSearcher(t, h)
	for _, users := range [][]string{{attacker, victim}, {attacker, attacker}} {
		_, err := s.Search(attackerCtx(), types.SearchRequest{
			Filter: types.SearchFilter{UserIDs: users},
		})
		if !errors.Is(err, search.ErrCrossUserRequiresAdmin) {
			t.Errorf("multi-user %v: got %v, want ErrCrossUserRequiresAdmin", users, err)
		}
	}
}

// TestEventsSearcher_AdminClaimReopensBothWidenings — under EACH claim a
// named foreign user reads that user's rows.
//
// The elided arm is deliberately DIFFERENT on this index and asserts the
// fold rather than a tenant-wide fan: the replay filter is single-valued
// and its fan-in flag WRITES an admin-scope notice into the ring, so an
// elided axis is not turned into an unrequested deployment-wide replay
// here. That is this index's pre-existing scope, preserved.
func TestEventsSearcher_AdminClaimReopensBothWidenings(t *testing.T) {
	t.Parallel()
	for _, scope := range []protocolauth.Scope{protocolauth.ScopeAdmin, protocolauth.ScopeConsoleFleet} {
		t.Run(string(scope), func(t *testing.T) {
			t.Parallel()
			h := newBusHarness(t)
			defer h.cleanup()
			seedTwoUsersOneTenant(t, h)

			s := claimedSearcher(t, h)
			ctx := protocolauth.WithScopes(attackerCtx(), []protocolauth.Scope{scope})

			named, err := s.Search(ctx, types.SearchRequest{
				Filter: types.SearchFilter{UserIDs: []string{victim}},
			})
			if err != nil {
				t.Fatalf("named foreign user under %s: %v", scope, err)
			}
			if len(named.Rows) == 0 {
				t.Fatalf("named foreign user under %s returned nothing — the widening is inert", scope)
			}
			for _, r := range named.Rows {
				if r.UserID != victim {
					t.Errorf("under %s a read widened to %q returned a row owned by %q", scope, victim, r.UserID)
				}
			}

			elided, err := s.Search(ctx, types.SearchRequest{})
			if err != nil {
				t.Fatalf("elided under %s: %v", scope, err)
			}
			for _, r := range elided.Rows {
				if r.UserID != attacker {
					t.Errorf("under %s an elided axis must keep this index's own-user scope, got %q", scope, r.UserID)
				}
			}
		})
	}
}

// TestEventsSearcher_WidenedReadSetsBusAdmin — without the flag the bus
// matches the widened read against the single scoped user and answers
// NOTHING, which looks exactly like a working gate while the granted
// widening is silently inert. Asserted through the observable rows,
// because the filter itself is internal to Search.
func TestEventsSearcher_WidenedReadSetsBusAdmin(t *testing.T) {
	t.Parallel()
	h := newBusHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)

	ctx := protocolauth.WithScopes(attackerCtx(), []protocolauth.Scope{protocolauth.ScopeAdmin})
	resp, err := claimedSearcher(t, h).Search(ctx, types.SearchRequest{
		Filter: types.SearchFilter{UserIDs: []string{victim}},
	})
	if err != nil {
		t.Fatalf("widened read: %v", err)
	}
	seeded := seededRows(resp.Rows)
	if len(seeded) != 1 || seeded[0].UserID != victim {
		t.Fatalf("widened read: got %d seeded rows %v, want exactly the victim's one row", len(seeded), seeded)
	}
}

func TestEventsSearcher_WideningEmitsExactlyOnceViaReplay(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		req  types.SearchRequest
	}{
		{name: "tenant axis", req: types.SearchRequest{Filter: types.SearchFilter{TenantIDs: []string{"tenant-target"}}}},
		{name: "user axis", req: types.SearchRequest{Filter: types.SearchFilter{UserIDs: []string{victim}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newBusHarness(t)
			defer h.cleanup()
			seedTwoUsersOneTenant(t, h)
			if tc.name == "tenant axis" {
				publishRuntimeError(t, h.bus, identity.Quadruple{Identity: identity.Identity{
					TenantID: "tenant-target", UserID: "target-user", SessionID: "target-session",
				}})
			}

			var sinkCalls atomic.Int64
			s, err := eventsearch.New(h.bus.(eventsubsys.Replayer), search.Deps{
				Redactor:   patterns.New(),
				AdminScope: func(context.Context) bool { return true },
				Audit: func(ctx context.Context, ev eventsubsys.Event) error {
					sinkCalls.Add(1)
					return h.bus.Publish(ctx, ev)
				},
			})
			if err != nil {
				t.Fatalf("eventsearch.New: %v", err)
			}
			resp, err := s.Search(attackerCtx(), tc.req)
			if err != nil {
				t.Fatalf("Search: %v", err)
			}
			if got := sinkCalls.Load(); got != 0 {
				t.Errorf("search audit sink calls = %d, want 0; Replay owns this index's notice", got)
			}
			notices := 0
			for _, row := range resp.Rows {
				if row.Facets["type"] == string(eventsubsys.EventTypeAdminScopeUsed) {
					notices++
				}
			}
			if notices != 1 {
				t.Fatalf("audit.admin_scope_used rows = %d, want exactly 1; rows=%+v", notices, resp.Rows)
			}
		})
	}
}

// seededRows drops the bus's own `audit.admin_scope_used` notice from a
// widened result. The bus WRITES that notice into the replay ring on every
// admin-flagged replay — which is where the accountability for a granted
// crossing on this index comes from — so it legitimately appears in the
// rows a widened search returns. The assertions below are about the
// SEEDED events, so the notice is filtered rather than asserted away.
func seededRows(rows []types.SearchResultRow) []types.SearchResultRow {
	out := make([]types.SearchResultRow, 0, len(rows))
	for _, r := range rows {
		if r.Facets["type"] == string(eventsubsys.EventTypeRuntimeError) {
			out = append(out, r)
		}
	}
	return out
}

// TestEventsSearcher_WidenedReadDoesNotAlsoCrossTheOtherAxis — the bus's
// per-event match short-circuits its WHOLE identity comparison when the
// fan-in flag is set, so a read widened on the USER axis would otherwise
// also return other TENANTS' rows. The searcher re-applies both axis
// bounds per row.
func TestEventsSearcher_WidenedReadDoesNotAlsoCrossTheOtherAxis(t *testing.T) {
	t.Parallel()
	h := newBusHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)
	publishRuntimeError(t, h.bus, identity.Quadruple{Identity: identity.Identity{
		TenantID: "t-other", UserID: victim, SessionID: "elsewhere",
	}})

	ctx := protocolauth.WithScopes(attackerCtx(), []protocolauth.Scope{protocolauth.ScopeAdmin})
	resp, err := claimedSearcher(t, h).Search(ctx, types.SearchRequest{
		Filter: types.SearchFilter{UserIDs: []string{victim}},
	})
	if err != nil {
		t.Fatalf("widened read: %v", err)
	}
	for _, r := range resp.Rows {
		if r.TenantID != oneTenant {
			t.Errorf("a user-axis widening also crossed the tenant axis: row %s tenant=%s", r.ID, r.TenantID)
		}
	}
	if seeded := seededRows(resp.Rows); len(seeded) != 1 {
		t.Fatalf("got %d seeded rows, want exactly the victim's row inside the caller's tenant", len(seeded))
	}
}

// TestEventsSearcher_CrossTenantReadWithElidedUserDoesNotFoldToTheCaller —
// the combination the shipped nine cases never made: an admin claim, a
// FOREIGN tenant, and an ELIDED user axis.
//
// Folding there is not a narrower answer, it is a wrong one. The caller's
// own user id names a principal of the CALLER's tenant, so bounding a
// foreign tenant's rows by it matches nothing and a granted, explicitly
// requested crossing answers an empty page — indistinguishable from "that
// tenant has no events", which is the silent-empty failure this surface's
// identity bound exists to prevent.
//
// The same-tenant elided arm keeps its fold; that is asserted separately by
// TestEventsSearcher_AdminClaimReopensBothWidenings, so this arm cannot be
// satisfied by simply deleting the fold.
func TestEventsSearcher_CrossTenantReadWithElidedUserDoesNotFoldToTheCaller(t *testing.T) {
	t.Parallel()
	const foreignTenant = "t-foreign"
	for _, scope := range []protocolauth.Scope{protocolauth.ScopeAdmin, protocolauth.ScopeConsoleFleet} {
		t.Run(string(scope), func(t *testing.T) {
			t.Parallel()
			h := newBusHarness(t)
			defer h.cleanup()
			seedTwoUsersOneTenant(t, h)
			// Two DIFFERENT users in the foreign tenant, and NEITHER is the
			// caller's own user id. A fold to the caller therefore returns
			// zero foreign rows; a correct widening returns both.
			for _, u := range []string{"foreign-one", "foreign-two"} {
				publishRuntimeError(t, h.bus, identity.Quadruple{Identity: identity.Identity{
					TenantID: foreignTenant, UserID: u, SessionID: u + "-sess",
				}})
			}

			ctx := protocolauth.WithScopes(attackerCtx(), []protocolauth.Scope{scope})
			resp, err := claimedSearcher(t, h).Search(ctx, types.SearchRequest{
				Filter: types.SearchFilter{TenantIDs: []string{foreignTenant}},
			})
			if err != nil {
				t.Fatalf("cross-tenant read under %s: %v", scope, err)
			}
			seeded := seededRows(resp.Rows)
			if len(seeded) == 0 {
				t.Fatalf("under %s a granted cross-tenant read with an elided user axis returned "+
					"NO rows — the user axis folded to the caller's own id, which is a principal "+
					"of a different tenant, so the granted crossing answers an empty page "+
					"indistinguishable from an empty tenant", scope)
			}
			got := map[string]bool{}
			for _, r := range seeded {
				if r.TenantID != foreignTenant {
					t.Errorf("under %s a tenant-axis widening returned a row from %q, want only %q",
						scope, r.TenantID, foreignTenant)
				}
				got[r.UserID] = true
			}
			for _, want := range []string{"foreign-one", "foreign-two"} {
				if !got[want] {
					t.Errorf("under %s the widened read is missing %q's rows (saw %v) — the "+
						"elided axis fanned to fewer users than the tenant holds", scope, want, got)
				}
			}
		})
	}
}

// TestEventsSearcher_CrossTenantElidedUserStillHoldsTheTenantBound — the
// widening above releases the USER bound only. The tenant bound is what
// still contains the read, and the bus's fan-in flag short-circuits its
// whole identity comparison, so an unheld tenant bound would return the
// entire ring.
func TestEventsSearcher_CrossTenantElidedUserStillHoldsTheTenantBound(t *testing.T) {
	t.Parallel()
	const wanted, unwanted = "t-wanted", "t-unwanted"
	h := newBusHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)
	publishRuntimeError(t, h.bus, identity.Quadruple{Identity: identity.Identity{
		TenantID: wanted, UserID: "w-user", SessionID: "w-sess",
	}})
	publishRuntimeError(t, h.bus, identity.Quadruple{Identity: identity.Identity{
		TenantID: unwanted, UserID: "x-user", SessionID: "x-sess",
	}})

	ctx := protocolauth.WithScopes(attackerCtx(), []protocolauth.Scope{protocolauth.ScopeAdmin})
	resp, err := claimedSearcher(t, h).Search(ctx, types.SearchRequest{
		Filter: types.SearchFilter{TenantIDs: []string{wanted}},
	})
	if err != nil {
		t.Fatalf("cross-tenant read: %v", err)
	}
	seeded := seededRows(resp.Rows)
	if len(seeded) != 1 || seeded[0].TenantID != wanted {
		t.Fatalf("got %d seeded rows %v, want exactly the one row in %q — the released user "+
			"bound must not release the tenant bound with it", len(seeded), seeded, wanted)
	}
}

func TestEventsSearcher_MultiSessionFanInRefused(t *testing.T) {
	t.Parallel()
	h := newBusHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)

	_, err := denyingSearcher(t, h).Search(attackerCtx(), types.SearchRequest{
		Filter: types.SearchFilter{SessionIDs: []string{"a", "b"}},
	})
	if !errors.Is(err, search.ErrCrossSessionRequiresAdmin) {
		t.Fatalf("multi-session fan-in: got %v, want ErrCrossSessionRequiresAdmin", err)
	}
}
