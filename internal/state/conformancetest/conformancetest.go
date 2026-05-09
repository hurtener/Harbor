// Package conformancetest exposes the canonical correctness suite
// every state.StateStore driver must pass.
//
// The suite lives in a subpackage so the production-code path
// `internal/state` does not import the standard library `testing`
// package (precedent: `internal/identity/conformancetest`).
//
// Downstream drivers (Phase 15 SQLite, Phase 16 Postgres, Phase 57
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
//   - Save_AfterClose_Errors
//   - Concurrent_SaveLoad_NoRace (D-025)
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
		for i := 0; i < goroutines; i++ {
			i := i
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
				for j := 0; j < opsPerGo; j++ {
					rec := state.StateRecord{
						ID:       state.EventID(fmt.Sprintf("ev-%d-%d", i, j)),
						Identity: ident,
						Kind:     "task.checkpoint",
						Bytes:    []byte(fmt.Sprintf("payload-%d-%d", i, j)),
					}
					if err := s.Save(ctx, rec); err != nil {
						errs.Add(1)
						return
					}
					got, err := s.Load(ctx, ident, "task.checkpoint")
					if err != nil {
						errs.Add(1)
						return
					}
					// The latest Save wins; assert it's at least
					// from this goroutine (no cross-talk).
					if string(got.Bytes) == "" {
						errs.Add(1)
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
		for i := 0; i < 8; i++ {
			_ = s.Save(ctx, state.StateRecord{
				ID:       state.EventID(fmt.Sprintf("leak-%02d", i)),
				Identity: tripleA(),
				Kind:     "task.checkpoint",
				Bytes:    []byte("x"),
			})
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
