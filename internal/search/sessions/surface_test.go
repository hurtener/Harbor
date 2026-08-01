package sessions_test

// Branch-tail coverage for the sessions searcher: the constructor guards,
// the index identity, and the row-shape arms an isolation test never
// reaches (closed sessions, the heavy-preview bypass, a facet-narrowed
// page, and the lister's own failure).

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	eventsubsys "github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
	sessionsearch "github.com/hurtener/Harbor/internal/search/sessions"
	sessionsubsys "github.com/hurtener/Harbor/internal/sessions"
)

func TestSessionsSearcher_New_RejectsMissingDeps(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()

	if _, err := sessionsearch.New(nil, search.Deps{
		Redactor: patterns.New(), AdminScope: func(context.Context) bool { return false }, Audit: testAudit,
	}); !errors.Is(err, search.ErrInvalidRequest) {
		t.Errorf("nil lister: got %v, want ErrInvalidRequest", err)
	}
	if _, err := sessionsearch.New(h.registry, search.Deps{
		AdminScope: func(context.Context) bool { return false }, Audit: testAudit,
	}); !errors.Is(err, search.ErrInvalidRequest) {
		t.Errorf("nil redactor: got %v, want ErrInvalidRequest", err)
	}
	if _, err := sessionsearch.New(h.registry, search.Deps{Redactor: patterns.New(), Audit: testAudit}); !errors.Is(err, search.ErrInvalidRequest) {
		t.Errorf("nil AdminScope: got %v, want ErrInvalidRequest", err)
	}
	if _, err := sessionsearch.New(h.registry, search.Deps{
		Redactor: patterns.New(), AdminScope: func(context.Context) bool { return false },
	}); !errors.Is(err, search.ErrInvalidRequest) {
		t.Errorf("nil Audit: got %v, want ErrInvalidRequest", err)
	}
}

func TestSessionsSearcher_Index_IsTheSessionsIndex(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()
	if got := denyingSearcher(t, h).Index(); got != types.SearchIndexSessions {
		t.Errorf("Index() = %q, want %q", got, types.SearchIndexSessions)
	}
}

// TestSessionsSearcher_ClosedSessionRowShape covers the closed-session
// arms of the preview + status projection, and the `status` facet in both
// directions.
func TestSessionsSearcher_ClosedSessionRowShape(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()

	open := identity.Identity{TenantID: oneTenant, UserID: attacker, SessionID: "still-open"}
	closed := identity.Identity{TenantID: oneTenant, UserID: attacker, SessionID: "已-closed"}
	openSession(t, h, open)
	openSession(t, h, closed)
	ctx := callerCtx(t, closed)
	if err := h.registry.Close(ctx, closed.SessionID, "operator ended the conversation"); err != nil {
		t.Fatalf("registry.Close: %v", err)
	}

	s := denyingSearcher(t, h)
	caller := attackerCtx(t)

	all, err := s.Search(caller, types.SearchRequest{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	var sawClosed bool
	for _, r := range all.Rows {
		if r.ID != closed.SessionID {
			continue
		}
		sawClosed = true
		if r.Facets["status"] != "closed" {
			t.Errorf("closed row status facet = %q, want closed", r.Facets["status"])
		}
		if r.Facets["running"] != "true" && r.Facets["running"] != "false" {
			t.Errorf("running facet must be a bool string, got %q", r.Facets["running"])
		}
		if !strings.Contains(r.Preview, "closed") || !strings.Contains(r.Preview, "operator ended") {
			t.Errorf("closed preview must name the state and the reason: %q", r.Preview)
		}
	}
	if !sawClosed {
		t.Fatal("the closed session did not appear in an unfiltered search")
	}

	// The `open` facet drops it; the `closed` facet keeps only it.
	openOnly, err := s.Search(caller, types.SearchRequest{
		Facets: []types.SearchFacet{{Key: "sessions.status", Value: "open"}},
	})
	if err != nil {
		t.Fatalf("Search open: %v", err)
	}
	for _, r := range openOnly.Rows {
		if r.Facets["status"] != "open" {
			t.Errorf("the open facet returned a %q row", r.Facets["status"])
		}
	}
	closedOnly, err := s.Search(caller, types.SearchRequest{
		Facets: []types.SearchFacet{{Key: "sessions.status", Value: "closed"}},
	})
	if err != nil {
		t.Fatalf("Search closed: %v", err)
	}
	if len(closedOnly.Rows) != 1 || closedOnly.Rows[0].ID != closed.SessionID {
		t.Errorf("the closed facet returned %v, want just the closed session", closedOnly.Rows)
	}
}

// TestSessionsSearcher_HeavyPreviewShipsRefNotBytes covers the heavy arm:
// a preview at or past the bound ships an empty Preview plus a Ref, never
// inline bytes.
func TestSessionsSearcher_HeavyPreviewShipsRefNotBytes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()

	// The preview is built from the session's own fields, so an id past
	// the bound is the only way to reach the heavy arm.
	huge := strings.Repeat("s", search.HeavyPreviewThreshold+1)
	ident := identity.Identity{TenantID: oneTenant, UserID: attacker, SessionID: huge}
	openSession(t, h, ident)

	resp, err := denyingSearcher(t, h).Search(callerCtx(t, ident), types.SearchRequest{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(resp.Rows))
	}
	row := resp.Rows[0]
	if row.Preview != "" {
		t.Errorf("a preview past the bound must not ship inline: got %d bytes", len(row.Preview))
	}
	if row.Ref == nil {
		t.Fatal("a heavy row must carry a Ref")
	}
	if row.Ref.MimeType != "application/json" || row.Ref.SizeBytes == 0 {
		t.Errorf("heavy Ref shape: %+v", row.Ref)
	}
}

// TestSessionsSearcher_ListerFailurePropagates — a closed registry is a
// loud failure, not an empty page.
func TestSessionsSearcher_ListerFailurePropagates(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)

	s := denyingSearcher(t, h)
	if err := h.registry.CloseRegistry(context.Background()); err != nil {
		t.Fatalf("CloseRegistry: %v", err)
	}
	if _, err := s.Search(attackerCtx(t), types.SearchRequest{}); err == nil {
		t.Fatal("a closed registry must not degrade to an empty page")
	}
}

type calledLister struct{ called bool }

func (l *calledLister) ListSnapshots(context.Context, sessionsubsys.SessionListFilter) ([]sessionsubsys.SessionSnapshot, error) {
	l.called = true
	return nil, nil
}

func TestSessionsSearcher_AuditFailureStopsBeforeStorage(t *testing.T) {
	t.Parallel()
	lister := &calledLister{}
	sinkErr := errors.New("audit sink unavailable")
	s, err := sessionsearch.New(lister, search.Deps{
		Redactor: patterns.New(), AdminScope: func(context.Context) bool { return true },
		Audit: func(context.Context, eventsubsys.Event) error { return sinkErr },
	})
	if err != nil {
		t.Fatalf("sessionsearch.New: %v", err)
	}
	caller := identity.Identity{TenantID: "tenant-own", UserID: "user-own", SessionID: "session-own"}
	ctx := callerCtx(t, caller)
	_, err = s.Search(ctx, types.SearchRequest{Filter: types.SearchFilter{UserIDs: []string{"user-target"}}})
	if !errors.Is(err, search.ErrAuditFailed) || !errors.Is(err, sinkErr) {
		t.Fatalf("Search error = %v, want ErrAuditFailed wrapping sink error", err)
	}
	if lister.called {
		t.Fatal("SessionLister was called after mandatory audit emission failed")
	}
}

// TestSessionsSearcher_CancelledCtxStopsTheWalk — ctx.Err() is honoured
// between rows rather than after the whole scan.
func TestSessionsSearcher_CancelledCtxStopsTheWalk(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)

	ctx, cancel := context.WithCancel(attackerCtx(t))
	cancel()
	if _, err := denyingSearcher(t, h).Search(ctx, types.SearchRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx: got %v, want context.Canceled", err)
	}
}
