package inprocess_test

import (
	"context"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
)

// freshRegistryWithBus is freshRegistry that also returns the bus so a
// test can subscribe and assert the typed group-cancel payload the
// engine emits (including the CancelOrigin it stamps at the call site).
func freshRegistryWithBus(t *testing.T) (tasks.TaskRegistry, events.EventBus, func()) {
	t.Helper()
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem New: %v", err)
	}
	redactor := auditpatterns.New()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
		ReplayBufferSize:         1024,
	}, redactor)
	if err != nil {
		t.Fatalf("events inmem New: %v", err)
	}
	r, err := tasks.OpenDriver("inprocess", tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: redactor,
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("OpenDriver: %v", err)
	}
	return r, bus, func() {
		ctx := context.Background()
		_ = r.Close(ctx)
		_ = bus.Close(ctx)
		_ = store.Close(ctx)
	}
}

// waitGroupCancelled drains the subscription until a task.group_cancelled
// event arrives (bounded — never a sleep) and returns its typed payload.
func waitGroupCancelled(t *testing.T, sub events.Subscription) tasks.TaskGroupCancelledPayload {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed before task.group_cancelled arrived")
			}
			if ev.Type != tasks.EventTypeTaskGroupCancelled {
				continue
			}
			p, ok := ev.Payload.(tasks.TaskGroupCancelledPayload)
			if !ok {
				t.Fatalf("task.group_cancelled payload type=%T, want TaskGroupCancelledPayload (SafePayload)", ev.Payload)
			}
			return p
		case <-deadline:
			t.Fatal("deadline before task.group_cancelled arrived")
		}
	}
}

// TestCancelGroup_StampsOperatorOrigin proves a direct CancelGroup — the
// operator/agent driving the cancel — stamps CancelOriginOperator, so a
// conversational mirror suppresses it (the actor already knows).
func TestCancelGroup_StampsOperatorOrigin(t *testing.T) {
	r, bus, cleanup := freshRegistryWithBus(t)
	defer cleanup()
	ctx := ctxA(t)

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{tasks.EventTypeTaskGroupCancelled},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{SessionID: tripleA().Identity})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.CancelGroup(ctx, g.ID, "operator says stop", true); err != nil {
		t.Fatalf("CancelGroup: %v", err)
	}

	p := waitGroupCancelled(t, sub)
	if p.Origin != tasks.CancelOriginOperator {
		t.Errorf("Origin=%q, want operator (a direct CancelGroup is operator-driven)", p.Origin)
	}
	if p.Completion.FinalStatus != tasks.GroupCancelled {
		t.Errorf("FinalStatus=%q, want cancelled", p.Completion.FinalStatus)
	}
}

// TestFailFastGroup_StampsFailFastOrigin proves the fail-fast gate
// firing on a member failure stamps CancelOriginFailFast, so the
// conversational mirror surfaces the unprompted cancel.
func TestFailFastGroup_StampsFailFastOrigin(t *testing.T) {
	r, bus, cleanup := freshRegistryWithBus(t)
	defer cleanup()
	ctx := ctxA(t)

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{tasks.EventTypeTaskGroupCancelled},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
		SessionID: tripleA().Identity,
		FailFast:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	members := make([]tasks.TaskID, 0, 3)
	for range 3 {
		h, err := r.Spawn(ctx, tasks.SpawnRequest{
			Identity: tripleA(),
			Kind:     tasks.KindForeground,
			GroupID:  g.ID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := r.MarkRunning(ctx, h.ID); err != nil {
			t.Fatal(err)
		}
		members = append(members, h.ID)
	}
	if err := r.SealGroup(ctx, g.ID); err != nil {
		t.Fatal(err)
	}
	// Fail the first member → fail-fast cancels the rest AND cancels the group.
	if err := r.MarkFailed(ctx, members[0], tasks.TaskError{Code: "boom"}); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	p := waitGroupCancelled(t, sub)
	if p.Origin != tasks.CancelOriginFailFast {
		t.Errorf("Origin=%q, want failfast (a fail-fast gate fired)", p.Origin)
	}
}
