package planner

import "testing"

func TestNewBatch_ProgressCoexistsWithTool(t *testing.T) {
	f := 0.5
	b, err := NewBatch([]CallTool{{Tool: "lookup", CallID: "tool-1"}}, nil, nil, []TaskProgress{{Fraction: &f, Phase: "working", CallID: "progress-1"}})
	if err != nil {
		t.Fatalf("NewBatch: %v", err)
	}
	if len(b.Tools) != 1 || len(b.Progress) != 1 || b.Progress[0].CallID != "progress-1" {
		t.Fatalf("batch lost ordered branches: %+v", b)
	}
}
