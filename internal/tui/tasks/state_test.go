package tasks

import (
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestDerive_TreeProgressGroupLatencyAndUnknownProgress(t *testing.T) {
	now := time.Now()
	progress := .25
	state := Derive(types.TaskListResponse{Rows: []types.TaskRow{{ID: "old", StartedAt: now}, {ID: "new", StartedAt: now.Add(time.Second), ParentTaskID: "parent", GroupID: "group", Progress: &progress, DurationMS: 42}}}, nil)
	if state.Rows[0].ID != "new" {
		t.Fatalf("order=%#v", state.Rows)
	}
	summary := state.Summary(state.Rows[0])
	if !strings.Contains(summary, "25%") || !strings.Contains(summary, "child of parent") || !strings.Contains(summary, "group group") {
		t.Fatalf("summary=%q", summary)
	}
	if !strings.Contains(state.Summary(state.Rows[1]), "indeterminate") {
		t.Fatal("missing progress fabricated as zero")
	}
}
