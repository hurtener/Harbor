package types_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestSessionUsageTurnRow_JSONContentFreeContract(t *testing.T) {
	exact := int64(42)
	row := types.SessionUsageTurnRow{
		TurnID: "turn-1", TaskID: "task-1", SessionID: "session-1", AgentID: "agent-1",
		Status: "complete", Sealed: true, Version: 3, LastAppliedEventSeq: 9,
		StartedAt:  time.Date(2026, 8, 25, 10, 0, 0, 0, time.UTC),
		FinishedAt: time.Date(2026, 8, 25, 10, 1, 0, 0, time.UTC),
		Usage: types.SessionTurnUsage{
			TotalTokens:  types.SessionTurnUsageMeasure{State: "exact", Value: &exact},
			CostMicroUSD: types.SessionTurnUsageMeasure{State: "unavailable"},
			Model:        "model-usage",
		},
	}
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("marshal usage row: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode usage row: %v", err)
	}
	want := map[string]struct{}{
		"turn_id": {}, "task_id": {}, "session_id": {}, "agent_id": {}, "status": {}, "sealed": {}, "version": {},
		"last_applied_event_seq": {}, "started_at": {}, "updated_at": {}, "finished_at": {}, "usage": {},
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("usage JSON exposes unexpected key %q", key)
		}
	}
	forbidden := []string{"query", "answer", "reasoning", "activity", string(methods.MethodPause), "apps", "inputs", "outputs", "run_id", "finish_message", "error_message", "tenant_id", "user_id"}
	for _, key := range forbidden {
		if _, ok := got[key]; ok {
			t.Errorf("usage JSON exposes forbidden content key %q", key)
		}
	}
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(got["usage"], &usage); err != nil {
		t.Fatalf("decode nested usage: %v", err)
	}
	wantUsage := map[string]struct{}{
		"prompt_tokens": {}, "completion_tokens": {}, "reasoning_tokens": {},
		"cache_read_tokens": {}, "cache_write_tokens": {}, "total_tokens": {},
		"cost_micro_usd": {}, "latency_ns": {}, "model": {},
	}
	if len(usage) != len(wantUsage) {
		t.Errorf("nested usage JSON key count = %d, want %d: keys=%v", len(usage), len(wantUsage), reflect.ValueOf(usage).MapKeys())
	}
	for key := range usage {
		if _, ok := wantUsage[key]; !ok {
			t.Errorf("nested usage JSON exposes unexpected key %q", key)
		}
	}
	for key := range wantUsage {
		if _, ok := usage[key]; !ok {
			t.Errorf("nested usage JSON is missing required key %q", key)
		}
	}

	var out types.SessionUsageTurnRow
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("round trip usage row: %v", err)
	}
	if !reflect.DeepEqual(out.Usage.TotalTokens, row.Usage.TotalTokens) || !reflect.DeepEqual(out.Usage.CostMicroUSD, row.Usage.CostMicroUSD) || out.Usage.Model != row.Usage.Model {
		t.Fatalf("usage states changed: got %+v want %+v", out.Usage, row.Usage)
	}
}
