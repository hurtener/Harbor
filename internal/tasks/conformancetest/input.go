package conformancetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
)

func runInputReceipts(t *testing.T, factory Factory) {
	running := func(t *testing.T, r tasks.TaskRegistry, ctx context.Context) tasks.TaskID {
		t.Helper()
		id, _ := identity.From(ctx)
		h, err := r.Spawn(ctx, tasks.SpawnRequest{Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground, Query: "original"})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.MarkRunning(ctx, h.ID); err != nil {
			t.Fatal(err)
		}
		return h.ID
	}

	t.Run("InputReceipts_ExpectedRevisionSerializesConcurrentIntent", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		id := running(t, r, ctx)
		var wg sync.WaitGroup
		results := make(chan tasks.InputRecord, 100)
		failures := make(chan error, 100)
		for i := range 100 {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				record, err := r.AcceptInput(ctx, id, fmt.Sprintf("intent-%d", i), "correction", 0)
				if err != nil {
					failures <- err
				} else {
					results <- record
				}
			}(i)
		}
		wg.Wait()
		close(results)
		close(failures)
		if len(results) != 1 || len(failures) != 99 {
			t.Fatalf("accepted=%d refused=%d", len(results), len(failures))
		}
		first := <-results
		for err := range failures {
			if !errors.Is(err, tasks.ErrInputRevisionConflict) {
				t.Fatalf("stale intent=%v", err)
			}
		}
		if _, err := r.AcceptInput(ctx, id, "second", "next", 1); err != nil {
			t.Fatal(err)
		}
		replay, err := r.AcceptInput(ctx, id, first.Receipt.EventID, "correction", 0)
		if err != nil || replay != first {
			t.Fatalf("ack recovery=%+v err=%v", replay, err)
		}
		if _, err := r.AcceptInput(ctx, id, first.Receipt.EventID, "correction", 2); !errors.Is(err, tasks.ErrIdempotencyConflict) {
			t.Fatalf("changed expectation=%v", err)
		}
		if _, err := r.GetInputReceipt(ctx, id, "never-accepted"); !errors.Is(err, tasks.ErrInputReceiptNotFound) {
			t.Fatalf("refusal left receipt=%v", err)
		}
	})
	t.Run("InputReceipts_ExactReplayRevisionAndSealedOutput", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		id := running(t, r, ctx)
		first, err := r.AcceptInput(ctx, id, "event-1", "first correction")
		if err != nil || first.Receipt.Status != tasks.InputAccepted || first.Receipt.Revision != 1 {
			t.Fatalf("first=%+v err=%v", first, err)
		}
		retry, err := r.AcceptInput(ctx, id, "event-1", "first correction")
		if err != nil || retry != first {
			t.Fatalf("retry=%+v err=%v", retry, err)
		}
		if _, err := r.AcceptInput(ctx, id, "event-1", "different correction"); !errors.Is(err, tasks.ErrIdempotencyConflict) {
			t.Fatalf("conflict=%v", err)
		}
		second, err := r.AcceptInput(ctx, id, "event-2", "second correction")
		if err != nil || second.Receipt.Revision != 2 {
			t.Fatalf("second=%+v err=%v", second, err)
		}
		if _, err := r.MarkInputApplied(ctx, id, "event-2", 2); !errors.Is(err, tasks.ErrInvalidTransition) {
			t.Fatalf("out-of-order apply=%v", err)
		}
		if _, err := r.MarkInputApplied(ctx, id, "event-1", 1); err != nil {
			t.Fatal(err)
		}
		if err := r.MarkComplete(ctx, id, tasks.TaskResult{Value: []byte(`"stale"`)}); !errors.Is(err, tasks.ErrInvalidRequest) {
			t.Fatalf("stale output=%v", err)
		}
		if err := r.MarkComplete(ctx, id, tasks.TaskResult{Value: []byte(`"ahead"`), IncorporatedInputRevision: 2}); !errors.Is(err, tasks.ErrInvalidRequest) {
			t.Fatalf("future output=%v", err)
		}
		if err := r.MarkComplete(ctx, id, tasks.TaskResult{Value: []byte(`"sealed"`), IncorporatedInputRevision: 1}); err != nil {
			t.Fatal(err)
		}
		got, err := r.Get(ctx, id)
		if err != nil || got.Result.IncorporatedInputRevision != 1 || got.InputRevision != 2 || got.AppliedInputRevision != 1 {
			t.Fatalf("result=%+v err=%v", got, err)
		}
		pending, err := r.GetInputReceipt(ctx, id, "event-2")
		if err != nil || pending.Status != tasks.InputTerminal || pending.Reason != string(tasks.StatusComplete) {
			t.Fatalf("pending=%+v err=%v", pending, err)
		}
		late, err := r.AcceptInput(ctx, id, "late", "late text")
		if err != nil || late.Receipt.Status != tasks.InputTerminal || late.Receipt.Revision != 0 {
			t.Fatalf("late=%+v err=%v", late, err)
		}
		got.InputReceipts[0].Message = "mutated"
		again, err := r.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if again.InputReceipts[0].Message == "mutated" {
			t.Fatal("task receipt aliases caller copy")
		}
	})
	t.Run("InputReceipts_BoundedNoEviction", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		id := running(t, r, ctx)
		for i := range tasks.MaxInputReceipts {
			if _, err := r.AcceptInput(ctx, id, fmt.Sprint(i), "text"); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := r.AcceptInput(ctx, id, "overflow", "text"); !errors.Is(err, tasks.ErrInputReceiptCapacity) {
			t.Fatalf("capacity=%v", err)
		}
		if _, err := r.AcceptInput(ctx, id, "0", "changed"); !errors.Is(err, tasks.ErrIdempotencyConflict) {
			t.Fatalf("evicted conflict=%v", err)
		}
		first, err := r.AcceptInput(ctx, id, "0", "text")
		if err != nil || first.Receipt.Revision != 1 {
			t.Fatalf("first=%+v err=%v", first, err)
		}
	})
	t.Run("InputReceipts_ConcurrentExactRetriesAndIsolation", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		id := running(t, r, ctx)
		var wg sync.WaitGroup
		for range 100 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rec, err := r.AcceptInput(ctx, id, "shared", "same text")
				if err != nil || rec.Receipt.Revision != 1 {
					t.Errorf("retry=%+v err=%v", rec, err)
				}
			}()
		}
		wg.Wait()
		got, err := r.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if got.InputRevision != 1 || len(got.InputReceipts) != 1 {
			t.Fatalf("dedupe=%+v", got)
		}
		foreign, err := identity.With(t.Context(), identity.Identity{TenantID: "other", UserID: "user", SessionID: "session"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.GetInputReceipt(foreign, id, "shared"); !errors.Is(err, tasks.ErrNotFound) {
			t.Fatalf("foreign read=%v", err)
		}
		if _, err := r.AcceptInput(foreign, id, "shared", "same text"); !errors.Is(err, tasks.ErrNotFound) {
			t.Fatalf("foreign write=%v", err)
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := r.AcceptInput(cancelled, id, "cancelled", "text"); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled admission=%v", err)
		}
		if _, err := r.AcceptInput(ctx, id, "live", "text"); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Cancel(ctx, id, "stop"); err != nil {
			t.Fatal(err)
		}
		for _, event := range []string{"shared", "live"} {
			rec, err := r.GetInputReceipt(ctx, id, event)
			if err != nil || rec.Status != tasks.InputTerminal || rec.Reason != "cancelled" {
				t.Fatalf("cancel receipt=%+v err=%v", rec, err)
			}
		}
	})
	t.Run("InputReceipts_UnavailableIsDurableDecline", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		ctx := ctxA()
		id := running(t, r, ctx)
		refused, err := r.RefuseInput(ctx, id, "no-inbox", "text", "run_not_active")
		if err != nil || refused.Receipt.Status != tasks.InputDeclined || refused.Receipt.Revision != 0 {
			t.Fatalf("decline=%+v err=%v", refused, err)
		}
		retry, err := r.AcceptInput(ctx, id, "no-inbox", "text")
		if err != nil || retry != refused {
			t.Fatalf("revived refusal=%+v err=%v", retry, err)
		}
	})
	t.Run("InputReceipts_ConcurrentSessionsAndCancellationIsolation", func(t *testing.T) {
		r, cleanup := factory()
		defer cleanup()
		var wg sync.WaitGroup
		for n := range 100 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := identity.Identity{TenantID: "input-tenant", UserID: fmt.Sprint("user-", n), SessionID: "shared-session-name"}
				ctx, err := identity.With(t.Context(), id)
				if err != nil {
					t.Error(err)
					return
				}
				h, err := r.Spawn(ctx, tasks.SpawnRequest{Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground})
				if err != nil {
					t.Error(err)
					return
				}
				if err := r.MarkRunning(ctx, h.ID); err != nil {
					t.Error(err)
					return
				}
				text := fmt.Sprint("private-correction-", n)
				accepted, err := r.AcceptInput(ctx, h.ID, "same-event", text)
				if err != nil || accepted.Message != text || accepted.Receipt.TaskID != h.ID || accepted.Receipt.Revision != 1 {
					t.Errorf("scope %d receipt=%+v err=%v", n, accepted, err)
					return
				}
				if n%2 == 0 {
					if _, err := r.Cancel(ctx, h.ID, "stop only this task"); err != nil {
						t.Error(err)
					}
				} else {
					if _, err := r.MarkInputApplied(ctx, h.ID, "same-event", 1); err != nil {
						t.Error(err)
						return
					}
					if err := r.MarkComplete(ctx, h.ID, tasks.TaskResult{IncorporatedInputRevision: 1}); err != nil {
						t.Error(err)
					}
				}
				receipt, err := r.GetInputReceipt(ctx, h.ID, "same-event")
				want := tasks.InputApplied
				if n%2 == 0 {
					want = tasks.InputTerminal
				}
				if err != nil || receipt.Status != want {
					t.Errorf("scope %d state=%+v err=%v", n, receipt, err)
				}
			}()
		}
		wg.Wait()
	})

}
