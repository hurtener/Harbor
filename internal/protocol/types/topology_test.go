package types_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

// TestTopologyProjection_JSONRoundTrip pins that a TopologyProjection
// marshals and unmarshals losslessly — the wire-shape contract every
// Protocol client (Console, third-party) depends on.
func TestTopologyProjection_JSONRoundTrip(t *testing.T) {
	at := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	want := types.TopologyProjection{
		EngineID:   "engine-abc123",
		OccurredAt: at,
		Nodes: []types.TopologyNode{
			{Name: "inlet", Kind: types.NodeKindInlet},
			{Name: "mid", Kind: types.NodeKindNode},
			{Name: "outlet", Kind: types.NodeKindOutlet},
		},
		Edges: []types.TopologyEdge{
			{From: "inlet", To: "mid", QueueDepth: 2, QueueCapacity: 64},
			{From: "mid", To: "outlet", QueueDepth: 0, QueueCapacity: 64},
		},
	}

	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var got types.TopologyProjection
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got.EngineID != want.EngineID {
		t.Errorf("EngineID = %q, want %q", got.EngineID, want.EngineID)
	}
	if !got.OccurredAt.Equal(want.OccurredAt) {
		t.Errorf("OccurredAt = %v, want %v", got.OccurredAt, want.OccurredAt)
	}
	if len(got.Nodes) != len(want.Nodes) {
		t.Fatalf("Nodes len = %d, want %d", len(got.Nodes), len(want.Nodes))
	}
	for i := range want.Nodes {
		if got.Nodes[i] != want.Nodes[i] {
			t.Errorf("Nodes[%d] = %+v, want %+v", i, got.Nodes[i], want.Nodes[i])
		}
	}
	if len(got.Edges) != len(want.Edges) {
		t.Fatalf("Edges len = %d, want %d", len(got.Edges), len(want.Edges))
	}
	for i := range want.Edges {
		if got.Edges[i] != want.Edges[i] {
			t.Errorf("Edges[%d] = %+v, want %+v", i, got.Edges[i], want.Edges[i])
		}
	}
}

// TestTopologyProjection_JSONFieldNames pins the wire field names —
// a third-party Console branches on them; a rename is a wire break.
func TestTopologyProjection_JSONFieldNames(t *testing.T) {
	p := types.TopologyProjection{
		EngineID:   "e1",
		OccurredAt: time.Unix(0, 0).UTC(),
		Nodes:      []types.TopologyNode{{Name: "n", Kind: types.NodeKindInlet}},
		Edges:      []types.TopologyEdge{{From: "a", To: "b", QueueDepth: 1, QueueCapacity: 2}},
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	for _, field := range []string{"engine_id", "occurred_at", "nodes", "edges"} {
		if _, ok := generic[field]; !ok {
			t.Errorf("TopologyProjection JSON missing field %q", field)
		}
	}
	var edges []map[string]json.RawMessage
	if err := json.Unmarshal(generic["edges"], &edges); err != nil {
		t.Fatalf("decode edges: %v", err)
	}
	for _, field := range []string{"from", "to", "queue_depth", "queue_capacity"} {
		if _, ok := edges[0][field]; !ok {
			t.Errorf("TopologyEdge JSON missing field %q", field)
		}
	}
}

// TestTopologyProjection_SortDeterministic pins the byte-stability
// contract: SortDeterministic produces the same ordering regardless of
// the input ordering, so the snapshot and event surfaces marshal to
// byte-identical JSON.
func TestTopologyProjection_SortDeterministic(t *testing.T) {
	at := time.Unix(100, 0).UTC()
	// Two projections of the same graph, nodes + edges in different
	// input orders.
	a := types.TopologyProjection{
		EngineID:   "e",
		OccurredAt: at,
		Nodes: []types.TopologyNode{
			{Name: "z", Kind: types.NodeKindOutlet},
			{Name: "a", Kind: types.NodeKindInlet},
			{Name: "m", Kind: types.NodeKindNode},
		},
		Edges: []types.TopologyEdge{
			{From: "m", To: "z"},
			{From: "a", To: "m"},
			{From: "a", To: "z"},
		},
	}
	b := types.TopologyProjection{
		EngineID:   "e",
		OccurredAt: at,
		Nodes: []types.TopologyNode{
			{Name: "m", Kind: types.NodeKindNode},
			{Name: "z", Kind: types.NodeKindOutlet},
			{Name: "a", Kind: types.NodeKindInlet},
		},
		Edges: []types.TopologyEdge{
			{From: "a", To: "z"},
			{From: "a", To: "m"},
			{From: "m", To: "z"},
		},
	}
	a.SortDeterministic()
	b.SortDeterministic()

	rawA, _ := json.Marshal(a)
	rawB, _ := json.Marshal(b)
	if string(rawA) != string(rawB) {
		t.Fatalf("SortDeterministic did not yield byte-stable JSON:\n a=%s\n b=%s", rawA, rawB)
	}

	// Nodes sorted by Name.
	for i := 1; i < len(a.Nodes); i++ {
		if a.Nodes[i-1].Name >= a.Nodes[i].Name {
			t.Errorf("Nodes not sorted by Name: %q >= %q", a.Nodes[i-1].Name, a.Nodes[i].Name)
		}
	}
	// Edges sorted by (From, To).
	for i := 1; i < len(a.Edges); i++ {
		prev, cur := a.Edges[i-1], a.Edges[i]
		if prev.From > cur.From || (prev.From == cur.From && prev.To > cur.To) {
			t.Errorf("Edges not sorted by (From,To): %+v before %+v", prev, cur)
		}
	}
}

// TestTopologyProjection_ZeroValue pins that a zero-value projection
// marshals cleanly (no nil-slice panic) — the shape an engine with no
// edges (a single outlet-only node) produces.
func TestTopologyProjection_ZeroValue(t *testing.T) {
	var p types.TopologyProjection
	p.SortDeterministic() // must not panic on nil slices
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("json.Marshal zero value: %v", err)
	}
	var got types.TopologyProjection
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json.Unmarshal zero value: %v", err)
	}
	if len(got.Nodes) != 0 || len(got.Edges) != 0 {
		t.Errorf("zero-value projection round-trip grew slices: nodes=%d edges=%d", len(got.Nodes), len(got.Edges))
	}
}

// TestTopologyNodeKind_Constants pins the V1 closed node-kind set.
func TestTopologyNodeKind_Constants(t *testing.T) {
	cases := map[types.TopologyNodeKind]string{
		types.NodeKindInlet:  "inlet",
		types.NodeKindNode:   "node",
		types.NodeKindOutlet: "outlet",
	}
	for k, want := range cases {
		if string(k) != want {
			t.Errorf("node kind wire string = %q, want %q", string(k), want)
		}
	}
}

// TestTopologySnapshotRequest_JSONRoundTrip pins the request wire
// shape — a flat identity scope, no more.
func TestTopologySnapshotRequest_JSONRoundTrip(t *testing.T) {
	want := types.TopologySnapshotRequest{
		Identity: types.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"},
	}
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var got types.TopologySnapshotRequest
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if got.Identity != want.Identity {
		t.Errorf("Identity = %+v, want %+v", got.Identity, want.Identity)
	}
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("decode generic: %v", err)
	}
	if _, ok := generic["identity"]; !ok {
		t.Error("TopologySnapshotRequest JSON missing field \"identity\"")
	}
}
