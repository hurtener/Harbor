// assemble_pauseresume_test.go — Phase 111c (D-200) coverage for the
// assembly's pause-durability wiring: the §13 primitive-with-consumer
// closure (`WithCheckpointStore` gains its production consumer here —
// cmd + devstack inherit as thin callers), the config-gated sweeper
// start, and the sweeper's closer-chain join.
package assemble_test

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

var pauseTestID = identity.Identity{TenantID: "t-111c", UserID: "u-111c", SessionID: "s-111c"}

// TestAssemble_CoordinatorIsCheckpointStoreBacked proves the assembled
// Coordinator persists pauses through the runtime's own StateStore: a
// pause requested on stack #1 survives a full stack Close + a fresh
// Assemble over the SAME sqlite DSN (the restart shape), and the
// destructive-Resume contract holds across the restart.
func TestAssemble_CoordinatorIsCheckpointStoreBacked(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "assemble-111c.sqlite")
	cfg := minimalCfg(t)
	cfg.State = config.StateConfig{Driver: "sqlite", DSN: dsn}

	ctx, err := identity.WithRun(context.Background(), pauseTestID, "run-111c")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}

	// Stack #1: request a checkpointed pause, then shut down.
	stack1, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
	if err != nil {
		t.Fatalf("Assemble #1: %v", err)
	}
	p, err := stack1.Coordinator.Request(ctx, pauseresume.PauseRequest{
		Identity: pauseTestID,
		Reason:   pauseresume.ReasonApprovalRequired,
		Payload:  map[string]any{"prompt": "approve?"},
	})
	if err != nil {
		t.Fatalf("Request on stack #1: %v", err)
	}
	if err := stack1.Close(context.Background()); err != nil {
		t.Fatalf("Close #1: %v", err)
	}

	// Stack #2 — the restarted Runtime over the same store.
	stack2, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
	if err != nil {
		t.Fatalf("Assemble #2: %v", err)
	}
	defer func() { _ = stack2.Close(context.Background()) }()

	st, err := stack2.Coordinator.Status(ctx, p.Token)
	if err != nil {
		t.Fatalf("Status on restarted stack: %v (pause did not survive the restart — WithCheckpointStore not wired)", err)
	}
	if st.State != pauseresume.StatusPaused {
		t.Fatalf("Status.State = %q on the restarted stack, want paused", st.State)
	}
	if err := stack2.Coordinator.Resume(ctx, p.Token, pauseresume.DecisionApprove, nil); err != nil {
		t.Fatalf("Resume on restarted stack: %v", err)
	}
	// Destructive Resume: a third restart cannot find the token.
	stack3, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
	if err != nil {
		t.Fatalf("Assemble #3: %v", err)
	}
	defer func() { _ = stack3.Close(context.Background()) }()
	if _, err := stack3.Coordinator.Status(ctx, p.Token); !errors.Is(err, pauseresume.ErrPauseNotFound) {
		t.Fatalf("post-resume restart Status err = %v, want ErrPauseNotFound (checkpoint not cleared)", err)
	}
}

// TestAssemble_SweeperStart_ConfigGated proves both halves of the
// gate: max_park_duration > 0 starts the sweeper (an expired pause
// surfaces a `pause.resumed` event with the typed `timeout` Decision
// on the stack's bus — DecisionTimeout's first producer, live in the
// assembly), and the default 0 starts nothing while Close stays clean
// (goroutine baseline restored either way — the sweeper joins the
// closer chain).
func TestAssemble_SweeperStart_ConfigGated(t *testing.T) {
	t.Run("enabled — expired pause reaped with timeout decision", func(t *testing.T) {
		baseline := runtime.NumGoroutine()
		cfg := minimalCfg(t)
		cfg.PauseResume = config.PauseResumeConfig{
			MaxParkDuration: 20 * time.Millisecond,
			SweepInterval:   10 * time.Millisecond,
		}
		stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{})
		if err != nil {
			t.Fatalf("Assemble: %v", err)
		}

		sub, err := stack.Bus.Subscribe(context.Background(), events.Filter{
			Tenant: pauseTestID.TenantID, User: pauseTestID.UserID, Session: pauseTestID.SessionID,
			Types: []events.EventType{pauseresume.EventTypePauseResumed},
		})
		if err != nil {
			t.Fatalf("Subscribe: %v", err)
		}

		ctx, err := identity.WithRun(context.Background(), pauseTestID, "run-sweep")
		if err != nil {
			t.Fatalf("identity.WithRun: %v", err)
		}
		p, err := stack.Coordinator.Request(ctx, pauseresume.PauseRequest{
			Identity: pauseTestID,
			Reason:   pauseresume.ReasonAwaitInput,
		})
		if err != nil {
			t.Fatalf("Request: %v", err)
		}

		select {
		case ev := <-sub.Events():
			payload, ok := ev.Payload.(pauseresume.PauseResumedPayload)
			if !ok {
				t.Fatalf("payload type = %T, want PauseResumedPayload", ev.Payload)
			}
			if payload.Token != string(p.Token) || payload.Decision != pauseresume.DecisionTimeout {
				t.Fatalf("pause.resumed token=%q decision=%q, want %q/timeout", payload.Token, payload.Decision, p.Token)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("no timeout pause.resumed within 5s — the assembly did not start the sweeper")
		}

		sub.Cancel()
		if err := stack.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		settleGoroutines(t, baseline) // the sweeper goroutine is joined by the closer
	})

	t.Run("disabled by default — no sweeper, clean close", func(t *testing.T) {
		baseline := runtime.NumGoroutine()
		stack, err := assemble.Assemble(context.Background(), minimalCfg(t), assemble.Options{})
		if err != nil {
			t.Fatalf("Assemble: %v", err)
		}
		ctx, err := identity.WithRun(context.Background(), pauseTestID, "run-nosweep")
		if err != nil {
			t.Fatalf("identity.WithRun: %v", err)
		}
		p, err := stack.Coordinator.Request(ctx, pauseresume.PauseRequest{
			Identity: pauseTestID,
			Reason:   pauseresume.ReasonAwaitInput,
		})
		if err != nil {
			t.Fatalf("Request: %v", err)
		}
		st, err := stack.Coordinator.Status(ctx, p.Token)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if st.State != pauseresume.StatusPaused {
			t.Fatalf("Status.State = %q, want paused (nothing must reap with max_park_duration 0)", st.State)
		}
		if err := stack.Close(context.Background()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		settleGoroutines(t, baseline)
	})
}
