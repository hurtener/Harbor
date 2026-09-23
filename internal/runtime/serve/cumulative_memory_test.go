package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/summarizer"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
)

const servedCumulativeConstraint = "Keep the emergency exit labelled NORTH-STAR-47."

// Never fabricates the early constraint: maintenance can carry it forward only
// if it appears in the request it actually received. This verifies transport,
// projection and persistence, not a real model's summarization proficiency.
type servedCumulativeClient struct {
	mu        sync.Mutex
	requests  map[string]string
	summaries []string
}

func (c *servedCumulativeClient) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	q, ok := identity.QuadrupleFrom(ctx)
	if !ok {
		return llm.CompleteResponse{}, llm.ErrIdentityMissing
	}
	var b strings.Builder
	for _, message := range req.Messages {
		if message.Content.Text != nil {
			b.WriteString(*message.Content.Text)
		}
	}
	body := b.String()
	c.mu.Lock()
	defer c.mu.Unlock()
	if strings.Contains(body, "You summarize historical agent execution") {
		c.summaries = append(c.summaries, body)
		facts := []string{"Layout inspected."}
		if strings.Contains(body, servedCumulativeConstraint) {
			facts = append(facts, servedCumulativeConstraint)
		}
		if strings.Contains(body, "Accent is amber.") {
			facts = append(facts, "Accent is amber.")
		} else if strings.Contains(body, "Accent is mint.") {
			facts = append(facts, "Accent is mint.")
		}
		encoded, err := json.Marshal(map[string]any{"goals": []string{"Iterate on the layout"}, "facts": facts, "pending": []string{"Verify the next edit"}, "last_output_digest": "Inspection complete", "note": ""})
		return llm.CompleteResponse{Content: string(encoded), FinishReason: "stop"}, err
	}
	c.requests[q.RunID] = body
	return llm.CompleteResponse{Content: "Inspection complete.", FinishReason: "stop"}, nil
}

func (*servedCumulativeClient) Close(context.Context) error { return nil }

func TestRetainedServer_CumulativeMemoryAcrossFiveWindows(t *testing.T) {
	for _, backend := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			cfg := config.StateConfig{Driver: backend}
			if backend == "sqlite" {
				cfg.DSN = filepath.Join(t.TempDir(), "cumulative.sqlite")
			}
			if backend == "postgres" {
				cfg.DSN = os.Getenv("HARBOR_PG_DSN")
				if cfg.DSN == "" {
					t.Skip("HARBOR_PG_DSN not set; cumulative PostgreSQL acceptance requires a real service")
				}
			}
			store, err := state.Open(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close(context.Background()) })
			client := &servedCumulativeClient{requests: map[string]string{}}
			name := "served-cumulative-" + string(state.NewEventID())
			llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return client, nil })
			blobs, err := artifacts.Open(t.Context(), config.ArtifactsConfig{Driver: "inmem"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = blobs.Close(context.Background()) })
			env, _, tools, legacy := retainedServerHarness(t, func(opts *RunLoopDriverOptions) {
				composed, err := llm.Open(t.Context(), llm.ConfigSnapshot{
					Driver: name, Model: "fixture", ContextWindowReserve: .05, HeavyOutputThreshold: 128 * 1024,
					ModelProfiles:      map[string]llm.ModelProfile{"fixture": {ContextWindowTokens: 100000}},
					DisableCorrections: true, DisableDowngrade: true, DisableRetry: true, DisableGovernance: true,
				}, llm.Deps{Artifacts: blobs, Bus: opts.Bus})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = composed.Close(context.Background()) })
				summary, err := summarizer.NewTrajectorySummariser(composed)
				if err != nil {
					t.Fatal(err)
				}
				opts.StateStore = store
				opts.ArtifactStore = blobs
				opts.SessionMemory = config.MemoryConfig{Strategy: "rolling_summary", RecentTurns: 20}
				opts.TokenBudget = 100000 // Storage pressure, not an artificially tiny token target.
				opts.Compression = planner.NewCompressionRunner(summary)
				opts.Planner = react.New(composed)
			})
			id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "cumulative-served-" + string(state.NewEventID())}
			t.Cleanup(func() {
				if _, err := store.DeleteScope(context.Background(), id); err != nil {
					t.Errorf("delete test session: %v", err)
				}
			})
			for turn := 1; turn <= 100; turn++ {
				query := fmt.Sprintf("Inspect layout revision %d. %s", turn, strings.Repeat("Synthetic layout detail. ", 10))
				if turn == 1 {
					query = servedCumulativeConstraint + " Accent is mint. " + query
				}
				if turn == 31 {
					query = "Correction: Accent is amber. " + query
				}
				result := retainedServerTurn(t, env, id, query, nil)
				if result.Status != tasks.StatusComplete {
					t.Fatalf("turn %d failed: %+v", turn, result.Error)
				}
				client.mu.Lock()
				body := client.requests[string(result.ID)]
				client.mu.Unlock()
				if !strings.Contains(body, servedCumulativeConstraint) {
					t.Fatalf("turn %d lost the turn-1 constraint from the actual decision request", turn)
				}
				if turn >= 31 && !strings.Contains(body, "Accent is amber.") {
					t.Fatalf("turn %d lost the correction", turn)
				}
				if len(body) > 128*1024 {
					t.Fatalf("unbounded decision request: %d bytes", len(body))
				}
				record, err := store.Load(t.Context(), identity.Quadruple{Identity: id}, serverRetainedKind)
				if err != nil {
					t.Fatal(err)
				}
				var committed struct {
					Generation uint64            `json:"generation"`
					Turns      []json.RawMessage `json:"turns"`
				}
				if err := json.Unmarshal(record.Bytes, &committed); err != nil {
					t.Fatal(err)
				}
				if len(committed.Turns) > 20 || len(record.Bytes) > 128*1024 {
					t.Fatal("unbounded private memory")
				}
				if turn == 100 && committed.Generation < 5 {
					t.Fatalf("only %d committed generations", committed.Generation)
				}
			}
			client.mu.Lock()
			defer client.mu.Unlock()
			if len(client.summaries) < 5 {
				t.Fatal("did not exercise five compactions")
			}
			for i, input := range client.summaries {
				if !strings.Contains(input, servedCumulativeConstraint) {
					t.Fatalf("maintenance %d lost the constraint", i+1)
				}
				if i > 0 {
					_, previous, ok := strings.Cut(input, "[Previous summary]\n")
					previous, _, _ = strings.Cut(previous, "\n\n[")
					if !ok || !strings.Contains(previous, servedCumulativeConstraint) {
						t.Fatalf("maintenance %d omitted its prior checkpoint", i+1)
					}
				}
			}
			if legacy.calls.Load() != 0 || tools.Load() != 0 {
				t.Fatal("cumulative memory consulted a second owner or replayed an action")
			}
		})
	}
}
