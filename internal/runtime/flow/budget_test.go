package flow_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/flow"
	"github.com/hurtener/Harbor/internal/runtime/messages"
	"github.com/hurtener/Harbor/internal/tools"
)

// TestBudget_Composition_ParentWinsLower: parent's tighter
// deadline wins via min().
func TestBudget_Composition_ParentWinsLower(t *testing.T) {
	def := flow.Definition{
		Name:  "noop_flow",
		Entry: "a",
		Exit:  "a",
		Budget: flow.Budget{
			Deadline: 1 * time.Second,
		},
		Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: func(ctx context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
				select {
				case <-ctx.Done():
					return messages.Envelope{}, ctx.Err()
				case <-time.After(200 * time.Millisecond):
					return in, nil
				}
			}},
		},
	}
	eng, err := flow.Compose(def)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if err = eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = eng.Stop(ctx)
	})

	cat := tools.NewCatalog()
	_, err = flow.RegisterAsTool(cat, def, eng)
	if err != nil {
		t.Fatalf("RegisterAsTool: %v", err)
	}
	d, _ := cat.Resolve("noop_flow")

	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, _ := identity.With(context.Background(), id)
	ctx = flow.WithBudget(ctx, flow.Budget{Deadline: 50 * time.Millisecond})

	start := time.Now()
	_, err = d.Invoke(ctx, []byte(`{}`))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected ErrFlowBudgetExceeded, got nil")
	}
	if !errors.Is(err, flow.ErrFlowBudgetExceeded) {
		t.Fatalf("expected ErrFlowBudgetExceeded, got: %v", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("budget should have fired before 200ms; elapsed=%v", elapsed)
	}
}

func TestBudget_Composition_SelfWinsLower(t *testing.T) {
	def := flow.Definition{
		Name:  "tight_flow",
		Entry: "a",
		Exit:  "a",
		Budget: flow.Budget{
			Deadline: 30 * time.Millisecond,
		},
		Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: func(ctx context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
				select {
				case <-ctx.Done():
					return messages.Envelope{}, ctx.Err()
				case <-time.After(200 * time.Millisecond):
					return in, nil
				}
			}},
		},
	}
	eng, err := flow.Compose(def)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if err = eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = eng.Stop(ctx)
	})

	cat := tools.NewCatalog()
	_, err = flow.RegisterAsTool(cat, def, eng)
	if err != nil {
		t.Fatalf("RegisterAsTool: %v", err)
	}
	d, _ := cat.Resolve("tight_flow")

	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, _ := identity.With(context.Background(), id)
	ctx = flow.WithBudget(ctx, flow.Budget{Deadline: 5 * time.Second})

	start := time.Now()
	_, err = d.Invoke(ctx, []byte(`{}`))
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("expected ErrFlowBudgetExceeded, got nil")
	}
	if !errors.Is(err, flow.ErrFlowBudgetExceeded) {
		t.Fatalf("expected ErrFlowBudgetExceeded, got: %v", err)
	}
	if elapsed > 200*time.Millisecond {
		t.Errorf("self-budget should have fired before 200ms; elapsed=%v", elapsed)
	}
}

func TestBudget_HopBudget_SingleInvocationSucceeds(t *testing.T) {
	def := flow.Definition{
		Name:  "hop_flow",
		Entry: "a",
		Exit:  "a",
		Budget: flow.Budget{
			HopBudget: 1,
		},
		Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: passthrough},
		},
	}
	eng, err := flow.Compose(def)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if err = eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = eng.Stop(ctx)
	})

	cat := tools.NewCatalog()
	_, err = flow.RegisterAsTool(cat, def, eng)
	if err != nil {
		t.Fatalf("RegisterAsTool: %v", err)
	}
	d, _ := cat.Resolve("hop_flow")
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, _ := identity.With(context.Background(), id)
	_, err = d.Invoke(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("HopBudget=1 + 1 invocation should succeed, got: %v", err)
	}
}

func TestBudget_NoCap_NoBudgetError(t *testing.T) {
	def := flow.Definition{
		Name:  "free_flow",
		Entry: "a",
		Exit:  "a",
		Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: passthrough},
		},
	}
	eng, err := flow.Compose(def)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if err = eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = eng.Stop(ctx)
	})

	cat := tools.NewCatalog()
	_, err = flow.RegisterAsTool(cat, def, eng)
	if err != nil {
		t.Fatalf("RegisterAsTool: %v", err)
	}
	d, _ := cat.Resolve("free_flow")
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, _ := identity.With(context.Background(), id)
	_, err = d.Invoke(ctx, json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("no-cap flow should succeed, got: %v", err)
	}
}

func TestBudget_DeadlineEvent_Emits(t *testing.T) {
	bus := newRecordingBus()

	def := flow.Definition{
		Name:  "slow_flow_event",
		Entry: "a",
		Exit:  "a",
		Budget: flow.Budget{
			Deadline: 10 * time.Millisecond,
		},
		Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: func(ctx context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
				select {
				case <-ctx.Done():
					return messages.Envelope{}, ctx.Err()
				case <-time.After(200 * time.Millisecond):
					return in, nil
				}
			}},
		},
	}
	eng, err := flow.Compose(def)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if err = eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = eng.Stop(ctx)
	})

	cat := tools.NewCatalog()
	_, err = flow.RegisterAsTool(cat, def, eng)
	if err != nil {
		t.Fatalf("RegisterAsTool: %v", err)
	}
	d, _ := cat.Resolve("slow_flow_event")
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, _ := identity.With(context.Background(), id)
	ctx = withBusOnCtx(ctx, bus)
	_, err = d.Invoke(ctx, json.RawMessage(`{}`))
	if err == nil {
		t.Fatalf("expected ErrFlowBudgetExceeded, got nil")
	}
	if got := bus.Count(flow.EventTypeFlowBudgetExceeded); got != 1 {
		t.Errorf("expected 1 flow.budget_exceeded event, got %d", got)
	}
}
