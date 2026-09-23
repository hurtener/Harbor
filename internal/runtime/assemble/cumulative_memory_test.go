package assemble_test

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/state"
)

const cumulativeConstraint = "Keep the emergency exit labelled NORTH-STAR-47."

// The fixture preserves the constraint only if it is present in the actual
// maintenance request. It must not fabricate the fact after runtime eviction.
type cumulativeMemoryDriver struct {
	mu        sync.Mutex
	requests  map[string]string
	summaries int
}

func (d *cumulativeMemoryDriver) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	q, ok := identity.QuadrupleFrom(ctx)
	if !ok {
		return llm.CompleteResponse{}, llm.ErrIdentityMissing
	}
	var content strings.Builder
	for _, msg := range req.Messages {
		if msg.Content.Text != nil {
			content.WriteString(*msg.Content.Text)
		}
	}
	body := content.String()
	d.mu.Lock()
	defer d.mu.Unlock()
	if strings.Contains(body, "You summarize historical agent execution") {
		d.summaries++
		facts := []string{"The layout has been inspected."}
		if strings.Contains(body, cumulativeConstraint) {
			facts = append(facts, cumulativeConstraint)
		}
		encoded, err := json.Marshal(map[string]any{
			"goals": []string{"Iterate on the layout"}, "facts": facts,
			"pending": []string{"Verify the next edit"}, "last_output_digest": "Inspection complete", "note": "",
		})
		return llm.CompleteResponse{Content: string(encoded), FinishReason: "stop"}, err
	}
	d.requests[q.RunID] = body
	return llm.CompleteResponse{Content: "Inspection complete.", FinishReason: "stop"}, nil
}

func (*cumulativeMemoryDriver) Close(context.Context) error { return nil }

func TestRunOnce_CumulativeMemory_FirstConstraintSurvivesWindowRollover(t *testing.T) {
	for _, backend := range []string{"inmem", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			cfg := minimalCfg(t)
			cfg.State.Driver = backend
			if backend == "sqlite" {
				cfg.State.DSN = filepath.Join(t.TempDir(), "cumulative.db")
			}
			// Exercise the current machinery before removing these activation
			// fields. The configuration migration will move both under memory.
			cfg.Sessions.RetainedContextTurns = 20
			cfg.Planner.TokenBudget = 1
			driver := &cumulativeMemoryDriver{requests: map[string]string{}}
			name := "cumulative-memory-" + string(state.NewEventID())
			llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return driver, nil })
			snapshot := llm.ConfigSnapshot{
				Driver: name, Model: "fixture", ContextWindowReserve: .05, HeavyOutputThreshold: 128 * 1024,
				ModelProfiles:      map[string]llm.ModelProfile{"fixture": {ContextWindowTokens: 100000}},
				DisableCorrections: true, DisableDowngrade: true, DisableRetry: true, DisableGovernance: true,
			}
			stack, err := assemble.Assemble(t.Context(), cfg, assemble.Options{LLMSnapshot: &snapshot})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = stack.Close(context.Background()) })
			id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "cumulative"}
			for turn := 1; turn <= 100; turn++ {
				run := fmt.Sprintf("turn-%03d", turn)
				query := fmt.Sprintf("Inspect layout revision %d. %s", turn, strings.Repeat("Synthetic layout detail. ", 80))
				if turn == 1 {
					query = cumulativeConstraint + " " + query
				}
				if _, err := stack.RunOnce(t.Context(), query, id, assemble.WithRunID(run)); err != nil {
					t.Fatalf("turn %d: %v", turn, err)
				}
				driver.mu.Lock()
				body, calls := driver.requests[run], driver.summaries
				driver.mu.Unlock()
				if !strings.Contains(body, cumulativeConstraint) {
					t.Fatalf("turn %d lost the turn-1 constraint from the actual decision request after %d maintenance calls (recent window 20)", turn, calls)
				}
			}
			driver.mu.Lock()
			defer driver.mu.Unlock()
			if driver.summaries < 5 {
				t.Fatalf("fixture did not exercise five compactions: %d", driver.summaries)
			}
		})
	}
}
