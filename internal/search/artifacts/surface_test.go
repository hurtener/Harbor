package artifacts_test

// Branch-tail coverage for the artifacts searcher: the constructor
// guards, the index identity, the facet arms, and the store's own
// failure.

import (
	"context"
	"errors"
	"testing"

	artifactsubsys "github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
	artifactsearch "github.com/hurtener/Harbor/internal/search/artifacts"
)

func TestArtifactsSearcher_New_RejectsMissingDeps(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	defer store.Close(context.Background())

	if _, err := artifactsearch.New(nil, search.Deps{
		Redactor: patterns.New(), AdminScope: func(context.Context) bool { return false }, Audit: testAudit,
	}); !errors.Is(err, search.ErrInvalidRequest) {
		t.Errorf("nil store: got %v, want ErrInvalidRequest", err)
	}
	if _, err := artifactsearch.New(store, search.Deps{
		AdminScope: func(context.Context) bool { return false }, Audit: testAudit,
	}); !errors.Is(err, search.ErrInvalidRequest) {
		t.Errorf("nil redactor: got %v, want ErrInvalidRequest", err)
	}
	if _, err := artifactsearch.New(store, search.Deps{Redactor: patterns.New(), Audit: testAudit}); !errors.Is(err, search.ErrInvalidRequest) {
		t.Errorf("nil AdminScope: got %v, want ErrInvalidRequest", err)
	}
	if _, err := artifactsearch.New(store, search.Deps{
		Redactor: patterns.New(), AdminScope: func(context.Context) bool { return false },
	}); !errors.Is(err, search.ErrInvalidRequest) {
		t.Errorf("nil Audit: got %v, want ErrInvalidRequest", err)
	}
}

func TestArtifactsSearcher_Index_IsTheArtifactsIndex(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	defer store.Close(context.Background())
	if got := denyingSearcher(t, store).Index(); got != types.SearchIndexArtifacts {
		t.Errorf("Index() = %q, want %q", got, types.SearchIndexArtifacts)
	}
}

// TestArtifactsSearcher_FacetsNarrow covers both facet arms plus the
// session narrowing, all inside the folded user scope.
func TestArtifactsSearcher_FacetsNarrow(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	defer store.Close(context.Background())

	scope := artifactsubsys.ArtifactScope{TenantID: oneTenant, UserID: attacker, SessionID: attacker + "-sess"}
	putArtifact(t, store, scope, "png", artifactsubsys.PutOpts{
		Filename: "chart.png", MimeType: "image/png", Namespace: "reports",
	})
	putArtifact(t, store, scope, "txt", artifactsubsys.PutOpts{
		Filename: "notes.txt", MimeType: "text/plain", Namespace: "scratch",
	})

	s := denyingSearcher(t, store)
	ctx := attackerCtx()

	byMime, err := s.Search(ctx, types.SearchRequest{
		Facets: []types.SearchFacet{{Key: "artifacts.mime", Value: "image/png"}},
	})
	if err != nil {
		t.Fatalf("mime facet: %v", err)
	}
	if len(byMime.Rows) != 1 || byMime.Rows[0].Facets["mime"] != "image/png" {
		t.Errorf("mime facet returned %v, want just the png", byMime.Rows)
	}

	byNS, err := s.Search(ctx, types.SearchRequest{
		Facets: []types.SearchFacet{{Key: "artifacts.namespace", Value: "scratch"}},
	})
	if err != nil {
		t.Fatalf("namespace facet: %v", err)
	}
	if len(byNS.Rows) != 1 || byNS.Rows[0].Facets["namespace"] != "scratch" {
		t.Errorf("namespace facet returned %v, want just the scratch row", byNS.Rows)
	}

	// A single session id narrows with no claim; one that is not the
	// caller's resolves to nothing under the folded user filter.
	bySession, err := s.Search(ctx, types.SearchRequest{
		Filter: types.SearchFilter{SessionIDs: []string{attacker + "-sess"}},
	})
	if err != nil {
		t.Fatalf("session narrowing: %v", err)
	}
	if len(bySession.Rows) != 2 {
		t.Errorf("own session narrowing returned %d rows, want 2", len(bySession.Rows))
	}
	elsewhere, err := s.Search(ctx, types.SearchRequest{
		Filter: types.SearchFilter{SessionIDs: []string{"not-mine"}},
	})
	if err != nil {
		t.Fatalf("foreign session: %v", err)
	}
	if len(elsewhere.Rows) != 0 {
		t.Errorf("a foreign session under the folded filter returned %v", elsewhere.Rows)
	}
}

// TestArtifactsSearcher_StoreFailurePropagates — a closed store is a loud
// failure, not an empty page.
func TestArtifactsSearcher_StoreFailurePropagates(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	seedTwoUsersOneTenant(t, store)
	s := denyingSearcher(t, store)
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("store.Close: %v", err)
	}
	if _, err := s.Search(attackerCtx(), types.SearchRequest{}); err == nil {
		t.Fatal("a closed store must not degrade to an empty page")
	}
}

func TestArtifactsSearcher_CancelledCtxStopsTheWalk(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	defer store.Close(context.Background())
	seedTwoUsersOneTenant(t, store)

	ctx, cancel := context.WithCancel(attackerCtx())
	cancel()
	if _, err := denyingSearcher(t, store).Search(ctx, types.SearchRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled ctx: got %v, want context.Canceled", err)
	}
}

// TestArtifactsSearcher_RedactorFailureRefusesTheRow — the row must never
// ship when redaction fails.
func TestArtifactsSearcher_RedactorFailureRefusesTheRow(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	defer store.Close(context.Background())
	seedTwoUsersOneTenant(t, store)

	s, err := artifactsearch.New(store, search.Deps{
		Redactor:   failingRedactor{},
		AdminScope: func(context.Context) bool { return false },
		Audit:      testAudit,
	})
	if err != nil {
		t.Fatalf("artifactsearch.New: %v", err)
	}
	if _, serr := s.Search(attackerCtx(), types.SearchRequest{}); !errors.Is(serr, search.ErrRedactionFailed) {
		t.Fatalf("got %v, want ErrRedactionFailed", serr)
	}
}

type failingRedactor struct{}

func (failingRedactor) Redact(context.Context, any) (any, error) {
	return nil, errors.New("forced redaction failure")
}
