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
//	    conformancetest.Run(t, func() (turns.Store, func()) {
//	        s := mydriver.MustNew(t)
//	        return s, func() { _ = s.Close(context.Background()) }
//	    })
//	}
//
// The factory must return a fresh, empty Store plus a cleanup closure.
// Erasure fencing is STORE-LOCAL: the suite exercises the driver's own
// FenceSession / DeleteScope contract, and a driver never inspects
// external StateStore slots (no cross-runtime authority). The suite
// uses the factory once per top-level subtest; invocations are
// independent.
package conformancetest

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// Factory builds a fresh turns.Store and returns a cleanup closure.
type Factory func() (turns.Store, func())

// Run executes the canonical correctness suite.
//
// subtests:
//
//   - Identity_Mandatory
//   - Append_MintsMonotonicPerSessionSequences
//   - Append_Idempotent_ExistingTurnReturnsExistingRow
//   - Append_Fenced_AfterFenceSession
//   - Update_AdvancesVersion_AndReplacesComponents
//   - Update_StaleVersion_Rejected
//   - Update_Sealed_Rejected
//   - Update_Fenced_AfterFenceSession
//   - Seal_Terminal_ImmutableAfter
//   - Seal_StaleVersion_Rejected
//   - GetTurn_NotFound_And_CrossSessionIsolation
//   - ListTurns_NewestFirst_KeysetPaging_NoSkipNoDuplicate
//   - ListTurns_EmptySession
//   - ListTurns_Cursor_BindsSessionAndSnapshot
//   - ListTurns_Cursor_BindsAuthoritativeBoundaryRow
//   - ListTurns_AppendDuringWalk_NoSkipNoDuplicate
//   - Checkpoint_MonotonicIdempotent
//   - Checkpoint_ConcurrentAdvances_ConvergeToMax
//   - FenceSession_FencesAllWrites_AndSurvivesErase
//   - DeleteScope_RemovesRowsAndCheckpoint_KeepsFence
//   - Row_HonestFields_RoundTrip
//   - Row_DeepCopy_NoAliasing
//   - Concurrent_AppendList_NoRace
//   - Close_Then_Rejects
func Run(t *testing.T, factory Factory) {
	t.Helper()

	t.Run("Identity_Mandatory", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		zero := identity.Identity{}
		if _, err := s.AppendTurnIf(ctx, zero, turns.TurnRow{TurnID: "r"}); !errors.Is(err, turns.ErrIdentityRequired) {
			t.Errorf("AppendTurnIf zero identity error=%v, want ErrIdentityRequired", err)
		}
		if _, err := s.UpdateTurnIf(ctx, zero, "r", 1, turns.TurnRow{}); !errors.Is(err, turns.ErrIdentityRequired) {
			t.Errorf("UpdateTurnIf zero identity error=%v, want ErrIdentityRequired", err)
		}
		if _, err := s.SealTurnIf(ctx, zero, "r", 1, turns.TurnRow{}); !errors.Is(err, turns.ErrIdentityRequired) {
			t.Errorf("SealTurnIf zero identity error=%v, want ErrIdentityRequired", err)
		}
		if err := s.FenceSession(ctx, zero); !errors.Is(err, turns.ErrIdentityRequired) {
			t.Errorf("FenceSession zero identity error=%v, want ErrIdentityRequired", err)
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
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		other := triple("tenant-b", "user-b", "session-b")

		r1 := freshRow("run-1", id)
		got1, err := s.AppendTurnIf(ctx, id, r1)
		if err != nil {
			t.Fatalf("append 1: %v", err)
		}
		if got1.Sequence != 1 {
			t.Errorf("first sequence=%d, want 1", got1.Sequence)
		}
		got2, err := s.AppendTurnIf(ctx, id, freshRow("run-2", id))
		if err != nil {
			t.Fatalf("append 2: %v", err)
		}
		if got2.Sequence != 2 {
			t.Errorf("second sequence=%d, want 2", got2.Sequence)
		}
		// A different session restarts the counter.
		gotOther, err := s.AppendTurnIf(ctx, other, freshRow("run-1", other))
		if err != nil {
			t.Fatalf("append other: %v", err)
		}
		if gotOther.Sequence != 1 {
			t.Errorf("other-session sequence=%d, want 1 (per-session counters)", gotOther.Sequence)
		}
	})

	t.Run("Append_Idempotent_ExistingTurnReturnsExistingRow", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		first, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id))
		if err != nil {
			t.Fatalf("first append: %v", err)
		}
		// A re-append (a replay of an already-applied observation)
		// returns the existing row unchanged — a no-op, never an error
		// and never an overwrite.
		dup := freshRow("run-1", id)
		dup.Query = turns.Query{Text: "MUST NOT OVERWRITE", Complete: turns.CompletenessComplete}
		second, err := s.AppendTurnIf(ctx, id, dup)
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

	t.Run("Append_Fenced_AfterFenceSession", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		if _, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id)); err != nil {
			t.Fatalf("pre-fence append: %v", err)
		}
		// The erasure cascade sets the STORE-LOCAL fence first.
		if err := s.FenceSession(ctx, id); err != nil {
			t.Fatalf("FenceSession: %v", err)
		}
		if _, err := s.AppendTurnIf(ctx, id, freshRow("run-2", id)); !errors.Is(err, turns.ErrErasureFenced) {
			t.Errorf("append after fence error=%v, want ErrErasureFenced", err)
		}
		// Fencing again is an idempotent no-op.
		if err := s.FenceSession(ctx, id); err != nil {
			t.Errorf("re-fence error=%v, want nil (idempotent)", err)
		}
	})

	t.Run("Update_AdvancesVersion_AndReplacesComponents", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		row, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id))
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		next := row
		next.Answer = turns.Answer{State: turns.AnswerStateInline, Inline: "hello"}
		got, err := s.UpdateTurnIf(ctx, id, "run-1", row.Version, next)
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
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		row, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id))
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if _, err := s.UpdateTurnIf(ctx, id, "run-1", row.Version+5, row); !errors.Is(err, turns.ErrStaleVersion) {
			t.Errorf("stale update error=%v, want ErrStaleVersion", err)
		}
		if _, err := s.UpdateTurnIf(ctx, id, "no-such", 1, row); !errors.Is(err, turns.ErrTurnNotFound) {
			t.Errorf("missing update error=%v, want ErrTurnNotFound", err)
		}
	})

	t.Run("Update_Sealed_Rejected", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		row, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id))
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		sealed := row
		sealed.Status = turns.StatusComplete
		sealed.Sealed = true
		if _, err := s.SealTurnIf(ctx, id, "run-1", row.Version, sealed); err != nil {
			t.Fatalf("seal: %v", err)
		}
		if _, err := s.UpdateTurnIf(ctx, id, "run-1", row.Version+1, row); !errors.Is(err, turns.ErrTurnSealed) {
			t.Errorf("update of sealed row error=%v, want ErrTurnSealed", err)
		}
		if _, err := s.SealTurnIf(ctx, id, "run-1", row.Version+1, sealed); !errors.Is(err, turns.ErrTurnSealed) {
			t.Errorf("re-seal error=%v, want ErrTurnSealed", err)
		}
	})

	t.Run("Update_Fenced_AfterFenceSession", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		row, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id))
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := s.FenceSession(ctx, id); err != nil {
			t.Fatalf("FenceSession: %v", err)
		}
		if _, err := s.UpdateTurnIf(ctx, id, "run-1", row.Version, row); !errors.Is(err, turns.ErrErasureFenced) {
			t.Errorf("fenced update error=%v, want ErrErasureFenced", err)
		}
	})

	t.Run("Seal_Terminal_ImmutableAfter", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		row, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id))
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		sealed := row
		sealed.Status = turns.StatusFailed
		sealed.Sealed = true
		sealed.ErrorClass = turns.ErrorClassTransient
		got, err := s.SealTurnIf(ctx, id, "run-1", row.Version, sealed)
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
		if _, err := s.SealTurnIf(ctx, id, "run-1", got.Version, sealed); !errors.Is(err, turns.ErrTurnSealed) {
			t.Errorf("second seal error=%v, want ErrTurnSealed", err)
		}
	})

	t.Run("Seal_StaleVersion_Rejected", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		row, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id))
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		sealed := row
		sealed.Sealed = true
		sealed.Status = turns.StatusCancelled
		if _, err := s.SealTurnIf(ctx, id, "run-1", row.Version+3, sealed); !errors.Is(err, turns.ErrStaleVersion) {
			t.Errorf("stale seal error=%v, want ErrStaleVersion", err)
		}
	})

	t.Run("GetTurn_NotFound_And_CrossSessionIsolation", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		other := triple("tenant-b", "user-b", "session-b")
		if _, err := s.GetTurn(ctx, id, "run-1"); !errors.Is(err, turns.ErrTurnNotFound) {
			t.Errorf("missing get error=%v, want ErrTurnNotFound", err)
		}
		if _, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id)); err != nil {
			t.Fatalf("append: %v", err)
		}
		// The same turn id under a different session is NOT addressable.
		if _, err := s.GetTurn(ctx, other, "run-1"); !errors.Is(err, turns.ErrTurnNotFound) {
			t.Errorf("cross-session get error=%v, want ErrTurnNotFound", err)
		}
		// And a different session can own the same turn id freely.
		if _, err := s.AppendTurnIf(ctx, other, freshRow("run-1", other)); err != nil {
			t.Errorf("same turn id in another session must be independent: %v", err)
		}
	})

	t.Run("ListTurns_NewestFirst_KeysetPaging_NoSkipNoDuplicate", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		const n = 25
		for i := 1; i <= n; i++ {
			if _, err := s.AppendTurnIf(ctx, id, freshRow(fmt.Sprintf("run-%02d", i), id)); err != nil {
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
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		rows, next, info, err := s.ListTurns(ctx, id, nil, 10)
		if err != nil {
			t.Fatalf("list empty: %v", err)
		}
		if len(rows) != 0 || next != nil || info.Truncated {
			t.Errorf("empty session page=%d rows next=%v truncated=%v, want 0/nil/false", len(rows), next, info.Truncated)
		}
		if info.Snapshot != 0 {
			t.Errorf("fresh-session snapshot=%d, want 0 (initial generation)", info.Snapshot)
		}
	})

	t.Run("ListTurns_Cursor_BindsSessionAndSnapshot", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		other := triple("tenant-b", "user-b", "session-b")
		for i := 1; i <= 5; i++ {
			if _, err := s.AppendTurnIf(ctx, id, freshRow(fmt.Sprintf("run-%d", i), id)); err != nil {
				t.Fatalf("append %d: %v", i, err)
			}
		}
		page, next, info, err := s.ListTurns(ctx, id, nil, 2)
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		if next == nil || len(page) != 2 || info.CountExact != true || info.Remaining != 3 {
			t.Errorf("page 1 wrong: %d rows next=%v remaining=%d count_exact=%v, want 2/non-nil/3/true",
				len(page), next != nil, info.Remaining, info.CountExact)
		}
		// The minted cursor binds the owning session + snapshot.
		if next.SessionID != id.SessionID {
			t.Errorf("cursor session=%q, want %q", next.SessionID, id.SessionID)
		}
		if next.Snapshot != info.Snapshot {
			t.Errorf("cursor snapshot=%d, want page snapshot %d", next.Snapshot, info.Snapshot)
		}
		// A FOREIGN-SESSION cursor is rejected with its distinct error.
		foreign := *next
		foreign.SessionID = other.SessionID
		if _, _, _, err := s.ListTurns(ctx, id, &foreign, 2); !errors.Is(err, turns.ErrCursorForeignSession) {
			t.Errorf("foreign-session cursor error=%v, want ErrCursorForeignSession", err)
		}
		// A STALE-SNAPSHOT cursor (projection erased under the walk) is
		// rejected with its distinct error.
		stale := *next
		stale.Snapshot++
		if _, _, _, err := s.ListTurns(ctx, id, &stale, 2); !errors.Is(err, turns.ErrCursorSnapshotStale) {
			t.Errorf("stale-snapshot cursor error=%v, want ErrCursorSnapshotStale", err)
		}
		// An EXPIRED cursor (boundary row never retained) is rejected
		// with its distinct error.
		expired := *next
		expired.TurnID = "never-existed"
		if _, _, _, err := s.ListTurns(ctx, id, &expired, 2); !errors.Is(err, turns.ErrCursorExpired) {
			t.Errorf("expired cursor error=%v, want ErrCursorExpired", err)
		}
		// A valid cursor still pages (no skips / duplicates).
		page2, next2, _, err := s.ListTurns(ctx, id, next, 2)
		if err != nil {
			t.Fatalf("page 2 with valid cursor: %v", err)
		}
		if len(page2) != 2 || next2 == nil {
			t.Errorf("page 2 wrong: %d rows next=%v, want 2/non-nil", len(page2), next2 != nil)
		}
		// After erasure (DeleteScope), the pre-erase cursor is stale:
		// the snapshot generation advanced.
		if _, err := s.DeleteScope(ctx, id); err != nil {
			t.Fatalf("DeleteScope: %v", err)
		}
		if _, _, _, err := s.ListTurns(ctx, id, next, 2); !errors.Is(err, turns.ErrCursorSnapshotStale) {
			t.Errorf("post-erase cursor error=%v, want ErrCursorSnapshotStale", err)
		}
	})

	t.Run("ListTurns_Cursor_BindsAuthoritativeBoundaryRow", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		for i := 1; i <= 5; i++ {
			if _, err := s.AppendTurnIf(ctx, id, freshRow(fmt.Sprintf("run-%d", i), id)); err != nil {
				t.Fatalf("append %d: %v", i, err)
			}
		}
		page, next, _, err := s.ListTurns(ctx, id, nil, 2)
		if err != nil {
			t.Fatalf("page 1: %v", err)
		}
		if next == nil || len(page) != 2 {
			t.Fatalf("page 1: %d rows next=%v, want 2/non-nil", len(page), next != nil)
		}
		// A forged / altered cursor: same session, same snapshot, same
		// RETAINED boundary turn id — but a sequence that does not equal
		// the stored boundary row's immutable sequence. It must fail
		// with the typed invalid-cursor behavior, never silently
		// re-keyset (which would skip or repeat rows).
		for _, delta := range []int64{1000, -1} {
			forged := *next
			forged.Seq = next.Seq + turns.Seq(delta)
			if _, _, _, err := s.ListTurns(ctx, id, &forged, 2); !errors.Is(err, turns.ErrInvalidCursor) {
				t.Errorf("forged cursor (seq %d) error=%v, want ErrInvalidCursor", forged.Seq, err)
			}
		}
		// The forged cursor is NOT one of the distinct binding errors:
		// the session and snapshot bindings are intact and the boundary
		// row IS retained — only the sequence is forged.
		forged := *next
		forged.Seq = next.Seq + 1000
		if _, _, _, err := s.ListTurns(ctx, id, &forged, 2); errors.Is(err, turns.ErrCursorForeignSession) ||
			errors.Is(err, turns.ErrCursorSnapshotStale) || errors.Is(err, turns.ErrCursorExpired) {
			t.Errorf("forged-seq cursor misclassified as a distinct binding error: %v", err)
		}
		// The genuine cursor still pages with no skips / duplicates.
		page2, next2, _, err := s.ListTurns(ctx, id, next, 2)
		if err != nil {
			t.Fatalf("genuine cursor page 2: %v", err)
		}
		if len(page2) != 2 || next2 == nil {
			t.Errorf("genuine cursor page 2: %d rows next=%v, want 2/non-nil", len(page2), next2 != nil)
		}
	})

	t.Run("ListTurns_AppendDuringWalk_NoSkipNoDuplicate", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		for i := 1; i <= 8; i++ {
			if _, err := s.AppendTurnIf(ctx, id, freshRow(fmt.Sprintf("pre-%d", i), id)); err != nil {
				t.Fatalf("append pre-%d: %v", i, err)
			}
		}
		// Page the first two pages, appending between them: a newly
		// appended turn (higher sequence) can never satisfy the issued
		// cursor, so the walk keeps returning exactly the pre-append
		// rows once each.
		page1, next, _, err := s.ListTurns(ctx, id, nil, 3)
		if err != nil || len(page1) != 3 || next == nil {
			t.Fatalf("page 1: %d rows next=%v err=%v, want 3/non-nil/nil", len(page1), next != nil, err)
		}
		if _, err := s.AppendTurnIf(ctx, id, freshRow("mid-append", id)); err != nil {
			t.Fatalf("mid-walk append: %v", err)
		}
		var walked []string
		for _, r := range page1 {
			walked = append(walked, string(r.TurnID))
		}
		for next != nil {
			rows, n2, _, err := s.ListTurns(ctx, id, next, 3)
			if err != nil {
				t.Fatalf("walk page: %v", err)
			}
			for _, r := range rows {
				walked = append(walked, string(r.TurnID))
			}
			next = n2
		}
		if len(walked) != 8 {
			t.Fatalf("walked %d rows, want 8 (the pre-append rows exactly once; the mid-append is NEWER and never satisfies the walk)", len(walked))
		}
		seen := map[string]bool{}
		for _, tid := range walked {
			if seen[tid] {
				t.Errorf("duplicate %q in walk (no duplicates)", tid)
			}
			seen[tid] = true
		}
		for _, r := range walked {
			if r == "mid-append" {
				t.Errorf("mid-walk append %q appeared in the older walk — a newer row cannot satisfy an issued cursor", r)
			}
		}
		// A fresh walk sees everything exactly once (no skips).
		fresh := 0
		var c *turns.Cursor
		for {
			rows, n2, _, err := s.ListTurns(ctx, id, c, 3)
			if err != nil {
				t.Fatalf("fresh walk: %v", err)
			}
			fresh += len(rows)
			if n2 == nil {
				break
			}
			c = n2
		}
		if fresh != 9 {
			t.Errorf("fresh walk has %d rows, want 9 (8 pre + 1 mid)", fresh)
		}
	})

	t.Run("Checkpoint_MonotonicIdempotent", func(t *testing.T) {
		s, cleanup := factory()
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
		s, cleanup := factory()
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

	t.Run("FenceSession_FencesAllWrites_AndSurvivesErase", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		other := triple("tenant-b", "user-b", "session-b")
		row, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id))
		if err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := s.SaveCheckpoint(ctx, id, 7); err != nil {
			t.Fatalf("checkpoint: %v", err)
		}
		// Fencing the session refuses EVERY write path.
		if err := s.FenceSession(ctx, id); err != nil {
			t.Fatalf("FenceSession: %v", err)
		}
		if _, err := s.AppendTurnIf(ctx, id, freshRow("run-2", id)); !errors.Is(err, turns.ErrErasureFenced) {
			t.Errorf("fenced append error=%v, want ErrErasureFenced", err)
		}
		if _, err := s.UpdateTurnIf(ctx, id, "run-1", row.Version, row); !errors.Is(err, turns.ErrErasureFenced) {
			t.Errorf("fenced update error=%v, want ErrErasureFenced", err)
		}
		sealed := row
		sealed.Sealed = true
		sealed.Status = turns.StatusCancelled
		if _, err := s.SealTurnIf(ctx, id, "run-1", row.Version, sealed); !errors.Is(err, turns.ErrErasureFenced) {
			t.Errorf("fenced seal error=%v, want ErrErasureFenced", err)
		}
		if err := s.SaveCheckpoint(ctx, id, 8); !errors.Is(err, turns.ErrErasureFenced) {
			t.Errorf("fenced checkpoint error=%v, want ErrErasureFenced (rebuild must not advance an erased session)", err)
		}
		// A sibling session is untouched by the fence.
		if _, err := s.AppendTurnIf(ctx, other, freshRow("run-1", other)); err != nil {
			t.Errorf("sibling session must survive the fence: %v", err)
		}
		// The erasure cascade deletes the scope; the FENCE SURVIVES the
		// erase — an erased session stays fenced (no resurrection after
		// replay / restart).
		if _, err := s.DeleteScope(ctx, id); err != nil {
			t.Fatalf("DeleteScope: %v", err)
		}
		if _, err := s.GetTurn(ctx, id, "run-1"); !errors.Is(err, turns.ErrTurnNotFound) {
			t.Errorf("post-erase get error=%v, want ErrTurnNotFound", err)
		}
		if seq, _ := s.LoadCheckpoint(ctx, id); seq != 0 {
			t.Errorf("post-erase checkpoint=%d, want 0", seq)
		}
		if _, err := s.AppendTurnIf(ctx, id, freshRow("run-3", id)); !errors.Is(err, turns.ErrErasureFenced) {
			t.Errorf("post-erase append error=%v, want ErrErasureFenced (fence survives erase)", err)
		}
		if err := s.SaveCheckpoint(ctx, id, 9); !errors.Is(err, turns.ErrErasureFenced) {
			t.Errorf("post-erase checkpoint error=%v, want ErrErasureFenced", err)
		}
		// Re-erase stays idempotent on the fenced session.
		if n, err := s.DeleteScope(ctx, id); err != nil || n != 0 {
			t.Errorf("re-erase = (%d, %v), want (0, nil)", n, err)
		}
		if err := s.FenceSession(ctx, id); err != nil {
			t.Errorf("re-fence error=%v, want nil (idempotent)", err)
		}
	})

	t.Run("DeleteScope_RemovesRowsAndCheckpoint_KeepsFence", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		other := triple("tenant-b", "user-b", "session-b")
		if _, err := s.AppendTurnIf(ctx, id, freshRow("run-1", id)); err != nil {
			t.Fatalf("append: %v", err)
		}
		if err := s.SaveCheckpoint(ctx, id, 7); err != nil {
			t.Fatalf("checkpoint: %v", err)
		}
		// A sibling session is untouched by the scope delete.
		if _, err := s.AppendTurnIf(ctx, other, freshRow("run-1", other)); err != nil {
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

	t.Run("Row_HonestFields_RoundTrip", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		row := freshRow("run-1", id)
		row.Query.At = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)
		row.Agent = turns.Agent{ID: "agent-1", Name: "Agent One", BindingSource: turns.AgentBindingExplicit, Complete: turns.CompletenessComplete}
		row.Inputs = []turns.Attachment{{ID: "in_1", Filename: "f.txt", Availability: turns.CompletenessComplete}}
		row.Outputs = []turns.Attachment{{ID: "out_1", Availability: turns.CompletenessUnavailable}}
		row.Pause = turns.Pause{Class: turns.PauseClassHitlApproval, Reason: "approval", Lifecycle: turns.PauseLifecycleActive, Availability: turns.CompletenessComplete}
		row.Answer = turns.Answer{State: turns.AnswerStateArtifactRef, Ref: &turns.AnswerRef{ID: "art_1", SizeBytes: 4096}}
		row.LastAppliedEventSeq = 42
		row.Apps = []turns.AppRef{
			{EffectiveAgentID: "agent-1", ServerID: "srv-1", ResourceURI: "ui://srv/app", DisplayMode: "inline", RawHTMLTrusted: false, ToolCallID: "tc-1", ToolName: "tool-a", Availability: turns.AppAvailable, Complete: turns.CompletenessComplete},
			{EffectiveAgentID: "", ServerID: "srv-2", ResourceURI: "ui://srv/app2", ToolName: "tool-b", Availability: turns.AppDegraded, Complete: turns.CompletenessComplete},
		}
		// The changed core DTO: per-measure usage, derived reasoning,
		// content-free activity with exact totals, and closed terminal
		// enums must survive a driver round-trip byte-for-byte.
		prompt := int64(100)
		total := int64(150)
		row.Usage = turns.Usage{
			PromptTokens: turns.UsageMeasure{State: turns.UsageExact, Value: &prompt},
			TotalTokens:  turns.UsageMeasure{State: turns.UsageExact, Value: &total},
			Model:        "model-x",
		}
		row.Reasoning = turns.Reasoning{
			Steps:    []turns.ReasoningStep{{Index: 0, Kind: turns.ReasoningKindToolCall}, {Index: 2, Kind: turns.ReasoningKindSpawn}},
			Complete: turns.CompletenessComplete,
			Seq:      7,
		}
		row.Activity = turns.Activity{
			Rows:     []turns.ActivityRow{{Position: 0, Tool: "t1", Status: turns.ActivitySucceeded, TerminalClass: turns.ActivityTerminalSucceeded}},
			Complete: turns.CompletenessComplete,
			Totals:   turns.ActivityTotals{Succeeded: 1},
		}
		row.FinishReason = turns.FinishGoal
		row.ErrorClass = turns.ErrorClassTransient
		row.FinishMessage = "goal reached"
		row.ErrorMessage = "recovered transiently"
		if _, err := s.AppendTurnIf(ctx, id, row); err != nil {
			t.Fatalf("append: %v", err)
		}
		read, err := s.GetTurn(ctx, id, "run-1")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if !read.Query.At.Equal(row.Query.At) {
			t.Errorf("Query.At lost: %v, want %v", read.Query.At, row.Query.At)
		}
		if read.Agent.BindingSource != turns.AgentBindingExplicit || read.Agent.ID != "agent-1" {
			t.Errorf("Agent binding source lost: %+v", read.Agent)
		}
		if len(read.Inputs) != 1 || read.Inputs[0].Availability != turns.CompletenessComplete {
			t.Errorf("input attachment availability lost: %+v", read.Inputs)
		}
		if len(read.Outputs) != 1 || read.Outputs[0].Availability != turns.CompletenessUnavailable {
			t.Errorf("output attachment availability lost: %+v", read.Outputs)
		}
		if read.Pause.Class != turns.PauseClassHitlApproval || read.Pause.Lifecycle != turns.PauseLifecycleActive || read.Pause.Availability != turns.CompletenessComplete {
			t.Errorf("pause component lost: %+v", read.Pause)
		}
		if read.Answer.State != turns.AnswerStateArtifactRef || read.Answer.Ref == nil || read.Answer.Ref.ID != "art_1" {
			t.Errorf("answer state lost: %+v", read.Answer)
		}
		if read.LastAppliedEventSeq != 42 {
			t.Errorf("LastAppliedEventSeq lost: %d, want 42", read.LastAppliedEventSeq)
		}
		// Per-measure usage round-trips with availability + exact value.
		if m := read.Usage.PromptTokens; m.State != turns.UsageExact || m.Value == nil || *m.Value != 100 {
			t.Errorf("usage prompt tokens lost: %+v", read.Usage.PromptTokens)
		}
		// The driver is a transport: the unreported measure's state is
		// preserved byte-for-byte exactly as fed (the projector is the
		// normalizer; the driver must not invent availability). The
		// whole Usage struct must round-trip unmodified.
		if !reflect.DeepEqual(read.Usage, row.Usage) {
			t.Errorf("usage struct drifted through the driver:\n  got:  %+v\n  want: %+v", read.Usage, row.Usage)
		}
		if read.Usage.Model != "model-x" {
			t.Errorf("usage model lost: %q", read.Usage.Model)
		}
		// Derived reasoning round-trips as index+kind only.
		if len(read.Reasoning.Steps) != 2 || read.Reasoning.Steps[1].Kind != turns.ReasoningKindSpawn || read.Reasoning.Seq != 7 {
			t.Errorf("derived reasoning lost: %+v", read.Reasoning)
		}
		// Content-free activity + exact totals round-trip.
		if len(read.Activity.Rows) != 1 || read.Activity.Rows[0].Tool != "t1" || read.Activity.Rows[0].TerminalClass != turns.ActivityTerminalSucceeded {
			t.Errorf("activity row lost: %+v", read.Activity.Rows)
		}
		if read.Activity.Totals != (turns.ActivityTotals{Succeeded: 1}) {
			t.Errorf("activity totals lost: %+v", read.Activity.Totals)
		}
		// Closed terminal enums + bounded messages round-trip.
		if read.FinishReason != turns.FinishGoal || read.ErrorClass != turns.ErrorClassTransient ||
			read.FinishMessage != "goal reached" || read.ErrorMessage != "recovered transiently" {
			t.Errorf("terminal fields lost: reason=%q class=%q fm=%q em=%q",
				read.FinishReason, read.ErrorClass, read.FinishMessage, read.ErrorMessage)
		}
		if len(read.Apps) != 2 {
			t.Fatalf("apps collection lost: %d entries, want 2 (ordered)", len(read.Apps))
		}
		if read.Apps[0].ToolCallID != "tc-1" || read.Apps[0].ResourceURI != "ui://srv/app" || read.Apps[0].EffectiveAgentID != "agent-1" {
			t.Errorf("app ref 0 correlation/render metadata lost: %+v", read.Apps[0])
		}
		if read.Apps[1].Availability != turns.AppDegraded || read.Apps[1].ToolName != "tool-b" {
			t.Errorf("app ref 1 availability lost: %+v", read.Apps[1])
		}
		// The ORDER of the collection is preserved (first declaration).
		if read.Apps[0].ServerID != "srv-1" || read.Apps[1].ServerID != "srv-2" {
			t.Errorf("apps collection order lost: %+v", read.Apps)
		}
	})

	// Row_DeepCopy_NoAliasing pins the store's deep-copy obligation: a
	// driver must never let caller memory reach (or escape) durable
	// state. Caller mutation of pointer-backed mutable fields
	// (Answer.Ref, UsageMeasure.Value) and slices after a write — or of
	// a returned read row — must never corrupt the stored row. A
	// byte-serializing driver passes by construction; a driver that
	// retains structs must clone on every write and read boundary.
	t.Run("Row_DeepCopy_NoAliasing", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")

		value := int64(42)
		row := freshRow("run-1", id)
		row.Answer = turns.Answer{State: turns.AnswerStateArtifactRef, Ref: &turns.AnswerRef{ID: "art_1", SizeBytes: 8}}
		row.Usage = turns.Usage{PromptTokens: turns.UsageMeasure{State: turns.UsageExact, Value: &value}}
		row.Inputs = []turns.Attachment{{ID: "in_1", Filename: "f.txt"}}
		if _, err := s.AppendTurnIf(ctx, id, row); err != nil {
			t.Fatalf("append: %v", err)
		}
		// Corrupt the caller's row after the write.
		row.Answer.Ref.ID = "CORRUPTED"
		value = -1
		row.Inputs[0].ID = "CORRUPTED"
		got, err := s.GetTurn(ctx, id, "run-1")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if got.Answer.Ref == nil || got.Answer.Ref.ID != "art_1" {
			t.Errorf("append Answer.Ref aliased the caller's memory: %+v", got.Answer.Ref)
		}
		if got.Usage.PromptTokens.Value == nil || *got.Usage.PromptTokens.Value != 42 {
			t.Errorf("append UsageMeasure.Value aliased the caller's memory: %+v", got.Usage.PromptTokens)
		}
		if len(got.Inputs) != 1 || got.Inputs[0].ID != "in_1" {
			t.Errorf("append attachment slice aliased the caller's memory: %+v", got.Inputs)
		}

		// The update boundary has the same obligation.
		v2 := int64(7)
		next := got
		next.Answer.Ref = &turns.AnswerRef{ID: "art_2", SizeBytes: 16}
		next.Usage = turns.Usage{TotalTokens: turns.UsageMeasure{State: turns.UsageExact, Value: &v2}}
		got2, err := s.UpdateTurnIf(ctx, id, "run-1", got.Version, next)
		if err != nil {
			t.Fatalf("update: %v", err)
		}
		next.Answer.Ref.ID = "CORRUPTED"
		v2 = -1
		read2, err := s.GetTurn(ctx, id, "run-1")
		if err != nil {
			t.Fatalf("get 2: %v", err)
		}
		if read2.Answer.Ref == nil || read2.Answer.Ref.ID != "art_2" {
			t.Errorf("update Answer.Ref aliased the caller's memory: %+v", read2.Answer.Ref)
		}
		if read2.Usage.TotalTokens.Value == nil || *read2.Usage.TotalTokens.Value != 7 {
			t.Errorf("update UsageMeasure.Value aliased the caller's memory: %+v", read2.Usage.TotalTokens)
		}

		// The READ boundary must not expose driver-internal memory:
		// mutating a returned read row must never corrupt the stored
		// row.
		got2.Answer.Ref.ID = "MUTATED-READ"
		final, err := s.GetTurn(ctx, id, "run-1")
		if err != nil {
			t.Fatalf("get 3: %v", err)
		}
		if final.Answer.Ref == nil || final.Answer.Ref.ID != "art_2" {
			t.Errorf("mutating a read row corrupted the stored row: %+v", final.Answer.Ref)
		}
	})

	t.Run("Concurrent_AppendList_NoRace", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
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
					if _, err := s.AppendTurnIf(ctx, id, freshRow(fmt.Sprintf("w%d-%02d", w, i), id)); err != nil {
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
		s, cleanup := factory()
		defer cleanup()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		if err := s.Close(ctx); err != nil {
			t.Fatalf("close: %v", err)
		}
		if err := s.Close(ctx); err != nil {
			t.Errorf("second close error=%v, want nil (idempotent)", err)
		}
		if _, err := s.AppendTurnIf(ctx, id, freshRow("r", id)); !errors.Is(err, turns.ErrStoreClosed) {
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
		s, cleanup := factory()
		baseline := runtime.NumGoroutine()
		ctx := context.Background()
		id := triple("tenant-a", "user-a", "session-a")
		for i := 0; i < 10; i++ {
			if _, err := s.AppendTurnIf(ctx, id, freshRow(fmt.Sprintf("r-%d", i), id)); err != nil {
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
