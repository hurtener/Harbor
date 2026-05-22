// Package conformancetest exposes the canonical correctness suite
// every artifacts.ArtifactStore driver must pass.
//
// The suite lives in a subpackage so the production-code path
// `internal/artifacts` does not import the standard library `testing`
// package (precedent: `internal/state/conformancetest`).
//
// Downstream drivers (Phase 18 SQLite-blob + Postgres-blob, Phase 19
// S3-style) consume it via:
//
//	import "github.com/hurtener/Harbor/internal/artifacts/conformancetest"
//
//	func TestMyDriver_Conformance(t *testing.T) {
//	    conformancetest.Run(t, func() (artifacts.ArtifactStore, func()) {
//	        s := mydriver.MustNew(t)
//	        return s, func() { _ = s.Close(context.Background()) }
//	    })
//	}
//
// The factory must return a fresh, empty ArtifactStore plus a
// cleanup callback. The suite uses the factory once per top-level
// subtest; invocations are independent.
package conformancetest

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
)

// Factory builds a fresh ArtifactStore and returns a cleanup closure.
type Factory func() (artifacts.ArtifactStore, func())

// Run executes the canonical correctness suite. Subtests:
//
//   - Put_Get_RoundTrip
//   - Put_DedupOnIdenticalBytes
//   - Put_DistinguishesByNamespace
//   - Put_DistinguishesByScope
//   - PutText_StoredAsBytes
//   - Get_NotFound
//   - GetRef_NotFound
//   - Delete_Idempotent
//   - List_FiltersByScope
//   - List_NilFieldsAreWildcards
//   - Put_Identity_Mandatory
//   - Get_CrossTenant_Isolation
//   - Delete_CrossTenant_Isolation
//   - Put_AfterClose_Errors
//   - Concurrent_PutGet_NoRace (D-025)
//   - Close_Idempotent
//   - GoroutineLeak_AfterClose
//   - Scoped_AutoStamps_Scope
//   - Scoped_PanicsOnInvalidScope
//   - Scoped_ImmutableScope
func Run(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("Put_Get_RoundTrip", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		ref, err := s.PutBytes(ctx, scopeA(), []byte("hello world"), artifacts.PutOpts{
			MimeType:  "text/plain",
			Filename:  "greeting.txt",
			Namespace: "tool.echo",
		})
		if err != nil {
			t.Fatalf("PutBytes: %v", err)
		}
		if ref.ID == "" {
			t.Fatalf("PutBytes returned empty ID")
		}
		if ref.Namespace != "tool.echo" {
			t.Errorf("ref.Namespace=%q, want %q", ref.Namespace, "tool.echo")
		}
		if ref.SizeBytes != int64(len("hello world")) {
			t.Errorf("ref.SizeBytes=%d, want %d", ref.SizeBytes, len("hello world"))
		}
		if !ref.Scope.Equal(scopeA()) {
			t.Errorf("ref.Scope=%+v, want %+v", ref.Scope, scopeA())
		}

		got, found, err := s.Get(ctx, scopeA(), ref.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if !found {
			t.Fatalf("Get: found=false after Put")
		}
		if string(got) != "hello world" {
			t.Errorf("Get bytes=%q, want %q", got, "hello world")
		}

		gotRef, found, err := s.GetRef(ctx, scopeA(), ref.ID)
		if err != nil {
			t.Fatalf("GetRef: %v", err)
		}
		if !found {
			t.Fatalf("GetRef: found=false after Put")
		}
		if gotRef.ID != ref.ID || gotRef.Namespace != ref.Namespace {
			t.Errorf("GetRef ref=%+v, want %+v", gotRef, ref)
		}

		exists, err := s.Exists(ctx, scopeA(), ref.ID)
		if err != nil {
			t.Fatalf("Exists: %v", err)
		}
		if !exists {
			t.Errorf("Exists=false after Put")
		}
	})

	t.Run("Put_DedupOnIdenticalBytes", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		opts := artifacts.PutOpts{Namespace: "ns.dedup"}
		ref1, err := s.PutBytes(ctx, scopeA(), []byte("dup-payload"), opts)
		if err != nil {
			t.Fatalf("PutBytes 1: %v", err)
		}
		ref2, err := s.PutBytes(ctx, scopeA(), []byte("dup-payload"), opts)
		if err != nil {
			t.Fatalf("PutBytes 2: %v", err)
		}
		if ref1.ID != ref2.ID {
			t.Errorf("dedup failed: ref1.ID=%q, ref2.ID=%q", ref1.ID, ref2.ID)
		}
		if ref1.SHA256 != ref2.SHA256 {
			t.Errorf("dedup SHA mismatch: %q vs %q", ref1.SHA256, ref2.SHA256)
		}
		got, err := s.List(ctx, scopeA())
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(got) != 1 {
			t.Errorf("List len=%d, want 1 (dedup should not produce duplicates)", len(got))
		}
	})

	t.Run("Put_DistinguishesByNamespace", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		bytes := []byte("ns-distinct")
		ref1, err := s.PutBytes(ctx, scopeA(), bytes, artifacts.PutOpts{Namespace: "ns.alpha"})
		if err != nil {
			t.Fatal(err)
		}
		ref2, err := s.PutBytes(ctx, scopeA(), bytes, artifacts.PutOpts{Namespace: "ns.beta"})
		if err != nil {
			t.Fatal(err)
		}
		if ref1.ID == ref2.ID {
			t.Errorf("different namespaces produced same ID: %q", ref1.ID)
		}
		// SHA256 itself is the same — ID embeds the namespace prefix.
		if ref1.SHA256 != ref2.SHA256 {
			t.Errorf("identical bytes should share SHA: %q vs %q", ref1.SHA256, ref2.SHA256)
		}
	})

	t.Run("Put_DistinguishesByScope", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		bytes := []byte("scope-distinct")
		opts := artifacts.PutOpts{Namespace: "ns"}
		_, err := s.PutBytes(ctx, scopeA(), bytes, opts)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.PutBytes(ctx, scopeB(), bytes, opts)
		if err != nil {
			t.Fatal(err)
		}
		listA, err := s.List(ctx, scopeA())
		if err != nil {
			t.Fatal(err)
		}
		listB, err := s.List(ctx, scopeB())
		if err != nil {
			t.Fatal(err)
		}
		if len(listA) != 1 || len(listB) != 1 {
			t.Errorf("cross-scope dedup leaked: listA=%d, listB=%d", len(listA), len(listB))
		}
		if !listA[0].Scope.Equal(scopeA()) {
			t.Errorf("listA scope wrong: %+v", listA[0].Scope)
		}
		if !listB[0].Scope.Equal(scopeB()) {
			t.Errorf("listB scope wrong: %+v", listB[0].Scope)
		}
	})

	t.Run("PutText_StoredAsBytes", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		ref, err := s.PutText(ctx, scopeA(), "lorem ipsum", artifacts.PutOpts{Namespace: "ns.text"})
		if err != nil {
			t.Fatal(err)
		}
		got, found, err := s.Get(ctx, scopeA(), ref.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Fatal("Get found=false after PutText")
		}
		if string(got) != "lorem ipsum" {
			t.Errorf("Get bytes=%q, want %q", got, "lorem ipsum")
		}
	})

	t.Run("Get_NotFound", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		got, found, err := s.Get(ctx, scopeA(), "ns_deadbeef0000")
		if err != nil {
			t.Errorf("Get on absent: err=%v, want nil", err)
		}
		if found {
			t.Errorf("Get on absent: found=true, want false")
		}
		if got != nil {
			t.Errorf("Get on absent: bytes=%q, want nil", got)
		}
		exists, err := s.Exists(ctx, scopeA(), "ns_deadbeef0000")
		if err != nil {
			t.Errorf("Exists on absent: err=%v", err)
		}
		if exists {
			t.Errorf("Exists on absent: true, want false")
		}
	})

	t.Run("GetRef_NotFound", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		ref, found, err := s.GetRef(ctx, scopeA(), "ns_deadbeef0000")
		if err != nil {
			t.Errorf("GetRef on absent: err=%v, want nil", err)
		}
		if found {
			t.Errorf("GetRef on absent: found=true, want false")
		}
		if ref != nil {
			t.Errorf("GetRef on absent: ref=%+v, want nil", ref)
		}
	})

	t.Run("Delete_Idempotent", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		// Delete on absent.
		existed, err := s.Delete(ctx, scopeA(), "ns_deadbeef0000")
		if err != nil {
			t.Errorf("Delete absent: err=%v", err)
		}
		if existed {
			t.Errorf("Delete absent: existed=true, want false")
		}

		// Put then delete.
		ref, err := s.PutBytes(ctx, scopeA(), []byte("for-delete"), artifacts.PutOpts{Namespace: "ns.del"})
		if err != nil {
			t.Fatal(err)
		}
		existed, err = s.Delete(ctx, scopeA(), ref.ID)
		if err != nil {
			t.Errorf("Delete present: err=%v", err)
		}
		if !existed {
			t.Errorf("Delete present: existed=false, want true")
		}

		// Subsequent Get returns (nil, false, nil).
		got, found, err := s.Get(ctx, scopeA(), ref.ID)
		if err != nil {
			t.Errorf("Get after Delete: err=%v", err)
		}
		if found {
			t.Errorf("Get after Delete: found=true, want false")
		}
		if got != nil {
			t.Errorf("Get after Delete: bytes=%q, want nil", got)
		}

		// Second Delete is also idempotent.
		existed, err = s.Delete(ctx, scopeA(), ref.ID)
		if err != nil {
			t.Errorf("Delete second time: err=%v", err)
		}
		if existed {
			t.Errorf("Delete second time: existed=true, want false")
		}
	})

	t.Run("List_FiltersByScope", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		// Three artifacts: 2 in scopeA, 1 in scopeB.
		_, err := s.PutBytes(ctx, scopeA(), []byte("a1"), artifacts.PutOpts{Namespace: "ns"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.PutBytes(ctx, scopeA(), []byte("a2"), artifacts.PutOpts{Namespace: "ns"})
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.PutBytes(ctx, scopeB(), []byte("b1"), artifacts.PutOpts{Namespace: "ns"})
		if err != nil {
			t.Fatal(err)
		}
		listA, err := s.List(ctx, scopeA())
		if err != nil {
			t.Fatal(err)
		}
		if len(listA) != 2 {
			t.Errorf("scopeA list len=%d, want 2", len(listA))
		}
		for _, r := range listA {
			if !r.Scope.Equal(scopeA()) {
				t.Errorf("scopeA list leaked: %+v", r.Scope)
			}
		}
		listB, err := s.List(ctx, scopeB())
		if err != nil {
			t.Fatal(err)
		}
		if len(listB) != 1 {
			t.Errorf("scopeB list len=%d, want 1", len(listB))
		}
		if !listB[0].Scope.Equal(scopeB()) {
			t.Errorf("scopeB list leaked: %+v", listB[0].Scope)
		}
	})

	t.Run("List_NilFieldsAreWildcards", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()

		// Same tenant, different users/sessions/tasks.
		s1 := artifacts.ArtifactScope{TenantID: "T", UserID: "U1", SessionID: "S1", TaskID: "K1"}
		s2 := artifacts.ArtifactScope{TenantID: "T", UserID: "U2", SessionID: "S2", TaskID: ""}
		s3 := artifacts.ArtifactScope{TenantID: "T2", UserID: "U", SessionID: "S", TaskID: "K"}
		opts := artifacts.PutOpts{Namespace: "ns"}
		_, err := s.PutBytes(ctx, s1, []byte("p1"), opts)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.PutBytes(ctx, s2, []byte("p2"), opts)
		if err != nil {
			t.Fatal(err)
		}
		_, err = s.PutBytes(ctx, s3, []byte("p3"), opts)
		if err != nil {
			t.Fatal(err)
		}

		// Empty user/session/task → all under tenant T.
		listT := mustList(t, s, ctx, artifacts.ArtifactScope{TenantID: "T"})
		if len(listT) != 2 {
			t.Errorf("wildcard tenant T list len=%d, want 2", len(listT))
		}

		// Empty everything → all 3.
		listAll := mustList(t, s, ctx, artifacts.ArtifactScope{})
		if len(listAll) != 3 {
			t.Errorf("full wildcard list len=%d, want 3", len(listAll))
		}

		// Tenant + user — narrows further.
		listTU := mustList(t, s, ctx, artifacts.ArtifactScope{TenantID: "T", UserID: "U1"})
		if len(listTU) != 1 {
			t.Errorf("tenant+user filter len=%d, want 1", len(listTU))
		}
	})

	t.Run("Put_Identity_Mandatory", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		cases := []artifacts.ArtifactScope{
			{},
			{UserID: "U", SessionID: "S"},
			{TenantID: "T", SessionID: "S"},
			{TenantID: "T", UserID: "U"},
		}
		for i, sc := range cases {
			_, err := s.PutBytes(ctx, sc, []byte("x"), artifacts.PutOpts{Namespace: "ns"})
			if !errors.Is(err, artifacts.ErrIdentityRequired) {
				t.Errorf("case %d (%+v): err=%v, want ErrIdentityRequired", i, sc, err)
			}
			_, err = s.PutText(ctx, sc, "x", artifacts.PutOpts{Namespace: "ns"})
			if !errors.Is(err, artifacts.ErrIdentityRequired) {
				t.Errorf("case %d PutText (%+v): err=%v, want ErrIdentityRequired", i, sc, err)
			}
		}

		// Empty TaskID is acceptable for session-scoped artifacts.
		okScope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
		ref, err := s.PutBytes(ctx, okScope, []byte("session-scoped"), artifacts.PutOpts{Namespace: "ns"})
		if err != nil {
			t.Errorf("session-scoped Put rejected: %v", err)
		}
		if ref.ID == "" {
			t.Errorf("session-scoped Put returned empty ID")
		}
	})

	t.Run("Get_CrossTenant_Isolation", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		ref, err := s.PutBytes(ctx, scopeA(), []byte("tenant-A-secret"), artifacts.PutOpts{Namespace: "ns"})
		if err != nil {
			t.Fatal(err)
		}

		// Tenant B asks for tenant A's id; raw store: (nil, false, nil).
		got, found, err := s.Get(ctx, scopeB(), ref.ID)
		if err != nil {
			t.Errorf("cross-tenant Get: err=%v, want nil", err)
		}
		if found {
			t.Errorf("cross-tenant Get: found=true (LEAK)")
		}
		if got != nil {
			t.Errorf("cross-tenant Get: bytes=%q (LEAK)", got)
		}

		// Via ScopedArtifacts: also returns (nil, false, nil) because
		// the underlying store filters by scope. ScopedArtifacts'
		// `ErrScopeMismatch` only fires if the underlying store leaks
		// a ref across scopes (driver bug); V1 drivers don't.
		facadeB := artifacts.NewScoped(s, scopeB())
		gotB, foundB, err := facadeB.Get(ctx, ref.ID)
		if err != nil {
			t.Errorf("facade cross-tenant Get: err=%v", err)
		}
		if foundB {
			t.Errorf("facade cross-tenant Get: found=true (LEAK)")
		}
		if gotB != nil {
			t.Errorf("facade cross-tenant Get: bytes=%q (LEAK)", gotB)
		}
	})

	t.Run("Delete_CrossTenant_Isolation", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		ref, err := s.PutBytes(ctx, scopeA(), []byte("tenant-A-bytes"), artifacts.PutOpts{Namespace: "ns"})
		if err != nil {
			t.Fatal(err)
		}
		// Tenant B's Delete on tenant A's id is a no-op.
		existed, err := s.Delete(ctx, scopeB(), ref.ID)
		if err != nil {
			t.Errorf("cross-tenant Delete: err=%v", err)
		}
		if existed {
			t.Errorf("cross-tenant Delete: existed=true (touched another tenant)")
		}
		// Tenant A's artifact is still there.
		exists, err := s.Exists(ctx, scopeA(), ref.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Errorf("tenant A's artifact deleted by tenant B's Delete (LEAK)")
		}
	})

	t.Run("Put_AfterClose_Errors", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		if err := s.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		_, err := s.PutBytes(ctx, scopeA(), []byte("x"), artifacts.PutOpts{Namespace: "ns"})
		if !errors.Is(err, artifacts.ErrStoreClosed) {
			t.Errorf("PutBytes after Close: err=%v, want ErrStoreClosed", err)
		}
		_, _, err = s.Get(ctx, scopeA(), "ns_deadbeef0000")
		if !errors.Is(err, artifacts.ErrStoreClosed) {
			t.Errorf("Get after Close: err=%v, want ErrStoreClosed", err)
		}
		_, err = s.List(ctx, scopeA())
		if !errors.Is(err, artifacts.ErrStoreClosed) {
			t.Errorf("List after Close: err=%v, want ErrStoreClosed", err)
		}
	})

	t.Run("Concurrent_PutGet_NoRace", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		baseline := runtime.NumGoroutine()
		const goroutines = 128
		const opsPerGo = 12

		var wg sync.WaitGroup
		var errs atomic.Int64
		wg.Add(goroutines)
		for i := range goroutines {
			go func() {
				defer wg.Done()
				ctx := context.Background()
				scope := artifacts.ArtifactScope{
					TenantID:  fmt.Sprintf("t-%d", i%17),
					UserID:    fmt.Sprintf("u-%d", i%41),
					SessionID: fmt.Sprintf("s-%d", i),
					TaskID:    fmt.Sprintf("k-%d", i%7),
				}
				for j := range opsPerGo {
					data := []byte(fmt.Sprintf("payload-%d-%d", i, j))
					ref, err := s.PutBytes(ctx, scope, data, artifacts.PutOpts{
						Namespace: fmt.Sprintf("ns-%d", j%3),
					})
					if err != nil {
						errs.Add(1)
						return
					}
					if got, found, err := s.Get(ctx, scope, ref.ID); err != nil {
						errs.Add(1)
						return
					} else if !found || string(got) != string(data) {
						errs.Add(1)
						return
					}
					if _, err := s.List(ctx, scope); err != nil {
						errs.Add(1)
						return
					}
					if exists, err := s.Exists(ctx, scope, ref.ID); err != nil {
						errs.Add(1)
						return
					} else if !exists {
						errs.Add(1)
						return
					}
					if j%4 == 0 {
						if _, err := s.Delete(ctx, scope, ref.ID); err != nil {
							errs.Add(1)
							return
						}
					}
				}
			}()
		}
		// Same-scope same-bytes contention: N goroutines Put-ing identical
		// payloads against a single shared scope. Dedup must serialize
		// correctly; all returned IDs must be equal.
		const dupGoroutines = 16
		dupScope := artifacts.ArtifactScope{
			TenantID: "shared", UserID: "shared", SessionID: "shared", TaskID: "shared",
		}
		dupBytes := []byte("identical-bytes-under-contention")
		var dupWg sync.WaitGroup
		ids := make([]string, dupGoroutines)
		dupWg.Add(dupGoroutines)
		for i := range dupGoroutines {
			go func() {
				defer dupWg.Done()
				ref, err := s.PutBytes(context.Background(), dupScope, dupBytes,
					artifacts.PutOpts{Namespace: "dedup"})
				if err != nil {
					errs.Add(1)
					return
				}
				ids[i] = ref.ID
			}()
		}
		dupWg.Wait()
		expected := ids[0]
		for i, id := range ids {
			if id != expected {
				errs.Add(1)
				t.Errorf("dedup race: ids[%d]=%q, ids[0]=%q", i, id, expected)
			}
		}

		wg.Wait()
		if n := errs.Load(); n != 0 {
			t.Fatalf("%d concurrent operations errored", n)
		}

		deadline := time.Now().Add(2 * time.Second)
		for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
			runtime.Gosched()
		}
		if delta := runtime.NumGoroutine() - baseline; delta > 0 {
			t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
		}
	})

	t.Run("Close_Idempotent", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		if err := s.Close(ctx); err != nil {
			t.Fatalf("Close 1: %v", err)
		}
		if err := s.Close(ctx); err != nil {
			t.Fatalf("Close 2 (idempotent): %v", err)
		}
	})

	t.Run("GoroutineLeak_AfterClose", func(t *testing.T) {
		s, cleanup := factory()
		baseline := runtime.NumGoroutine()
		ctx := context.Background()
		// A few writes to trigger any internal goroutines (none in V1
		// drivers; future drivers may spin pumps).
		for i := range 8 {
			if _, err := s.PutBytes(ctx, scopeA(),
				[]byte(fmt.Sprintf("leak-%02d", i)),
				artifacts.PutOpts{Namespace: "ns"}); err != nil {
				t.Fatalf("PutBytes: %v", err)
			}
		}
		if err := s.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		cleanup()
		// Bounded wait for goroutines to settle. Gosched-only —
		// time.Sleep is forbidden as a synchronisation primitive per
		// AGENTS.md §11. The 2-second cap is a hard deadline: a
		// goroutine that doesn't exit in 2s under -race is a leak,
		// not a flake.
		deadline := time.Now().Add(2 * time.Second)
		for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
			runtime.Gosched()
		}
		if delta := runtime.NumGoroutine() - baseline; delta > 0 {
			t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
		}
	})

	t.Run("Scoped_AutoStamps_Scope", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		facade := artifacts.NewScoped(s, scopeA())
		ref, err := facade.PutBytes(ctx, []byte("scoped-put"), artifacts.PutOpts{Namespace: "ns"})
		if err != nil {
			t.Fatalf("facade.PutBytes: %v", err)
		}
		if !ref.Scope.Equal(scopeA()) {
			t.Errorf("facade did not stamp scope: ref.Scope=%+v", ref.Scope)
		}
		if !facade.Scope().Equal(scopeA()) {
			t.Errorf("facade.Scope mutated: %+v", facade.Scope())
		}
		got, found, err := facade.Get(ctx, ref.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			t.Errorf("facade.Get found=false")
		}
		if string(got) != "scoped-put" {
			t.Errorf("facade.Get bytes=%q", got)
		}
	})

	t.Run("Scoped_PanicsOnInvalidScope", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		// Empty everything.
		assertPanics(t, func() { _ = artifacts.NewScoped(s, artifacts.ArtifactScope{}) })
		// Empty session.
		assertPanics(t, func() {
			_ = artifacts.NewScoped(s, artifacts.ArtifactScope{
				TenantID: "T", UserID: "U",
			})
		})
		// Empty tenant.
		assertPanics(t, func() {
			_ = artifacts.NewScoped(s, artifacts.ArtifactScope{
				UserID: "U", SessionID: "S",
			})
		})
		// Nil store.
		assertPanics(t, func() { _ = artifacts.NewScoped(nil, scopeA()) })
	})

	t.Run("Scoped_ImmutableScope", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		facade := artifacts.NewScoped(s, scopeA())
		first := facade.Scope()
		// Mutate the returned copy — facade's internal scope must not
		// change.
		first.TenantID = "MUTATED"
		if first.TenantID != "MUTATED" {
			t.Fatalf("returned scope copy did not accept the mutation")
		}
		second := facade.Scope()
		if !second.Equal(scopeA()) {
			t.Errorf("facade Scope mutated through returned copy: %+v", second)
		}
	})
}

func mustList(t *testing.T, s artifacts.ArtifactStore, ctx context.Context, filter artifacts.ArtifactScope) []artifacts.ArtifactRef {
	t.Helper()
	got, err := s.List(ctx, filter)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	return got
}

func assertPanics(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic; got none")
		}
	}()
	fn()
}

func scopeA() artifacts.ArtifactScope {
	return artifacts.ArtifactScope{
		TenantID:  "tenant-A",
		UserID:    "user-1",
		SessionID: "sess-1",
		TaskID:    "task-1",
	}
}

func scopeB() artifacts.ArtifactScope {
	return artifacts.ArtifactScope{
		TenantID:  "tenant-B",
		UserID:    "user-9",
		SessionID: "sess-9",
		TaskID:    "task-9",
	}
}
