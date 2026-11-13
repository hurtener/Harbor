package materializer

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/tasks"
)

func provenanceCostEv(id identity.Identity, runID string, usage llm.Usage, cost llm.Cost) events.Event {
	return events.Event{
		Type:     llm.EventTypeCostRecorded,
		Identity: testQuad(id, runID),
		Payload:  llm.CostRecordedPayload{Identity: testQuad(id, runID), Model: "model-p", Cost: cost, Usage: usage},
	}
}

// TestMaterialize_UsageProvenancePerMeasure pins per-call provenance: a
// measure is exact only while every contributing call was a driver report;
// Harbor's price-table/usage backfill or a cost of unknown provenance makes
// it estimated, and estimated is sticky for the turn.
func TestMaterialize_UsageProvenancePerMeasure(t *testing.T) {
	reported := llm.Usage{ReportPresent: true, PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}
	cases := []struct {
		name                 string
		wantTokens, wantCost turns.UsageState
		wantCostMicro        int64
	}{
		{"gateway reported cost is exact", turns.UsageExact, turns.UsageExact, 300_000},
		{"price-table backfill is estimated", turns.UsageExact, turns.UsageEstimated, 300_000},
		{"unknown cost provenance is estimated", turns.UsageExact, turns.UsageEstimated, 300_000},
		{"estimated token backfill is estimated", turns.UsageEstimated, turns.UsageExact, 300_000},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "")
			defer h.closeStore()
			m := h.newMaterializer(t)
			quad := testQuad(h.id, "run-p")
			h.src.publish(t, spawnEv(h.id, quad.RunID, "task-p", tasks.KindForeground, ""))
			first, second := llm.Cost{ReportPresent: true, TotalCost: 0.1, Currency: "USD"}, llm.Cost{ReportPresent: true, TotalCost: 0.2, Currency: "USD"}
			usage := reported
			switch i {
			case 1:
				second = llm.Cost{Estimated: true, TotalCost: 0.2, Currency: "USD"}
			case 2:
				second = llm.Cost{TotalCost: 0.2, Currency: "USD"}
			case 3:
				usage = llm.Usage{Estimated: true, PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120}
			}
			h.src.publish(t, provenanceCostEv(h.id, quad.RunID, reported, first))
			h.src.publish(t, provenanceCostEv(h.id, quad.RunID, usage, second))
			h.src.publish(t, failedEv(h.id, "task-p", "timeout"))
			if _, err := m.Materialize(context.Background()); err != nil {
				t.Fatalf("materialize: %v", err)
			}
			row := mustGetRow(t, h, "task-p")
			if row.Usage.TotalTokens.State != tc.wantTokens || usageValue(t, row.Usage.TotalTokens) != 240 {
				t.Errorf("total tokens = %+v, want %s 240", row.Usage.TotalTokens, tc.wantTokens)
			}
			if row.Usage.PromptTokens.State != tc.wantTokens {
				t.Errorf("prompt tokens state = %s, want %s", row.Usage.PromptTokens.State, tc.wantTokens)
			}
			if row.Usage.CostMicroUSD.State != tc.wantCost || usageValue(t, row.Usage.CostMicroUSD) != tc.wantCostMicro {
				t.Errorf("cost = %+v, want %s %d", row.Usage.CostMicroUSD, tc.wantCost, tc.wantCostMicro)
			}
		})
	}
}
