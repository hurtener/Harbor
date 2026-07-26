// Package conformancetest exposes the canonical correctness suite
// every artifacts.ArtifactStore driver must pass.
//
// The suite lives in a subpackage so the production-code path
// `internal/artifacts` does not import the standard library `testing`
// package (precedent: `internal/state/conformancetest`).
//
// Downstream drivers (SQLite-blob + Postgres-blob, the
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
//   - List_WildcardsWithinTenant
//   - List_RequiresTenant
//   - ReadKey_IgnoresTaskID
//   - ReadKey_RePutUnderDifferingTask_FirstWriterWins
//   - ReadKey_CrossSession_NotFound
//   - Scoped_GetRef_AcceptsSiblingTaskStamp
//   - Concurrent_ReconciledKey_DifferingTasks
//   - Put_Identity_Mandatory
//   - Get_CrossTenant_Isolation
//   - Delete_CrossTenant_Isolation
//   - Put_AfterClose_Errors
//   - Concurrent_PutGet_NoRace
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

	// The read key is the isolation triple; the list filter is a
	// predicate. This row pins the FILTER half — every field below the
	// tenant is still a wildcard — and List_RequiresTenant pins the
	// precondition that stops the zero-value scope being an
	// all-tenants filter at the store boundary.
	t.Run("List_WildcardsWithinTenant", func(t *testing.T) {
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

		// Tenant T2 sees only its own row — there is no filter shape
		// that spans both tenants.
		listT2 := mustList(t, s, ctx, artifacts.ArtifactScope{TenantID: "T2"})
		if len(listT2) != 1 {
			t.Errorf("wildcard tenant T2 list len=%d, want 1", len(listT2))
		}

		// Tenant + user — narrows further.
		listTU := mustList(t, s, ctx, artifacts.ArtifactScope{TenantID: "T", UserID: "U1"})
		if len(listTU) != 1 {
			t.Errorf("tenant+user filter len=%d, want 1", len(listTU))
		}
	})

	// List is the one method that used to validate nothing, which made
	// the zero-value scope a legal all-tenants filter at the store
	// boundary. Every discovery surface is built on List, so the
	// precondition belongs here rather than in each of them.
	t.Run("List_RequiresTenant", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		cases := []artifacts.ArtifactScope{
			{},
			{UserID: "U"},
			{SessionID: "S"},
			{UserID: "U", SessionID: "S", TaskID: "K"},
		}
		for i, filter := range cases {
			got, err := s.List(ctx, filter)
			if !errors.Is(err, artifacts.ErrIdentityRequired) {
				t.Errorf("case %d (%+v): err=%v, want ErrIdentityRequired", i, filter, err)
			}
			if got != nil {
				t.Errorf("case %d (%+v): rows returned alongside the refusal: %d",
					i, filter, len(got))
			}
		}
	})

	// The read key is `(tenant, user, session, id)`. A caller that
	// stamped a task and a caller that did not must resolve the same
	// artifact — this is the row the two shapes drifted on before, and
	// the one the session-artifact manifest depends on: the manifest
	// lists on the triple, so every ref it shows must be fetchable.
	t.Run("ReadKey_IgnoresTaskID", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		producer := artifacts.ArtifactScope{
			TenantID: "T", UserID: "U", SessionID: "S", TaskID: "run-alpha",
		}
		payload := []byte("written by run-alpha")
		ref, err := s.PutBytes(ctx, producer, payload, artifacts.PutOpts{Namespace: "ns"})
		if err != nil {
			t.Fatal(err)
		}

		readers := []artifacts.ArtifactScope{
			producer,
			{TenantID: "T", UserID: "U", SessionID: "S"},
			{TenantID: "T", UserID: "U", SessionID: "S", TaskID: "run-beta"},
		}
		for _, reader := range readers {
			got, found, gerr := s.Get(ctx, reader, ref.ID)
			if gerr != nil {
				t.Fatalf("Get(task=%q): %v", reader.TaskID, gerr)
			}
			if !found {
				t.Fatalf("Get(task=%q) found=false; the read key must ignore TaskID", reader.TaskID)
			}
			if string(got) != string(payload) {
				t.Errorf("Get(task=%q) bytes=%q, want %q", reader.TaskID, got, payload)
			}

			gotRef, found, rerr := s.GetRef(ctx, reader, ref.ID)
			if rerr != nil {
				t.Fatalf("GetRef(task=%q): %v", reader.TaskID, rerr)
			}
			if !found {
				t.Fatalf("GetRef(task=%q) found=false", reader.TaskID)
			}
			// The stamp is the PRODUCER's, not the reader's — TaskID is
			// provenance, so it travels with the artifact.
			if gotRef.Scope.TaskID != "run-alpha" {
				t.Errorf("GetRef(task=%q).Scope.TaskID=%q, want the producer's %q",
					reader.TaskID, gotRef.Scope.TaskID, "run-alpha")
			}

			exists, eerr := s.Exists(ctx, reader, ref.ID)
			if eerr != nil {
				t.Fatalf("Exists(task=%q): %v", reader.TaskID, eerr)
			}
			if !exists {
				t.Errorf("Exists(task=%q)=false", reader.TaskID)
			}
		}

		// Delete keys on the triple too: a sibling run's scope deletes.
		deleted, err := s.Delete(ctx,
			artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S", TaskID: "run-beta"},
			ref.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !deleted {
			t.Errorf("Delete under a sibling task returned existed=false")
		}
		if _, found, gerr := s.Get(ctx, producer, ref.ID); gerr != nil || found {
			t.Errorf("after Delete: found=%v err=%v, want found=false", found, gerr)
		}
	})

	// The honest cost of narrowing the read key: the WRITE key narrows
	// with it, so two runs storing identical bytes in one session
	// collapse to ONE artifact and the stamp is the first writer's.
	// A TaskID filter therefore under-reports. Pinned rather than left
	// to be discovered.
	t.Run("ReadKey_RePutUnderDifferingTask_FirstWriterWins", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		triple := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
		first := triple
		first.TaskID = "run-1"
		second := triple
		second.TaskID = "run-2"
		payload := []byte("identical bytes from two runs")
		opts := artifacts.PutOpts{Namespace: "ns"}

		firstRef, err := s.PutBytes(ctx, first, payload, opts)
		if err != nil {
			t.Fatal(err)
		}
		secondRef, err := s.PutBytes(ctx, second, payload, opts)
		if err != nil {
			t.Fatal(err)
		}
		if secondRef.ID != firstRef.ID {
			t.Errorf("re-Put under a differing task produced a second id: %q vs %q",
				secondRef.ID, firstRef.ID)
		}
		if secondRef.Scope.TaskID != "run-1" {
			t.Errorf("re-Put returned stamp %q, want the first writer's %q",
				secondRef.Scope.TaskID, "run-1")
		}

		rows := mustList(t, s, ctx, triple)
		if len(rows) != 1 {
			t.Fatalf("session list len=%d, want 1 (the two Puts must collapse)", len(rows))
		}
		if rows[0].Scope.TaskID != "run-1" {
			t.Errorf("listed stamp=%q, want %q", rows[0].Scope.TaskID, "run-1")
		}

		byFirst := mustList(t, s, ctx, first)
		if len(byFirst) != 1 {
			t.Errorf("TaskID=run-1 filter len=%d, want 1", len(byFirst))
		}
		// THE LOSSY HALF: run-2 stored these bytes, and the filter does
		// not return them, because the row carries run-1's stamp.
		bySecond := mustList(t, s, ctx, second)
		if len(bySecond) != 0 {
			t.Errorf("TaskID=run-2 filter len=%d, want 0 — the filter answers "+
				"\"which artifacts is this run the recorded producer of\", not "+
				"\"which did it write\"", len(bySecond))
		}
	})

	// Narrowing the key to the triple must not widen the boundary. The
	// session is the innermost isolation scope; a sibling SESSION is a
	// different principal even under the same tenant and user.
	t.Run("ReadKey_CrossSession_NotFound", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		owner := artifacts.ArtifactScope{
			TenantID: "T", UserID: "U", SessionID: "sess-owner", TaskID: "k",
		}
		other := artifacts.ArtifactScope{
			TenantID: "T", UserID: "U", SessionID: "sess-other", TaskID: "k",
		}
		ref, err := s.PutBytes(ctx, owner, []byte("owning session only"), artifacts.PutOpts{Namespace: "ns"})
		if err != nil {
			t.Fatal(err)
		}
		if _, found, gerr := s.Get(ctx, other, ref.ID); gerr != nil || found {
			t.Errorf("cross-session Get: found=%v err=%v, want (false, nil)", found, gerr)
		}
		if _, found, gerr := s.GetRef(ctx, other, ref.ID); gerr != nil || found {
			t.Errorf("cross-session GetRef: found=%v err=%v, want (false, nil)", found, gerr)
		}
		if exists, eerr := s.Exists(ctx, other, ref.ID); eerr != nil || exists {
			t.Errorf("cross-session Exists: %v err=%v, want false", exists, eerr)
		}
		if existed, derr := s.Delete(ctx, other, ref.ID); derr != nil || existed {
			t.Errorf("cross-session Delete: existed=%v err=%v, want false", existed, derr)
		}
		// The owner still has it — the cross-session Delete was a no-op.
		if _, found, gerr := s.Get(ctx, owner, ref.ID); gerr != nil || !found {
			t.Errorf("owner Get after cross-session Delete: found=%v err=%v, want true",
				found, gerr)
		}
	})

	// The facade's scope check compares the TRIPLE. A ref stamped by a
	// sibling run is exactly the case the reconciled key enables, so
	// comparing the whole scope there would refuse the read the change
	// exists to allow.
	t.Run("Scoped_GetRef_AcceptsSiblingTaskStamp", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		producer := artifacts.ArtifactScope{
			TenantID: "T", UserID: "U", SessionID: "S", TaskID: "producing-run",
		}
		ref, err := s.PutBytes(ctx, producer, []byte("stamped elsewhere"), artifacts.PutOpts{Namespace: "ns"})
		if err != nil {
			t.Fatal(err)
		}
		facade := artifacts.NewScoped(s, artifacts.ArtifactScope{
			TenantID: "T", UserID: "U", SessionID: "S",
		})
		got, found, err := facade.GetRef(ctx, ref.ID)
		if err != nil {
			t.Fatalf("facade.GetRef on a sibling-stamped ref: %v", err)
		}
		if !found {
			t.Fatal("facade.GetRef found=false")
		}
		if got.Scope.TaskID != "producing-run" {
			t.Errorf("facade.GetRef stamp=%q, want %q", got.Scope.TaskID, "producing-run")
		}
		// And the facade's List answers the same question its reads do.
		rows, err := facade.List(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Errorf("facade.List len=%d, want 1 — listing and reading must agree", len(rows))
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

	// The concurrent-reuse contract over the RECONCILED KEY specifically:
	// N goroutines race identical bytes into ONE session under N distinct
	// task stamps. The dedup happens on the triple, so the race is
	// between writers that a task-keyed store could not have collided.
	//
	// WHAT IS ASSERTED, AND WHAT DELIBERATELY IS NOT. The store must
	// converge on ONE artifact, every racer must observe the same id, and
	// every racer must be able to read the stored bytes. The row does NOT
	// assert that all racers see the SAME provenance stamp, because
	// first-writer-wins is a property of ORDERED writes: under a genuine
	// tie there is no first writer, so a single stamp is not a property
	// the contract has. The sequential row
	// (ReadKey_RePutUnderDifferingTask_FirstWriterWins) is where "first"
	// is defined and where the exact stamp is pinned. Asserting more here
	// would be asserting a guarantee the interface does not make — the
	// same class of overclaim §13 names, arriving through a test.
	t.Run("Concurrent_ReconciledKey_DifferingTasks", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		baseline := runtime.NumGoroutine()
		const goroutines = 128
		triple := artifacts.ArtifactScope{
			TenantID: "race-t", UserID: "race-u", SessionID: "race-s",
		}
		payload := []byte("one artifact, many claimed producers")

		var wg sync.WaitGroup
		var errs atomic.Int64
		ids := make([]string, goroutines)
		stamps := make([]string, goroutines)
		wg.Add(goroutines)
		for i := range goroutines {
			go func() {
				defer wg.Done()
				scope := triple
				scope.TaskID = fmt.Sprintf("run-%03d", i)
				ref, err := s.PutBytes(context.Background(), scope, payload,
					artifacts.PutOpts{Namespace: "race"})
				if err != nil {
					errs.Add(1)
					return
				}
				ids[i] = ref.ID
				stamps[i] = ref.Scope.TaskID
			}()
		}
		wg.Wait()
		if n := errs.Load(); n != 0 {
			t.Fatalf("%d concurrent Puts errored", n)
		}
		claimed := map[string]struct{}{}
		for i := range goroutines {
			if ids[i] != ids[0] {
				t.Fatalf("ids diverged: ids[%d]=%q ids[0]=%q", i, ids[i], ids[0])
			}
			claimed[fmt.Sprintf("run-%03d", i)] = struct{}{}
		}
		// Whatever stamp a racer got back, it must be SOME racer's — a
		// driver may not invent or blank the provenance under a race.
		for i := range goroutines {
			if _, ok := claimed[stamps[i]]; !ok {
				t.Fatalf("racer %d returned stamp %q, which no racer supplied", i, stamps[i])
			}
		}

		rows := mustList(t, s, context.Background(), triple)
		if len(rows) != 1 {
			t.Errorf("session holds %d artifacts after %d racing Puts, want 1 "+
				"(the racers must converge on one artifact, not one per task)",
				len(rows), goroutines)
		}
		// The store settled on ONE stamp, and it is one a racer supplied.
		settled, found, err := s.GetRef(context.Background(), triple, ids[0])
		if err != nil || !found {
			t.Fatalf("GetRef after the race: found=%v err=%v", found, err)
		}
		if _, ok := claimed[settled.Scope.TaskID]; !ok {
			t.Errorf("settled stamp %q was supplied by no racer", settled.Scope.TaskID)
		}
		// Every racer can read what the winner stored, whatever stamp it
		// carries.
		for i := range goroutines {
			reader := triple
			reader.TaskID = fmt.Sprintf("run-%03d", i)
			got, found, err := s.Get(context.Background(), reader, ids[0])
			if err != nil || !found || string(got) != string(payload) {
				t.Fatalf("racer %d cannot read the winner's bytes: found=%v err=%v", i, found, err)
			}
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
