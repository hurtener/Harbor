// Package conformancetest exposes the canonical correctness suite
// every state.StateStore driver must pass.
//
// The suite lives in a subpackage so the production-code path
// `internal/state` does not import the standard library `testing`
// package (precedent: `internal/identity/conformancetest`).
//
// Downstream drivers (SQLite, Postgres, the
// durable-log) consume it via:
//
//	import "github.com/hurtener/Harbor/internal/state/conformancetest"
//
//	func TestMyDriver_Conformance(t *testing.T) {
//	    conformancetest.Run(t, func() (state.StateStore, func()) {
//	        s := mydriver.MustNew(t)
//	        return s, func() { _ = s.Close(context.Background()) }
//	    })
//	}
//
// The factory must return a fresh, empty StateStore plus a cleanup
// callback. The suite uses the factory once per top-level subtest;
// invocations are independent.
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

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// Factory builds a fresh StateStore and returns a cleanup closure.
type Factory func() (state.StateStore, func())

// Run executes the canonical correctness suite.
//
// Subtests:
//
//   - Save_Load_RoundTrip
//   - Save_Idempotent_SameIDSameContent
//   - Save_Idempotent_SameIDDifferentBytes
//   - Save_Idempotent_SameIDDifferentKey
//   - Save_OverwritesSlotWithDifferentEventID
//   - Load_NotFound
//   - LoadByEventID_RoundTrip
//   - LoadByEventID_NotFound
//   - Save_Identity_Mandatory
//   - Save_CrossTenant_Isolation
//   - Save_CrossSession_Isolation
//   - Save_AcceptsEmptyRunID (session-scoped state)
//   - Delete_Idempotent
//   - DeleteScope_RemovesAllKindsAndRuns
//   - DeleteScope_Idempotent_AbsentScope
//   - DeleteScope_Identity_Mandatory
//   - DeleteScope_CrossTenant_Isolation
//   - DeleteScope_AfterClose_Errors
//   - ListKind_PrefixMatchesAcrossIdentities
//   - ListKind_RequiresMaintenanceScope
//   - ListKind_EmptyPrefixRejected
//   - ListKind_NoMatchesReturnsEmpty
//   - ListKind_MetacharactersMatchLiterally
//   - ListKind_AfterClose_Errors
//   - ListKindForIdentity_IsolatedAndFailClosed
//   - ListKindForIdentity_ConcurrentReuse_NoCrossTalk
//   - Save_AfterClose_Errors
//   - Concurrent_SaveLoad_NoRace
//   - GoroutineLeak_AfterClose
func Run(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("Save_Load_RoundTrip", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		want := state.StateRecord{
			ID:       "01HABXXX0000000000",
			Identity: tripleA(),
			Kind:     "session.lifecycle",
			Bytes:    []byte("hello"),
			Version:  1,
		}
		if err := s.Save(ctx, want); err != nil {
			t.Fatalf("Save: %v", err)
		}
		got, err := s.Load(ctx, tripleA(), "session.lifecycle")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.ID != want.ID || got.Kind != want.Kind || got.Version != want.Version {
			t.Errorf("Load returned %+v, want %+v (modulo Bytes/UpdatedAt)", got, want)
		}
		if string(got.Bytes) != "hello" {
			t.Errorf("Bytes round-trip failed: got %q", got.Bytes)
		}
	})

	t.Run("Save_Idempotent_SameIDSameContent", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		rec := state.StateRecord{
			ID:       "01HABXXX0000000001",
			Identity: tripleA(),
			Kind:     "task.checkpoint",
			Bytes:    []byte("payload"),
			Version:  1,
		}
		if err := s.Save(ctx, rec); err != nil {
			t.Fatalf("Save 1: %v", err)
		}
		if err := s.Save(ctx, rec); err != nil {
			t.Fatalf("Save 2 (idempotent): %v", err)
		}
	})

	t.Run("Save_Idempotent_SameIDDifferentBytes", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		rec := state.StateRecord{
			ID:       "01HABXXX0000000002",
			Identity: tripleA(),
			Kind:     "task.checkpoint",
			Bytes:    []byte("v1"),
		}
		if err := s.Save(ctx, rec); err != nil {
			t.Fatalf("Save 1: %v", err)
		}
		rec.Bytes = []byte("v2")
		err := s.Save(ctx, rec)
		if !errors.Is(err, state.ErrIdempotencyConflict) {
			t.Fatalf("err=%v, want errors.Is ErrIdempotencyConflict", err)
		}
	})

	t.Run("Save_Idempotent_SameIDDifferentKey", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		rec := state.StateRecord{
			ID:       "01HABXXX0000000003",
			Identity: tripleA(),
			Kind:     "session.lifecycle",
			Bytes:    []byte("p"),
		}
		if err := s.Save(ctx, rec); err != nil {
			t.Fatalf("Save 1: %v", err)
		}
		// Same EventID, different Kind — that's a routing mistake.
		rec.Kind = "task.checkpoint"
		err := s.Save(ctx, rec)
		if !errors.Is(err, state.ErrIdempotencyConflict) {
			t.Fatalf("err=%v, want errors.Is ErrIdempotencyConflict (different Kind)", err)
		}
	})

	t.Run("Save_OverwritesSlotWithDifferentEventID", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		oldRec := state.StateRecord{
			ID:       "01HABXXX0000000004",
			Identity: tripleA(),
			Kind:     "task.checkpoint",
			Bytes:    []byte("v1"),
			Version:  1,
		}
		if err := s.Save(ctx, oldRec); err != nil {
			t.Fatalf("Save old: %v", err)
		}
		newRec := state.StateRecord{
			ID:       "01HABXXX0000000005",
			Identity: tripleA(),
			Kind:     "task.checkpoint",
			Bytes:    []byte("v2"),
			Version:  2,
		}
		if err := s.Save(ctx, newRec); err != nil {
			t.Fatalf("Save new: %v", err)
		}
		got, err := s.Load(ctx, tripleA(), "task.checkpoint")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if got.ID != newRec.ID || string(got.Bytes) != "v2" || got.Version != 2 {
			t.Errorf("slot did not update: got %+v", got)
		}
		// Old EventID should no longer be LoadByEventID-resolvable.
		_, err = s.LoadByEventID(ctx, oldRec.ID)
		if !errors.Is(err, state.ErrNotFound) {
			t.Errorf("old EventID should be evicted; err=%v", err)
		}
	})

	t.Run("Load_NotFound", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		_, err := s.Load(context.Background(), tripleA(), "missing")
		if !errors.Is(err, state.ErrNotFound) {
			t.Fatalf("err=%v, want errors.Is ErrNotFound", err)
		}
	})

	t.Run("LoadByEventID_RoundTrip", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		rec := state.StateRecord{
			ID:       "01HABXXX0000000006",
			Identity: tripleA(),
			Kind:     "task.checkpoint",
			Bytes:    []byte("by-id"),
		}
		if err := s.Save(ctx, rec); err != nil {
			t.Fatalf("Save: %v", err)
		}
		got, err := s.LoadByEventID(ctx, rec.ID)
		if err != nil {
			t.Fatalf("LoadByEventID: %v", err)
		}
		if string(got.Bytes) != "by-id" {
			t.Errorf("LoadByEventID Bytes=%q, want %q", got.Bytes, "by-id")
		}
	})

	t.Run("LoadByEventID_NotFound", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		_, err := s.LoadByEventID(context.Background(), "01HABXXX-not-real")
		if !errors.Is(err, state.ErrNotFound) {
			t.Fatalf("err=%v, want errors.Is ErrNotFound", err)
		}
	})

	t.Run("Save_Identity_Mandatory", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		cases := []identity.Quadruple{
			{},
			{Identity: identity.Identity{UserID: "U", SessionID: "S"}},
			{Identity: identity.Identity{TenantID: "T", SessionID: "S"}},
			{Identity: identity.Identity{TenantID: "T", UserID: "U"}},
		}
		for i, q := range cases {
			err := s.Save(ctx, state.StateRecord{
				ID:       state.EventID(fmt.Sprintf("01HABXXX-id-mand-%02d", i)),
				Identity: q,
				Kind:     "k",
				Bytes:    []byte("x"),
			})
			if !errors.Is(err, state.ErrIdentityRequired) {
				t.Errorf("case %d (%+v): err=%v, want ErrIdentityRequired", i, q, err)
			}
		}
	})

	t.Run("Save_CrossTenant_Isolation", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		recA := state.StateRecord{
			ID:       "01HABXXX0000000007",
			Identity: tripleA(),
			Kind:     "task.checkpoint",
			Bytes:    []byte("tenant-A"),
		}
		recB := state.StateRecord{
			ID:       "01HABXXX0000000008",
			Identity: tripleB(),
			Kind:     "task.checkpoint",
			Bytes:    []byte("tenant-B"),
		}
		if err := s.Save(ctx, recA); err != nil {
			t.Fatal(err)
		}
		if err := s.Save(ctx, recB); err != nil {
			t.Fatal(err)
		}
		// Tenant A's load returns A's record.
		got, err := s.Load(ctx, tripleA(), "task.checkpoint")
		if err != nil {
			t.Fatal(err)
		}
		if string(got.Bytes) != "tenant-A" {
			t.Errorf("tenant A leaked tenant B's bytes: %q", got.Bytes)
		}
		// And vice versa.
		gotB, err := s.Load(ctx, tripleB(), "task.checkpoint")
		if err != nil {
			t.Fatal(err)
		}
		if string(gotB.Bytes) != "tenant-B" {
			t.Errorf("tenant B leaked tenant A's bytes: %q", gotB.Bytes)
		}
	})

	t.Run("Save_CrossSession_Isolation", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		s1 := identity.Quadruple{
			Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S1"},
		}
		s2 := identity.Quadruple{
			Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S2"},
		}
		recA := state.StateRecord{
			ID:       "01HABXXX0000000009",
			Identity: s1,
			Kind:     "session.lifecycle",
			Bytes:    []byte("S1"),
		}
		recB := state.StateRecord{
			ID:       "01HABXXX000000000A",
			Identity: s2,
			Kind:     "session.lifecycle",
			Bytes:    []byte("S2"),
		}
		if err := s.Save(ctx, recA); err != nil {
			t.Fatal(err)
		}
		if err := s.Save(ctx, recB); err != nil {
			t.Fatal(err)
		}
		got1, err := s.Load(ctx, s1, "session.lifecycle")
		if err != nil {
			t.Fatal(err)
		}
		if string(got1.Bytes) != "S1" {
			t.Errorf("session 1 leaked session 2's bytes: %q", got1.Bytes)
		}
		got2, err := s.Load(ctx, s2, "session.lifecycle")
		if err != nil {
			t.Fatal(err)
		}
		if string(got2.Bytes) != "S2" {
			t.Errorf("session 2 leaked session 1's bytes: %q", got2.Bytes)
		}
	})

	t.Run("Save_AcceptsEmptyRunID", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		q := identity.Quadruple{
			Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"},
			// RunID intentionally empty.
		}
		rec := state.StateRecord{
			ID:       "01HABXXX000000000B",
			Identity: q,
			Kind:     "session.lifecycle",
			Bytes:    []byte("session-scoped"),
		}
		if err := s.Save(ctx, rec); err != nil {
			t.Fatalf("Save with empty RunID rejected: %v", err)
		}
		got, err := s.Load(ctx, q, "session.lifecycle")
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if string(got.Bytes) != "session-scoped" {
			t.Errorf("round-trip failed: %q", got.Bytes)
		}
	})

	t.Run("Delete_Idempotent", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		// Delete on absent key is a no-op.
		if err := s.Delete(ctx, tripleA(), "never.existed"); err != nil {
			t.Fatalf("Delete absent: %v", err)
		}
		// Save then delete then load.
		rec := state.StateRecord{
			ID:       "01HABXXX000000000C",
			Identity: tripleA(),
			Kind:     "task.checkpoint",
			Bytes:    []byte("p"),
		}
		if err := s.Save(ctx, rec); err != nil {
			t.Fatal(err)
		}
		if err := s.Delete(ctx, tripleA(), "task.checkpoint"); err != nil {
			t.Fatal(err)
		}
		_, err := s.Load(ctx, tripleA(), "task.checkpoint")
		if !errors.Is(err, state.ErrNotFound) {
			t.Errorf("Load after Delete: err=%v, want ErrNotFound", err)
		}
		// And the EventID secondary should also be cleared.
		_, err = s.LoadByEventID(ctx, rec.ID)
		if !errors.Is(err, state.ErrNotFound) {
			t.Errorf("LoadByEventID after Delete: err=%v, want ErrNotFound", err)
		}
	})

	t.Run("DeleteScope_RemovesAllKindsAndRuns", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		scope := identity.Identity{TenantID: "tenant-A", UserID: "user-1", SessionID: "sess-1"}
		// Seed multiple kinds + runs under the scope, plus one record in a
		// DIFFERENT session that must survive.
		seeds := []state.StateRecord{
			{ID: "01HABXXX00000000DS", Identity: identity.Quadruple{Identity: scope}, Kind: "session.lifecycle", Bytes: []byte("life")},
			{ID: "01HABXXX00000001DS", Identity: identity.Quadruple{Identity: scope, RunID: "run-1"}, Kind: "task.checkpoint", Bytes: []byte("ckpt")},
			{ID: "01HABXXX00000002DS", Identity: identity.Quadruple{Identity: scope}, Kind: "events.durable.entry/0001", Bytes: []byte("ev")},
			{ID: "01HABXXX00000003DS", Identity: identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-A", UserID: "user-1", SessionID: "sess-OTHER"}}, Kind: "session.lifecycle", Bytes: []byte("keep")},
		}
		for _, r := range seeds {
			if err := s.Save(ctx, r); err != nil {
				t.Fatalf("Save(%s): %v", r.Kind, err)
			}
		}
		n, err := s.DeleteScope(ctx, scope)
		if err != nil {
			t.Fatalf("DeleteScope: %v", err)
		}
		if n != 3 {
			t.Fatalf("DeleteScope deleted %d records, want 3 (all kinds + runs under the triple)", n)
		}
		// The three scoped slots are gone.
		if _, err := s.Load(ctx, identity.Quadruple{Identity: scope}, "session.lifecycle"); !errors.Is(err, state.ErrNotFound) {
			t.Errorf("session.lifecycle survived DeleteScope: err=%v", err)
		}
		if _, err := s.Load(ctx, identity.Quadruple{Identity: scope, RunID: "run-1"}, "task.checkpoint"); !errors.Is(err, state.ErrNotFound) {
			t.Errorf("run-scoped task.checkpoint survived DeleteScope: err=%v", err)
		}
		// The EventID secondary index for a deleted record is also cleared.
		if _, err := s.LoadByEventID(ctx, "01HABXXX00000000DS"); !errors.Is(err, state.ErrNotFound) {
			t.Errorf("DeleteScope left an EventID resolvable: err=%v", err)
		}
		// The other session is untouched.
		other := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-A", UserID: "user-1", SessionID: "sess-OTHER"}}
		got, err := s.Load(ctx, other, "session.lifecycle")
		if err != nil {
			t.Fatalf("sibling session erased by DeleteScope: %v", err)
		}
		if string(got.Bytes) != "keep" {
			t.Errorf("sibling session bytes corrupted: %q", got.Bytes)
		}
	})

	t.Run("DeleteScope_Idempotent_AbsentScope", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		n, err := s.DeleteScope(context.Background(),
			identity.Identity{TenantID: "tenant-A", UserID: "user-1", SessionID: "never-existed"})
		if err != nil {
			t.Fatalf("DeleteScope absent: %v", err)
		}
		if n != 0 {
			t.Errorf("DeleteScope on absent scope deleted %d, want 0", n)
		}
	})

	t.Run("DeleteScope_Identity_Mandatory", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		cases := []identity.Identity{
			{},
			{UserID: "U", SessionID: "S"},
			{TenantID: "T", SessionID: "S"},
			{TenantID: "T", UserID: "U"},
		}
		for i, id := range cases {
			if _, err := s.DeleteScope(ctx, id); !errors.Is(err, state.ErrIdentityRequired) {
				t.Errorf("case %d (%+v): err=%v, want ErrIdentityRequired", i, id, err)
			}
		}
	})

	t.Run("DeleteScope_CrossTenant_Isolation", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		if err := s.Save(ctx, state.StateRecord{ID: "01HABXXX00000010DS", Identity: tripleA(), Kind: "k", Bytes: []byte("A")}); err != nil {
			t.Fatal(err)
		}
		if err := s.Save(ctx, state.StateRecord{ID: "01HABXXX00000011DS", Identity: tripleB(), Kind: "k", Bytes: []byte("B")}); err != nil {
			t.Fatal(err)
		}
		// Erase tenant A's scope — tenant B's record must survive.
		if _, err := s.DeleteScope(ctx, tripleA().Identity); err != nil {
			t.Fatalf("DeleteScope tenant A: %v", err)
		}
		if got, err := s.Load(ctx, tripleB(), "k"); err != nil || string(got.Bytes) != "B" {
			t.Errorf("tenant B record affected by tenant A DeleteScope: got=%q err=%v", got.Bytes, err)
		}
	})

	t.Run("DeleteScope_AfterClose_Errors", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		if err := s.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if _, err := s.DeleteScope(ctx, tripleA().Identity); !errors.Is(err, state.ErrStoreClosed) {
			t.Fatalf("err=%v, want errors.Is ErrStoreClosed", err)
		}
	})

	t.Run("ListKind_PrefixMatchesAcrossIdentities", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		// Two records under the SAME kind prefix in DIFFERENT tenants,
		// plus one non-matching kind: the maintenance scan must return
		// both prefix matches (it deliberately crosses identity
		// boundaries — RFC §6.11) and nothing else.
		seeds := []state.StateRecord{
			{ID: "01HABXXX00000000LK", Identity: tripleA(), Kind: "pauseresume.checkpoint:tok-A", Bytes: []byte("a")},
			{ID: "01HABXXX00000001LK", Identity: tripleB(), Kind: "pauseresume.checkpoint:tok-B", Bytes: []byte("b")},
			{ID: "01HABXXX00000002LK", Identity: tripleA(), Kind: "session.lifecycle", Bytes: []byte("c")},
		}
		for _, r := range seeds {
			if err := s.Save(ctx, r); err != nil {
				t.Fatalf("Save(%s): %v", r.Kind, err)
			}
		}
		got, err := s.ListKind(ctx, state.ListScope{MaintenanceScoped: true}, "pauseresume.checkpoint:")
		if err != nil {
			t.Fatalf("ListKind: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("ListKind returned %d records, want 2: %+v", len(got), got)
		}
		byKind := map[string]state.StateRecord{}
		for _, r := range got {
			byKind[r.Kind] = r
		}
		a, ok := byKind["pauseresume.checkpoint:tok-A"]
		if !ok {
			t.Fatal("ListKind missing tenant A's checkpoint record")
		}
		if a.Identity != tripleA() || string(a.Bytes) != "a" || a.ID != "01HABXXX00000000LK" {
			t.Errorf("tenant A record round-trip failed: %+v", a)
		}
		b, ok := byKind["pauseresume.checkpoint:tok-B"]
		if !ok {
			t.Fatal("ListKind missing tenant B's checkpoint record")
		}
		if b.Identity != tripleB() || string(b.Bytes) != "b" {
			t.Errorf("tenant B record round-trip failed: %+v", b)
		}
	})

	t.Run("ListKind_RequiresMaintenanceScope", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		_, err := s.ListKind(context.Background(), state.ListScope{}, "pauseresume.checkpoint:")
		if !errors.Is(err, state.ErrMaintenanceScopeRequired) {
			t.Fatalf("err=%v, want errors.Is ErrMaintenanceScopeRequired (the scan must fail closed)", err)
		}
	})

	t.Run("ListKind_EmptyPrefixRejected", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		_, err := s.ListKind(context.Background(), state.ListScope{MaintenanceScoped: true}, "")
		if !errors.Is(err, state.ErrInvalidRecord) {
			t.Fatalf("err=%v, want errors.Is ErrInvalidRecord (a whole-store dump is never a valid scan)", err)
		}
	})

	t.Run("ListKind_NoMatchesReturnsEmpty", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		got, err := s.ListKind(context.Background(), state.ListScope{MaintenanceScoped: true}, "never.seen:")
		if err != nil {
			t.Fatalf("ListKind: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("ListKind on empty store returned %d records, want 0", len(got))
		}
	})

	t.Run("ListKind_MetacharactersMatchLiterally", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		// `%` and `_` in the prefix must match literally — a SQL driver
		// that forgets LIKE escaping would match the decoy kinds.
		seeds := []state.StateRecord{
			{ID: "01HABXXX00000003LK", Identity: tripleA(), Kind: "a%b_c:match", Bytes: []byte("m")},
			{ID: "01HABXXX00000004LK", Identity: tripleA(), Kind: "aXbYc:decoy", Bytes: []byte("d")},
		}
		for _, r := range seeds {
			if err := s.Save(ctx, r); err != nil {
				t.Fatalf("Save(%s): %v", r.Kind, err)
			}
		}
		got, err := s.ListKind(ctx, state.ListScope{MaintenanceScoped: true}, "a%b_c:")
		if err != nil {
			t.Fatalf("ListKind: %v", err)
		}
		if len(got) != 1 || got[0].Kind != "a%b_c:match" {
			t.Fatalf("ListKind metacharacter prefix returned %+v, want exactly the literal match", got)
		}
	})

	t.Run("ListKind_AfterClose_Errors", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		if err := s.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		_, err := s.ListKind(ctx, state.ListScope{MaintenanceScoped: true}, "k:")
		if !errors.Is(err, state.ErrStoreClosed) {
			t.Fatalf("err=%v, want errors.Is ErrStoreClosed", err)
		}
	})

	t.Run("ListKindForIdentity_IsolatedAndFailClosed", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		want := tripleA()
		seeds := []state.StateRecord{
			{ID: "01HABXXX00000005LS", Identity: tripleA(), Kind: "agentcfg.revision.a", Bytes: []byte("a")},
			{ID: "01HABXXX00000006LS", Identity: tripleA(), Kind: "agentcfg.revision.b", Bytes: []byte("b")},
			{ID: "01HABXXX00000007LS", Identity: identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-B", UserID: want.UserID, SessionID: want.SessionID}, RunID: want.RunID}, Kind: "agentcfg.revision.a", Bytes: []byte("other-tenant")},
			{ID: "01HABXXX00000008LS", Identity: identity.Quadruple{Identity: identity.Identity{TenantID: want.TenantID, UserID: "user-9", SessionID: want.SessionID}, RunID: want.RunID}, Kind: "agentcfg.revision.a", Bytes: []byte("other-user")},
			{ID: "01HABXXX00000009LS", Identity: identity.Quadruple{Identity: identity.Identity{TenantID: want.TenantID, UserID: want.UserID, SessionID: "sess-9"}, RunID: want.RunID}, Kind: "agentcfg.revision.a", Bytes: []byte("other-session")},
			{ID: "01HABXXX00000010LS", Identity: identity.Quadruple{Identity: want.Identity, RunID: "run-9"}, Kind: "agentcfg.revision.a", Bytes: []byte("other-run")},
			{ID: "01HABXXX00000011LS", Identity: tripleA(), Kind: "other.revision.a", Bytes: []byte("other-kind")},
		}
		for _, rec := range seeds {
			if err := s.Save(ctx, rec); err != nil {
				t.Fatalf("Save(%s): %v", rec.ID, err)
			}
		}
		got, err := s.ListKindForIdentity(ctx, tripleA(), "agentcfg.revision.")
		if err != nil {
			t.Fatalf("ListKindForIdentity: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("ListKindForIdentity returned %d records, want 2: %+v", len(got), got)
		}
		for _, rec := range got {
			if rec.Identity != tripleA() {
				t.Fatalf("ListKindForIdentity leaked %+v, want only %+v", rec.Identity, tripleA())
			}
		}
		if _, err := s.ListKindForIdentity(ctx, identity.Quadruple{}, "agentcfg.revision."); !errors.Is(err, state.ErrIdentityRequired) {
			t.Fatalf("incomplete identity err=%v, want ErrIdentityRequired", err)
		}
		if _, err := s.ListKindForIdentity(ctx, tripleA(), ""); !errors.Is(err, state.ErrInvalidRecord) {
			t.Fatalf("empty prefix err=%v, want ErrInvalidRecord", err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.ListKindForIdentity(cancelled, tripleA(), "agentcfg.revision."); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled context err=%v, want context.Canceled", err)
		}
	})

	t.Run("ListKindForIdentity_ConcurrentReuse_NoCrossTalk", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		const callers = 128
		kindPrefix := "agentcfg.revision."
		targets := make([]identity.Quadruple, callers)
		for i := range callers {
			target := identity.Quadruple{
				Identity: identity.Identity{
					TenantID:  fmt.Sprintf("target-tenant-%03d", i),
					UserID:    fmt.Sprintf("target-user-%03d", i),
					SessionID: fmt.Sprintf("target-session-%03d", i),
				},
				RunID: fmt.Sprintf("target-run-%03d", i),
			}
			targets[i] = target
			seeds := []state.StateRecord{
				{ID: state.EventID(fmt.Sprintf("scoped-target-%03d", i)), Identity: target, Kind: kindPrefix + "target", Bytes: []byte(fmt.Sprintf("target-%03d", i))},
				{ID: state.EventID(fmt.Sprintf("scoped-tenant-decoy-%03d", i)), Identity: identity.Quadruple{Identity: identity.Identity{TenantID: fmt.Sprintf("decoy-tenant-%03d", i), UserID: target.UserID, SessionID: target.SessionID}, RunID: target.RunID}, Kind: kindPrefix + "tenant-decoy", Bytes: []byte("tenant-decoy")},
				{ID: state.EventID(fmt.Sprintf("scoped-user-decoy-%03d", i)), Identity: identity.Quadruple{Identity: identity.Identity{TenantID: target.TenantID, UserID: fmt.Sprintf("decoy-user-%03d", i), SessionID: target.SessionID}, RunID: target.RunID}, Kind: kindPrefix + "user-decoy", Bytes: []byte("user-decoy")},
				{ID: state.EventID(fmt.Sprintf("scoped-session-decoy-%03d", i)), Identity: identity.Quadruple{Identity: identity.Identity{TenantID: target.TenantID, UserID: target.UserID, SessionID: fmt.Sprintf("decoy-session-%03d", i)}, RunID: target.RunID}, Kind: kindPrefix + "session-decoy", Bytes: []byte("session-decoy")},
				{ID: state.EventID(fmt.Sprintf("scoped-run-decoy-%03d", i)), Identity: identity.Quadruple{Identity: target.Identity, RunID: fmt.Sprintf("decoy-run-%03d", i)}, Kind: kindPrefix + "run-decoy", Bytes: []byte("run-decoy")},
			}
			for _, rec := range seeds {
				if err := s.Save(ctx, rec); err != nil {
					t.Fatalf("Save(%s): %v", rec.ID, err)
				}
			}
		}

		start := make(chan struct{})
		errCh := make(chan error, callers)
		var wg sync.WaitGroup
		wg.Add(callers)
		for i := range callers {
			go func() {
				defer wg.Done()
				<-start
				callCtx := ctx
				if i%16 == 0 {
					cancelled, cancel := context.WithCancel(ctx)
					cancel()
					callCtx = cancelled
				}
				got, err := s.ListKindForIdentity(callCtx, targets[i], kindPrefix)
				if i%16 == 0 {
					if !errors.Is(err, context.Canceled) {
						errCh <- fmt.Errorf("caller %d: cancelled: %w, want context.Canceled", i, err)
					}
					return
				}
				if err != nil {
					errCh <- fmt.Errorf("caller %d: ListKindForIdentity: %w", i, err)
					return
				}
				if len(got) != 1 || got[0].Identity != targets[i] || string(got[0].Bytes) != fmt.Sprintf("target-%03d", i) {
					errCh <- fmt.Errorf("caller %d: rows=%+v, want one exact target", i, got)
				}
			}()
		}
		close(start)
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Error(err)
		}
	})

	t.Run("Save_AfterClose_Errors", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		if err := s.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		err := s.Save(ctx, state.StateRecord{
			ID:       "01HABXXX000000000D",
			Identity: tripleA(),
			Kind:     "k",
			Bytes:    []byte("x"),
		})
		if !errors.Is(err, state.ErrStoreClosed) {
			t.Fatalf("Save: err=%v, want ErrStoreClosed", err)
		}
	})

	t.Run("Concurrent_SaveLoad_NoRace", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		baseline := runtime.NumGoroutine()
		const goroutines = 128
		const opsPerGo = 16

		var wg sync.WaitGroup
		var errs atomic.Int64
		wg.Add(goroutines)
		for i := range goroutines {

			go func() {
				defer wg.Done()
				ctx := context.Background()
				ident := identity.Quadruple{
					Identity: identity.Identity{
						TenantID:  fmt.Sprintf("t-%d", i%17),
						UserID:    fmt.Sprintf("u-%d", i%41),
						SessionID: fmt.Sprintf("s-%d", i),
					},
				}
				// Mix of Save / Load / LoadByEventID / Delete per
				// iteration so the conformance gate covers every
				// method's concurrent-correctness contract — the SQLite work
				// SQLite + Postgres inherit this.
				for j := range opsPerGo {
					eventID := state.EventID(fmt.Sprintf("ev-%d-%d", i, j))
					rec := state.StateRecord{
						ID:       eventID,
						Identity: ident,
						Kind:     "task.checkpoint",
						Bytes:    []byte(fmt.Sprintf("payload-%d-%d", i, j)),
					}
					if err := s.Save(ctx, rec); err != nil {
						errs.Add(1)
						return
					}
					if got, err := s.Load(ctx, ident, "task.checkpoint"); err != nil {
						errs.Add(1)
						return
					} else if string(got.Bytes) == "" {
						errs.Add(1)
					}
					if got, err := s.LoadByEventID(ctx, eventID); err != nil && !errors.Is(err, state.ErrNotFound) {
						errs.Add(1)
						return
					} else if err == nil && got.ID != eventID {
						errs.Add(1)
					}
					// Delete every fourth iteration to exercise the
					// erase path under concurrency. Subsequent Load
					// MAY return ErrNotFound — both outcomes are
					// valid given other goroutines may have
					// re-saved the slot.
					if j%4 == 0 {
						if err := s.Delete(ctx, ident, "task.checkpoint"); err != nil {
							errs.Add(1)
							return
						}
					}
				}
			}()
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

	t.Run("GoroutineLeak_AfterClose", func(t *testing.T) {
		s, cleanup := factory()
		baseline := runtime.NumGoroutine()
		// A few writes to make sure any internal goroutines have
		// kicked in (there are none for InMem; future drivers may
		// spin pumps).
		ctx := context.Background()
		for i := range 8 {
			if err := s.Save(ctx, state.StateRecord{
				ID:       state.EventID(fmt.Sprintf("leak-%02d", i)),
				Identity: tripleA(),
				Kind:     "task.checkpoint",
				Bytes:    []byte("x"),
			}); err != nil {
				t.Fatalf("warm-up Save(%d): %v", i, err)
			}
		}
		if err := s.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		cleanup()
		deadline := time.Now().Add(2 * time.Second)
		for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
			runtime.Gosched()
			time.Sleep(10 * time.Millisecond)
		}
		if delta := runtime.NumGoroutine() - baseline; delta > 0 {
			t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
		}
	})
}

func tripleA() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant-A", UserID: "user-1", SessionID: "sess-1"},
		RunID:    "run-1",
	}
}

func tripleB() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant-B", UserID: "user-9", SessionID: "sess-9"},
		RunID:    "run-9",
	}
}
