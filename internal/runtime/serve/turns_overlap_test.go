package serve

import (
	"context"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestTaskSnapshotAdapter_OverlapReadsPeerInstanceRecord reproduces the
// rolling-deploy overlap at the serve seam: two durable task registries
// (the old and the new instance) share one StateStore. A task spawned on
// the NEW instance is not in the OLD instance's memory, yet the old
// instance's snapshot adapter — the materializer's window onto task
// records — returns the query and agent from the persisted record
// instead of reporting the record absent (which used to append the turn
// without the user's message).
func TestTaskSnapshotAdapter_OverlapReadsPeerInstanceRecord(t *testing.T) {
	ctx := context.Background()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	open := func() tasks.TaskRegistry {
		bus, err := eventsinmem.New(config.EventsConfig{
			Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32,
			IdleTimeout: 30 * time.Second, DropWindow: time.Second,
		}, auditpatterns.New())
		if err != nil {
			t.Fatalf("bus: %v", err)
		}
		reg, err := tasks.OpenDriver("durable", tasks.Dependencies{
			Store: st, Bus: bus, Redactor: auditpatterns.New(), Cfg: config.TasksConfig{Driver: "durable"},
		})
		if err != nil {
			t.Fatalf("durable registry: %v", err)
		}
		t.Cleanup(func() { _ = reg.Close(ctx); _ = bus.Close(ctx) })
		return reg
	}
	oldInst, newInst := open(), open()
	t.Cleanup(func() { _ = st.Close(ctx) })

	id := identity.Identity{TenantID: "t-ov", UserID: "u-ov", SessionID: "s-ov"}
	spawnCtx, _ := identity.With(ctx, id)
	h, err := newInst.Spawn(spawnCtx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground,
		Query: "summarize the report", AgentID: "analyst",
	})
	if err != nil {
		t.Fatalf("spawn on the new instance: %v", err)
	}
	adapter := &taskSnapshotAdapter{reg: oldInst, clock: time.Now}
	snap, err := adapter.Task(ctx, id, string(h.ID))
	if err != nil {
		t.Fatalf("old instance snapshot: %v (must not report the peer's record absent)", err)
	}
	if !snap.QueryPresent || snap.Query != "summarize the report" || !snap.AgentPresent || snap.AgentID != "analyst" {
		t.Fatalf("snapshot = %+v; want the peer's persisted query and agent", snap)
	}
}
