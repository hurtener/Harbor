package materializer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/tasks"
)

// ---------------------------------------------------------------------------
// Coordination: rolling-deploy overlap, identity repair, the single-writer
// lease, and the persisted resume cursor.
// ---------------------------------------------------------------------------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{t: time.Now()} }

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// coordHarness is one durable turns store (a file — the shared database
// of a rolling deploy) plus the shared event source; each instance is a
// materializer over it.
type coordHarness struct {
	*harness
	dsn   string
	clock *fakeClock
	ms    turns.MaterializerStore
}

func newCoordHarness(t *testing.T) *coordHarness {
	t.Helper()
	clock := newFakeClock()
	dsn := filepath.Join(t.TempDir(), "turns.sqlite")
	store, err := sqlite.New(sqlite.Config{DSN: dsn, Clock: clock.now})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	proj, err := turns.New(store)
	if err != nil {
		t.Fatalf("projector: %v", err)
	}
	t.Cleanup(func() { _ = proj.Close(context.Background()) })
	h := &harness{
		id:    identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-a"},
		store: store,
		proj:  proj,
		src:   &fakeSource{},
	}
	return &coordHarness{harness: h, dsn: dsn, clock: clock, ms: store.(turns.MaterializerStore)}
}

// instance builds one runtime instance's materializer over the shared
// store and source.
func (c *coordHarness) instance(t *testing.T, holder string, reader TaskSnapshotReader, opts ...Option) *Materializer {
	t.Helper()
	all := append([]Option{
		WithCoordination(c.ms),
		WithTaskSnapshotReader(reader),
		withClock(c.clock.now),
		withHolder(holder),
		WithLeaseTTL(30 * time.Second),
	}, opts...)
	return c.newMaterializer(t, all...)
}

func querySnap(taskID, query string) TaskSnapshot {
	return TaskSnapshot{
		TaskID: taskID, QueryPresent: true, Query: query,
		AgentPresent: true, AgentID: "agent-a", AgentName: "agent-a", AgentBindingSource: turns.AgentBindingExplicit,
	}
}

func mustMaterialize(t *testing.T, m *Materializer) Result {
	t.Helper()
	res, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return res
}

// TestCoordination_OverlapSpawnDefersUntilRecordReadable reproduces the
// production overlap: the event was published by the OTHER instance and
// the task record is not readable here yet. The live spawn is deferred
// (no row without the user's message), and when the record becomes
// readable the row is appended WITH the query and agent.
func TestCoordination_OverlapSpawnDefersUntilRecordReadable(t *testing.T) {
	c := newCoordHarness(t)
	reader := newFakeTaskReader().markMissing("task-x")
	m := c.instance(t, "old-instance", reader)

	c.src.publish(t, spawnEv(c.id, "", "task-x", tasks.KindForeground, ""))
	c.src.publish(t, startedEv(c.id, "task-x"))

	res := mustMaterialize(t, m)
	if !res.SpawnDeferred {
		t.Fatalf("result = %+v; want the spawn deferred", res)
	}
	if _, err := c.proj.Get(context.Background(), c.id, "task-x"); err == nil {
		t.Fatalf("a row was appended while the task record was unreadable")
	}
	reader.unmarkMissing("task-x")
	reader.set("task-x", querySnap("task-x", "what is the weather?"))
	res = mustMaterialize(t, m)
	if res.SpawnDeferred {
		t.Fatalf("still deferred after the record appeared: %+v", res)
	}
	row := mustGetRow(t, c.harness, "task-x")
	if row.Query.Text != "what is the weather?" || row.Query.Complete != turns.CompletenessComplete || row.Agent.ID != "agent-a" {
		t.Fatalf("row query/agent = %+v / %+v; want the user's message", row.Query, row.Agent)
	}
	if row.Status != turns.StatusRunning {
		t.Fatalf("row status = %q; want running (started applied after the deferred spawn)", row.Status)
	}
}

// TestCoordination_DeferralIsBoundedThenRepaired: a record that stays
// unreadable defers only within the bound (attempts AND grace); the row
// is then appended honestly unavailable, and once the record is readable
// the repair pass fills the query / agent exactly once — even after the
// row sealed.
func TestCoordination_DeferralIsBoundedThenRepaired(t *testing.T) {
	c := newCoordHarness(t)
	reader := newFakeTaskReader().markMissing("task-y")
	m := c.instance(t, "inst", reader)
	c.src.publish(t, spawnEv(c.id, "", "task-y", tasks.KindForeground, ""))
	c.src.publish(t, failedEv(c.id, "task-y", "timeout"))

	for i := 0; i < defaultSpawnDeferAttempts; i++ {
		if res := mustMaterialize(t, m); !res.SpawnDeferred {
			t.Fatalf("attempt %d: not deferred within the bound", i)
		}
	}
	// Attempts exhausted but the grace window is not: still deferred.
	if res := mustMaterialize(t, m); !res.SpawnDeferred {
		t.Fatalf("deferral ended before the grace window")
	}
	c.clock.advance(defaultSpawnDeferGrace)
	if res := mustMaterialize(t, m); res.SpawnDeferred {
		t.Fatalf("deferral never ended: %+v", res)
	}
	row := mustGetRow(t, c.harness, "task-y")
	if !row.Sealed || row.Query.Complete != turns.CompletenessUnavailable {
		t.Fatalf("row = %+v; want sealed with an honestly unavailable query", row)
	}
	// The record becomes readable; the next idle pass repairs the row.
	reader.unmarkMissing("task-y")
	reader.set("task-y", querySnap("task-y", "late question"))
	c.clock.advance(defaultRepairRescan)
	if err := m.convergeQueued(context.Background()); err != nil {
		t.Fatalf("idle pass: %v", err)
	}
	row = mustGetRow(t, c.harness, "task-y")
	if row.Query.Text != "late question" || row.Agent.ID != "agent-a" || !row.Sealed || row.Status != turns.StatusFailed {
		t.Fatalf("repaired row = %+v; want query/agent filled, seal intact", row)
	}
	if keys, _ := c.ms.ListIdentityRepairCandidates(context.Background(), turns.IdentityRepairKey{}, 10); len(keys) != 0 {
		t.Fatalf("repair flag not cleared: %+v", keys)
	}
}

// TestCoordination_RepairsExistingBrokenRows: rows already persisted
// without their query (the production state before this fix) are
// repaired by the bounded repair pass on the next boot with no new
// event; a complete row is never overwritten.
func TestCoordination_RepairsExistingBrokenRows(t *testing.T) {
	c := newCoordHarness(t)
	ctx := context.Background()
	for i := range 3 {
		taskID := fmt.Sprintf("broken-%d", i)
		if _, err := c.proj.Append(ctx, c.id, turns.Append{TurnID: turns.TurnID(taskID), Status: turns.StatusPending, EventSeq: uint64(i + 1)}); err != nil {
			t.Fatalf("seed append: %v", err)
		}
		row, _ := c.proj.Get(ctx, c.id, turns.TurnID(taskID))
		if _, err := c.proj.Seal(ctx, c.id, turns.TurnID(taskID), row.Version, turns.Seal{Status: turns.StatusCancelled, EventSeq: uint64(i + 10)}); err != nil {
			t.Fatalf("seed seal: %v", err)
		}
	}
	if _, err := c.proj.Append(ctx, c.id, turns.Append{TurnID: "complete", Query: "original", AgentID: "agent-o", EventSeq: 20}); err != nil {
		t.Fatalf("seed complete: %v", err)
	}
	reader := newFakeTaskReader()
	for i := range 3 {
		reader.set(fmt.Sprintf("broken-%d", i), querySnap(fmt.Sprintf("broken-%d", i), fmt.Sprintf("question %d", i)))
	}
	reader.set("complete", querySnap("complete", "SHOULD NOT OVERWRITE"))
	m := c.instance(t, "inst", reader)
	mustMaterialize(t, m) // empty log: only the repair pass does work
	for i := range 3 {
		row := mustGetRow(t, c.harness, fmt.Sprintf("broken-%d", i))
		if row.Query.Text != fmt.Sprintf("question %d", i) || !row.Sealed || row.Status != turns.StatusCancelled {
			t.Fatalf("broken-%d = %+v; want repaired, seal intact", i, row)
		}
	}
	if row := mustGetRow(t, c.harness, "complete"); row.Query.Text != "original" || row.Agent.ID != "agent-o" {
		t.Fatalf("complete row overwritten: %+v", row)
	}
	if m.repaired != 3 {
		t.Fatalf("repaired = %d; want 3", m.repaired)
	}
}

// TestCoordination_LeaseExclusivityAndTakeover: two instances share one
// store; only the lease holder pages and writes, the standby does
// nothing, and when the holder stops renewing the standby takes over
// from the PERSISTED cursor while the old holder is fenced.
func TestCoordination_LeaseExclusivityAndTakeover(t *testing.T) {
	c := newCoordHarness(t)
	reader := newFakeTaskReader()
	reader.set("task-1", querySnap("task-1", "q1"))
	reader.set("task-2", querySnap("task-2", "q2"))
	a := c.instance(t, "instance-a", reader)
	b := c.instance(t, "instance-b", reader)

	c.src.publish(t, spawnEv(c.id, "", "task-1", tasks.KindForeground, ""))
	c.src.publish(t, failedEv(c.id, "task-1", "timeout"))
	if res := mustMaterialize(t, a); res.Standby || res.EventsApplied != 2 {
		t.Fatalf("holder pass = %+v", res)
	}
	pagesBefore := c.src.pageCallCount()
	if res := mustMaterialize(t, b); !res.Standby {
		t.Fatalf("second instance materialized while A holds the lease: %+v", res)
	}
	if c.src.pageCallCount() != pagesBefore {
		t.Fatalf("the standby paged the source")
	}

	// A stops renewing (crash); after the TTL B takes over.
	c.clock.advance(31 * time.Second)
	c.src.publish(t, spawnEv(c.id, "", "task-2", tasks.KindForeground, ""))
	c.src.publish(t, failedEv(c.id, "task-2", "timeout"))
	afterIdx := len(c.src.pageAfters)
	res := mustMaterialize(t, b)
	if res.Standby {
		t.Fatalf("B did not take over the expired lease")
	}
	if first := c.src.pageAfters[afterIdx]; first < 2 {
		t.Fatalf("takeover paged from %d; want the persisted cursor (>= 2), not a full rescan", first)
	}
	if row := mustGetRow(t, c.harness, "task-2"); !row.Sealed || row.Query.Text != "q2" {
		t.Fatalf("task-2 = %+v", row)
	}
	// A wakes up: it renews, finds B holding the lease, and stands by.
	if res := mustMaterialize(t, a); !res.Standby {
		t.Fatalf("old holder kept materializing after takeover: %+v", res)
	}
	if err := c.ms.SaveMaterializerCursor(context.Background(), "instance-a", 1<<40); !errors.Is(err, turns.ErrLeaseNotHeld) {
		t.Fatalf("old holder cursor save: %v; want ErrLeaseNotHeld", err)
	}
}

// TestCoordination_RunReleasesLeaseOnShutdown: a graceful Run exit
// releases the lease, so the standby takes over immediately.
func TestCoordination_RunReleasesLeaseOnShutdown(t *testing.T) {
	c := newCoordHarness(t)
	reader := newFakeTaskReader()
	a := c.instance(t, "instance-a", reader)
	b := c.instance(t, "instance-b", reader)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	if !eventually(t, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		return a.held
	}) {
		t.Fatalf("A never took the lease")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res := mustMaterialize(t, b); res.Standby {
		t.Fatalf("B could not take over after A's graceful shutdown")
	}
}

// TestCoordination_RestartResumesFromPersistedCursor: a restart pages
// only events after the persisted cursor (no full rescan), an open turn
// within the lookback pins the cursor so its accumulators rebuild by
// replay, and an open turn older than the lookback is adopted from its
// durable row on its next event and still seals.
func TestCoordination_RestartResumesFromPersistedCursor(t *testing.T) {
	c := newCoordHarness(t)
	reader := newFakeTaskReader()
	// An old open turn (beyond the lookback by restart time).
	reader.set("old-open", querySnap("old-open", "old"))
	c.src.publish(t, spawnEv(c.id, "", "old-open", tasks.KindForeground, ""))
	c.src.publish(t, toolInvokedEv(c.id, "old-open", "clock.now", time.Unix(1_700_000_100, 0)))
	// Many sealed turns (within the store's per-session retention).
	for i := range 150 {
		id := fmt.Sprintf("done-%03d", i)
		reader.set(id, querySnap(id, id))
		c.src.publish(t, spawnEv(c.id, "", id, tasks.KindForeground, ""))
		c.src.publish(t, failedEv(c.id, id, "timeout"))
	}
	// A recent open turn (inside the lookback): pins the cursor.
	reader.set("recent-open", querySnap("recent-open", "recent"))
	recent := c.src.publish(t, spawnEv(c.id, "", "recent-open", tasks.KindForeground, ""))
	c.src.publish(t, toolInvokedEv(c.id, "recent-open", "search", time.Unix(1_700_000_200, 0)))
	for i := range 20 {
		id := fmt.Sprintf("tail-%02d", i)
		reader.set(id, querySnap(id, id))
		c.src.publish(t, spawnEv(c.id, "", id, tasks.KindForeground, ""))
		c.src.publish(t, failedEv(c.id, id, "timeout"))
	}
	m1 := c.instance(t, "boot-1", reader, WithPageLimit(16), WithResumeLookback(100))
	mustMaterialize(t, m1)
	m1.releaseLease() // graceful shutdown

	lease, err := c.ms.AcquireMaterializerLease(context.Background(), "peek", time.Nanosecond)
	if err != nil {
		t.Fatalf("peek lease: %v", err)
	}
	_ = c.ms.ReleaseMaterializerLease(context.Background(), "peek")
	if want := recent.Sequence - 1; lease.Cursor != want {
		t.Fatalf("persisted cursor = %d; want %d (held just before the recent open turn)", lease.Cursor, want)
	}

	// Restart: a fresh instance over the same store.
	m2 := c.instance(t, "boot-2", reader, WithPageLimit(16), WithResumeLookback(100))
	pagesBefore, afterIdx := c.src.pageCallCount(), len(c.src.pageAfters)
	mustMaterialize(t, m2)
	if first := c.src.pageAfters[afterIdx]; first != lease.Cursor {
		t.Fatalf("restart paged from %d; want the persisted cursor %d", first, lease.Cursor)
	}
	if pages := c.src.pageCallCount() - pagesBefore; pages > 4 {
		t.Fatalf("restart read %d pages; want only the events after the cursor (<= 4)", pages)
	}

	// New events for both open turns after the restart.
	c.src.publish(t, toolCompletedEv(c.id, "recent-open", "search", 7))
	c.src.publish(t, failedEv(c.id, "recent-open", "timeout"))
	c.src.publish(t, failedEv(c.id, "old-open", "timeout"))
	mustMaterialize(t, m2)
	rec := mustGetRow(t, c.harness, "recent-open")
	if !rec.Sealed || rec.Activity.Totals.Succeeded != 1 || len(rec.Activity.Rows) != 1 {
		t.Fatalf("recent-open = sealed %v activity %+v; want sealed with the replayed dispatch completed", rec.Sealed, rec.Activity)
	}
	old := mustGetRow(t, c.harness, "old-open")
	if !old.Sealed || old.Status != turns.StatusFailed || old.Query.Text != "old" || len(old.Activity.Rows) != 1 {
		t.Fatalf("old-open = %+v; want adopted and sealed with its row content intact", old)
	}
}

// TestCoordination_ConcurrentInstancesRaceFree: two instances hammer one
// store concurrently (-race gate); every turn ends sealed exactly once
// with its query, and only one instance ever materialized.
func TestCoordination_ConcurrentInstancesRaceFree(t *testing.T) {
	c := newCoordHarness(t)
	reader := newFakeTaskReader()
	for i := range 50 {
		id := fmt.Sprintf("t-%02d", i)
		reader.set(id, querySnap(id, "q-"+id))
		c.src.publish(t, spawnEv(c.id, "", id, tasks.KindForeground, ""))
		c.src.publish(t, failedEv(c.id, id, "timeout"))
	}
	insts := []*Materializer{c.instance(t, "a", reader), c.instance(t, "b", reader)}
	var wg sync.WaitGroup
	var mu sync.Mutex
	active := map[int]bool{}
	for i, m := range insts {
		for range 10 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := m.Materialize(context.Background())
				if err != nil {
					t.Errorf("instance %d: %v", i, err)
					return
				}
				if !res.Standby {
					mu.Lock()
					active[i] = true
					mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	if len(active) != 1 {
		t.Fatalf("active instances = %v; want exactly one lease holder", active)
	}
	for i := range 50 {
		id := fmt.Sprintf("t-%02d", i)
		row := mustGetRow(t, c.harness, id)
		if !row.Sealed || row.Query.Text != "q-"+id {
			t.Fatalf("%s = %+v", id, row)
		}
	}
}
