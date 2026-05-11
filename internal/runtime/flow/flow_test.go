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

func passthrough(_ context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
	return in, nil
}

func TestDefinition_Validate_RejectsMissingFields(t *testing.T) {
	cases := []struct {
		name string
		def  flow.Definition
		want error
	}{
		{"empty-name", flow.Definition{Entry: "a", Exit: "a", Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: passthrough},
		}}, flow.ErrFlowInvalidDefinition},
		{"empty-entry", flow.Definition{Name: "x", Exit: "a", Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: passthrough},
		}}, flow.ErrFlowInvalidDefinition},
		{"empty-exit", flow.Definition{Name: "x", Entry: "a", Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: passthrough},
		}}, flow.ErrFlowInvalidDefinition},
		{"empty-nodes", flow.Definition{Name: "x", Entry: "a", Exit: "a"}, flow.ErrFlowInvalidDefinition},
		{"entry-not-in-nodes", flow.Definition{Name: "x", Entry: "missing", Exit: "a", Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: passthrough},
		}}, flow.ErrFlowEntryExitMismatch},
		{"exit-not-in-nodes", flow.Definition{Name: "x", Entry: "a", Exit: "missing", Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: passthrough},
		}}, flow.ErrFlowEntryExitMismatch},
		{"to-references-missing", flow.Definition{Name: "x", Entry: "a", Exit: "a", Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: passthrough, To: []flow.NodeID{"missing"}},
		}}, flow.ErrFlowEntryExitMismatch},
		{"nil-func", flow.Definition{Name: "x", Entry: "a", Exit: "a", Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: nil},
		}}, flow.ErrFlowInvalidDefinition},
		{"negative-budget-deadline", flow.Definition{Name: "x", Entry: "a", Exit: "a", Budget: flow.Budget{Deadline: -1 * time.Second}, Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: passthrough},
		}}, flow.ErrFlowInvalidDefinition},
		{"negative-budget-hops", flow.Definition{Name: "x", Entry: "a", Exit: "a", Budget: flow.Budget{HopBudget: -1}, Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: passthrough},
		}}, flow.ErrFlowInvalidDefinition},
	}
	for _, c := range cases {
		err := c.def.Validate()
		if err == nil {
			t.Errorf("%s: expected error, got nil", c.name)
			continue
		}
		if !errors.Is(err, c.want) {
			t.Errorf("%s: expected %v, got %v", c.name, c.want, err)
		}
	}
}

func TestDefinition_Validate_Valid_3Node(t *testing.T) {
	def := flow.Definition{
		Name:  "three_node",
		Entry: "a",
		Exit:  "c",
		Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: passthrough, To: []flow.NodeID{"b"}},
			"b": {Func: passthrough, To: []flow.NodeID{"c"}},
			"c": {Func: passthrough},
		},
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestCompose_BuildsEngine(t *testing.T) {
	def := flow.Definition{
		Name:  "two_node",
		Entry: "a",
		Exit:  "b",
		Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: passthrough, To: []flow.NodeID{"b"}},
			"b": {Func: passthrough},
		},
	}
	eng, err := flow.Compose(def)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if eng == nil {
		t.Fatalf("Compose returned nil engine")
	}
}

func TestRegisterAsTool_3NodeFlow_RoundTrips(t *testing.T) {
	def := flow.Definition{
		Name:        "three_node",
		Description: "3-node passthrough",
		Entry:       "a",
		Exit:        "c",
		Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: passthrough, To: []flow.NodeID{"b"}},
			"b": {Func: passthrough, To: []flow.NodeID{"c"}},
			"c": {Func: passthrough},
		},
	}
	type FlowIn struct {
		Q string `json:"q"`
	}
	type FlowOut struct {
		Result string `json:"result"`
	}
	def, err := flow.WithSchemasFrom[FlowIn, FlowOut](def)
	if err != nil {
		t.Fatalf("WithSchemasFrom: %v", err)
	}

	eng, err := flow.Compose(def)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if err := eng.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = eng.Stop(ctx)
	})

	cat := tools.NewCatalog()
	tool, err := flow.RegisterAsTool(cat, def, eng)
	if err != nil {
		t.Fatalf("RegisterAsTool: %v", err)
	}
	if tool.Transport != tools.TransportFlow {
		t.Errorf("expected TransportFlow, got %q", tool.Transport)
	}
	if string(tool.ArgsSchema) == "" || string(tool.ArgsSchema) == "{}" {
		t.Errorf("expected derived InSchema, got %q", tool.ArgsSchema)
	}
	if string(tool.OutSchema) == "" || string(tool.OutSchema) == "{}" {
		t.Errorf("expected derived OutSchema, got %q", tool.OutSchema)
	}

	d, ok := cat.Resolve("three_node")
	if !ok {
		t.Fatalf("Resolve: not found")
	}

	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	args := json.RawMessage(`{"q":"hello"}`)
	res, err := d.Invoke(ctx, args)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if res.Value == nil {
		t.Fatalf("expected non-nil result")
	}
}

func TestRegisterAsTool_DeadlineExceeded_FiresBudgetEvent(t *testing.T) {
	slow := func(ctx context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
		select {
		case <-ctx.Done():
			return messages.Envelope{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
			return in, nil
		}
	}
	def := flow.Definition{
		Name:  "slow_flow",
		Entry: "a",
		Exit:  "a",
		Budget: flow.Budget{
			Deadline: 10 * time.Millisecond,
		},
		Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: slow},
		},
	}
	eng, err := flow.Compose(def)
	if err != nil {
		t.Fatalf("Compose: %v", err)
	}
	if err := eng.Run(context.Background()); err != nil {
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

	d, _ := cat.Resolve("slow_flow")
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, _ := identity.With(context.Background(), id)
	_, err = d.Invoke(ctx, []byte(`{}`))
	if err == nil {
		t.Fatalf("expected ErrFlowBudgetExceeded, got nil")
	}
	if !errors.Is(err, flow.ErrFlowBudgetExceeded) {
		t.Fatalf("expected ErrFlowBudgetExceeded, got: %v", err)
	}
}

func TestRegisterAsTool_NoIdentity_Rejects(t *testing.T) {
	def := flow.Definition{
		Name:  "test_flow",
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
	if err := eng.Run(context.Background()); err != nil {
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
	d, _ := cat.Resolve("test_flow")
	_, err = d.Invoke(context.Background(), []byte(`{}`))
	if err == nil {
		t.Fatalf("expected identity error, got nil")
	}
}

func TestRegisterAsTool_NilCatalog_Rejects(t *testing.T) {
	def := flow.Definition{
		Name:  "x",
		Entry: "a",
		Exit:  "a",
		Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: passthrough},
		},
	}
	eng, _ := flow.Compose(def)
	_, err := flow.RegisterAsTool(nil, def, eng)
	if err == nil {
		t.Fatalf("expected nil-catalog error, got nil")
	}
}

func TestRegisterAsTool_NilEngine_Rejects(t *testing.T) {
	def := flow.Definition{
		Name:  "x",
		Entry: "a",
		Exit:  "a",
		Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Func: passthrough},
		},
	}
	cat := tools.NewCatalog()
	_, err := flow.RegisterAsTool(cat, def, nil)
	if err == nil {
		t.Fatalf("expected nil-engine error, got nil")
	}
}
