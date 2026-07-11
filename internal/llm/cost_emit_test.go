package llm_test

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

// NOTE: the per-attempt cost-emit cadence under retry-with-feedback is pinned
// in `internal/llm/retry` (package retry_test), NOT here — blank-importing the
// retry package would SEAT its wrapper across this whole test binary and break
// TestOpen_WarnsOnUnseatedWrapperHooks (which asserts the retry hook is
// unseated). The retry-package test drives the full `llm.Open` chain (safety
// band + seated retry) and asserts one cost event per driver-level attempt.

// costEmitCtx stamps the FULL run quadruple so the emitted
// llm.cost.recorded envelope is turn-attributable.
func costEmitCtx(t *testing.T) context.Context {
	t.Helper()
	id := identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	ctx, err = identity.WithRun(ctx, id, "run-cost-emit")
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// countCostEvents drains cost.recorded events over a bounded window.
func countCostEvents(sub events.Subscription, window time.Duration) []llm.CostRecordedPayload {
	got := make([]llm.CostRecordedPayload, 0, 4)
	deadline := time.After(window)
	for {
		select {
		case ev := <-sub.Events():
			if p, ok := ev.Payload.(llm.CostRecordedPayload); ok {
				got = append(got, p)
			}
		case <-deadline:
			return got
		}
	}
}

// TestCostEmit_Mock_OnePerCompletion pins R1: the mandatory safety
// wrapper emits llm.cost.recorded for the dev-posture MOCK driver too
// (driver-neutral) — exactly ONE event per driver-level completion (no
// double emission after the bifrost-local emit was deleted), carrying
// the payload keys and the run quadruple on the envelope.
func TestCostEmit_Mock_OnePerCompletion(t *testing.T) {
	t.Parallel()
	deps, cleanup := makeDeps(t)
	defer cleanup()

	sub, err := deps.Bus.Subscribe(context.Background(), events.Filter{
		Admin: true,
		Types: []events.EventType{llm.EventTypeCostRecorded},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	cfg := makeSnapshot("m", 1000)
	// DisableRetry so the single Complete cannot fan into multiple driver
	// completions — the "exactly one" assertion is unambiguous.
	cfg.DisableRetry = true
	client, err := llm.Open(context.Background(), cfg, deps)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = client.Close(context.Background()) }()

	text := "hello"
	resp, err := client.Complete(costEmitCtx(t), llm.CompleteRequest{
		Model:    "m",
		Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &text}}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.TotalTokens == 0 {
		t.Fatal("mock driver reported zero usage; the emit test needs synthetic usage")
	}

	events := countCostEvents(sub, 600*time.Millisecond)
	if len(events) != 1 {
		t.Fatalf("observed %d llm.cost.recorded events, want exactly 1 (no double emission)", len(events))
	}
	p := events[0]
	if p.Model != "m" {
		t.Errorf("payload Model=%q, want m", p.Model)
	}
	if p.Usage.TotalTokens == 0 {
		t.Errorf("payload Usage empty: %+v", p.Usage)
	}
	if p.ContextWindowTokens != 1000 {
		t.Errorf("payload ContextWindowTokens=%d, want 1000 (from the model profile)", p.ContextWindowTokens)
	}
	if p.Identity.RunID != "run-cost-emit" {
		t.Errorf("payload Identity RunID=%q, want run-cost-emit (attribution-dead)", p.Identity.RunID)
	}
}
