package llm_test

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/summarizer"
	"github.com/hurtener/Harbor/internal/planner"
)

const maintenanceNarrative = `{"goals":["edit"],"facts":["preserve source"],"pending":["verify"],"last_output_digest":"read complete","note":"compact"}`

type maintenanceCapacityDriver struct {
	mu       sync.Mutex
	requests []llm.CompleteRequest
}

func (d *maintenanceCapacityDriver) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	d.mu.Lock()
	d.requests = append(d.requests, req)
	d.mu.Unlock()
	return llm.CompleteResponse{Content: maintenanceNarrative, FinishReason: "stop"}, nil
}
func (*maintenanceCapacityDriver) Close(context.Context) error { return nil }

func TestCompactionCapacity_ChronologicalChunksFitSelectedModel(t *testing.T) {
	t.Parallel()
	deps, cleanup := makeDeps(t)
	defer cleanup()
	driver := &maintenanceCapacityDriver{}
	name := uniqueDriverName("summary-capacity")
	llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return driver, nil })
	cfg := makeSnapshot("m", 4096)
	cfg.Driver = name
	cfg.Model = "m"
	cfg.DisableCorrections, cfg.DisableDowngrade, cfg.DisableRetry, cfg.DisableGovernance = true, true, true, true
	client, err := llm.Open(t.Context(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	sum, err := summarizer.NewTrajectorySummariser(client)
	if err != nil {
		t.Fatal(err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}, RunID: "run"}
	ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
	if err != nil {
		t.Fatal(err)
	}
	tr := &planner.Trajectory{Query: "edit"}
	for i := range 6 {
		tr.Steps = append(tr.Steps, planner.Step{LLMObservation: fmt.Sprintf("receipt-%d:%s", i, strings.Repeat("x", 5000))})
	}
	calls := 0
	ctx = llm.WithContextPreparation(ctx, llm.ContextPreparation{InputTarget: 3000, Compact: func(callCtx context.Context, _, _ int) (bool, error) {
		calls++
		_, e := sum.Summarise(callCtx, planner.RunContext{Quadruple: q, Query: tr.Query}, tr)
		return e == nil, e
	}})
	large, small := "please edit "+strings.Repeat("y", 15000), "edit from retained checkpoint"
	out := 512
	_, err = client.Complete(ctx, llm.CompleteRequest{Model: "m", MaxTokens: &out, Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &large}}}, RebuildMessages: func() ([]llm.ChatMessage, error) {
		return []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &small}}}, nil
	}})
	if err != nil {
		t.Fatalf("model-sized compaction failed: %v", err)
	}
	if calls != 1 {
		t.Fatalf("recursive compaction: %d", calls)
	}
	driver.mu.Lock()
	defer driver.mu.Unlock()
	if len(driver.requests) < 3 {
		t.Fatalf("expected multiple maintenance chunks plus decision, got %d requests", len(driver.requests))
	}
	all := ""
	for _, r := range driver.requests[:len(driver.requests)-1] {
		if r.MaxTokens == nil {
			t.Fatal("unbounded maintenance output")
		}
		if n := llm.EstimateRequestTokens(r, cfg.ModelProfiles["m"]); n+*r.MaxTokens >= 3891 {
			t.Fatalf("request does not reserve model capacity: %d + %d", n, *r.MaxTokens)
		}
		for _, m := range r.Messages {
			if m.Content.Text != nil {
				all += *m.Content.Text
			}
		}
	}
	for i := range 6 {
		if strings.Count(all, fmt.Sprintf("receipt-%d:", i)) != 1 {
			t.Fatalf("receipt %d missing/duplicated", i)
		}
	}
}
