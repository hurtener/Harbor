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
	"reflect"
	"runtime"
	"strings"
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
//   - Save_ZeroLengthBytes_NilAndEmptyAreByteEqual
//   - Save_Idempotent_SameIDSameContent
//   - Save_Idempotent_SameIDDifferentBytes
//   - Save_Idempotent_SameIDDifferentKey
//   - Save_OverwritesSlotWithDifferentEventID
//   - SaveIf_MatchingStaleAbsentAndMultiSlot
//   - SaveIf_ConcurrentOneWinner
//   - DeleteIf_ExactStaleAndAbsent
//   - DeleteIf_InvalidExpectationFailsLoud
//   - DeleteIf_ConcurrentReplacementNeverDeleted
//   - DeleteIf_CancelledAndClosedFailLoud
//   - FenceIf_ExactStaleAbsentAndCallbackError
//   - FenceIf_SerializesSaveIf
//   - FenceIf_InvalidCancelledAndClosedFailLoud
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
//   - ScanKindForTenant_BoundedKeysetAndFailClosed
//   - ScanKindForTenant_ConcurrentReuse_NoCrossTalk
//   - ListKindForIdentity_IsolatedAndFailClosed
//   - ListKindForIdentityBounded_IsolatedAndFailClosed
//   - ListKindForIdentity_ConcurrentReuse_NoCrossTalk
//   - Save_AfterClose_Errors
//   - Concurrent_SaveLoad_NoRace
//   - GoroutineLeak_AfterClose
func Run(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("SaveBatchIf_AtomicValidationAndRollback", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		a := state.StateRecord{ID: "01HABXXX00000000BA", Identity: tripleA(), Kind: "batch.a", Bytes: []byte("a")}
		b := state.StateRecord{ID: "01HABXXX00000000BB", Identity: tripleA(), Kind: "batch.b", Bytes: []byte("b")}
		expect := []state.SlotExpectation{{Identity: a.Identity, Kind: a.Kind}, {Identity: b.Identity, Kind: b.Kind}}
		if err := s.SaveBatchIf(ctx, expect, []state.StateRecord{a, b}); err != nil {
			t.Fatalf("SaveBatchIf initial: %v", err)
		}
		for _, rec := range []state.StateRecord{a, b} {
			got, err := s.Load(ctx, rec.Identity, rec.Kind)
			if err != nil || got.ID != rec.ID {
				t.Fatalf("Load(%s) = %+v, %v", rec.Kind, got, err)
			}
		}
		nextA := state.StateRecord{ID: "01HABXXX00000000BC", Identity: a.Identity, Kind: a.Kind, Bytes: []byte("next-a")}
		nextB := state.StateRecord{ID: "01HABXXX00000000BD", Identity: b.Identity, Kind: b.Kind, Bytes: []byte("next-b")}
		stale := []state.SlotExpectation{{Identity: a.Identity, Kind: a.Kind, ExpectedEventID: a.ID}, {Identity: b.Identity, Kind: b.Kind, ExpectedEventID: "stale"}}
		if err := s.SaveBatchIf(ctx, stale, []state.StateRecord{nextA, nextB}); !errors.Is(err, state.ErrConditionFailed) {
			t.Fatalf("stale batch = %v, want ErrConditionFailed", err)
		}
		got, err := s.Load(ctx, a.Identity, a.Kind)
		if err != nil || got.ID != a.ID {
			t.Fatalf("partial write after failed batch = %+v, %v", got, err)
		}
		duplicate := []state.SlotExpectation{{Identity: a.Identity, Kind: a.Kind, ExpectedEventID: a.ID}, {Identity: a.Identity, Kind: a.Kind, ExpectedEventID: a.ID}}
		if err := s.SaveBatchIf(ctx, duplicate, []state.StateRecord{nextA}); !errors.Is(err, state.ErrInvalidRecord) {
			t.Fatalf("duplicate expectations validation = %v", err)
		}
		if err := s.SaveBatchIf(ctx, []state.SlotExpectation{{Identity: a.Identity, Kind: a.Kind, ExpectedEventID: a.ID}}, []state.StateRecord{nextB}); !errors.Is(err, state.ErrInvalidRecord) {
			t.Fatalf("unconditioned write = %v, want ErrInvalidRecord", err)
		}
	})

	t.Run("InternalNamespace_ProtectedFromExternalMutationAndDeleteScope", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		q := identity.InternalCoordinationQuadruple()
		kind := state.InternalKindPrefix + "conformance/authority"
		internal := state.NewInternalRecord("01HABXXX00000000BI", q, kind, []byte("authority"))
		if err := s.Save(ctx, internal); err != nil {
			t.Fatalf("seed internal: %v", err)
		}
		if err := s.Save(ctx, state.StateRecord{ID: "01HABXXX00000000BJ", Identity: q, Kind: kind, Bytes: []byte("collision")}); !errors.Is(err, state.ErrReservedKind) {
			t.Fatalf("external internal-kind Save = %v, want ErrReservedKind", err)
		}
		if err := s.Save(ctx, state.StateRecord{ID: "01HABXXX00000000BK", Identity: q, Kind: "ordinary", Bytes: []byte("collision")}); !errors.Is(err, state.ErrReservedIdentity) {
			t.Fatalf("external sentinel-identity Save = %v, want ErrReservedIdentity", err)
		}
		if _, err := s.DeleteScope(ctx, q.Identity); err != nil {
			t.Fatalf("DeleteScope sentinel identity: %v", err)
		}
		got, err := s.Load(ctx, q, kind)
		if err != nil || got.ID != internal.ID {
			t.Fatalf("internal authority after DeleteScope = %+v, %v", got, err)
		}
		if err := s.Delete(ctx, q, kind); !errors.Is(err, state.ErrReservedKind) {
			t.Fatalf("external Delete internal kind = %v, want ErrReservedKind", err)
		}
		deleted, err := s.DeleteIf(ctx, state.InternalSlotExpectation(q, kind, internal.ID))
		if err != nil || !deleted {
			t.Fatalf("authorized internal DeleteIf = %v, %v", deleted, err)
		}

		ordinary := tripleA()
		legacy := state.NewInternalRecord("01HABXXX00000000BL", ordinary, kind, []byte("legacy ordinary scope"))
		if err := s.Save(ctx, legacy); err != nil {
			t.Fatalf("seed legacy ordinary internal-kind row: %v", err)
		}
		if n, err := s.DeleteScope(ctx, ordinary.Identity); err != nil || n != 1 {
			t.Fatalf("DeleteScope legacy ordinary row = %d, %v; want 1", n, err)
		}
		if _, err := s.Load(ctx, ordinary, kind); !errors.Is(err, state.ErrNotFound) {
			t.Fatalf("legacy ordinary internal-kind row survived erasure: %v", err)
		}
	})

	t.Run("SaveIf_MatchingStaleAbsentAndMultiSlot", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		first := state.StateRecord{ID: "01HABXXX00000000CS", Identity: tripleA(), Kind: "conditional.a", Bytes: []byte("a")}
		if err := s.SaveIf(ctx, []state.SlotExpectation{{Identity: first.Identity, Kind: first.Kind}}, first); err != nil {
			t.Fatalf("SaveIf expected absent: %v", err)
		}
		if err := s.SaveIf(ctx, []state.SlotExpectation{{Identity: first.Identity, Kind: first.Kind}}, state.StateRecord{ID: "01HABXXX00000000CT", Identity: first.Identity, Kind: first.Kind, Bytes: []byte("b")}); !errors.Is(err, state.ErrConditionFailed) {
			t.Fatalf("SaveIf stale absence = %v, want ErrConditionFailed", err)
		}
		second := state.StateRecord{ID: "01HABXXX00000000CU", Identity: first.Identity, Kind: first.Kind, Bytes: []byte("b")}
		other := state.StateRecord{ID: "01HABXXX00000000CV", Identity: tripleA(), Kind: "conditional.b", Bytes: []byte("other")}
		if err := s.Save(ctx, other); err != nil {
			t.Fatalf("seed second condition: %v", err)
		}
		if err := s.SaveIf(ctx, []state.SlotExpectation{{Identity: first.Identity, Kind: first.Kind, ExpectedEventID: first.ID}, {Identity: other.Identity, Kind: other.Kind, ExpectedEventID: other.ID}}, second); err != nil {
			t.Fatalf("SaveIf matching multi-slot: %v", err)
		}
		got, err := s.Load(ctx, first.Identity, first.Kind)
		if err != nil || got.ID != second.ID {
			t.Fatalf("Load after SaveIf = %+v, %v; want event %q", got, err, second.ID)
		}
	})

	t.Run("SaveIf_ConcurrentOneWinner", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		base := state.StateRecord{ID: "01HABXXX00000000CW", Identity: tripleA(), Kind: "conditional.race", Bytes: []byte("base")}
		if err := s.Save(ctx, base); err != nil {
			t.Fatalf("seed: %v", err)
		}
		const workers = 128
		var winners atomic.Int64
		errCh := make(chan error, workers)
		var wg sync.WaitGroup
		for i := range workers {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				next := state.StateRecord{ID: state.EventID(fmt.Sprintf("01HABXXX%018d", i+100)), Identity: base.Identity, Kind: base.Kind, Bytes: []byte(fmt.Sprintf("winner-%d", i))}
				err := s.SaveIf(ctx, []state.SlotExpectation{{Identity: base.Identity, Kind: base.Kind, ExpectedEventID: base.ID}}, next)
				if err == nil {
					winners.Add(1)
					return
				}
				if !errors.Is(err, state.ErrConditionFailed) {
					errCh <- err
				}
			}(i)
		}
		wg.Wait()
		close(errCh)
		for err := range errCh {
			t.Fatalf("SaveIf race returned non-condition error: %v", err)
		}
		if got := winners.Load(); got != 1 {
			t.Fatalf("SaveIf winners = %d, want exactly 1", got)
		}
	})

	t.Run("FenceIf_ExactStaleAbsentAndCallbackError", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		rec := state.StateRecord{ID: "01HABXXX00000000F1", Identity: tripleA(), Kind: "conditional.fence", Bytes: []byte("base")}
		if err := s.Save(ctx, rec); err != nil {
			t.Fatal(err)
		}
		calls := 0
		exact := state.SlotExpectation{Identity: rec.Identity, Kind: rec.Kind, ExpectedEventID: rec.ID}
		if err := s.FenceIf(ctx, exact, func() error { calls++; return nil }); err != nil || calls != 1 {
			t.Fatalf("exact FenceIf = calls=%d err=%v", calls, err)
		}
		stale := exact
		stale.ExpectedEventID = "01HABXXX00000000F2"
		if err := s.FenceIf(ctx, stale, func() error { calls++; return nil }); !errors.Is(err, state.ErrConditionFailed) || calls != 1 {
			t.Fatalf("stale FenceIf = calls=%d err=%v, want condition failure without callback", calls, err)
		}
		absent := state.SlotExpectation{Identity: rec.Identity, Kind: "conditional.fence.absent", ExpectedEventID: rec.ID}
		if err := s.FenceIf(ctx, absent, func() error { calls++; return nil }); !errors.Is(err, state.ErrConditionFailed) || calls != 1 {
			t.Fatalf("absent FenceIf = calls=%d err=%v, want condition failure", calls, err)
		}
		callbackErr := errors.New("fence callback failed")
		if err := s.FenceIf(ctx, exact, func() error { return callbackErr }); !errors.Is(err, callbackErr) {
			t.Fatalf("callback error = %v, want injected error", err)
		}
		got, err := s.Load(ctx, rec.Identity, rec.Kind)
		if err != nil || got.ID != rec.ID || !reflect.DeepEqual(got.Bytes, rec.Bytes) {
			t.Fatalf("callback error mutated slot: got=%+v err=%v", got, err)
		}
	})

	t.Run("FenceIf_SerializesSaveIf", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		base := state.StateRecord{ID: "01HABXXX00000000F3", Identity: tripleA(), Kind: "conditional.fence.race", Bytes: []byte("base")}
		if err := s.Save(ctx, base); err != nil {
			t.Fatal(err)
		}
		entered := make(chan struct{})
		release := make(chan struct{})
		fenceDone := make(chan error, 1)
		go func() {
			fenceDone <- s.FenceIf(ctx, state.SlotExpectation{Identity: base.Identity, Kind: base.Kind, ExpectedEventID: base.ID}, func() error {
				close(entered)
				<-release
				return nil
			})
		}()
		<-entered
		next := state.StateRecord{ID: "01HABXXX00000000F4", Identity: base.Identity, Kind: base.Kind, Bytes: []byte("removed")}
		saveDone := make(chan error, 1)
		go func() {
			saveDone <- s.SaveIf(ctx, []state.SlotExpectation{{Identity: base.Identity, Kind: base.Kind, ExpectedEventID: base.ID}}, next)
		}()
		select {
		case err := <-saveDone:
			t.Fatalf("SaveIf crossed an active FenceIf: %v", err)
		case <-time.After(25 * time.Millisecond):
		}
		close(release)
		if err := <-fenceDone; err != nil {
			t.Fatalf("FenceIf: %v", err)
		}
		if err := <-saveDone; err != nil {
			t.Fatalf("SaveIf after fence: %v", err)
		}
	})

	t.Run("FenceIf_InvalidCancelledAndClosedFailLoud", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		rec := state.StateRecord{ID: "01HABXXX00000000F5", Identity: tripleA(), Kind: "conditional.fence.closed", Bytes: []byte("base")}
		if err := s.Save(context.Background(), rec); err != nil {
			t.Fatal(err)
		}
		exact := state.SlotExpectation{Identity: rec.Identity, Kind: rec.Kind, ExpectedEventID: rec.ID}
		if err := s.FenceIf(context.Background(), exact, nil); !errors.Is(err, state.ErrInvalidRecord) {
			t.Fatalf("nil callback = %v, want ErrInvalidRecord", err)
		}
		invalid := exact
		invalid.ExpectedEventID = ""
		if err := s.FenceIf(context.Background(), invalid, func() error { return nil }); !errors.Is(err, state.ErrInvalidRecord) {
			t.Fatalf("empty generation = %v, want ErrInvalidRecord", err)
		}
		invalid = exact
		invalid.Kind = ""
		if err := s.FenceIf(context.Background(), invalid, func() error { return nil }); !errors.Is(err, state.ErrInvalidRecord) {
			t.Fatalf("empty kind = %v, want ErrInvalidRecord", err)
		}
		invalid = exact
		invalid.Identity.SessionID = ""
		if err := s.FenceIf(context.Background(), invalid, func() error { return nil }); !errors.Is(err, state.ErrIdentityRequired) {
			t.Fatalf("incomplete identity = %v, want ErrIdentityRequired", err)
		}
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		if err := s.FenceIf(cancelled, exact, func() error { return nil }); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled FenceIf = %v, want context.Canceled", err)
		}
		if err := s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := s.FenceIf(context.Background(), exact, func() error { return nil }); !errors.Is(err, state.ErrStoreClosed) {
			t.Fatalf("closed FenceIf = %v, want ErrStoreClosed", err)
		}
	})

	t.Run("DeleteIf_ExactStaleAndAbsent", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		rec := state.StateRecord{ID: "01HABXXX00000000DY", Identity: tripleA(), Kind: "conditional.delete", Bytes: []byte("candidate")}
		if err := s.Save(ctx, rec); err != nil {
			t.Fatalf("seed: %v", err)
		}
		stale := state.SlotExpectation{Identity: rec.Identity, Kind: rec.Kind, ExpectedEventID: "01HABXXX00000000DZ"}
		if changed, err := s.DeleteIf(ctx, stale); err != nil || changed {
			t.Fatalf("stale DeleteIf = changed=%t err=%v, want false nil", changed, err)
		}
		if changed, err := s.DeleteIf(ctx, state.SlotExpectation{Identity: rec.Identity, Kind: rec.Kind, ExpectedEventID: rec.ID}); err != nil || !changed {
			t.Fatalf("exact DeleteIf = changed=%t err=%v, want true nil", changed, err)
		}
		if _, err := s.Load(ctx, rec.Identity, rec.Kind); !errors.Is(err, state.ErrNotFound) {
			t.Fatalf("Load after exact DeleteIf = %v, want ErrNotFound", err)
		}
		if changed, err := s.DeleteIf(ctx, state.SlotExpectation{Identity: rec.Identity, Kind: rec.Kind, ExpectedEventID: rec.ID}); err != nil || changed {
			t.Fatalf("absent DeleteIf = changed=%t err=%v, want false nil", changed, err)
		}
	})

	t.Run("DeleteIf_InvalidExpectationFailsLoud", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		rec := state.StateRecord{
			ID:        "01HABXXX00000000DI",
			Identity:  tripleA(),
			Kind:      "conditional.delete.invalid",
			Version:   17,
			Bytes:     []byte("original-generation-content"),
			UpdatedAt: time.Unix(1_725_000_000, 123_000_000).UTC(),
		}
		if err := s.Save(ctx, rec); err != nil {
			t.Fatalf("seed: %v", err)
		}
		before, err := s.Load(ctx, rec.Identity, rec.Kind)
		if err != nil {
			t.Fatalf("load seed: %v", err)
		}
		cases := []struct {
			name        string
			expectation state.SlotExpectation
			wantErr     error
		}{
			{
				name:        "incomplete identity",
				expectation: state.SlotExpectation{Kind: rec.Kind, ExpectedEventID: rec.ID},
				wantErr:     state.ErrIdentityRequired,
			},
			{
				name:        "empty kind",
				expectation: state.SlotExpectation{Identity: rec.Identity, ExpectedEventID: rec.ID},
				wantErr:     state.ErrInvalidRecord,
			},
			{
				name:        "empty event id",
				expectation: state.SlotExpectation{Identity: rec.Identity, Kind: rec.Kind},
				wantErr:     state.ErrInvalidRecord,
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				changed, err := s.DeleteIf(ctx, tc.expectation)
				if changed || !errors.Is(err, tc.wantErr) {
					t.Fatalf("DeleteIf = changed=%t err=%v, want false %v", changed, err, tc.wantErr)
				}
				after, loadErr := s.Load(ctx, rec.Identity, rec.Kind)
				if loadErr != nil {
					t.Fatalf("Load after invalid DeleteIf: %v", loadErr)
				}
				if !reflect.DeepEqual(after, before) {
					t.Fatalf("invalid DeleteIf mutated original record:\n before=%+v\n after=%+v", before, after)
				}
			})
		}
	})

	t.Run("DeleteIf_ConcurrentReplacementNeverDeleted", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		const rounds = 32
		for i := range rounds {
			kind := fmt.Sprintf("conditional.delete.race.%d", i)
			base := state.StateRecord{ID: state.EventID(fmt.Sprintf("01HABDEL%018d", i)), Identity: tripleA(), Kind: kind, Bytes: []byte("candidate")}
			next := state.StateRecord{ID: state.EventID(fmt.Sprintf("01HABWIN%018d", i)), Identity: tripleA(), Kind: kind, Bytes: []byte("winner")}
			if err := s.Save(ctx, base); err != nil {
				t.Fatalf("round %d seed: %v", i, err)
			}
			start := make(chan struct{})
			errCh := make(chan error, 2)
			go func() {
				<-start
				_, err := s.DeleteIf(ctx, state.SlotExpectation{Identity: base.Identity, Kind: base.Kind, ExpectedEventID: base.ID})
				errCh <- err
			}()
			go func() {
				<-start
				errCh <- s.Save(ctx, next)
			}()
			close(start)
			for range 2 {
				if err := <-errCh; err != nil {
					t.Fatalf("round %d concurrent mutation: %v", i, err)
				}
			}
			got, err := s.Load(ctx, next.Identity, next.Kind)
			if err != nil || got.ID != next.ID {
				t.Fatalf("round %d winner was deleted: got=%+v err=%v", i, got, err)
			}
		}
	})

	t.Run("DeleteIf_CancelledAndClosedFailLoud", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		rec := state.StateRecord{ID: "01HABXXX00000000DC", Identity: tripleA(), Kind: "conditional.delete.cancel", Bytes: []byte("candidate")}
		if err := s.Save(context.Background(), rec); err != nil {
			t.Fatalf("seed: %v", err)
		}
		expectation := state.SlotExpectation{Identity: rec.Identity, Kind: rec.Kind, ExpectedEventID: rec.ID}
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()
		if changed, err := s.DeleteIf(cancelled, expectation); !errors.Is(err, context.Canceled) || changed {
			t.Fatalf("DeleteIf cancelled = changed=%t err=%v, want false context.Canceled", changed, err)
		}
		if err := s.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if changed, err := s.DeleteIf(context.Background(), expectation); !errors.Is(err, state.ErrStoreClosed) || changed {
			t.Fatalf("DeleteIf after Close = changed=%t err=%v, want false ErrStoreClosed", changed, err)
		}
	})

	t.Run("SaveIf_CancelledAndClosedFailLoud", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		next := state.StateRecord{ID: "01HABXXX00000000CX", Identity: tripleA(), Kind: "conditional.cancel", Bytes: []byte("x")}
		if err := s.SaveIf(ctx, []state.SlotExpectation{{Identity: next.Identity, Kind: next.Kind}}, next); !errors.Is(err, context.Canceled) {
			t.Fatalf("SaveIf cancelled = %v, want context.Canceled", err)
		}
		if err := s.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if err := s.SaveIf(context.Background(), []state.SlotExpectation{{Identity: next.Identity, Kind: next.Kind}}, next); !errors.Is(err, state.ErrStoreClosed) {
			t.Fatalf("SaveIf after Close = %v, want ErrStoreClosed", err)
		}
	})

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

	t.Run("Save_ZeroLengthBytes_NilAndEmptyAreByteEqual", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		rec := state.StateRecord{
			ID:       "01HABXXX00000000ZB",
			Identity: tripleA(),
			Kind:     "empty.payload",
			Bytes:    nil,
			Version:  1,
		}
		if err := s.Save(ctx, rec); err != nil {
			t.Fatalf("Save nil Bytes: %v", err)
		}

		got, err := s.Load(ctx, tripleA(), rec.Kind)
		if err != nil {
			t.Fatalf("Load nil Bytes: %v", err)
		}
		if len(got.Bytes) != 0 {
			t.Fatalf("Load Bytes length = %d, want 0", len(got.Bytes))
		}

		// The idempotency contract compares payloads byte-for-byte. A nil
		// slice and an allocated zero-length slice therefore name the same
		// payload and must remain an idempotent no-op across every driver.
		rec.Bytes = []byte{}
		if err := s.Save(ctx, rec); err != nil {
			t.Fatalf("Save empty Bytes after nil Bytes: %v", err)
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
		// that treats either as a pattern would match the decoy kind.
		seeds := []state.StateRecord{
			{ID: "01HABXXX00000003LK", Identity: tripleA(), Kind: "a%b_c:match", Bytes: []byte("m")},
			{ID: "01HABXXX00000004LK", Identity: tripleA(), Kind: "aXbYc:decoy", Bytes: []byte("d")},
			{ID: "01HABXXX00000005LK", Identity: tripleA(), Kind: "A%b_c:case-decoy", Bytes: []byte("d")},
			{ID: "01HABXXX00000006LK", Identity: tripleA(), Kind: "slash\\match", Bytes: []byte("m")},
			{ID: "01HABXXX00000007LK", Identity: tripleA(), Kind: "slashXmatch", Bytes: []byte("d")},
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
		slash, err := s.ListKind(ctx, state.ListScope{MaintenanceScoped: true}, "slash\\")
		if err != nil || len(slash) != 1 || slash[0].Kind != "slash\\match" || strings.Count(slash[0].Kind, "\\") != 1 {
			t.Fatalf("ListKind backslash prefix = %+v, %v; want one literal backslash", slash, err)
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

	t.Run("ScanKindForTenant_BoundedKeysetAndFailClosed", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		scope := state.ListScope{MaintenanceScoped: true}
		const tenant = "scan-tenant"
		const prefix = "a"
		seeds := []state.StateRecord{
			{ID: "01HABXXX00000020SC", Identity: identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "u-2", SessionID: "s-1"}, RunID: "r-2"}, Kind: "ab", Bytes: []byte("2")},
			{ID: "01HABXXX00000021SC", Identity: identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "u-1", SessionID: "s-2"}, RunID: ""}, Kind: "a", Bytes: []byte("1")},
			{ID: "01HABXXX00000022SC", Identity: identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "u-1", SessionID: "s-1"}, RunID: "r-1"}, Kind: "abc", Bytes: []byte("0")},
			{ID: "01HABXXX00000023SC", Identity: identity.Quadruple{Identity: identity.Identity{TenantID: "scan-other", UserID: "u-0", SessionID: "s-0"}, RunID: "r-0"}, Kind: "a", Bytes: []byte("decoy")},
			{ID: "01HABXXX00000024SC", Identity: identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "u-0", SessionID: "s-0"}, RunID: "r-0"}, Kind: "z", Bytes: []byte("decoy")},
			{ID: "01HABXXX00000025SC", Identity: identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "u-3", SessionID: "s-1"}, RunID: "r-1"}, Kind: "a%_\\literal", Bytes: []byte("literal")},
			{ID: "01HABXXX00000026SC", Identity: identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "u-4", SessionID: "s-1"}, RunID: "r-1"}, Kind: "aXxliteral", Bytes: []byte("wildcard-decoy")},
			{ID: "01HABXXX00000027SC", Identity: identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "u-5", SessionID: "s-1"}, RunID: "r-1"}, Kind: "AUpper", Bytes: []byte("case-decoy")},
			{ID: "01HABXXX00000028SC", Identity: identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "u-6", SessionID: "s-1"}, RunID: "r-1"}, Kind: "special%_\\one", Bytes: []byte("one-backslash")},
			{ID: "01HABXXX00000029SC", Identity: identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "u-7", SessionID: "s-1"}, RunID: "r-1"}, Kind: "special%_\\\\two", Bytes: []byte("two-backslash")},
			{ID: "01HABXXX00000030SC", Identity: identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "u-8", SessionID: "s-1"}, RunID: "r-1"}, Kind: "specialXXone", Bytes: []byte("wildcard-decoy")},
		}
		for _, rec := range seeds {
			if err := s.Save(ctx, rec); err != nil {
				t.Fatalf("seed %s: %v", rec.ID, err)
			}
		}
		if _, err := s.ScanKindForTenant(ctx, state.ListScope{}, tenant, prefix, 1, ""); !errors.Is(err, state.ErrMaintenanceScopeRequired) {
			t.Fatalf("missing scope = %v, want ErrMaintenanceScopeRequired", err)
		}
		for _, bad := range []struct {
			tenant, prefix string
			limit          int
		}{{"", prefix, 1}, {tenant, "", 1}, {tenant, prefix, 0}, {tenant, prefix, state.MaxStateScanLimit + 1}} {
			if _, err := s.ScanKindForTenant(ctx, scope, bad.tenant, bad.prefix, bad.limit, ""); !errors.Is(err, state.ErrInvalidScan) {
				t.Fatalf("invalid scan %+v = %v, want ErrInvalidScan", bad, err)
			}
		}

		var all []state.StateRecord
		continuation := ""
		for {
			page, err := s.ScanKindForTenant(ctx, scope, tenant, prefix, 1, continuation)
			if err != nil {
				t.Fatalf("ScanKindForTenant: %v", err)
			}
			all = append(all, page.Records...)
			if page.Continuation == "" {
				break
			}
			continuation = page.Continuation
		}
		if len(all) != 5 {
			t.Fatalf("paged scan rows=%d, want five tenant/prefix records: %+v", len(all), all)
		}
		for i, rec := range all {
			if rec.Identity.TenantID != tenant || (i > 0 && scanTuple(rec) <= scanTuple(all[i-1])) {
				t.Fatalf("scan tuple %d = %+v, want strict tenant-local order", i, rec)
			}
		}
		last := all[len(all)-1]
		terminalCursor, err := state.EncodeStateScanContinuation(state.StateScanCursor{UserID: last.Identity.UserID, SessionID: last.Identity.SessionID, RunID: last.Identity.RunID, Kind: last.Kind}, tenant, prefix, scope)
		if err != nil {
			t.Fatalf("encode terminal cursor: %v", err)
		}
		terminal, err := s.ScanKindForTenant(ctx, scope, tenant, prefix, 1, terminalCursor)
		if err != nil || len(terminal.Records) != 0 || terminal.Continuation != "" {
			t.Fatalf("terminal cursor replay = %+v, %v; want empty terminal page", terminal, err)
		}
		first, err := s.ScanKindForTenant(ctx, scope, tenant, prefix, 1, "")
		if err != nil || len(first.Records) != 1 || first.Continuation == "" {
			t.Fatalf("first page = %+v, %v; want continuation", first, err)
		}
		replay, err := s.ScanKindForTenant(ctx, scope, tenant, prefix, 1, first.Continuation)
		if err != nil || len(replay.Records) != 1 || replay.Records[0].ID == first.Records[0].ID {
			t.Fatalf("first cursor replay = %+v, %v; want deterministic next page", replay, err)
		}
		replayAgain, err := s.ScanKindForTenant(ctx, scope, tenant, prefix, 1, first.Continuation)
		if err != nil || len(replayAgain.Records) != 1 || replayAgain.Records[0].ID != replay.Records[0].ID || replayAgain.Continuation != replay.Continuation {
			t.Fatalf("same-query cursor replay = %+v, %v; want %+v", replayAgain, err, replay)
		}
		for _, pageLimit := range []int{2, state.MaxStateScanLimit} {
			page, err := s.ScanKindForTenant(ctx, scope, tenant, prefix, pageLimit, "")
			if err != nil {
				t.Fatalf("limit %d first page: %v", pageLimit, err)
			}
			if pageLimit == 2 && (len(page.Records) != 2 || page.Continuation == "") {
				t.Fatalf("limit 2 page = %+v, want two rows and continuation", page)
			}
			if pageLimit == state.MaxStateScanLimit && (len(page.Records) != 5 || page.Continuation != "") {
				t.Fatalf("max limit page = %+v, want all rows terminal", page)
			}
		}
		for _, tc := range []struct{ name, tenant, prefix, cursor string }{
			{"malformed", tenant, prefix, "%%%"},
			{"json-object", tenant, prefix, "e30"},
			{"tenant-mismatch", "scan-other", prefix, first.Continuation},
			{"prefix-mismatch", tenant, "ab", first.Continuation},
		} {
			if _, err := s.ScanKindForTenant(ctx, scope, tc.tenant, tc.prefix, 1, tc.cursor); !errors.Is(err, state.ErrInvalidScan) {
				t.Fatalf("%s cursor = %v, want ErrInvalidScan", tc.name, err)
			}
		}
		literal, err := s.ScanKindForTenant(ctx, scope, tenant, "a%_\\", 8, "")
		if err != nil || len(literal.Records) != 1 || literal.Records[0].Kind != "a%_\\literal" {
			t.Fatalf("literal wildcard prefix = %+v, %v", literal, err)
		}
		singleBackslash, err := s.ScanKindForTenant(ctx, scope, tenant, "special%_\\", 8, "")
		if err != nil || len(singleBackslash.Records) != 2 || strings.Count(singleBackslash.Records[0].Kind, "\\") != 1 || strings.Count(singleBackslash.Records[1].Kind, "\\") != 2 {
			t.Fatalf("single-backslash literal prefix = %+v, %v; want one- and two-backslash rows", singleBackslash, err)
		}
		doubleBackslash, err := s.ScanKindForTenant(ctx, scope, tenant, "special%_\\\\", 8, "")
		if err != nil || len(doubleBackslash.Records) != 1 || doubleBackslash.Records[0].Kind != "special%_\\\\two" || strings.Count(doubleBackslash.Records[0].Kind, "\\") != 2 {
			t.Fatalf("double-backslash literal prefix = %+v, %v", doubleBackslash, err)
		}
		upper, err := s.ScanKindForTenant(ctx, scope, tenant, "A", 8, "")
		if err != nil || len(upper.Records) != 1 || upper.Records[0].Kind != "AUpper" {
			t.Fatalf("case-sensitive prefix = %+v, %v", upper, err)
		}
		ab, err := s.ScanKindForTenant(ctx, scope, tenant, "ab", 8, "")
		if err != nil || len(ab.Records) != 2 {
			t.Fatalf("a/ab narrowing = %+v, %v; want ab and abc only", ab, err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.ScanKindForTenant(cancelled, scope, tenant, prefix, 1, ""); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled scan = %v, want context.Canceled", err)
		}
		if err := s.Close(ctx); err != nil {
			t.Fatalf("Close: %v", err)
		}
		if _, err := s.ScanKindForTenant(ctx, scope, tenant, prefix, 1, ""); !errors.Is(err, state.ErrStoreClosed) {
			t.Fatalf("closed scan = %v, want ErrStoreClosed", err)
		}
	})

	t.Run("ScanKindForTenant_ConcurrentReuse_NoCrossTalk", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		const callers = 128
		for i := range callers {
			q := identity.Quadruple{Identity: identity.Identity{TenantID: fmt.Sprintf("scan-target-%03d", i), UserID: "user", SessionID: "session"}, RunID: "run"}
			if err := s.Save(ctx, state.StateRecord{ID: state.EventID(fmt.Sprintf("scan-race-%03d", i)), Identity: q, Kind: "scan.prefix.target", Bytes: []byte(q.TenantID)}); err != nil {
				t.Fatalf("seed caller %d: %v", i, err)
			}
		}
		start := make(chan struct{})
		errs := make(chan error, callers)
		var wg sync.WaitGroup
		for i := range callers {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				callCtx := ctx
				if i%16 == 0 {
					cancelled, cancel := context.WithCancel(ctx)
					cancel()
					callCtx = cancelled
				}
				tenantID := fmt.Sprintf("scan-target-%03d", i)
				page, err := s.ScanKindForTenant(callCtx, state.ListScope{MaintenanceScoped: true}, tenantID, "scan.prefix.", 1, "")
				if i%16 == 0 {
					if !errors.Is(err, context.Canceled) {
						errs <- fmt.Errorf("caller %d cancelled scan = %w", i, err)
					}
					return
				}
				if err != nil {
					errs <- fmt.Errorf("caller %d ScanKindForTenant: %w", i, err)
					return
				}
				if len(page.Records) != 1 || page.Records[0].Identity.TenantID != tenantID || page.Continuation != "" {
					errs <- fmt.Errorf("caller %d unexpected page: %+v", i, page)
				}
			}(i)
		}
		close(start)
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
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
			{ID: "01HABXXX00000012LS", Identity: tripleA(), Kind: "literal\\match", Bytes: []byte("literal")},
			{ID: "01HABXXX00000013LS", Identity: tripleA(), Kind: "literalXmatch", Bytes: []byte("decoy")},
			{ID: "01HABXXX00000014LS", Identity: tripleA(), Kind: "Case.Match", Bytes: []byte("upper")},
			{ID: "01HABXXX00000015LS", Identity: tripleA(), Kind: "case.Match", Bytes: []byte("lower")},
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
		bounded, err := s.ListKindForIdentityBounded(ctx, tripleA(), "agentcfg.revision.", 1)
		if err != nil || len(bounded) != 1 || bounded[0].Identity != tripleA() {
			t.Fatalf("ListKindForIdentityBounded = (%+v, %v), want one exact identity row", bounded, err)
		}
		if _, err := s.ListKindForIdentityBounded(ctx, identity.Quadruple{}, "agentcfg.revision.", 1); !errors.Is(err, state.ErrIdentityRequired) {
			t.Fatalf("bounded incomplete identity err=%v, want ErrIdentityRequired", err)
		}
		if _, err := s.ListKindForIdentityBounded(ctx, tripleA(), "", 1); !errors.Is(err, state.ErrInvalidRecord) {
			t.Fatalf("bounded empty prefix err=%v, want ErrInvalidRecord", err)
		}
		if _, err := s.ListKindForIdentityBounded(ctx, tripleA(), "agentcfg.revision.", 0); !errors.Is(err, state.ErrInvalidRecord) {
			t.Fatalf("bounded zero limit err=%v, want ErrInvalidRecord", err)
		}
		if _, err := s.ListKindForIdentityBounded(ctx, tripleA(), "agentcfg.revision.", state.MaxStateIdentityListLimit+1); !errors.Is(err, state.ErrInvalidRecord) {
			t.Fatalf("bounded oversized limit err=%v, want ErrInvalidRecord", err)
		}
		if _, err := s.ListKindForIdentity(ctx, identity.Quadruple{}, "agentcfg.revision."); !errors.Is(err, state.ErrIdentityRequired) {
			t.Fatalf("incomplete identity err=%v, want ErrIdentityRequired", err)
		}
		if _, err := s.ListKindForIdentity(ctx, tripleA(), ""); !errors.Is(err, state.ErrInvalidRecord) {
			t.Fatalf("empty prefix err=%v, want ErrInvalidRecord", err)
		}
		literal, err := s.ListKindForIdentity(ctx, tripleA(), "literal\\")
		if err != nil || len(literal) != 1 || literal[0].Kind != "literal\\match" || strings.Count(literal[0].Kind, "\\") != 1 {
			t.Fatalf("identity literal backslash prefix = %+v, %v", literal, err)
		}
		upper, err := s.ListKindForIdentity(ctx, tripleA(), "Case")
		if err != nil || len(upper) != 1 || upper[0].Kind != "Case.Match" {
			t.Fatalf("identity case-sensitive prefix = %+v, %v", upper, err)
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

func scanTuple(rec state.StateRecord) string {
	return rec.Identity.UserID + "\x00" + rec.Identity.SessionID + "\x00" + rec.Identity.RunID + "\x00" + rec.Kind
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
