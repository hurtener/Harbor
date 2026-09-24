package devstack_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/harbortest/devstack"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/tasks"
)

type sessionMemoryProbe struct{ requests chan string }

func (p *sessionMemoryProbe) Next(_ context.Context, rc planner.RunContext) (planner.Decision, error) {
	data, err := json.Marshal(rc.Trajectory)
	if err != nil {
		return nil, err
	}
	p.requests <- string(data)
	return planner.Finish{Reason: planner.FinishGoal, Payload: "acknowledged"}, nil
}

// The actual dev assembly must select the same execution history as serving
// and RunOnce. A hand-constructed driver would miss this wiring regression.
func TestDevStack_CumulativeMemoryUsesSharedConfig(t *testing.T) {
	cfg := minimalConfig(t)
	cfg.Memory = config.Defaults().Memory
	probe := &sessionMemoryProbe{requests: make(chan string, 2)}
	stack := devstack.Assemble(t, cfg, devstack.AssembleOpts{PlannerOverride: probe, SkipAuth: true, SkipTransports: true})
	defer stack.Close()
	id := identity.Identity{TenantID: "memory-tenant", UserID: "memory-user", SessionID: "memory-session"}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	ctx, err := identity.With(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for i, query := range []string{"Preserve navigation constraint ALPHA-791.", "Edit the footer."} {
		sub, err := stack.Bus.Subscribe(ctx, events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
			Types: []events.EventType{tasks.EventTypeTaskCompleted, tasks.EventTypeTaskFailed, tasks.EventTypeTaskCancelled}})
		if err != nil {
			t.Fatal(err)
		}
		handle, err := stack.Tasks.Spawn(ctx, tasks.SpawnRequest{Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground, Query: query})
		if err != nil {
			sub.Cancel()
			t.Fatal(err)
		}
		select {
		case ev, ok := <-sub.Events():
			if !ok || ev.Type != tasks.EventTypeTaskCompleted {
				task, getErr := stack.Tasks.Get(ctx, handle.ID)
				sub.Cancel()
				t.Fatalf("turn %d not complete: task=%+v err=%v", i, task, getErr)
			}
		case <-ctx.Done():
			sub.Cancel()
			t.Fatal("memory turn did not terminate")
		}
		sub.Cancel()
		select {
		case request := <-probe.requests:
			if i == 1 && !strings.Contains(request, "ALPHA-791") {
				t.Fatal("dev assembly lost first-turn execution evidence")
			}
		case <-ctx.Done():
			t.Fatal("planner input not observed")
		}
	}
}
