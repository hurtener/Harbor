package tasks_test

// Branch-tail coverage for the tasks searcher: the constructor guards,
// the index identity, the status/kind facets, the heavy arm, and the
// lister's own failure.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
	tasksearch "github.com/hurtener/Harbor/internal/search/tasks"
	tasksubsys "github.com/hurtener/Harbor/internal/tasks"
)

func TestTasksSearcher_New_RejectsMissingDeps(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()
	deps := search.Deps{Redactor: patterns.New(), AdminScope: func(context.Context) bool { return false }, Audit: testAudit}

	if _, err := tasksearch.New(nil, h.tasks, deps); !errors.Is(err, search.ErrInvalidRequest) {
		t.Errorf("nil lister: got %v, want ErrInvalidRequest", err)
	}
	if _, err := tasksearch.New(h.sessions, nil, deps); !errors.Is(err, search.ErrInvalidRequest) {
		t.Errorf("nil task registry: got %v, want ErrInvalidRequest", err)
	}
	if _, err := tasksearch.New(h.sessions, h.tasks, search.Deps{
		AdminScope: func(context.Context) bool { return false }, Audit: testAudit,
	}); !errors.Is(err, search.ErrInvalidRequest) {
		t.Errorf("nil redactor: got %v, want ErrInvalidRequest", err)
	}
	if _, err := tasksearch.New(h.sessions, h.tasks, search.Deps{Redactor: patterns.New(), Audit: testAudit}); !errors.Is(err, search.ErrInvalidRequest) {
		t.Errorf("nil AdminScope: got %v, want ErrInvalidRequest", err)
	}
	if _, err := tasksearch.New(h.sessions, h.tasks, search.Deps{
		Redactor: patterns.New(), AdminScope: func(context.Context) bool { return false },
	}); !errors.Is(err, search.ErrInvalidRequest) {
		t.Errorf("nil Audit: got %v, want ErrInvalidRequest", err)
	}
}

func TestTasksSearcher_Index_IsTheTasksIndex(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()
	if got := denyingSearcher(t, h).Index(); got != types.SearchIndexTasks {
		t.Errorf("Index() = %q, want %q", got, types.SearchIndexTasks)
	}
}

// TestTasksSearcher_FacetsNarrow covers both facet keys, inside the
// folded user scope.
func TestTasksSearcher_FacetsNarrow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)

	s := denyingSearcher(t, h)
	byKind, err := s.Search(attackerCtx(t), types.SearchRequest{
		Facets: []types.SearchFacet{{Key: "tasks.kind", Value: string(tasksubsys.KindForeground)}},
	})
	if err != nil {
		t.Fatalf("kind facet: %v", err)
	}
	for _, r := range byKind.Rows {
		if r.Facets["kind"] != string(tasksubsys.KindForeground) {
			t.Errorf("kind facet returned a %q row", r.Facets["kind"])
		}
		if r.UserID != attacker {
			t.Errorf("CROSS-USER LEAK through a facet: row %s user=%s", r.ID, r.UserID)
		}
	}

	// An unmatched status narrows to nothing rather than erroring.
	byStatus, err := s.Search(attackerCtx(t), types.SearchRequest{
		Facets: []types.SearchFacet{{Key: "tasks.status", Value: "no-such-status"}},
	})
	if err != nil {
		t.Fatalf("status facet: %v", err)
	}
	if len(byStatus.Rows) != 0 {
		t.Errorf("an unmatched status facet returned %d rows", len(byStatus.Rows))
	}
}

// TestTasksSearcher_HeavyPreviewShipsRefNotBytes covers the heavy arm.
func TestTasksSearcher_HeavyPreviewShipsRefNotBytes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()

	ident := identity.Identity{TenantID: oneTenant, UserID: attacker, SessionID: attacker + "-sess"}
	openSession(t, h, ident)
	// The preview is built from the task's own fields, so a description
	// past the bound is the only way to reach the heavy arm.
	spawnTask(t, h, ident, strings.Repeat("d", search.HeavyPreviewThreshold+1))

	resp, err := denyingSearcher(t, h).Search(attackerCtx(t), types.SearchRequest{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(resp.Rows))
	}
	if resp.Rows[0].Preview != "" {
		t.Errorf("a preview past the bound must not ship inline: got %d bytes", len(resp.Rows[0].Preview))
	}
	if resp.Rows[0].Ref == nil {
		t.Fatal("a heavy row must carry a Ref")
	}
}

// TestTasksSearcher_ListerFailurePropagates — a closed session registry
// is a loud failure, not an empty page.
func TestTasksSearcher_ListerFailurePropagates(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)

	s := denyingSearcher(t, h)
	if err := h.sessions.CloseRegistry(context.Background()); err != nil {
		t.Fatalf("CloseRegistry: %v", err)
	}
	if _, err := s.Search(attackerCtx(t), types.SearchRequest{}); err == nil {
		t.Fatal("a closed registry must not degrade to an empty page")
	}
}

func TestTasksSearcher_CancelledCtxStopsTheWalk(t *testing.T) {
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

func TestTasksSearcher_RedactorFailureRefusesTheRow(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	defer h.cleanup()
	seedTwoUsersOneTenant(t, h)

	s, err := tasksearch.New(h.sessions, h.tasks, search.Deps{
		Redactor:   failingRedactor{},
		AdminScope: func(context.Context) bool { return false },
		Audit:      testAudit,
	})
	if err != nil {
		t.Fatalf("tasksearch.New: %v", err)
	}
	if _, serr := s.Search(attackerCtx(t), types.SearchRequest{}); !errors.Is(serr, search.ErrRedactionFailed) {
		t.Fatalf("got %v, want ErrRedactionFailed", serr)
	}
}

type failingRedactor struct{}

func (failingRedactor) Redact(context.Context, any) (any, error) {
	return nil, errors.New("forced redaction failure")
}
