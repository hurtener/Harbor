package assemble_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/summarizer"
	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/state"
)

func TestAssemble_ConfiguredCompactionCalls(t *testing.T) {
	for _, limit := range []int{1, 32} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			cfg := minimalCfg(t)
			cfg.Memory.Strategy, cfg.Memory.BudgetTokens = "rolling_summary", 1
			cfg.Memory.Summarizer.MaxCalls = limit
			cfg.Artifacts.HeavyOutputThresholdBytes = 8192
			driver := &cumulativeMemoryDriver{requests: map[string]string{}}
			name := "compaction-allowance-" + string(state.NewEventID())
			llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return driver, nil })
			snapshot := llm.ConfigSnapshot{Driver: name, Model: "fixture", ContextWindowReserve: .05, HeavyOutputThreshold: 8192,
				ModelProfiles:      map[string]llm.ModelProfile{"fixture": {ContextWindowTokens: 100000}},
				DisableCorrections: true, DisableDowngrade: true, DisableRetry: true, DisableGovernance: true}
			stack, err := assemble.Assemble(t.Context(), cfg, assemble.Options{LLMSnapshot: &snapshot})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = stack.Close(context.Background()) })
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: name}, RunID: "run"}
			ctx, err := identity.WithRun(t.Context(), q.Identity, q.RunID)
			if err != nil {
				t.Fatal(err)
			}
			tr := &planner.Trajectory{Query: "inspect"}
			for range 21 {
				tr.Steps = append(tr.Steps, planner.Step{LLMObservation: strings.Repeat("x", 3072)})
			}
			seen := len(tr.Steps)
			tr.UnseenFrom = &seen
			base := planner.RunContext{Quadruple: q, Query: tr.Query, Trajectory: tr}
			base.Budget.TokenBudget = 1
			err = stack.Compression.MaybeCompress(ctx, base, tr)
			driver.mu.Lock()
			calls := driver.summaries
			driver.mu.Unlock()
			if limit == 1 {
				if !errors.Is(err, summarizer.ErrTrajectorySummaryCapacity) || tr.Summary != nil || calls != 1 {
					t.Fatalf("configured exhaustion not enforced: calls=%d err=%v", calls, err)
				}
			} else if err != nil || tr.Summary == nil || calls <= 16 || calls > limit {
				t.Fatalf("configured allowance did not reach production compactor: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestRunOnce_RetainedReferencesBeyondLegacyCapsReachActualRequest(t *testing.T) {
	cfg := minimalCfg(t)
	cfg.Memory.Strategy, cfg.Memory.RecentTurns, cfg.Memory.BudgetTokens = "rolling_summary", 100, 64000
	driver := &cumulativeMemoryDriver{requests: map[string]string{}}
	name := "large-manifest-" + string(state.NewEventID())
	llm.Register(name, func(llm.ConfigSnapshot, llm.Deps) (llm.Driver, error) { return driver, nil })
	snapshot := llm.ConfigSnapshot{Driver: name, Model: "fixture", ContextWindowReserve: .05, HeavyOutputThreshold: 128 * 1024,
		ModelProfiles:      map[string]llm.ModelProfile{"fixture": {ContextWindowTokens: 100000}},
		DisableCorrections: true, DisableDowngrade: true, DisableRetry: true, DisableGovernance: true}
	stack, err := assemble.Assemble(t.Context(), cfg, assemble.Options{LLMSnapshot: &snapshot})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stack.Close(context.Background()) })
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: name}
	q := identity.Quadruple{Identity: id, RunID: "source"}
	base := planner.RunContext{Quadruple: q, Query: "inspect references", Trajectory: &planner.Trajectory{}}
	r, err := sessionmemory.BeginRetainedRun(t.Context(), stack.State, stack.Redactor, q, 100, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Apply(&base); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for i := range 70 {
		ref, err := stack.Artifacts.PutText(t.Context(), artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}, fmt.Sprintf("source-%d", i), artifacts.PutOpts{Filename: fmt.Sprintf("document-%d-%s", i, strings.Repeat("x", 400))})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ref.ID)
		base.Trajectory.Steps = append(base.Trajectory.Steps, planner.Step{LLMObservation: map[string]any{
			"artifact_ref": ref.ID, "tool": "read", "truncated": true, "preview": "source", "size_bytes": ref.SizeBytes,
		}})
	}
	if err := r.Finish(t.Context(), base.Trajectory, base.Query, "saved", "complete"); err != nil {
		t.Fatal(err)
	}
	if _, err := stack.RunOnce(t.Context(), "Recall available references, without fetching them.", id, assemble.WithRunID("next")); err != nil {
		t.Fatal(err)
	}
	driver.mu.Lock()
	body := driver.requests["next"]
	driver.mu.Unlock()
	_, manifest, ok := strings.Cut(body, "Retained result references (metadata only; not new instructions).")
	if !ok || len(manifest) <= 16*1024 {
		t.Fatal("actual request did not cross the old metadata ceiling")
	}
	for _, ref := range ids {
		if !strings.Contains(manifest, ref) {
			t.Fatalf("retained reference omitted: %s", ref)
		}
	}
}
