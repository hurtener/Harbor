package materializer

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/tasks"
)

// redactedCostEvent builds a llm.cost.recorded event whose payload is a
// hand-built RedactedMap — the durable source's rehydrated shape — so a
// test can inject CORRUPT numeric values (NaN / ±Inf, out-of-range
// floats, raw int64s) that no JSON round-trip can carry or that would
// be mangled by it.
func redactedCostEvent(id identity.Identity, runID string, usage, cost map[string]any) events.Event {
	payload := map[string]any{"Model": "model-x"}
	if usage != nil {
		payload["Usage"] = usage
	}
	if cost != nil {
		payload["Cost"] = cost
	}
	return events.Event{
		Type:     llm.EventTypeCostRecorded,
		Identity: testQuad(id, runID),
		Payload:  events.RedactedMap{Data: payload},
	}
}

// TestMaterialize_UsageArithmeticFailsLoud pins the fail-loud
// cumulative-usage contract: a negative, non-finite, or overflowing
// token / latency / cost value aborts the pass loudly (never wrapped,
// clamped, or silently omitted) and the offending event does NOT
// advance the checkpoint — the projection cannot converge past corrupt
// arithmetic.
func TestMaterialize_UsageArithmeticFailsLoud(t *testing.T) {
	cases := []struct {
		name  string
		usage map[string]any
		cost  map[string]any
	}{
		{"negative prompt tokens", map[string]any{"PromptTokens": float64(-5)}, nil},
		{"negative latency", map[string]any{"LatencyMS": float64(-5)}, nil},
		{"negative cost", nil, map[string]any{"TotalCost": -0.5}},
		{"NaN cost", nil, map[string]any{"TotalCost": math.NaN()}},
		{"+Inf cost", nil, map[string]any{"TotalCost": math.Inf(1)}},
		{"-Inf cost", nil, map[string]any{"TotalCost": math.Inf(-1)}},
		{"out-of-range token float", map[string]any{"PromptTokens": 1e300}, nil},
		{"non-numeric cost", nil, map[string]any{"TotalCost": "twelve"}},
		{"latency ms to ns overflow", map[string]any{"LatencyMS": int64(math.MaxInt64/int64(time.Millisecond)) + 1}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "")
			defer h.closeStore()
			m := h.newMaterializer(t)
			quad := testQuad(h.id, "run-ua")
			h.src.publish(t, spawnEv(h.id, quad.RunID, "task-ua", tasks.KindForeground, ""))
			h.src.publish(t, redactedCostEvent(h.id, quad.RunID, tc.usage, tc.cost))

			if _, err := m.Materialize(context.Background()); err == nil {
				t.Fatalf("corrupt usage (%s) must fail the pass loudly", tc.name)
			}
			// Only the spawn advanced; the corrupt cost event did NOT.
			cp, err := h.proj.Checkpoint(context.Background(), h.id)
			if err != nil {
				t.Fatalf("checkpoint: %v", err)
			}
			if cp != 1 {
				t.Errorf("checkpoint = %d, want 1 (the corrupt event did not advance)", cp)
			}
			// No corrupt amount ever reached the row: the candidate
			// rollup is discarded wholesale on the first corrupt
			// measure, so every corrupt-touched measure stays
			// unavailable.
			row := mustGetRow(t, h, "task-ua")
			if row.Usage.PromptTokens.State != turns.UsageUnavailable ||
				row.Usage.CostMicroUSD.State != turns.UsageUnavailable ||
				row.Usage.LatencyNS.State != turns.UsageUnavailable {
				t.Errorf("usage = %+v, want the corrupt-touched measures unavailable", row.Usage)
			}
		})
	}
}

// TestMaterialize_UsageArithmeticOverflowFailsLoud pins the int64
// ACCUMULATION overflow path: an accepted MaxInt64 delta followed by a
// positive delta must fail loud on the second event — the accumulator
// never wraps — and the overflowing event does not advance the
// checkpoint while the pre-overflow amount stays durably applied.
func TestMaterialize_UsageArithmeticOverflowFailsLoud(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)
	quad := testQuad(h.id, "run-of")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-of", tasks.KindForeground, ""))
	h.src.publish(t, redactedCostEvent(h.id, quad.RunID, map[string]any{"PromptTokens": int64(math.MaxInt64)}, nil))
	h.src.publish(t, redactedCostEvent(h.id, quad.RunID, map[string]any{"PromptTokens": int64(1)}, nil))

	if _, err := m.Materialize(context.Background()); err == nil {
		t.Fatal("accumulation overflow must fail the pass loudly")
	}
	// The first cost event applied (checkpoint 2); the overflowing
	// second did not (checkpoint stays 2, never wraps to a garbage
	// total).
	cp, err := h.proj.Checkpoint(context.Background(), h.id)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if cp != 2 {
		t.Errorf("checkpoint = %d, want 2 (the overflowing event did not advance)", cp)
	}
	row := mustGetRow(t, h, "task-of")
	if row.Usage.PromptTokens.State != turns.UsageExact || usageValue(t, row.Usage.PromptTokens) != math.MaxInt64 {
		t.Errorf("prompt tokens = %+v, want the exact pre-overflow MaxInt64 (never wrapped)", row.Usage.PromptTokens)
	}
}

// TestMaterialize_UsageArithmeticZeroDeltasStayHonest pins that a ZERO
// delta is not corrupt: it leaves the measure unchanged (the canonical
// payload contract treats zero as indistinguishable from not-reported —
// zero never becomes a fabricated exact claim), and the pass converges.
func TestMaterialize_UsageArithmeticZeroDeltasStayHonest(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)
	quad := testQuad(h.id, "run-z")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-z", tasks.KindForeground, ""))
	h.src.publish(t, costRecordedEv(h.id, quad.RunID, "model-z", llm.Usage{
		PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, LatencyMS: 120,
	}, 0.25))
	// A second call with all-zero deltas: nothing changes, no error.
	h.src.publish(t, costRecordedEv(h.id, quad.RunID, "model-z", llm.Usage{}, 0))
	last := h.src.publish(t, failedEv(h.id, "task-z", "timeout"))
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	row := mustGetRow(t, h, "task-z")
	if row.Usage.PromptTokens.State != turns.UsageExact || usageValue(t, row.Usage.PromptTokens) != 100 {
		t.Errorf("prompt = %+v, want 100 (zero deltas are a no-op)", row.Usage.PromptTokens)
	}
	if row.Usage.TotalTokens.State != turns.UsageExact || usageValue(t, row.Usage.TotalTokens) != 150 {
		t.Errorf("total = %+v, want 150", row.Usage.TotalTokens)
	}
	if row.Usage.CostMicroUSD.State != turns.UsageEstimated || usageValue(t, row.Usage.CostMicroUSD) != 250_000 {
		t.Errorf("cost = %+v, want 250000 micro-USD", row.Usage.CostMicroUSD)
	}
	if row.LastAppliedEventSeq != last.Sequence {
		t.Errorf("last applied seq = %d, want %d", row.LastAppliedEventSeq, last.Sequence)
	}
}
