package conformancetest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// RunMaterializerStore executes the turns.MaterializerStore suite (the
// single-writer lease, the fenced monotonic resume cursor, and the
// identity repair queue). The factory's store MUST also implement
// turns.MaterializerStore.
//
// subtests:
//
//   - Lease_Exclusive_RenewAndRelease
//   - Lease_ExpiryTakeover_FencesOldHolder
//   - Cursor_LeaseFenced_Monotonic
//   - Repair_FlagListFillSettle
//   - Repair_KeysetPaging_AcrossSessions
//   - Repair_Fenced_And_NotFound
//   - Fence_StaleGenerationRejectsEveryWrite
//   - PendingCompletion_MarkListClearedBySeal
//   - Routes_SaveLoadPruneErase
func RunMaterializerStore(t *testing.T, factory Factory) {
	t.Helper()
	open := func(t *testing.T) (turns.Store, turns.MaterializerStore) {
		t.Helper()
		s, cleanup := factory()
		t.Cleanup(cleanup)
		ms, ok := s.(turns.MaterializerStore)
		if !ok {
			t.Fatalf("store %T does not implement turns.MaterializerStore", s)
		}
		return s, ms
	}
	ctx := func(t *testing.T) context.Context {
		c, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		t.Cleanup(cancel)
		return c
	}

	runFenceSuite(t, open)
	runBootstrapSuite(t, open)

	t.Run("Routes_SaveLoadPruneErase", func(t *testing.T) {
		s, ms := open(t)
		c := context.Background()
		id := triple("t-route", "u", "s")
		other := triple("t-route", "u", "s-other")
		if _, err := s.AppendTurnIf(c, id, freshRow("root", id)); err != nil {
			t.Fatalf("append: %v", err)
		}
		for _, r := range []turns.TaskRoute{
			{TaskID: "root", RunID: "run-explicit", RootTaskID: "root", EventSeq: 1},
			{TaskID: "child", ParentTaskID: "root", RunID: "run-child", RootTaskID: "root", EventSeq: 2},
			{TaskID: "other-root", RunID: "run-reused", RootTaskID: "other-root", EventSeq: 3},
			{TaskID: "later", RunID: "run-reused", RootTaskID: "later", EventSeq: 9},
		} {
			if err := ms.SaveTaskRoute(c, id, r); err != nil {
				t.Fatalf("save %s: %v", r.TaskID, err)
			}
		}
		// An upsert without a run keeps the bound run.
		if err := ms.SaveTaskRoute(c, id, turns.TaskRoute{TaskID: "child", ParentTaskID: "root", EventSeq: 2}); err != nil {
			t.Fatalf("re-save: %v", err)
		}
		if r, ok, err := ms.LoadTaskRoute(c, id, "child"); err != nil || !ok || r.ParentTaskID != "root" || r.RunID != "run-child" || r.RootTaskID != "root" {
			t.Fatalf("child route = %+v %v %v", r, ok, err)
		}
		if r, ok, err := ms.LoadRunRoute(c, id, "run-explicit"); err != nil || !ok || r.TaskID != "root" {
			t.Fatalf("run route = %+v %v", r, ok)
		}
		if r, ok, err := ms.LoadRunRoute(c, id, "run-reused"); err != nil || !ok || r.TaskID != "later" {
			t.Fatalf("reused run resolves to %+v; want the most recent binding", r)
		}
		if _, ok, err := ms.LoadTaskRoute(c, other, "child"); err != nil || ok {
			t.Fatalf("a route leaked across sessions")
		}
		cur, err := s.GetTurn(c, id, "root")
		if err != nil {
			t.Fatalf("get turn: %v", err)
		}
		sealed := cur
		sealed.Status = turns.StatusCancelled
		if _, err := s.SealTurnIf(c, id, "root", cur.Version, sealed); err != nil {
			t.Fatalf("seal: %v", err)
		}
		if _, ok, err := ms.LoadTaskRoute(c, id, "child"); err != nil || ok {
			t.Fatalf("routes of a sealed root survived the seal")
		}
		if _, ok, err := ms.LoadTaskRoute(c, id, "later"); err != nil || !ok {
			t.Fatalf("an unrelated root's route was pruned")
		}
		if _, err := s.DeleteScope(c, id); err != nil {
			t.Fatalf("erase: %v", err)
		}
		if _, ok, err := ms.LoadTaskRoute(c, id, "later"); err != nil || ok {
			t.Fatalf("routes survived the session erasure")
		}
		if err := s.FenceSession(c, id); err != nil {
			t.Fatalf("fence: %v", err)
		}
		if err := ms.SaveTaskRoute(c, id, turns.TaskRoute{TaskID: "x"}); !errors.Is(err, turns.ErrErasureFenced) {
			t.Fatalf("fenced route save: %v", err)
		}
	})

	t.Run("PendingCompletion_MarkListClearedBySeal", func(t *testing.T) {
		s, ms := open(t)
		c := context.Background()
		id := triple("t-pend", "u", "s")
		for _, turn := range []string{"p1", "p2"} {
			if _, err := s.AppendTurnIf(c, id, freshRow(turn, id)); err != nil {
				t.Fatalf("append: %v", err)
			}
			if err := ms.MarkPendingCompletion(c, id, turns.TurnID(turn)); err != nil {
				t.Fatalf("mark %s: %v", turn, err)
			}
		}
		keys, err := ms.ListPendingCompletions(c, turns.IdentityRepairKey{}, 10)
		if err != nil || len(keys) != 2 {
			t.Fatalf("pending = %+v, %v; want both rows", keys, err)
		}
		if page, err := ms.ListPendingCompletions(c, keys[0], 10); err != nil || len(page) != 1 || page[0] != keys[1] {
			t.Fatalf("keyset page after %+v = %+v", keys[0], page)
		}
		cur, err := s.GetTurn(c, id, "p1")
		if err != nil {
			t.Fatalf("get turn: %v", err)
		}
		sealed := cur
		sealed.Status = turns.StatusComplete
		sealed.Answer = turns.Answer{State: turns.AnswerStateEmpty, Complete: turns.CompletenessComplete}
		if _, err := s.SealTurnIf(c, id, "p1", cur.Version, sealed); err != nil {
			t.Fatalf("seal: %v", err)
		}
		if err := ms.MarkPendingCompletion(c, id, "p1"); err != nil {
			t.Fatalf("mark sealed (no-op): %v", err)
		}
		if keys, err := ms.ListPendingCompletions(c, turns.IdentityRepairKey{}, 10); err != nil || len(keys) != 1 || keys[0].TurnID != "p2" {
			t.Fatalf("pending after seal = %+v; want only p2", keys)
		}
		if err := ms.MarkPendingCompletion(c, id, "missing"); !errors.Is(err, turns.ErrTurnNotFound) {
			t.Fatalf("missing: %v", err)
		}
		if err := s.FenceSession(c, id); err != nil {
			t.Fatalf("fence: %v", err)
		}
		if err := ms.MarkPendingCompletion(c, id, "p2"); !errors.Is(err, turns.ErrErasureFenced) {
			t.Fatalf("fenced: %v", err)
		}
	})

	t.Run("Lease_Exclusive_RenewAndRelease", func(t *testing.T) {
		_, ms := open(t)
		c := ctx(t)
		a, err := ms.AcquireMaterializerLease(c, "holder-a", time.Minute)
		if err != nil || !a.Held || a.Holder != "holder-a" {
			t.Fatalf("A acquire = %+v, %v; want held by A", a, err)
		}
		b, err := ms.AcquireMaterializerLease(c, "holder-b", time.Minute)
		if err != nil || b.Held || b.Holder != "holder-a" {
			t.Fatalf("B acquire while A holds = %+v, %v; want not held, holder A", b, err)
		}
		if r, err := ms.AcquireMaterializerLease(c, "holder-a", time.Minute); err != nil || !r.Held {
			t.Fatalf("A renew = %+v, %v; want held", r, err)
		}
		// Releasing someone else's lease is a no-op.
		if err := ms.ReleaseMaterializerLease(c, "holder-b"); err != nil {
			t.Fatalf("B release: %v", err)
		}
		if b, err := ms.AcquireMaterializerLease(c, "holder-b", time.Minute); err != nil || b.Held {
			t.Fatalf("B took the lease after releasing a lease it never held")
		}
		if err := ms.ReleaseMaterializerLease(c, "holder-a"); err != nil {
			t.Fatalf("A release: %v", err)
		}
		if b, err := ms.AcquireMaterializerLease(c, "holder-b", time.Minute); err != nil || !b.Held {
			t.Fatalf("B acquire after A released = %+v, %v; want held", b, err)
		}
		if _, err := ms.AcquireMaterializerLease(c, "", time.Minute); !errors.Is(err, turns.ErrInvalidInput) {
			t.Fatalf("empty holder: %v; want ErrInvalidInput", err)
		}
	})

	t.Run("Lease_ExpiryTakeover_FencesOldHolder", func(t *testing.T) {
		_, ms := open(t)
		c := ctx(t)
		if a, err := ms.AcquireMaterializerLease(c, "holder-a", time.Millisecond); err != nil || !a.Held {
			t.Fatalf("A acquire = %+v, %v", a, err)
		}
		// A stops renewing (a crashed instance). B polls until the
		// lease expires on the store clock — a bounded condition poll,
		// not a sleep-for-synchronization.
		deadline := time.Now().Add(10 * time.Second)
		for {
			b, err := ms.AcquireMaterializerLease(c, "holder-b", time.Minute)
			if err != nil {
				t.Fatalf("B acquire: %v", err)
			}
			if b.Held {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("B never took over the expired lease (holder %q)", b.Holder)
			}
			time.Sleep(5 * time.Millisecond)
		}
		if err := ms.SaveMaterializerCursor(c, "holder-a", 7); !errors.Is(err, turns.ErrLeaseNotHeld) {
			t.Fatalf("old holder cursor save: %v; want ErrLeaseNotHeld", err)
		}
		if a, err := ms.AcquireMaterializerLease(c, "holder-a", time.Minute); err != nil || a.Held {
			t.Fatalf("old holder re-took a live lease")
		}
	})

	t.Run("Cursor_LeaseFenced_Monotonic", func(t *testing.T) {
		_, ms := open(t)
		c := ctx(t)
		if l, err := ms.AcquireMaterializerLease(c, "holder-a", time.Minute); err != nil || l.Cursor != 0 {
			t.Fatalf("fresh acquire = %+v, %v; want cursor 0", l, err)
		}
		if err := ms.SaveMaterializerCursor(c, "holder-b", 5); !errors.Is(err, turns.ErrLeaseNotHeld) {
			t.Fatalf("non-holder save: %v; want ErrLeaseNotHeld", err)
		}
		if err := ms.SaveMaterializerCursor(c, "holder-a", 10); err != nil {
			t.Fatalf("save 10: %v", err)
		}
		if err := ms.SaveMaterializerCursor(c, "holder-a", 4); err != nil {
			t.Fatalf("save 4 (no-op): %v", err)
		}
		if err := ms.ReleaseMaterializerLease(c, "holder-a"); err != nil {
			t.Fatalf("release: %v", err)
		}
		l, err := ms.AcquireMaterializerLease(c, "holder-b", time.Minute)
		if err != nil || !l.Held || l.Cursor != 10 {
			t.Fatalf("takeover acquire = %+v, %v; want held with cursor 10 (kept across release, never regressed)", l, err)
		}
	})

	t.Run("Repair_FlagListFillSettle", func(t *testing.T) {
		s, ms := open(t)
		c := ctx(t)
		id := triple("t-rep", "u-rep", "s-rep")
		broken := repairRow("broken", id)
		if _, err := s.AppendTurnIf(c, id, broken); err != nil {
			t.Fatalf("append broken: %v", err)
		}
		complete := repairRow("complete", id)
		complete.Query = turns.Query{Text: "hello", Complete: turns.CompletenessComplete}
		complete.Agent = turns.Agent{ID: "agent", BindingSource: turns.AgentBindingExplicit, Complete: turns.CompletenessComplete}
		if _, err := s.AppendTurnIf(c, id, complete); err != nil {
			t.Fatalf("append complete: %v", err)
		}
		keys, err := ms.ListIdentityRepairCandidates(c, turns.IdentityRepairKey{}, 10)
		if err != nil || len(keys) != 1 || keys[0].TurnID != "broken" || keys[0].Identity != id {
			t.Fatalf("candidates = %+v, %v; want only the broken row", keys, err)
		}
		// Seal the broken row: the repair must still fill it.
		cur, err := s.GetTurn(c, id, "broken")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		sealed := cur
		sealed.Status = turns.StatusComplete
		sealed.Answer = turns.Answer{State: turns.AnswerStateInline, Inline: "a", Complete: turns.CompletenessComplete}
		if _, err := s.SealTurnIf(c, id, "broken", cur.Version, sealed); err != nil {
			t.Fatalf("seal: %v", err)
		}
		cur, err = s.GetTurn(c, id, "broken")
		if err != nil {
			t.Fatalf("get turn: %v", err)
		}

		// Query only: the agent stays unavailable, so the flag stays.
		row, changed, err := ms.RepairTurnIdentity(c, id, "broken", turns.IdentityFill{QueryPresent: true, Query: "what is up"})
		if err != nil || !changed {
			t.Fatalf("repair query = %v changed=%v", err, changed)
		}
		if row.Query.Text != "what is up" || row.Query.Complete != turns.CompletenessComplete || !row.Sealed || row.Version != cur.Version+1 {
			t.Fatalf("repaired row = %+v; want filled query, still sealed, version+1", row)
		}
		if got, err := s.GetTurn(c, id, "broken"); err != nil || got.Query.Text != "what is up" || got.Answer.Inline != "a" || got.Status != turns.StatusComplete {
			t.Fatalf("stored row after repair = %+v; want query filled, seal content intact", got)
		}
		if keys, err := ms.ListIdentityRepairCandidates(c, turns.IdentityRepairKey{}, 10); err != nil || len(keys) != 1 {
			t.Fatalf("candidates after partial repair = %+v; want still flagged (agent unavailable)", keys)
		}
		// A complete component is never overwritten.
		row, changed, err = ms.RepairTurnIdentity(c, id, "broken", turns.IdentityFill{QueryPresent: true, Query: "other"})
		if err != nil || changed || row.Query.Text != "what is up" {
			t.Fatalf("overwrite attempt = %+v changed=%v err=%v; want unchanged", row.Query, changed, err)
		}
		// Agent fill clears the flag.
		row, changed, err = ms.RepairTurnIdentity(c, id, "broken", turns.IdentityFill{AgentPresent: true, AgentID: "agent-x", AgentName: "agent-x"})
		if err != nil || !changed || row.Agent.ID != "agent-x" || row.Agent.BindingSource != turns.AgentBindingExplicit {
			t.Fatalf("agent repair = %+v changed=%v err=%v", row.Agent, changed, err)
		}
		if keys, err := ms.ListIdentityRepairCandidates(c, turns.IdentityRepairKey{}, 10); err != nil || len(keys) != 0 {
			t.Fatalf("candidates after full repair = %+v; want none", keys)
		}

		// Settle: nothing to fill, the flag is cleared without a write.
		empty := repairRow("empty", id)
		if _, err := s.AppendTurnIf(c, id, empty); err != nil {
			t.Fatalf("append empty: %v", err)
		}
		before, err := s.GetTurn(c, id, "empty")
		if err != nil {
			t.Fatalf("get turn: %v", err)
		}
		row, changed, err = ms.RepairTurnIdentity(c, id, "empty", turns.IdentityFill{Settle: true})
		if err != nil || changed || row.Version != before.Version {
			t.Fatalf("settle = %+v changed=%v err=%v; want no write", row, changed, err)
		}
		if keys, err := ms.ListIdentityRepairCandidates(c, turns.IdentityRepairKey{}, 10); err != nil || len(keys) != 0 {
			t.Fatalf("candidates after settle = %+v; want none", keys)
		}
		if _, _, err := ms.RepairTurnIdentity(c, id, "complete", turns.IdentityFill{QueryPresent: true, Query: strings.Repeat("x", turns.MaxQueryRunes+1)}); err != nil {
			t.Fatalf("repair of a complete row must be a no-op even with an over-bound fill (nothing to fill): %v", err)
		}
		if _, _, err := ms.RepairTurnIdentity(c, id, "empty", turns.IdentityFill{QueryPresent: true, Query: strings.Repeat("x", turns.MaxQueryRunes+1)}); !errors.Is(err, turns.ErrInvalidInput) {
			t.Fatalf("over-bound fill: %v; want ErrInvalidInput", err)
		}
	})

	t.Run("Repair_KeysetPaging_AcrossSessions", func(t *testing.T) {
		s, ms := open(t)
		c := ctx(t)
		want := map[turns.IdentityRepairKey]bool{}
		for _, sess := range []string{"s-a", "s-b", "s-c"} {
			id := triple("t-page", "u-page", sess)
			for _, turn := range []string{"t1", "t2"} {
				if _, err := s.AppendTurnIf(c, id, repairRow(turn, id)); err != nil {
					t.Fatalf("append: %v", err)
				}
				want[turns.IdentityRepairKey{Identity: id, TurnID: turns.TurnID(turn)}] = true
			}
		}
		var after turns.IdentityRepairKey
		seen := map[turns.IdentityRepairKey]bool{}
		for range 10 {
			keys, err := ms.ListIdentityRepairCandidates(c, after, 4)
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(keys) == 0 {
				break
			}
			for _, k := range keys {
				if seen[k] {
					t.Fatalf("duplicate candidate %+v", k)
				}
				seen[k] = true
			}
			after = keys[len(keys)-1]
		}
		if len(seen) != len(want) {
			t.Fatalf("paged %d candidates, want %d", len(seen), len(want))
		}
	})

	t.Run("Repair_Fenced_And_NotFound", func(t *testing.T) {
		s, ms := open(t)
		c := ctx(t)
		id := triple("t-fence", "u-fence", "s-fence")
		if _, err := s.AppendTurnIf(c, id, repairRow("r", id)); err != nil {
			t.Fatalf("append: %v", err)
		}
		if _, _, err := ms.RepairTurnIdentity(c, id, "missing", turns.IdentityFill{QueryPresent: true, Query: "q"}); !errors.Is(err, turns.ErrTurnNotFound) {
			t.Fatalf("missing row: %v; want ErrTurnNotFound", err)
		}
		if _, _, err := ms.RepairTurnIdentity(c, identity.Identity{TenantID: "t"}, "r", turns.IdentityFill{}); !errors.Is(err, turns.ErrIdentityRequired) {
			t.Fatalf("partial identity: %v; want ErrIdentityRequired", err)
		}
		if err := s.FenceSession(c, id); err != nil {
			t.Fatalf("fence: %v", err)
		}
		if _, _, err := ms.RepairTurnIdentity(c, id, "r", turns.IdentityFill{QueryPresent: true, Query: "q"}); !errors.Is(err, turns.ErrErasureFenced) {
			t.Fatalf("fenced repair: %v; want ErrErasureFenced", err)
		}
	})
}

// runFenceSuite pins the lease write fence: after the lease changes
// hands, every write carrying the former tenure's fence is refused, the new
// tenure's writes succeed, and a renewal keeps the generation.
func runFenceSuite(t *testing.T, open func(t *testing.T) (turns.Store, turns.MaterializerStore)) {
	t.Run("Fence_StaleGenerationRejectsEveryWrite", func(t *testing.T) {
		s, ms := open(t)
		c := context.Background()
		id := triple("t-fence-gen", "u", "s")
		a, err := ms.AcquireMaterializerLease(c, "holder-a", time.Minute)
		if err != nil || !a.Held {
			t.Fatalf("A acquire: %+v %v", a, err)
		}
		if r, err := ms.AcquireMaterializerLease(c, "holder-a", time.Minute); err != nil || r.Generation != a.Generation {
			t.Fatalf("renewal moved the generation %d -> %d", a.Generation, r.Generation)
		}
		ctxA := turns.WithWriteFence(c, turns.WriteFence{Holder: "holder-a", Generation: a.Generation})
		if _, err := s.AppendTurnIf(ctxA, id, repairRow("r1", id)); err != nil {
			t.Fatalf("append under the live fence: %v", err)
		}
		if err := ms.ReleaseMaterializerLease(c, "holder-a"); err != nil {
			t.Fatalf("release: %v", err)
		}
		b, err := ms.AcquireMaterializerLease(c, "holder-b", time.Minute)
		if err != nil || !b.Held || b.Generation == a.Generation {
			t.Fatalf("B acquire = %+v %v; want a new generation", b, err)
		}
		cur, err := s.GetTurn(c, id, "r1")
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		writes := map[string]func(context.Context) error{
			"AppendTurnIf": func(x context.Context) error { _, err := s.AppendTurnIf(x, id, repairRow("r2", id)); return err },
			"UpdateTurnIf": func(x context.Context) error { _, err := s.UpdateTurnIf(x, id, "r1", cur.Version, cur); return err },
			"SealTurnIf": func(x context.Context) error {
				sealed := cur
				sealed.Status = turns.StatusCancelled
				_, err := s.SealTurnIf(x, id, "r1", cur.Version, sealed)
				return err
			},
			"SaveCheckpoint": func(x context.Context) error { return s.SaveCheckpoint(x, id, 99) },
			"RepairTurnIdentity": func(x context.Context) error {
				_, _, err := ms.RepairTurnIdentity(x, id, "r1", turns.IdentityFill{QueryPresent: true, Query: "q"})
				return err
			},
			"SaveMaterializerCursor": func(x context.Context) error { return ms.SaveMaterializerCursor(x, "holder-a", 50) },
			"SaveTaskRoute":          func(x context.Context) error { return ms.SaveTaskRoute(x, id, turns.TaskRoute{TaskID: "r1"}) },
			"MarkPendingCompletion":  func(x context.Context) error { return ms.MarkPendingCompletion(x, id, "r1") },
		}
		for name, w := range writes {
			if err := w(ctxA); !errors.Is(err, turns.ErrLeaseNotHeld) {
				t.Fatalf("%s under a superseded fence: %v; want ErrLeaseNotHeld", name, err)
			}
		}
		if got, err := s.GetTurn(c, id, "r1"); err != nil || got.Version != cur.Version || got.Query.Complete != turns.CompletenessUnavailable {
			t.Fatalf("a fenced-out write changed the row: %+v", got)
		}
		if cp, err := s.LoadCheckpoint(c, id); err != nil || cp == 99 {
			t.Fatalf("a fenced-out checkpoint landed")
		}
		ctxB := turns.WithWriteFence(c, turns.WriteFence{Holder: "holder-b", Generation: b.Generation})
		if _, err := s.UpdateTurnIf(ctxB, id, "r1", cur.Version, cur); err != nil {
			t.Fatalf("write under the new fence: %v", err)
		}
		if err := s.SaveCheckpoint(ctxB, id, 7); err != nil {
			t.Fatalf("checkpoint under the new fence: %v", err)
		}
	})
}

// repairRow builds a mutable row whose query and agent are unavailable
// (the shape a spawn projected without its task record produces).
func repairRow(turnID string, id identity.Identity) turns.TurnRow {
	r := freshRow(turnID, id)
	r.Query = turns.Query{Complete: turns.CompletenessUnavailable}
	r.Agent = turns.Agent{BindingSource: turns.AgentBindingUnknown, Complete: turns.CompletenessUnavailable}
	return r
}

// RunRouteRetention pins that routes never outlive their turns: the
// factory MUST build a store retaining ONE row per session. Appending a
// second turn evicts the first, and the evicted turn's routes (its own
// and its children's) go with it.
func RunRouteRetention(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("Routes_EvictedWithTheirTurn", func(t *testing.T) {
		s, cleanup := factory()
		defer cleanup()
		ms, isMS := s.(turns.MaterializerStore)
		if !isMS {
			t.Fatalf("store does not implement turns.MaterializerStore")
		}
		c := context.Background()
		id := triple("t-ret", "u", "s")
		for _, tid := range []string{"r-0", "r-1"} {
			if _, err := s.AppendTurnIf(c, id, freshRow(tid, id)); err != nil {
				t.Fatalf("append %s: %v", tid, err)
			}
			if err := ms.SaveTaskRoute(c, id, turns.TaskRoute{TaskID: tid, RootTaskID: tid}); err != nil {
				t.Fatalf("route %s: %v", tid, err)
			}
			if err := ms.SaveTaskRoute(c, id, turns.TaskRoute{TaskID: tid + "-child", ParentTaskID: tid, RootTaskID: tid}); err != nil {
				t.Fatalf("child route %s: %v", tid, err)
			}
		}
		if _, err := s.GetTurn(c, id, "r-0"); !errors.Is(err, turns.ErrTurnNotFound) {
			t.Fatalf("expected r-0 evicted: %v", err)
		}
		for _, tid := range []string{"r-0", "r-0-child"} {
			if r, ok, err := ms.LoadTaskRoute(c, id, tid); err != nil || ok {
				t.Fatalf("route %s outlived its evicted turn: %+v", tid, r)
			}
		}
		if _, ok, err := ms.LoadTaskRoute(c, id, "r-1-child"); err != nil || !ok {
			t.Fatalf("a retained turn's route was dropped")
		}
	})
}

// runBootstrapSuite pins the bootstrap storage primitives.
func runBootstrapSuite(t *testing.T, open func(t *testing.T) (turns.Store, turns.MaterializerStore)) {
	t.Run("Bootstrap_ProgressFencedAndUnsealedSessions", func(t *testing.T) {
		s, ms := open(t)
		c := context.Background()
		a, err := ms.AcquireMaterializerLease(c, "holder-a", time.Minute)
		if err != nil || !a.Held {
			t.Fatalf("acquire: %+v %v", a, err)
		}
		fa := turns.WithWriteFence(c, turns.WriteFence{Holder: "holder-a", Generation: a.Generation})
		if err := ms.SaveMaterializerBootstrap(fa, "holder-a", `{"phase":"replay"}`); err != nil {
			t.Fatalf("save progress: %v", err)
		}
		if got, err := ms.LoadMaterializerBootstrap(c); err != nil || got != `{"phase":"replay"}` {
			t.Fatalf("progress = %q", got)
		}
		if err := ms.SaveMaterializerBootstrap(c, "holder-b", "x"); !errors.Is(err, turns.ErrLeaseNotHeld) {
			t.Fatalf("non-holder progress save: %v", err)
		}
		open1, open2, closed := triple("t-b", "u", "s1"), triple("t-b", "u", "s2"), triple("t-b", "u", "s3")
		for _, id := range []identity.Identity{open1, open2, closed} {
			if _, err := s.AppendTurnIf(c, id, freshRow("x", id)); err != nil {
				t.Fatalf("append: %v", err)
			}
		}
		cur, err := s.GetTurn(c, closed, "x")
		if err != nil {
			t.Fatalf("get turn: %v", err)
		}
		sealed := cur
		sealed.Status = turns.StatusCancelled
		if _, err := s.SealTurnIf(c, closed, "x", cur.Version, sealed); err != nil {
			t.Fatalf("seal: %v", err)
		}
		got, err := ms.ListUnsealedSessions(c, identity.Identity{}, 10)
		if err != nil || len(got) != 2 || got[0] != open1 || got[1] != open2 {
			t.Fatalf("unsealed sessions = %+v, %v; want s1, s2", got, err)
		}
		if page, err := ms.ListUnsealedSessions(c, open1, 10); err != nil || len(page) != 1 || page[0] != open2 {
			t.Fatalf("keyset page after s1 = %+v", page)
		}
		if err := ms.PruneTaskRoutes(turns.WithWriteFence(c, turns.WriteFence{Holder: "holder-a", Generation: a.Generation + 7}), open1, "x"); !errors.Is(err, turns.ErrLeaseNotHeld) {
			t.Fatalf("prune under a stale fence: %v", err)
		}
		if err := ms.SaveTaskRoute(c, open1, turns.TaskRoute{TaskID: "bg", RunID: "bg-run"}); err != nil {
			t.Fatalf("save: %v", err)
		}
		if err := ms.PruneTaskRoutes(fa, open1, "bg"); err != nil {
			t.Fatalf("prune: %v", err)
		}
		if _, ok, err := ms.LoadTaskRoute(c, open1, "bg"); err != nil || ok {
			t.Fatalf("pruned route survived")
		}
		if err := ms.EnsureMaterializerIndexes(c); err != nil {
			t.Fatalf("ensure indexes: %v", err)
		}
	})
}
