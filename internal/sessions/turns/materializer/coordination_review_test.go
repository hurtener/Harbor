package materializer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/tasks"
)

// hookReader wraps a fake reader with a hook run before each read.
type hookReader struct {
	*fakeTaskReader
	mu     sync.Mutex
	before func(taskID string)
}

func (h *hookReader) Task(ctx context.Context, id identity.Identity, taskID string) (TaskSnapshot, error) {
	h.mu.Lock()
	fn := h.before
	h.mu.Unlock()
	if fn != nil {
		fn(taskID)
	}
	return h.fakeTaskReader.Task(ctx, id, taskID)
}

// countingStore counts row/checkpoint writes the store ACCEPTED while
// carrying a given holder's fence after a takeover was declared.
type countingStore struct {
	turns.Store
	turns.MaterializerStore
	holder   string
	takeover atomic.Bool
	accepted atomic.Int64
}

func (c *countingStore) note(ctx context.Context, err error) {
	if f, ok := turns.WriteFenceFrom(ctx); ok && f.Holder == c.holder && c.takeover.Load() && err == nil {
		c.accepted.Add(1)
	}
}

func (c *countingStore) AppendTurnIf(ctx context.Context, id identity.Identity, row turns.TurnRow) (turns.TurnRow, error) {
	r, err := c.Store.AppendTurnIf(ctx, id, row)
	c.note(ctx, err)
	return r, err
}

func (c *countingStore) UpdateTurnIf(ctx context.Context, id identity.Identity, turnID turns.TurnID, v int, row turns.TurnRow) (turns.TurnRow, error) {
	r, err := c.Store.UpdateTurnIf(ctx, id, turnID, v, row)
	c.note(ctx, err)
	return r, err
}

func (c *countingStore) SealTurnIf(ctx context.Context, id identity.Identity, turnID turns.TurnID, v int, row turns.TurnRow) (turns.TurnRow, error) {
	r, err := c.Store.SealTurnIf(ctx, id, turnID, v, row)
	c.note(ctx, err)
	return r, err
}

func (c *countingStore) SaveCheckpoint(ctx context.Context, id identity.Identity, seq uint64) error {
	err := c.Store.SaveCheckpoint(ctx, id, seq)
	c.note(ctx, err)
	return err
}

func (c *countingStore) SaveTaskRoute(ctx context.Context, id identity.Identity, r turns.TaskRoute) error {
	err := c.MaterializerStore.SaveTaskRoute(ctx, id, r)
	c.note(ctx, err)
	return err
}

// TestCoordination_PausedFormerHolderIsFencedOut: instance A is paused
// in the middle of a page (here: inside a task-record read) while its
// lease expires and B takes over and materializes. When A resumes, every
// projection write it attempts is refused by the store (lease generation
// fence) and A steps down — renewal checks alone would let it write.
func TestCoordination_PausedFormerHolderIsFencedOut(t *testing.T) {
	c := newCoordHarness(t)
	cs := &countingStore{Store: c.store, MaterializerStore: c.ms, holder: "instance-a"}
	proj, err := turns.New(cs)
	if err != nil {
		t.Fatalf("projector: %v", err)
	}
	c.proj = proj
	c.ms = cs

	base := newFakeTaskReader()
	for i := range 4 {
		id := fmt.Sprintf("t%d", i)
		base.set(id, querySnap(id, "q-"+id))
		c.src.publish(t, spawnEv(c.id, "", id, tasks.KindForeground, ""))
		c.src.publish(t, startedEv(c.id, id))
	}
	readerB := &hookReader{fakeTaskReader: base}
	b := c.instance(t, "instance-b", readerB)
	readerA := &hookReader{fakeTaskReader: base}
	a := c.instance(t, "instance-a", readerA)

	var once sync.Once
	readerA.before = func(taskID string) {
		if taskID != "t2" {
			return
		}
		once.Do(func() {
			// A is "paused" here; its lease expires and B takes over.
			c.clock.advance(31 * time.Second)
			if res, err := b.Materialize(context.Background()); err != nil || res.Standby {
				t.Errorf("B takeover pass = %+v, %v", res, err)
			}
			cs.takeover.Store(true)
		})
	}
	res, err := a.Materialize(context.Background())
	if err != nil {
		t.Fatalf("A pass: %v", err)
	}
	if !res.Standby {
		t.Fatalf("A kept materializing after the takeover: %+v", res)
	}
	if n := cs.accepted.Load(); n != 0 {
		t.Fatalf("the store accepted %d writes from the fenced-out former holder", n)
	}
	for i := range 4 {
		id := fmt.Sprintf("t%d", i)
		if row := mustGetRow(t, c.harness, id); row.Query.Text != "q-"+id || row.Status != turns.StatusRunning {
			t.Fatalf("%s = %+v", id, row)
		}
	}
	if err := c.ms.SaveMaterializerCursor(turns.WithWriteFence(context.Background(), turns.WriteFence{Holder: "instance-a", Generation: 1}), "instance-a", 1<<30); !errors.Is(err, turns.ErrLeaseNotHeld) {
		t.Fatalf("A cursor save after takeover: %v", err)
	}
}

// TestCoordination_PendingCompletionSurvivesRestartPastItsEvents: a
// completion whose answer has not converged is deferred; the cursor then
// moves far past its events (beyond the open-turn lookback). After a
// restart the pending queue is rebuilt from the durable mark, so when the
// answer appears the row seals — it does not stay running forever.
func TestCoordination_PendingCompletionSurvivesRestartPastItsEvents(t *testing.T) {
	c := newCoordHarness(t)
	reader := newFakeTaskReader()
	reader.set("pend", querySnap("pend", "slow answer please"))
	c.src.publish(t, spawnEv(c.id, "", "pend", tasks.KindForeground, ""))
	c.src.publish(t, startedEv(c.id, "pend"))
	done := c.src.publish(t, completedEv(c.id, "pend"))
	for i := range 60 {
		id := fmt.Sprintf("other-%02d", i)
		reader.set(id, querySnap(id, id))
		c.src.publish(t, spawnEv(c.id, "", id, tasks.KindForeground, ""))
		c.src.publish(t, failedEv(c.id, id, "timeout"))
	}
	m1 := c.instance(t, "boot-1", reader, WithPageLimit(16), WithResumeLookback(20))
	if res := mustMaterialize(t, m1); res.PendingComplete != 1 {
		t.Fatalf("pending after first boot = %d; want 1", res.PendingComplete)
	}
	m1.releaseLease()
	lease, _ := c.ms.AcquireMaterializerLease(context.Background(), "peek", time.Nanosecond)
	_ = c.ms.ReleaseMaterializerLease(context.Background(), "peek")
	if lease.Cursor <= done.Sequence {
		t.Fatalf("precondition: cursor %d did not pass the completion %d", lease.Cursor, done.Sequence)
	}
	// The answer converges while no instance runs.
	snap := querySnap("pend", "slow answer please")
	snap.AnswerPresent = true
	snap.Answer = turns.Answer{State: turns.AnswerStateInline, Inline: "here it is"}
	reader.set("pend", snap)

	m2 := c.instance(t, "boot-2", reader, WithPageLimit(16), WithResumeLookback(20))
	mustMaterialize(t, m2)
	row := mustGetRow(t, c.harness, "pend")
	if !row.Sealed || row.Status != turns.StatusComplete || row.Answer.Inline != "here it is" {
		t.Fatalf("pending turn after restart = sealed %v status %q answer %+v; want sealed complete", row.Sealed, row.Status, row.Answer)
	}
	if keys, _ := c.ms.ListPendingCompletions(context.Background(), turns.IdentityRepairKey{}, 10); len(keys) != 0 {
		t.Fatalf("pending mark survived the seal: %+v", keys)
	}
}

// TestCoordination_ResumePastSpawnsKeepsExplicitRunAndChildRouting: a
// root turn with an EXPLICIT run id and a child task with its own run
// were spawned before the resume cursor (older than the lookback). After
// a restart their run-scoped events still fold into the root turn
// through the persisted routes — nothing is guessed or skipped — and the
// root's terminal seals a row that carries the child's activity.
func TestCoordination_ResumePastSpawnsKeepsExplicitRunAndChildRouting(t *testing.T) {
	c := newCoordHarness(t)
	reader := newFakeTaskReader()
	reader.set("root", querySnap("root", "do the thing"))
	c.src.publish(t, spawnEv(c.id, "run-root", "root", tasks.KindForeground, ""))
	c.src.publish(t, startedEv(c.id, "root"))
	c.src.publish(t, spawnEv(c.id, "run-child", "child", tasks.KindForeground, "root"))
	for i := range 60 {
		id := fmt.Sprintf("other-%02d", i)
		reader.set(id, querySnap(id, id))
		c.src.publish(t, spawnEv(c.id, "", id, tasks.KindForeground, ""))
		c.src.publish(t, failedEv(c.id, id, "timeout"))
	}
	m1 := c.instance(t, "boot-1", reader, WithPageLimit(16), WithResumeLookback(20))
	mustMaterialize(t, m1)
	m1.releaseLease()

	m2 := c.instance(t, "boot-2", reader, WithPageLimit(16), WithResumeLookback(20))
	mustMaterialize(t, m2)
	at := time.Unix(1_700_000_500, 0)
	c.src.publish(t, toolInvokedEv(c.id, "run-child", "search", at))
	c.src.publish(t, toolCompletedEv(c.id, "run-child", "search", 9))
	c.src.publish(t, toolInvokedEv(c.id, "run-root", "clock.now", at))
	c.src.publish(t, toolCompletedEv(c.id, "run-root", "clock.now", 3))
	c.src.publish(t, failedEv(c.id, "child", "timeout")) // a child terminal never seals the root
	mustMaterialize(t, m2)
	row := mustGetRow(t, c.harness, "root")
	if row.Sealed {
		t.Fatalf("the child's terminal sealed the root")
	}
	if row.Activity.Totals.Succeeded != 2 || len(row.Activity.Rows) != 2 {
		t.Fatalf("root activity = %+v; want the child's and the root's dispatches", row.Activity)
	}
	c.src.publish(t, failedEv(c.id, "root", "timeout"))
	mustMaterialize(t, m2)
	if row := mustGetRow(t, c.harness, "root"); !row.Sealed || row.Status != turns.StatusFailed || row.RunID != "run-root" {
		t.Fatalf("root after its terminal = %+v", row)
	}
	if _, ok, _ := c.ms.LoadTaskRoute(context.Background(), c.id, "child"); ok {
		t.Fatalf("routes of the sealed root were not pruned")
	}
}
