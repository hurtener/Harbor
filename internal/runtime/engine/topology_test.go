package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// passthrough is a no-op NodeFunc — enough for graph-shape tests that
// never Run the engine.
func passthrough(_ context.Context, env messages.Envelope, _ *NodeContext) (messages.Envelope, error) {
	return env, nil
}

// topoCtx builds a context carrying a complete identity triple — the
// identity-mandatory Topology accessor needs one.
func topoCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), identity.Identity{
		TenantID:  "tenant-topo",
		UserID:    "user-topo",
		SessionID: "session-topo",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func nodesByName(p types.TopologyProjection) map[string]types.TopologyNode {
	out := make(map[string]types.TopologyNode, len(p.Nodes))
	for _, n := range p.Nodes {
		out[n.Name] = n
	}
	return out
}

// TestTopology_LinearGraph_KindsAndEdges — a simple inlet → mid →
// outlet chain projects three nodes with the right kinds and two edges.
func TestTopology_LinearGraph_KindsAndEdges(t *testing.T) {
	in := Node{Name: "in", Func: passthrough}
	mid := Node{Name: "mid", Func: passthrough}
	out := Node{Name: "out", Func: passthrough}
	eng, err := New([]Adjacency{
		{From: in, To: []Node{mid}},
		{From: mid, To: []Node{out}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	proj, err := eng.Topology(topoCtx(t))
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}
	if proj.EngineID == "" {
		t.Error("projection EngineID is empty")
	}
	byName := nodesByName(proj)
	if byName["in"].Kind != types.NodeKindInlet {
		t.Errorf("in kind = %q, want inlet", byName["in"].Kind)
	}
	if byName["mid"].Kind != types.NodeKindNode {
		t.Errorf("mid kind = %q, want node", byName["mid"].Kind)
	}
	if byName["out"].Kind != types.NodeKindOutlet {
		t.Errorf("out kind = %q, want outlet", byName["out"].Kind)
	}
	if len(proj.Edges) != 2 {
		t.Fatalf("Edges len = %d, want 2", len(proj.Edges))
	}
	// Deterministic order: (in,mid) then (mid,out).
	if proj.Edges[0].From != "in" || proj.Edges[0].To != "mid" {
		t.Errorf("Edges[0] = %+v, want in→mid", proj.Edges[0])
	}
	if proj.Edges[1].From != "mid" || proj.Edges[1].To != "out" {
		t.Errorf("Edges[1] = %+v, want mid→out", proj.Edges[1])
	}
	// Default queue capacity is DefaultQueueSize on every edge.
	for _, e := range proj.Edges {
		if e.QueueCapacity != DefaultQueueSize {
			t.Errorf("edge %s→%s QueueCapacity = %d, want %d", e.From, e.To, e.QueueCapacity, DefaultQueueSize)
		}
		if e.QueueDepth != 0 {
			t.Errorf("edge %s→%s QueueDepth = %d, want 0 (idle engine)", e.From, e.To, e.QueueDepth)
		}
	}
}

// TestTopology_MultiInletMultiOutlet — a diamond with two inlets and
// two outlets tags each node correctly.
func TestTopology_MultiInletMultiOutlet(t *testing.T) {
	inA := Node{Name: "inA", Func: passthrough}
	inB := Node{Name: "inB", Func: passthrough}
	hub := Node{Name: "hub", Func: passthrough}
	outA := Node{Name: "outA", Func: passthrough}
	outB := Node{Name: "outB", Func: passthrough}
	eng, err := New([]Adjacency{
		{From: inA, To: []Node{hub}},
		{From: inB, To: []Node{hub}},
		{From: hub, To: []Node{outA, outB}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proj, err := eng.Topology(topoCtx(t))
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}
	byName := nodesByName(proj)
	for _, n := range []string{"inA", "inB"} {
		if byName[n].Kind != types.NodeKindInlet {
			t.Errorf("%s kind = %q, want inlet", n, byName[n].Kind)
		}
	}
	for _, n := range []string{"outA", "outB"} {
		if byName[n].Kind != types.NodeKindOutlet {
			t.Errorf("%s kind = %q, want outlet", n, byName[n].Kind)
		}
	}
	if byName["hub"].Kind != types.NodeKindNode {
		t.Errorf("hub kind = %q, want node", byName["hub"].Kind)
	}
	if len(proj.Edges) != 4 {
		t.Fatalf("Edges len = %d, want 4", len(proj.Edges))
	}
}

// TestTopology_ChannelOverride_QueueCapacity — a WithChannelOverride
// edge reports its overridden capacity, not the engine default.
func TestTopology_ChannelOverride_QueueCapacity(t *testing.T) {
	in := Node{Name: "in", Func: passthrough}
	out := Node{Name: "out", Func: passthrough}
	const override = 7
	eng, err := New([]Adjacency{{From: in, To: []Node{out}}},
		WithChannelOverride(in.Ref(), out.Ref(), override))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	proj, err := eng.Topology(topoCtx(t))
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}
	if len(proj.Edges) != 1 {
		t.Fatalf("Edges len = %d, want 1", len(proj.Edges))
	}
	if proj.Edges[0].QueueCapacity != override {
		t.Errorf("QueueCapacity = %d, want %d (override)", proj.Edges[0].QueueCapacity, override)
	}
}

// TestTopology_CycleGraph_AllowCycle — a controller self-loop projects
// without error; the self-edge appears in the edge set. The graph needs
// a real inlet (a node with no parent) feeding the loop — a bare
// self-loop has no inlet and New rejects it.
func TestTopology_CycleGraph_AllowCycle(t *testing.T) {
	in := Node{Name: "in", Func: passthrough}
	loop := Node{Name: "loop", Func: passthrough, AllowCycle: true}
	sink := Node{Name: "sink", Func: passthrough}
	eng, err := New([]Adjacency{
		{From: in, To: []Node{loop}},
		{From: loop, To: []Node{loop, sink}},
	})
	if err != nil {
		t.Fatalf("New (AllowCycle): %v", err)
	}
	proj, err := eng.Topology(topoCtx(t))
	if err != nil {
		t.Fatalf("Topology: %v", err)
	}
	var hasSelfEdge bool
	for _, e := range proj.Edges {
		if e.From == "loop" && e.To == "loop" {
			hasSelfEdge = true
		}
	}
	if !hasSelfEdge {
		t.Error("self-edge loop→loop missing from projection")
	}
}

// TestTopology_IdentityMandatory_RejectsUnscopedCtx — an unscoped ctx
// fails closed with ErrIdentityRequired (CLAUDE.md §6 rule 9).
func TestTopology_IdentityMandatory_RejectsUnscopedCtx(t *testing.T) {
	in := Node{Name: "in", Func: passthrough}
	out := Node{Name: "out", Func: passthrough}
	eng, err := New([]Adjacency{{From: in, To: []Node{out}}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, topoErr := eng.Topology(context.Background())
	if !errors.Is(topoErr, ErrIdentityRequired) {
		t.Fatalf("Topology(unscoped ctx) error = %v, want ErrIdentityRequired", topoErr)
	}
}

// TestTopology_Deterministic — two calls against the same idle engine
// return byte-identical projections (modulo OccurredAt).
func TestTopology_Deterministic(t *testing.T) {
	in := Node{Name: "z", Func: passthrough}
	mid := Node{Name: "a", Func: passthrough}
	out := Node{Name: "m", Func: passthrough}
	eng, err := New([]Adjacency{
		{From: in, To: []Node{mid}},
		{From: mid, To: []Node{out}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := topoCtx(t)
	p1, err := eng.Topology(ctx)
	if err != nil {
		t.Fatalf("Topology #1: %v", err)
	}
	p2, err := eng.Topology(ctx)
	if err != nil {
		t.Fatalf("Topology #2: %v", err)
	}
	if len(p1.Nodes) != len(p2.Nodes) || len(p1.Edges) != len(p2.Edges) {
		t.Fatalf("projection shape drifted between calls")
	}
	for i := range p1.Nodes {
		if p1.Nodes[i] != p2.Nodes[i] {
			t.Errorf("Nodes[%d] drift: %+v vs %+v", i, p1.Nodes[i], p2.Nodes[i])
		}
	}
	for i := range p1.Edges {
		if p1.Edges[i] != p2.Edges[i] {
			t.Errorf("Edges[%d] drift: %+v vs %+v", i, p1.Edges[i], p2.Edges[i])
		}
	}
	// Nodes are sorted lexicographically by Name.
	for i := 1; i < len(p1.Nodes); i++ {
		if p1.Nodes[i-1].Name >= p1.Nodes[i].Name {
			t.Errorf("Nodes not sorted: %q >= %q", p1.Nodes[i-1].Name, p1.Nodes[i].Name)
		}
	}
}
