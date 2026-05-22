package artifacts_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	artifactsubsys "github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
	artifactsearch "github.com/hurtener/Harbor/internal/search/artifacts"
)

func newStore(t *testing.T) artifactsubsys.ArtifactStore {
	t.Helper()
	store, err := inmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts inmem: %v", err)
	}
	return store
}

func putArtifact(t *testing.T, store artifactsubsys.ArtifactStore, scope artifactsubsys.ArtifactScope, name string, opts artifactsubsys.PutOpts) artifactsubsys.ArtifactRef {
	t.Helper()
	ref, err := store.PutText(context.Background(), scope, "content for "+name, opts)
	if err != nil {
		t.Fatalf("PutText %s: %v", name, err)
	}
	return ref
}

func TestArtifactsSearcher_RejectsMissingIdentity(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	defer store.Close(context.Background())

	s, err := artifactsearch.New(store, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("artifactsearch.New: %v", err)
	}
	_, err = s.Search(context.Background(), types.SearchRequest{})
	if !errors.Is(err, search.ErrIdentityRequired) {
		t.Fatalf("got %v, want ErrIdentityRequired", err)
	}
}

func TestArtifactsSearcher_RejectsCrossTenantWithoutAdmin(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	defer store.Close(context.Background())

	s, err := artifactsearch.New(store, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("artifactsearch.New: %v", err)
	}
	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "t1", UserID: "u", SessionID: "s"})
	_, err = s.Search(ctx, types.SearchRequest{
		Filter: types.SearchFilter{TenantIDs: []string{"t1", "t2"}},
	})
	if !errors.Is(err, search.ErrCrossTenantRequiresAdmin) {
		t.Fatalf("got %v, want ErrCrossTenantRequiresAdmin", err)
	}
}

func TestArtifactsSearcher_ScopesToCallerTenantAndCarriesRef(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	defer store.Close(context.Background())

	// Two tenants, distinct files.
	putArtifact(t, store, artifactsubsys.ArtifactScope{
		TenantID: "t1", UserID: "u", SessionID: "s1",
	}, "report.pdf", artifactsubsys.PutOpts{Filename: "report.pdf", MimeType: "application/pdf"})
	putArtifact(t, store, artifactsubsys.ArtifactScope{
		TenantID: "t2", UserID: "u", SessionID: "s2",
	}, "secret.bin", artifactsubsys.PutOpts{Filename: "secret.bin", MimeType: "application/octet-stream"})

	s, err := artifactsearch.New(store, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("artifactsearch.New: %v", err)
	}
	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "t1", UserID: "u", SessionID: "s1"})
	resp, err := s.Search(ctx, types.SearchRequest{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	for _, r := range resp.Rows {
		if r.TenantID != "t1" {
			t.Errorf("CROSS-TENANT LEAK: tenant=%s, caller t1", r.TenantID)
		}
		if r.Ref == nil {
			t.Errorf("ArtifactRef must be populated for every artifact row, got nil for %s", r.ID)
		}
		if strings.Contains(strings.ToLower(r.Ref.Filename), "secret") {
			t.Errorf("CROSS-TENANT LEAK in Ref: %v", r.Ref)
		}
	}
}

func TestArtifactsSearcher_QueryMatch(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	defer store.Close(context.Background())

	scope := artifactsubsys.ArtifactScope{TenantID: "t1", UserID: "u", SessionID: "s"}
	putArtifact(t, store, scope, "deploy-log", artifactsubsys.PutOpts{Filename: "deploy.log", MimeType: "text/plain"})
	putArtifact(t, store, scope, "config", artifactsubsys.PutOpts{Filename: "config.yaml", MimeType: "text/yaml"})

	s, err := artifactsearch.New(store, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("artifactsearch.New: %v", err)
	}
	ctx, _ := identity.With(context.Background(), identity.Identity{TenantID: "t1", UserID: "u", SessionID: "s"})
	resp, err := s.Search(ctx, types.SearchRequest{Query: "deploy"})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(resp.Rows) != 1 {
		t.Errorf("Query 'deploy' rows: got %d, want 1", len(resp.Rows))
	}
}

func TestArtifactsSearcher_Concurrent_NoCrossTalk(t *testing.T) {
	const N = 100
	store := newStore(t)
	defer store.Close(context.Background())

	for i := range 10 {
		scope := artifactsubsys.ArtifactScope{
			TenantID:  fmt.Sprintf("t-%d", i),
			UserID:    "u",
			SessionID: fmt.Sprintf("s-%d", i),
		}
		putArtifact(t, store, scope, fmt.Sprintf("file-%d.txt", i), artifactsubsys.PutOpts{Filename: fmt.Sprintf("file-%d.txt", i)})
	}

	s, err := artifactsearch.New(store, search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return false },
	})
	if err != nil {
		t.Fatalf("artifactsearch.New: %v", err)
	}

	runtime.GC()
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	failures := make(chan string, N)
	for i := range N {

		wg.Add(1)
		go func() {
			defer wg.Done()
			tIdx := i % 10
			ident := identity.Identity{TenantID: fmt.Sprintf("t-%d", tIdx), UserID: "u", SessionID: fmt.Sprintf("s-%d", tIdx)}
			ctx, _ := identity.With(context.Background(), ident)
			resp, qerr := s.Search(ctx, types.SearchRequest{})
			if qerr != nil {
				failures <- fmt.Sprintf("g%d: %v", i, qerr)
				return
			}
			for _, r := range resp.Rows {
				if r.TenantID != ident.TenantID {
					failures <- fmt.Sprintf("g%d: LEAK tenant=%s caller=%s", i, r.TenantID, ident.TenantID)
				}
			}
		}()
	}
	wg.Wait()
	close(failures)
	var msgs []string
	for f := range failures {
		msgs = append(msgs, f)
	}
	if len(msgs) > 0 {
		t.Fatalf("concurrent-reuse failures: %v", msgs)
	}
	time.Sleep(50 * time.Millisecond)
	runtime.GC()
	if got := runtime.NumGoroutine(); got > baseline+5 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, got)
	}
}
