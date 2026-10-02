package conformancetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
)

// RunScopeFence verifies the atomic byte-store capability separately from the
// mandatory store contract. Unsupported stores must not run or advertise it.
func RunScopeFence(t *testing.T, open func() (artifacts.ArtifactStore, func())) {
	t.Helper()
	t.Run("LateWriterAndOrdinaryUploads", func(t *testing.T) {
		s, closeStore := open()
		defer closeStore()
		f, ok := s.(artifacts.ScopeFencer)
		if !ok {
			t.Fatal("missing scope fence capability")
		}
		scope := artifacts.ArtifactScope{TenantID: "fence-tenant", UserID: "owner", SessionID: "session"}
		ref, err := s.PutText(t.Context(), scope, "prior", artifacts.PutOpts{})
		if err != nil {
			t.Fatal(err)
		}
		// Pause the actual caller before entering the real driver. No mocked
		// transaction decides this race: the resumed real Put must see the
		// permanent tombstone, including through a different store instance.
		ready, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		go func() {
			close(ready)
			<-release
			_, e := s.PutText(t.Context(), scope, "late", artifacts.PutOpts{})
			done <- e
		}()
		<-ready
		if err = f.FenceScope(t.Context(), scope); err != nil {
			t.Fatal(err)
		}
		close(release)
		if err = <-done; !errors.Is(err, artifacts.ErrScopeFenced) {
			t.Fatalf("late put %v", err)
		}
		if _, found, err := s.Get(t.Context(), scope, ref.ID); err != nil || found {
			t.Fatalf("fenced content readable %v %v", found, err)
		}
		if _, found, err := s.GetRef(t.Context(), scope, ref.ID); err != nil || found {
			t.Fatalf("fenced ref readable %v %v", found, err)
		}
		if found, err := s.Exists(t.Context(), scope, ref.ID); err != nil || found {
			t.Fatalf("fenced existence %v %v", found, err)
		}
		refs, err := s.List(t.Context(), scope)
		if err != nil || len(refs) != 1 {
			t.Fatalf("cleanup cannot see prior blob %d %v", len(refs), err)
		}
		if _, err = s.Delete(t.Context(), scope, ref.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = s.PutText(t.Context(), scope, "prior", artifacts.PutOpts{}); !errors.Is(err, artifacts.ErrScopeFenced) {
			t.Fatalf("dedup replay resurrected %v", err)
		}
		other := scope
		other.SessionID = "sibling"
		if _, err = s.PutText(t.Context(), other, "unaffected", artifacts.PutOpts{}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("ConcurrentWritersAndFence", func(t *testing.T) {
		s, closeStore := open()
		defer closeStore()
		f, ok := s.(artifacts.ScopeFencer)
		if !ok {
			t.Fatal("missing scope fence capability")
		}
		var wg sync.WaitGroup
		for i := range 100 {
			scope := artifacts.ArtifactScope{TenantID: "concurrent-fence", UserID: "owner", SessionID: fmt.Sprint(i)}
			wg.Add(1)
			go func() {
				defer wg.Done()
				start := make(chan struct{})
				written := make(chan error, 1)
				go func() {
					<-start
					_, err := s.PutText(t.Context(), scope, "during", artifacts.PutOpts{})
					written <- err
				}()
				close(start)
				if err := f.FenceScope(t.Context(), scope); err != nil {
					t.Error(err)
				}
				err := <-written
				if err != nil && !errors.Is(err, artifacts.ErrScopeFenced) {
					t.Error(err)
				}
				if _, err = s.PutText(t.Context(), scope, "after", artifacts.PutOpts{}); !errors.Is(err, artifacts.ErrScopeFenced) {
					t.Errorf("post-fence write %v", err)
				}
				refs, err := s.List(t.Context(), scope)
				if err != nil {
					t.Error(err)
					return
				}
				for _, ref := range refs {
					if _, err = s.Delete(t.Context(), scope, ref.ID); err != nil {
						t.Error(err)
					}
				}
				refs, err = s.List(t.Context(), scope)
				if err != nil || len(refs) != 0 {
					t.Errorf("late resurrection %d %v", len(refs), err)
				}
			}()
		}
		wg.Wait()
	})
}

// AssertFenceSurvivesReopen exercises actual persistent close/reopen rather
// than inferring crash behavior from an in-memory receipt.
func AssertFenceSurvivesReopen(t *testing.T, open func() artifacts.ArtifactStore) {
	t.Helper()
	s := open()
	scope := artifacts.ArtifactScope{TenantID: "restart-fence", UserID: "owner", SessionID: "session"}
	f, ok := s.(artifacts.ScopeFencer)
	if !ok {
		t.Fatal("missing scope fence capability")
	}
	if err := f.FenceScope(t.Context(), scope); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	s = open()
	defer func() { _ = s.Close(context.Background()) }()
	if _, err := s.PutText(t.Context(), scope, "restart replay", artifacts.PutOpts{}); !errors.Is(err, artifacts.ErrScopeFenced) {
		t.Fatalf("reopen lost fence %v", err)
	}
}
