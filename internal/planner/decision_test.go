package planner

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/tasks"
)

// TestNewBatch_RejectsDegenerate — a would-be Batch with fewer than two
// combined Tools+Spawns branches is degenerate: NewBatch fails loud
// wrapping ErrInvalidDecision. Producers must construct the plain
// single-shape Decision instead (one representation per semantic).
func TestNewBatch_RejectsDegenerate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		tools  []CallTool
		spawns []SpawnTask
	}{
		{name: "empty", tools: nil, spawns: nil},
		{name: "one_tool_only", tools: []CallTool{{Tool: "a", Args: json.RawMessage(`{}`)}}},
		{name: "one_spawn_only", spawns: []SpawnTask{{Kind: tasks.KindBackground}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewBatch(tc.tools, tc.spawns, nil)
			if !errors.Is(err, ErrInvalidDecision) {
				t.Fatalf("NewBatch(%d tools, %d spawns) err = %v, want ErrInvalidDecision",
					len(tc.tools), len(tc.spawns), err)
			}
		})
	}
}

// TestNewBatch_RejectsRetainTurnSpawn — a retain-turn spawn inside a
// non-blocking batch dispatch is a contradiction; NewBatch fails loud
// naming the offending spawn index.
func TestNewBatch_RejectsRetainTurnSpawn(t *testing.T) {
	t.Parallel()
	_, err := NewBatch(
		[]CallTool{{Tool: "a", Args: json.RawMessage(`{}`)}},
		[]SpawnTask{{Kind: tasks.KindBackground, Spec: SpawnSpec{RetainTurn: true}}},
		nil,
	)
	if !errors.Is(err, ErrInvalidDecision) {
		t.Fatalf("NewBatch(retain-turn spawn) err = %v, want ErrInvalidDecision", err)
	}
}

// TestNewBatch_AcceptsMinimumValidShapes — the minimum valid Batch
// shapes all construct without error: one tool + one spawn; zero tools
// + two spawns; two tools + zero spawns. The two-tools/zero-spawns case
// is a valid Batch structurally (the constructor does not forbid it),
// but the PROJECTOR never produces it — a spawn-free multi-call response
// projects to CallParallel (see TestProjectBatch_* in the react package).
func TestNewBatch_AcceptsMinimumValidShapes(t *testing.T) {
	t.Parallel()
	tool := CallTool{Tool: "a", Args: json.RawMessage(`{}`)}
	spawn := SpawnTask{Kind: tasks.KindBackground, Spec: SpawnSpec{Query: "q"}}
	cases := []struct {
		name       string
		tools      []CallTool
		spawns     []SpawnTask
		wantTools  int
		wantSpawns int
	}{
		{name: "one_tool_one_spawn", tools: []CallTool{tool}, spawns: []SpawnTask{spawn}, wantTools: 1, wantSpawns: 1},
		{name: "two_spawns", spawns: []SpawnTask{spawn, spawn}, wantSpawns: 2},
		{name: "two_tools", tools: []CallTool{tool, tool}, wantTools: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, err := NewBatch(tc.tools, tc.spawns, nil)
			if err != nil {
				t.Fatalf("NewBatch(%s) err = %v, want nil", tc.name, err)
			}
			if len(b.Tools) != tc.wantTools {
				t.Errorf("Batch.Tools len = %d, want %d", len(b.Tools), tc.wantTools)
			}
			if len(b.Spawns) != tc.wantSpawns {
				t.Errorf("Batch.Spawns len = %d, want %d", len(b.Spawns), tc.wantSpawns)
			}
		})
	}
}

// TestBatch_IsDecision — Batch satisfies the sealed Decision sum.
func TestBatch_IsDecision(t *testing.T) {
	t.Parallel()
	var _ Decision = Batch{}
}
