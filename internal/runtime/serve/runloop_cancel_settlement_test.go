package serve

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
)

// Pin the live warning's actual outcome through the served driver and real
// registry. A rejected second terminal write must not replace cancellation
// with a failure, or allow a late successful planner response to win.
func TestPerTaskRunLoopDriver_HardCancelPreservesTerminalSettlement(t *testing.T) {
	for _, lateSuccess := range []bool{false, true} {
		name := "context_error"
		if lateSuccess {
			name = "late_success"
		}
		t.Run(name, func(t *testing.T) {
			red := auditpatterns.New()
			bus := mkDriverTestBus(t, red)
			reg := mkDriverTestTaskRegistry(t, bus, red)
			steerReg := steering.NewRegistry()
			loop, err := steering.NewRunLoop(steerReg, pauseresume.New(pauseresume.WithBus(bus)), steering.WithRunLoopBus(bus))
			if err != nil {
				t.Fatal(err)
			}
			started, interrupted := make(chan struct{}), make(chan struct{})
			p := plannerFunc(func(ctx context.Context, _ planner.RunContext) (planner.Decision, error) {
				close(started)
				<-ctx.Done()
				close(interrupted)
				if lateSuccess {
					return planner.Finish{Reason: planner.FinishGoal}, nil
				}
				return nil, ctx.Err()
			})
			var logs bytes.Buffer // Read only after Close joins every run.
			driver, err := NewRunLoopDriver(RunLoopDriverOptions{
				SessionMemory: config.MemoryConfig{Strategy: "none"},
				Bus:           bus, RunLoop: loop, Planner: p, Tasks: reg,
				Logger: slog.New(slog.NewTextHandler(&logs, nil)),
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := driver.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = driver.Close(context.Background()) })
			taskID := spawnDriverTestTask(t, reg)
			select {
			case <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("planner did not start")
			}
			ctx, err := identity.With(t.Context(), runLoopDriverTestID)
			if err != nil {
				t.Fatal(err)
			}
			if changed, err := reg.Cancel(ctx, taskID, "owner stop"); err != nil || !changed {
				t.Fatalf("cancel = %v, %v", changed, err)
			}
			q := identity.Quadruple{Identity: runLoopDriverTestID, RunID: string(taskID)}
			inbox, err := steerReg.Lookup(q)
			if err != nil {
				t.Fatal(err)
			}
			if err := inbox.Enqueue(steering.ControlEvent{
				Type: steering.ControlCancel, Identity: q,
				CallerScope: steering.ScopeOwnerUser, CallerTenant: q.TenantID,
				Payload: map[string]any{"hard": true},
			}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-interrupted:
			case <-time.After(2 * time.Second):
				t.Fatal("hard Stop did not interrupt the active planner")
			}
			if err := driver.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			task, err := reg.Get(ctx, taskID)
			if err != nil {
				t.Fatal(err)
			}
			if task.Status != tasks.StatusCancelled || task.Error != nil || task.Result != nil {
				t.Fatalf("terminal task changed after Stop: status=%s error=%v result=%v", task.Status, task.Error, task.Result)
			}
			if !strings.Contains(logs.String(), "MarkFailed after Run error failed") {
				t.Fatal("did not reproduce the rejected second terminal-write warning")
			}
			if steerReg.Len() != 0 {
				t.Fatal("cancelled run left an active steering inbox")
			}
		})
	}
}
