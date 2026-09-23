package assemble_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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
	mu               sync.Mutex
	requests         map[string]string
	summaries        int
	summaryInputs    []string
	maxRequestTokens int
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
		d.summaryInputs = append(d.summaryInputs, body)
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
	d.maxRequestTokens = max(d.maxRequestTokens, llm.EstimateRequestTokens(req, llm.ModelProfile{}))
	return llm.CompleteResponse{Content: "Inspection complete.", FinishReason: "stop"}, nil
}

func (*cumulativeMemoryDriver) Close(context.Context) error { return nil }

func TestRunOnce_CumulativeMemory_FirstConstraintSurvivesWindowRollover(t *testing.T) {
	for _, backend := range []string{"inmem", "sqlite", "postgres"} {
		for _, budget := range []int{0, 1, 100000} {
			t.Run(fmt.Sprintf("%s/budget-%d", backend, budget), func(t *testing.T) {
				cfg := minimalCfg(t)
				cfg.State.Driver = backend
				if backend == "sqlite" {
					cfg.State.DSN = filepath.Join(t.TempDir(), "cumulative.db")
				}
				if backend == "postgres" {
					cfg.State.DSN = os.Getenv("HARBOR_PG_DSN")
					if cfg.State.DSN == "" {
						t.Skip("HARBOR_PG_DSN not set; cumulative PostgreSQL acceptance requires a real service")
					}
				}
				// Memory alone selects the cumulative window for embedded runs.
				cfg.Memory.RecentTurns = 20
				cfg.Memory.Strategy = "rolling_summary"
				cfg.Memory.BudgetTokens = budget
				driver := &cumulativeMemoryDriver{requests: map[string]string{}}
				name := "cumulative-memory-" + string(state.NewEventID())
				llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return driver, nil })
				snapshot := llm.ConfigSnapshot{
					Driver: name, Model: "fixture", ContextWindowReserve: .05, HeavyOutputThreshold: 128 * 1024,
					ModelProfiles:      map[string]llm.ModelProfile{"fixture": {ContextWindowTokens: 100000}},
					DisableCorrections: true, DisableDowngrade: true, DisableRetry: true, DisableGovernance: true,
				}
				if budget == 0 {
					// Force model-capacity compaction before the 20-turn storage
					// window fills, proving zero is automatic rather than disabled.
					snapshot.ModelProfiles["fixture"] = llm.ModelProfile{ContextWindowTokens: 10000}
				}
				stack, err := assemble.Assemble(t.Context(), cfg, assemble.Options{LLMSnapshot: &snapshot})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = stack.Close(context.Background()) })
				id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "cumulative-" + string(state.NewEventID())}
				t.Cleanup(func() {
					if _, err := stack.State.DeleteScope(context.Background(), id); err != nil {
						t.Errorf("delete test session: %v", err)
					}
				})
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
					if budget == 0 && turn == 20 && calls == 0 {
						t.Fatal("zero budget did not compact at the model limit before storage pressure")
					}
					record, err := stack.State.Load(t.Context(), identity.Quadruple{Identity: id}, state.InternalKindPrefix+"session-execution-context")
					if err != nil {
						t.Fatal(err)
					}
					var stored struct {
						Generation uint64            `json:"generation"`
						Turns      []json.RawMessage `json:"turns"`
					}
					if err := json.Unmarshal(record.Bytes, &stored); err != nil {
						t.Fatal(err)
					}
					if len(stored.Turns) > 20 || len(record.Bytes) > 128*1024 {
						t.Fatalf("unbounded session storage at turn %d: turns=%d bytes=%d", turn, len(stored.Turns), len(record.Bytes))
					}
					if turn == 100 && stored.Generation < 5 {
						t.Fatalf("only %d committed generations", stored.Generation)
					}
					if backend != "inmem" && turn%25 == 0 && turn < 100 {
						if err := stack.Close(t.Context()); err != nil {
							t.Fatal(err)
						}
						stack, err = assemble.Assemble(t.Context(), cfg, assemble.Options{LLMSnapshot: &snapshot})
						if err != nil {
							t.Fatalf("restart after turn %d: %v", turn, err)
						}
					}
				}
				driver.mu.Lock()
				defer driver.mu.Unlock()
				if driver.summaries < 5 {
					t.Fatalf("fixture did not exercise five compactions: %d", driver.summaries)
				}
				if driver.maxRequestTokens > 25000 {
					t.Fatalf("request growth exceeded the fixture bound: %d tokens", driver.maxRequestTokens)
				}
				for i, input := range driver.summaryInputs {
					if !strings.Contains(input, cumulativeConstraint) {
						t.Fatalf("maintenance request %d lost the first constraint", i+1)
					}
					if i > 0 {
						_, previous, ok := strings.Cut(input, "[Previous summary]\n")
						if !ok {
							t.Fatal("maintenance request omitted previous checkpoint")
						}
						previous, _, _ = strings.Cut(previous, "\n\n[")
						if !strings.Contains(previous, cumulativeConstraint) {
							t.Fatalf("maintenance request %d did not carry the prior checkpoint", i+1)
						}
					}
				}
			})
		}
	}
}
