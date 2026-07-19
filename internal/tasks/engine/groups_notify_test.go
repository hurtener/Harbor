package engine_test

import (
	"context"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/engine"
)

// TestEngine_CollectMemberOutcomes_PopulatesDescription asserts the
// additive MemberOutcome.Description is populated from the member
// Task.Description at the resolve call site — the ref-shaped field a
// conversational-notification mirror reads without a second registry
// read. It exercises the real resolve path through WatchGroup.
func TestEngine_CollectMemberOutcomes_PopulatesDescription(t *testing.T) {
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	eng, err := engine.New(bus, auditpatterns.New(), &memBackend{})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	id := idQuad()
	ctx, _ := identity.With(context.Background(), id.Identity)

	g, err := eng.ResolveOrCreateGroup(ctx, tasks.GroupRequest{SessionID: id.Identity})
	if err != nil {
		t.Fatalf("ResolveOrCreateGroup: %v", err)
	}
	descs := map[tasks.TaskID]string{}
	for _, d := range []string{"fetch the changelog", "summarise the diff"} {
		h, err := eng.Spawn(ctx, tasks.SpawnRequest{
			Identity:    id,
			Kind:        tasks.KindBackground,
			Description: d,
			GroupID:     g.ID,
		})
		if err != nil {
			t.Fatalf("Spawn: %v", err)
		}
		descs[h.ID] = d
	}
	if err := eng.SealGroup(ctx, g.ID); err != nil {
		t.Fatalf("SealGroup: %v", err)
	}
	watch, cancel, err := eng.WatchGroup(id.Identity, g.ID)
	if err != nil {
		t.Fatalf("WatchGroup: %v", err)
	}
	defer cancel()
	for tid := range descs {
		if err := eng.MarkRunning(ctx, tid); err != nil {
			t.Fatalf("MarkRunning: %v", err)
		}
		if err := eng.MarkComplete(ctx, tid, tasks.TaskResult{Value: []byte(`"ok"`)}); err != nil {
			t.Fatalf("MarkComplete: %v", err)
		}
	}
	select {
	case c, ok := <-watch:
		if !ok {
			t.Fatal("WatchGroup closed without delivery")
		}
		if len(c.Members) != len(descs) {
			t.Fatalf("Members len=%d, want %d", len(c.Members), len(descs))
		}
		for _, m := range c.Members {
			want := descs[m.TaskID]
			if m.Description != want {
				t.Errorf("member %q Description=%q, want %q (populated from Task.Description)", m.TaskID, m.Description, want)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("WatchGroup did not deliver")
	}
}

// TestEngine_MarkComplete_PopulatesNotifyOnComplete asserts the
// task.completed event carries the completing task's NotifyOnComplete
// opt-in — the field a conversational-notification consumer gates the
// background-success wake line on. A non-opted task carries false.
func TestEngine_MarkComplete_PopulatesNotifyOnComplete(t *testing.T) {
	for _, tc := range []struct {
		name   string
		notify bool
	}{
		{"opted-in", true},
		{"not-opted", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bus := mkBus(t)
			defer func() { _ = bus.Close(context.Background()) }()
			eng, err := engine.New(bus, auditpatterns.New(), &memBackend{})
			if err != nil {
				t.Fatalf("engine.New: %v", err)
			}
			id := idQuad()
			ctx, _ := identity.With(context.Background(), id.Identity)

			sub, err := bus.Subscribe(ctx, events.Filter{
				Tenant:  id.TenantID,
				User:    id.UserID,
				Session: id.SessionID,
				Types:   []events.EventType{tasks.EventTypeTaskCompleted},
			})
			if err != nil {
				t.Fatalf("Subscribe: %v", err)
			}
			defer sub.Cancel()

			h, err := eng.Spawn(ctx, tasks.SpawnRequest{
				Identity:         id,
				Kind:             tasks.KindBackground,
				NotifyOnComplete: tc.notify,
			})
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			if err := eng.MarkRunning(ctx, h.ID); err != nil {
				t.Fatalf("MarkRunning: %v", err)
			}
			if err := eng.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`"done"`)}); err != nil {
				t.Fatalf("MarkComplete: %v", err)
			}
			select {
			case ev, ok := <-sub.Events():
				if !ok {
					t.Fatal("subscription closed without delivery")
				}
				p, ok := ev.Payload.(tasks.TaskCompletedPayload)
				if !ok {
					t.Fatalf("payload type=%T, want TaskCompletedPayload", ev.Payload)
				}
				if p.NotifyOnComplete != tc.notify {
					t.Errorf("NotifyOnComplete=%v, want %v (echoed from Task record)", p.NotifyOnComplete, tc.notify)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("no task.completed event delivered")
			}
		})
	}
}
