package flow_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/flow"
	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// regPassthrough is a minimal engine.NodeFunc for registry-test
// definition fixtures.
func regPassthrough(_ context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
	return in, nil
}

// fixtureDef builds a minimal valid two-node flow Definition.
func fixtureDef(name string) flow.Definition {
	return flow.Definition{
		Name:  name,
		Entry: "a",
		Exit:  "b",
		Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Name: "a", Func: regPassthrough, To: []flow.NodeID{"b"}},
			"b": {Name: "b", Func: regPassthrough},
		},
		Budget:    flow.Budget{Deadline: time.Minute, HopBudget: 10, CostCap: 1.5},
		InSchema:  json.RawMessage(`{}`),
		OutSchema: json.RawMessage(`{}`),
	}
}

func TestRegistry_Register_RejectsDuplicate(t *testing.T) {
	r := flow.NewRegistry()
	def := fixtureDef("flow-x")
	if err := r.Register(def, flow.Metadata{Owner: "team-a", PlannerFamily: "graph"}); err != nil {
		t.Fatalf("Register: unexpected error: %v", err)
	}
	if err := r.Register(def, flow.Metadata{}); err == nil {
		t.Fatal("Register: expected duplicate-registration error, got nil")
	}
}

func TestRegistry_Register_RejectsInvalidDefinition(t *testing.T) {
	r := flow.NewRegistry()
	if err := r.Register(flow.Definition{}, flow.Metadata{}); err == nil {
		t.Fatal("Register: expected invalid-definition error, got nil")
	}
}

func TestRegistry_Names_SortedAndDefinitionRoundTrips(t *testing.T) {
	r := flow.NewRegistry()
	for _, n := range []string{"flow-c", "flow-a", "flow-b"} {
		if err := r.Register(fixtureDef(n), flow.Metadata{Source: "internal/flows/" + n + ".go"}); err != nil {
			t.Fatalf("Register(%s): %v", n, err)
		}
	}
	names := r.Names()
	want := []string{"flow-a", "flow-b", "flow-c"}
	if len(names) != len(want) {
		t.Fatalf("Names() = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("Names()[%d] = %q, want %q", i, names[i], want[i])
		}
	}
	def, meta, ok := r.Definition("flow-b")
	if !ok {
		t.Fatal("Definition(flow-b): not found")
	}
	if def.Name != "flow-b" {
		t.Fatalf("Definition name = %q, want flow-b", def.Name)
	}
	if meta.Source != "internal/flows/flow-b.go" {
		t.Fatalf("Definition meta.Source = %q", meta.Source)
	}
}

func TestRegistry_RecordRun_UnknownFlowFailsLoud(t *testing.T) {
	r := flow.NewRegistry()
	if err := r.RecordRun(flow.RunRecord{FlowName: "ghost", RunID: "r1"}); err == nil {
		t.Fatal("RecordRun: expected unknown-flow error, got nil")
	}
}

func TestRegistry_RecordRun_RingIsBounded(t *testing.T) {
	r := flow.NewRegistry()
	if err := r.Register(fixtureDef("flow-ring"), flow.Metadata{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	const overshoot = 1100
	for i := 0; i < overshoot; i++ {
		if err := r.RecordRun(flow.RunRecord{
			FlowName:  "flow-ring",
			RunID:     fmt.Sprintf("run-%d", i),
			StartedAt: time.Now(),
			Identity:  identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
		}); err != nil {
			t.Fatalf("RecordRun(%d): %v", i, err)
		}
	}
	runs, ok := r.Runs("flow-ring")
	if !ok {
		t.Fatal("Runs(flow-ring): not found")
	}
	if len(runs) != 1000 {
		t.Fatalf("Runs() length = %d, want 1000 (ring bound)", len(runs))
	}
}

func TestRegistry_RunByID_FindsAcrossFlows(t *testing.T) {
	r := flow.NewRegistry()
	if err := r.Register(fixtureDef("flow-1"), flow.Metadata{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.Register(fixtureDef("flow-2"), flow.Metadata{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.RecordRun(flow.RunRecord{FlowName: "flow-2", RunID: "target-run", StartedAt: time.Now()}); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}
	rec, ok := r.RunByID("target-run")
	if !ok {
		t.Fatal("RunByID(target-run): not found")
	}
	if rec.FlowName != "flow-2" {
		t.Fatalf("RunByID flow = %q, want flow-2", rec.FlowName)
	}
	if _, ok := r.RunByID("missing"); ok {
		t.Fatal("RunByID(missing): expected not found")
	}
}
