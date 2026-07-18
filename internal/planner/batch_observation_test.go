package planner_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
)

// TestBatchObservation_JSONRoundTrip — a BatchObservation survives a
// JSON round-trip with both halves index-aligned and every field
// preserved (the trajectory persists Step.Observation across
// checkpoints, so the shape must be JSON-stable).
func TestBatchObservation_JSONRoundTrip(t *testing.T) {
	t.Parallel()
	orig := planner.BatchObservation{
		Tools: []planner.ParallelBranchObservation{
			{CallID: "t0", Tool: "alpha", Index: 0, Value: map[string]any{"ok": true}},
			{CallID: "t1", Tool: "beta", Index: 1, Error: "boom"},
		},
		Spawns: []planner.BatchSpawnObservation{
			{CallID: "s0", Index: 0, TaskID: "task-1", GroupID: "grp-1"},
			{CallID: "s1", Index: 1, Error: "sealed group"},
		},
	}
	enc, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got planner.BatchObservation
	if err := json.Unmarshal(enc, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Tools) != 2 || len(got.Spawns) != 2 {
		t.Fatalf("round-trip lengths tools=%d spawns=%d, want 2/2", len(got.Tools), len(got.Spawns))
	}
	// Index alignment + call-id correlation survive the round-trip.
	for i, want := range []string{"t0", "t1"} {
		if got.Tools[i].CallID != want || got.Tools[i].Index != i {
			t.Errorf("Tools[%d] = {CallID:%q Index:%d}, want {%q %d}", i, got.Tools[i].CallID, got.Tools[i].Index, want, i)
		}
	}
	for i, want := range []string{"s0", "s1"} {
		if got.Spawns[i].CallID != want || got.Spawns[i].Index != i {
			t.Errorf("Spawns[%d] = {CallID:%q Index:%d}, want {%q %d}", i, got.Spawns[i].CallID, got.Spawns[i].Index, want, i)
		}
	}
	if got.Spawns[0].TaskID != "task-1" || got.Spawns[0].GroupID != "grp-1" {
		t.Errorf("Spawns[0] registration outcome lost: %+v", got.Spawns[0])
	}
	if got.Spawns[1].Error != "sealed group" {
		t.Errorf("Spawns[1].Error = %q, want %q", got.Spawns[1].Error, "sealed group")
	}
}

// TestBatchObservation_OmitsEmptyHalves — a spawns-only BatchObservation
// omits the Tools key (and vice versa) so a spawns-only batch's
// trajectory does not persist an empty tools array.
func TestBatchObservation_OmitsEmptyHalves(t *testing.T) {
	t.Parallel()
	spawnsOnly := planner.BatchObservation{
		Spawns: []planner.BatchSpawnObservation{{CallID: "s0", Index: 0, TaskID: "t"}},
	}
	enc, err := json.Marshal(spawnsOnly)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(enc, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := m["tools"]; present {
		t.Errorf("spawns-only observation encoded a tools key: %s", enc)
	}
	if _, present := m["spawns"]; !present {
		t.Errorf("spawns-only observation missing spawns key: %s", enc)
	}
}

// TestBatchObservation_UnserializableValue_FailsLoud — a non-JSON-
// encodable tool Value inside a BatchObservation carried on a trajectory
// step surfaces the same ErrUnserializable contract Trajectory.Serialize
// enforces (no silent drop of the tool half).
func TestBatchObservation_UnserializableValue_FailsLoud(t *testing.T) {
	t.Parallel()
	traj := &planner.Trajectory{
		Steps: []planner.Step{
			{
				Observation: planner.BatchObservation{
					Tools: []planner.ParallelBranchObservation{
						// A channel is not JSON-encodable — Serialize must
						// fail loud rather than silently drop the branch.
						{CallID: "t0", Tool: "alpha", Index: 0, Value: make(chan int)},
					},
				},
			},
		},
	}
	_, err := traj.Serialize()
	if err == nil {
		t.Fatal("Serialize returned nil error for a non-encodable BatchObservation value, want ErrUnserializable")
	}
	var unser planner.ErrUnserializable
	if !errors.As(err, &unser) {
		t.Fatalf("err = %v, want ErrUnserializable", err)
	}
}
