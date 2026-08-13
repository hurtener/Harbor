// Package conformancetest exposes the canonical correctness suite
// every turns.Store driver must pass.
//
// The suite lives in a subpackage so the production-code path
// `internal/sessions/turns` does not import the standard library
// `testing` package (precedent: `internal/state/conformancetest`,
// `internal/tasks/conformancetest`, `internal/artifacts/conformancetest`).
//
// Downstream driver lanes consume it via:
//
//	import "github.com/hurtener/Harbor/internal/sessions/turns/conformancetest"
//
//	func TestMyDriver_Conformance(t *testing.T) {
//	    conformancetest.Run(t, func() (turns.Store, state.StateStore, func()) {
//	        s, st := mydriver.MustNew(t)
//	        return s, st, func() { _ = s.Close(context.Background()) }
//	    })
//	}
//
// The factory must return a fresh, empty Store plus the shared
// state.StateStore the driver fences against, plus a cleanup closure.
// The StateStore is part of the factory contract because the erasure
// fence slots (internal/agentcfg/sessionfence) live in StateStore by
// construction — the suite writes them to pin the fence contract.
// The suite uses the factory once per top-level subtest; invocations
// are independent.
package conformancetest

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/state"
)

// Factory builds a fresh turns.Store over a fresh shared
// state.StateStore and returns a cleanup closure.
type Factory func() (turns.Store, state.StateStore, func())

// Run executes the canonical correctness suite.
//
// subtests:
//
//   - Identity_Mandatory
//   - Append_MintsMonotonicPerSessionSequences
//   - Append_Idempotent_ExistingTurnReturnsExistingRow
//   - Append_Fenced_PendingErasure
//   - Append_Fenced_ConvergedErasure
//   - Update_AdvancesVersion_AndReplacesComponents
//   - Update_StaleVersion_Rejected
//   - Update_Sealed_Rejected
//   - Update_Fenced_PendingErasure
//   - Seal_Terminal_ImmutableAfter
//   - Seal_StaleVersion_Rejected
//   - GetTurn_NotFound_And_CrossSessionIsolation
//   - ListTurns_NewestFirst_KeysetPaging_NoSkipNoDuplicate
//   - ListTurns_EmptySession
//   - Checkpoint_MonotonicIdempotent
//   - Checkpoint_ConcurrentAdvances_ConvergeToMax
//   - DeleteScope_RemovesRowsAndCheckpoint
//   - Concurrent_AppendList_NoRace
//   - Close_Then_Rejects
func Run(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("Identity_Mandatory", func(t *testing.T) {
		s, _, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		zero := identity.Identity{}
		fence, err := turns.FenceFor(zero)
		if err == nil {
			t.Fatalf("FenceFor(zero) must fail")
		}
		_ = fence
		if _, err := s.AppendTurnIf(ctx, zero, turns.TurnRow{TurnID: "r"}, turns.Fence{}); !errors.Is(err, turns.ErrIdentityRequired) {
			t.Errorf("AppendTurnIf zero identity error=%v, want ErrIdentityRequired", err)
		}
		if _, err := s.UpdateTurnIf(ctx, zero, "r", 1, turns.TurnRow{}, turns.Fence{}); !errors.Is(err, turns.ErrIdentityRequired) {
			t.Errorf("UpdateTurnIf zero identity error=%v, want ErrIdentityRequired", err)
		}
		if _, err := s.SealTurnIf(ctx, zero, "r", 1, turns.TurnRow{}, turns.Fence{}); !errors.Is(err, turns.ErrIdentityRequired) {
			t.Errorf("SealTurnIf zero identity error=%v, want ErrIdentityRequired", err)
		}
		if _, err := s.GetTurn(ctx, zero, "r"); !errors.Is(err, turns.ErrIdentityRequired) {
			t.Errorf("GetTurn zero identity error=%v, want ErrIdentityRequired", err)
		}
		if _, _, _, err := s.ListTurns(ctx, zero, nil, 10); !errors.Is(err, turns.ErrIdentityRequired) {
			t.Errorf("ListTurns zero identity error=%v, want ErrIdentityRequired", err)
		}
		if _, err := s.LoadCheckpoint(ctx, zero); !errors.Is(err, turns.ErrIdentityRequired) {
			t.Errorf("LoadCheckpoint zero identity error=%v, want ErrIdentityRequired", err)
		}
		if err := s.SaveCheckpoint(ctx, zero, 1); !errors.Is(err, turns.ErrIdentityRequired) {
			t.Errorf("SaveCheckpoint zero identity error=%v, want ErrIdentityRequired", err)
		}
		if _, err := s.DeleteScope(ctx, zero); !errors.Is(err, turns.ErrIdentityRequired) {
			t.Errorf("DeleteScope zero identity error=%v, want ErrIdentityRequired", err)
		}
	})

	t.Run("Append_MintsMonotonicPerSessionSequences", func(t *testing.T) {
		s, _, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		other := triple("tenant-b", "user-b", "session-b")
		fence := fenceFor(t, id)
		otherFence := fenceFor(t, other)

		r1 := freshRow("run-1", id)
		got1, err := s.AppendTurnIf(ctx, id, r1, fence)
		if err != nil {
			t.Fatalf("append 1: %v", err)
		}
		if got1.Sequence != 1 {
			t.Errorf("first sequence=%d, want 1", got1.Sequence)
		}
		got2, err := s.AppendTurnIf(ctx, id, freshRow("run-2", id), fence)
		if err != nil {
			t.Fatalf("append 2: %v", err)
		}
		if got2.Sequence != 2 {
			t.Errorf("second sequence=%d, want 2", got2.Sequence)
		}
		// A different session restarts the counter.
		gotOther, err := s.AppendTurnIf(ctx, other, freshRow("run-1", other), otherFence)
		if err != nil {
			t.Fatalf("append other: %v", err)
		}
		if gotOther.Sequence != 1 {
			t.Errorf("other-session sequence=%d, want 1 (per-session counters)", gotOther.Sequence)
		}
	})

	t.Run("Append_Idempotent_ExistingTurnReturnsExistingRow", func(t *testing.T) {
		s, _, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		fence := fenceFor(t, id)
		first, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id), fence)
		if err != nil {
			t.Fatalf("first append: %v", err)
		}
		// A re-append (a replay of an already-applied observation)
		// returns the existing row unchanged — a no-op, never an error
		// and never an overwrite.
		dup := freshRow("run-1", id)
		dup.Query = turns.Query{Text: "MUST NOT OVERWRITE", Complete: turns.CompletenessComplete}
		second, err := s.AppendTurnIf(ctx, id, dup, fence)
		if err != nil {
			t.Fatalf("re-append: %v", err)
		}
		if second.Sequence != first.Sequence || second.Version != first.Version {
			t.Errorf("re-append returned a changed row: %+v vs %+v", second, first)
		}
		if second.Query.Text == "MUST NOT OVERWRITE" {
			t.Errorf("re-append overwrote the stored row")
		}
	})

	t.Run("Append_Fenced_PendingErasure", func(t *testing.T) {
		s, st, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		fence := fenceFor(t, id)
		if _, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id), fence); err != nil {
			t.Fatalf("pre-fence append: %v", err)
		}
		// The erasure cascade writes the pending ledger first.
		writeFenceSlot(t, st, fence.PendingAbsent)
		if _, err := s.AppendTurnIf(ctx, id, freshRow("run-2", id), fence); !errors.Is(err, turns.ErrErasureFenced) {
			t.Errorf("append with pending ledger error=%v, want ErrErasureFenced", err)
		}
	})

	t.Run("Append_Fenced_ConvergedErasure", func(t *testing.T) {
		s, st, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		fence := fenceFor(t, id)
		if _, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id), fence); err != nil {
			t.Fatalf("pre-fence append: %v", err)
		}
		// A converged erasure leaves the terminal tombstone.
		writeFenceSlot(t, st, fence.TombstoneAbsent)
		if _, err := s.AppendTurnIf(ctx, id, freshRow("run-2", id), fence); !errors.Is(err, turns.ErrErasureFenced) {
			t.Errorf("append with tombstone error=%v, want ErrErasureFenced", err)
		}
	})

	t.Run("Update_AdvancesVersion_AndReplacesComponents", func(t *testing.T) {
		s, _, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		fence := fenceFor(t, id)
		row, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id), fence)
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		next := row
		next.Answer = turns.Answer{Inline: "hello", Complete: turns.CompletenessComplete}
		got, err := s.UpdateTurnIf(ctx, id, "run-1", row.Version, next, fence)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		if got.Version != row.Version+1 {
			t.Errorf("version=%d, want %d", got.Version, row.Version+1)
		}
		if got.Answer.Inline != "hello" {
			t.Errorf("answer not replaced: %+v", got.Answer)
		}
		if got.Sequence != row.Sequence {
			t.Errorf("sequence changed on update — the ordering key is immutable")
		}
	})

	t.Run("Update_StaleVersion_Rejected", func(t *testing.T) {
		s, _, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		fence := fenceFor(t, id)
		row, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id), fence)
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if _, err := s.UpdateTurnIf(ctx, id, "run-1", row.Version+5, row, fence); !errors.Is(err, turns.ErrStaleVersion) {
			t.Errorf("stale update error=%v, want ErrStaleVersion", err)
		}
		if _, err := s.UpdateTurnIf(ctx, id, "no-such", 1, row, fence); !errors.Is(err, turns.ErrTurnNotFound) {
			t.Errorf("missing update error=%v, want ErrTurnNotFound", err)
		}
	})

	t.Run("Update_Sealed_Rejected", func(t *testing.T) {
		s, _, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		fence := fenceFor(t, id)
		row, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id), fence)
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		sealed := row
		sealed.Status = turns.StatusComplete
		sealed.Sealed = true
		if _, err := s.SealTurnIf(ctx, id, "run-1", row.Version, sealed, fence); err != nil {
			t.Fatalf("seal: %v", err)
		}
		if _, err := s.UpdateTurnIf(ctx, id, "run-1", row.Version+1, row, fence); !errors.Is(err, turns.ErrTurnSealed) {
			t.Errorf("update of sealed row error=%v, want ErrTurnSealed", err)
		}
		if _, err := s.SealTurnIf(ctx, id, "run-1", row.Version+1, sealed, fence); !errors.Is(err, turns.ErrTurnSealed) {
			t.Errorf("re-seal error=%v, want ErrTurnSealed", err)
		}
	})

	t.Run("Update_Fenced_PendingErasure", func(t *testing.T) {
		s, st, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		fence := fenceFor(t, id)
		row, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id), fence)
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		writeFenceSlot(t, st, fence.PendingAbsent)
		if _, err := s.UpdateTurnIf(ctx, id, "run-1", row.Version, row, fence); !errors.Is(err, turns.ErrErasureFenced) {
			t.Errorf("fenced update error=%v, want ErrErasureFenced", err)
		}
	})

	t.Run("Seal_Terminal_ImmutableAfter", func(t *testing.T) {
		s, _, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		fence := fenceFor(t, id)
		row, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id), fence)
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		sealed := row
		sealed.Status = turns.StatusFailed
		sealed.Sealed = true
		sealed.ErrorClass = "runloop_error"
		got, err := s.SealTurnIf(ctx, id, "run-1", row.Version, sealed, fence)
		if err != nil {
			t.Fatalf("seal: %v", err)
		}
		if !got.Sealed || got.Status != turns.StatusFailed {
			t.Errorf("sealed row wrong: %+v", got)
		}
		if got.Version != row.Version+1 {
			t.Errorf("sealed version=%d, want %d", got.Version, row.Version+1)
		}
		// Immutable after: a second seal of the same row fails.
		if _, err := s.SealTurnIf(ctx, id, "run-1", got.Version, sealed, fence); !errors.Is(err, turns.ErrTurnSealed) {
			t.Errorf("second seal error=%v, want ErrTurnSealed", err)
		}
	})

	t.Run("Seal_StaleVersion_Rejected", func(t *testing.T) {
		s, _, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		fence := fenceFor(t, id)
		row, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id), fence)
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		sealed := row
		sealed.Sealed = true
		sealed.Status = turns.StatusCancelled
		if _, err := s.SealTurnIf(ctx, id, "run-1", row.Version+3, sealed, fence); !errors.Is(err, turns.ErrStaleVersion) {
			t.Errorf("stale seal error=%v, want ErrStaleVersion", err)
		}
	})

	t.Run("GetTurn_NotFound_And_CrossSessionIsolation", func(t *testing.T) {
		s, _, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		other := triple("tenant-b", "user-b", "session-b")
		fence := fenceFor(t, id)
		otherFence := fenceFor(t, other)
		if _, err := s.GetTurn(ctx, id, "run-1"); !errors.Is(err, turns.ErrTurnNotFound) {
			t.Errorf("missing get error=%v, want ErrTurnNotFound", err)
		}
		if _, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id), fence); err != nil {
			t.Fatalf("append: %v", err)
		}
		// The same turn id under a different session is NOT addressable.
		if _, err := s.GetTurn(ctx, other, "run-1"); !errors.Is(err, turns.ErrTurnNotFound) {
			t.Errorf("cross-session get error=%v, want ErrTurnNotFound", err)
		}
		// And a different session can own the same turn id freely.
		if _, err := s.AppendTurnIf(ctx, other, freshRow("run-1", other), otherFence); err != nil {
			t.Errorf("same turn id in another session must be independent: %v", err)
		}
	})

	t.Run("ListTurns_NewestFirst_KeysetPaging_NoSkipNoDuplicate", func(t *testing.T) {
		s, _, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		fence := fenceFor(t, id)
		const n = 25
		for i := 1; i <= n; i++ {
			if _, err := s.AppendTurnIf(ctx, id, freshRow(fmt.Sprintf("run-%02d", i), id), fence); err != nil {
				t.Fatalf("append %d: %v", i, err)
			}
		}
		var walked []turns.TurnRow
		var cursor *turns.Cursor
		for {
			rows, next, _, err := s.ListTurns(ctx, id, cursor, 7)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			walked = append(walked, rows...)
			if next == nil {
				break
			}
			cursor = next
		}
		if len(walked) != n {
			t.Fatalf("walked %d rows, want %d (no skips)", len(walked), n)
		}
		seen := map[string]bool{}
		for i, row := range walked {
			if seen[string(row.TurnID)] {
				t.Errorf("duplicate %q in walk (no duplicates)", row.TurnID)
			}
			seen[string(row.TurnID)] = true
			if i > 0 && walked[i-1].Sequence <= row.Sequence {
				t.Errorf("walk not newest-first at %q", row.TurnID)
			}
		}
	})

	t.Run("ListTurns_EmptySession", func(t *testing.T) {
		s, _, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		rows, next, truncated, err := s.ListTurns(ctx, id, nil, 10)
		if err != nil {
			t.Fatalf("list empty: %v", err)
		}
		if len(rows) != 0 || next != nil || truncated {
			t.Errorf("empty session page=%d rows next=%v truncated=%v, want 0/nil/false", len(rows), next, truncated)
		}
	})

	t.Run("Checkpoint_MonotonicIdempotent", func(t *testing.T) {
		s, _, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		if seq, err := s.LoadCheckpoint(ctx, id); err != nil || seq != 0 {
			t.Fatalf("fresh checkpoint=%d err=%v, want 0 nil", seq, err)
		}
		if err := s.SaveCheckpoint(ctx, id, 10); err != nil {
			t.Fatalf("save 10: %v", err)
		}
		if seq, _ := s.LoadCheckpoint(ctx, id); seq != 10 {
			t.Errorf("checkpoint=%d, want 10", seq)
		}
		// A regress or same-value save is a no-op, never an error.
		if err := s.SaveCheckpoint(ctx, id, 5); err != nil {
			t.Errorf("regress error=%v, want nil", err)
		}
		if err := s.SaveCheckpoint(ctx, id, 10); err != nil {
			t.Errorf("same-value error=%v, want nil", err)
		}
		if seq, _ := s.LoadCheckpoint(ctx, id); seq != 10 {
			t.Errorf("checkpoint after regress=%d, want 10 (never regresses)", seq)
		}
	})

	t.Run("Checkpoint_ConcurrentAdvances_ConvergeToMax", func(t *testing.T) {
		s, _, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		const writers = 16
		start := make(chan struct{})
		var wg sync.WaitGroup
		for w := 0; w < writers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				<-start
				for v := 0; v < 20; v++ {
					_ = s.SaveCheckpoint(ctx, id, uint64(w*20+v))
				}
			}(w)
		}
		close(start)
		wg.Wait()
		seq, err := s.LoadCheckpoint(ctx, id)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		if seq != uint64(writers*20-1) {
			t.Errorf("checkpoint=%d, want %d (concurrent advances converge to the max)", seq, uint64(writers*20-1))
		}
	})

	t.Run("DeleteScope_RemovesRowsAndCheckpoint", func(t *testing.T) {
		s, _, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		other := triple("tenant-b", "user-b", "session-b")
		fence := fenceFor(t, id)
		otherFence := fenceFor(t, other)
		if _, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id), fence); err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := s.SaveCheckpoint(ctx, id, 7); err != nil {
			t.Fatalf("checkpoint: %v", err)
		}
		// A sibling session is untouched by the scope delete.
		if _, err := s.AppendTurnIf(ctx, other, freshRow("run-1", other), otherFence); err != nil {
			t.Fatalf("append other: %v", err)
		}
		n, err := s.DeleteScope(ctx, id)
		if err != nil {
			t.Fatalf("DeleteScope: %v", err)
		}
		if n < 2 {
			t.Errorf("DeleteScope removed %d records, want >= 2", n)
		}
		if _, err := s.GetTurn(ctx, id, "run-1"); !errors.Is(err, turns.ErrTurnNotFound) {
			t.Errorf("post-erase get error=%v, want ErrTurnNotFound", err)
		}
		if seq, _ := s.LoadCheckpoint(ctx, id); seq != 0 {
			t.Errorf("post-erase checkpoint=%d, want 0", seq)
		}
		// Idempotent.
		if _, err := s.DeleteScope(ctx, id); err != nil {
			t.Errorf("re-delete: %v", err)
		}
		if _, err := s.GetTurn(ctx, other, "run-1"); err != nil {
			t.Errorf("sibling session must survive the scope delete: %v", err)
		}
	})

	t.Run("Concurrent_AppendList_NoRace", func(t *testing.T) {
		s, _, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		fence := fenceFor(t, id)
		const appenders = 8
		const perAppender = 8
		start := make(chan struct{})
		errs := make(chan error, appenders)
		var wg sync.WaitGroup
		for w := 0; w < appenders; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				<-start
				for i := 0; i < perAppender; i++ {
					if _, err := s.AppendTurnIf(ctx, id, freshRow(fmt.Sprintf("w%d-%02d", w, i), id), fence); err != nil {
						errs <- err
						return
					}
				}
			}(w)
		}
		close(start)
		wg.Wait()
		select {
		case err := <-errs:
			t.Fatalf("append: %v", err)
		default:
		}
		var walked []turns.TurnRow
		var cursor *turns.Cursor
		for {
			rows, next, _, err := s.ListTurns(ctx, id, cursor, 5)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			walked = append(walked, rows...)
			if next == nil {
				break
			}
			cursor = next
		}
		if len(walked) != appenders*perAppender {
			t.Fatalf("walked %d rows, want %d", len(walked), appenders*perAppender)
		}
		seen := map[string]bool{}
		for _, row := range walked {
			if seen[string(row.TurnID)] {
				t.Errorf("duplicate %q", row.TurnID)
			}
			seen[string(row.TurnID)] = true
		}
	})

	t.Run("Close_Then_Rejects", func(t *testing.T) {
		s, _, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		fence := fenceFor(t, id)
		if err := s.Close(ctx); err != nil {
			t.Fatalf("close: %v", err)
		}
		if err := s.Close(ctx); err != nil {
			t.Errorf("second close error=%v, want nil (idempotent)", err)
		}
		if _, err := s.AppendTurnIf(ctx, id, freshRow("r", id), fence); !errors.Is(err, turns.ErrStoreClosed) {
			t.Errorf("post-close append error=%v, want ErrStoreClosed", err)
		}
		if _, err := s.GetTurn(ctx, id, "r"); !errors.Is(err, turns.ErrStoreClosed) {
			t.Errorf("post-close get error=%v, want ErrStoreClosed", err)
		}
		if _, _, _, err := s.ListTurns(ctx, id, nil, 5); !errors.Is(err, turns.ErrStoreClosed) {
			t.Errorf("post-close list error=%v, want ErrStoreClosed", err)
		}
	})

	// The store contract's goroutine-leak gate: the mini-driver spawns
	// no goroutines, but a driver that does must join them before Close
	// returns. The leak check is a smoke assertion of the contract.
	t.Run("GoroutineLeak_AfterClose", func(t *testing.T) {
		s, _, cleanup := factory()
		baseline := runtime.NumGoroutine()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		fence := fenceFor(t, id)
		for i := 0; i < 10; i++ {
			if _, err := s.AppendTurnIf(ctx, id, freshRow(fmt.Sprintf("r-%d", i), id), fence); err != nil {
				t.Fatalf("append: %v", err)
			}
		}
		if err := s.Close(ctx); err != nil {
			t.Fatalf("close: %v", err)
		}
		cleanup()
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if runtime.NumGoroutine() <= baseline+1 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if got := runtime.NumGoroutine(); got > baseline+1 {
			t.Errorf("goroutine leak: baseline=%d after=%d", baseline, got)
		}
	})
}

func triple(tenant, user, session string) identity.Identity {
	return identity.Identity{TenantID: tenant, UserID: user, SessionID: session}
}

func fenceFor(t *testing.T, id identity.Identity) turns.Fence {
	t.Helper()
	fence, err := turns.FenceFor(id)
	if err != nil {
		t.Fatalf("FenceFor: %v", err)
	}
	return fence
}

// freshRow builds a minimal valid mutable turn row (the store mints the
// sequence; the driver must not require a caller-supplied one).
func freshRow(turnID string, id identity.Identity) turns.TurnRow {
	return turns.TurnRow{
		TurnID:     turns.TurnID(turnID),
		SessionID:  id.SessionID,
		TieBreaker: turns.TurnID(turnID),
		Status:     turns.StatusRunning,
		Query:      turns.Query{Text: "q", Complete: turns.CompletenessComplete},
	}
}

// writeFenceSlot writes a present erasure fence slot into the shared
// StateStore (the shape the runtime's erasure cascade produces).
func writeFenceSlot(t *testing.T, st state.StateStore, slot turns.Slot) {
	t.Helper()
	if err := st.Save(context.Background(), state.StateRecord{
		ID: state.NewEventID(), Identity: slot.Identity, Kind: slot.Kind, Bytes: []byte(`{}`),
	}); err != nil {
		t.Fatalf("write fence slot %q: %v", slot.Kind, err)
	}
}
