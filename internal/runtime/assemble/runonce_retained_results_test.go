package assemble_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/builtin"
)

type resultSummary struct{}

func (resultSummary) Summarise(context.Context, planner.RunContext, *planner.Trajectory) (*planner.Summary, error) {
	return &planner.Summary{Facts: []string{"The prior source was inspected."}}, nil
}

type resultClient struct {
	fn func(llm.CompleteRequest) (llm.CompleteResponse, error)
}

func (c resultClient) Complete(_ context.Context, r llm.CompleteRequest) (llm.CompleteResponse, error) {
	return c.fn(r)
}
func (resultClient) Close(context.Context) error { return nil }

// Real tool dispatch produces the offloaded result; the installed summary
// intentionally carries no artifact ID. Neither the final answer nor a session
// wide List can be relied on to recover the reference for an embedded caller.
func retainedLargeResult(t *testing.T) (*assemble.Stack, identity.Identity, string, string, *atomic.Int64) {
	t.Helper()
	s := runnableStack(t)
	s.Cfg.Memory.Strategy, s.Cfg.Memory.RecentTurns = "rolling_summary", 4
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	if err := builtin.RegisterWith(builtin.RegistryContext{Catalog: s.Catalog, ArtifactStore: s.Artifacts}, []string{"artifact_fetch"}); err != nil {
		t.Fatal(err)
	}
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "large-results"}
	q := identity.Quadruple{Identity: id, RunID: "source"}
	ctx, err := identity.WithRun(t.Context(), id, q.RunID)
	if err != nil {
		t.Fatal(err)
	}
	base, err := runctx.NewRunContext(ctx, runctx.Sources{Catalog: s.Catalog, Artifacts: s.Artifacts}, q, "inspect source")
	if err != nil {
		t.Fatal(err)
	}
	retained, err := runctx.BeginRetainedRun(ctx, s.State, s.Redactor, q, 4, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := retained.Apply(&base); err != nil {
		t.Fatal(err)
	}
	if err := retained.Start(ctx, base); err != nil {
		t.Fatal(err)
	}
	payload := `{"source":"` + strings.Repeat("source-", 15000) + `","resource_id":"doc-large","version":9007199254740993,"more":false}`
	calls := &atomic.Int64{}
	if err := s.Catalog.Register(tools.ToolDescriptor{Tool: tools.Tool{Name: "large_read", Description: "Synthetic large result", ArgsSchema: json.RawMessage(`{"type":"object"}`), Transport: tools.TransportInProcess, Source: "fixture", Loading: tools.LoadingAlways}, Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
		calls.Add(1)
		return tools.ToolResult{Value: json.RawMessage(payload)}, nil
	}}); err != nil {
		t.Fatal(err)
	}
	action := planner.CallTool{Tool: "large_read", CallID: "read-one", Args: json.RawMessage(`{}`)}
	step := planner.Step{Action: action}
	if err := retained.BeforeDispatch(ctx, base, step); err != nil {
		t.Fatal(err)
	}
	step.Observation, step.LLMObservation, err = s.Executor.ExecuteDecision(ctx, base, action)
	if err != nil {
		t.Fatal(err)
	}
	if err := retained.AfterDispatch(ctx, base, step); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(step.LLMObservation)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Ref string `json:"artifact_ref"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil || envelope.Ref == "" {
		t.Fatalf("real result not offloaded: %s %v", encoded, err)
	}
	// The fixture bypasses the run loop: model the earlier decision having
	// consumed the old read and verification, with the latest result still fresh.
	base.Trajectory.Steps = append(base.Trajectory.Steps, step, planner.Step{LLMObservation: "later verification"}, planner.Step{LLMObservation: "latest verification"})
	seen := len(base.Trajectory.Steps) - 1
	base.Trajectory.UnseenFrom = &seen
	base.Budget.TokenBudget = 1
	if err := planner.NewCompressionRunner(resultSummary{}).MaybeCompress(ctx, base, base.Trajectory); err != nil {
		t.Fatal(err)
	}
	if base.Trajectory.Summary == nil {
		t.Fatal("fixture did not compact")
	}
	if err := retained.Finish(ctx, base.Trajectory, base.Query, "inspected", "complete"); err != nil {
		t.Fatal(err)
	}
	return s, id, envelope.Ref, payload, calls
}

func TestRunOnce_RetainedResults_ReferenceSurvivesAndFetchReachesRequest(t *testing.T) {
	s, id, ref, payload, originalCalls := retainedLargeResult(t)
	modelCalls := 0
	s.Planner = react.New(resultClient{fn: func(req llm.CompleteRequest) (llm.CompleteResponse, error) {
		modelCalls++
		if modelCalls == 1 {
			found := false
			for _, m := range req.Messages {
				if m.Role == llm.RoleUser && m.Content.Text != nil && strings.Contains(*m.Content.Text, ref) {
					found = true
				}
			}
			if !found {
				t.Error("compacted reference missing from the next actual request")
			}
			args, _ := json.Marshal(builtin.ArtifactFetchArgs{Ref: ref, Offset: len(payload) - 256, MaxBytes: 512})
			return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{ID: "recover", Name: "artifact_fetch", Args: args}}}, nil
		}
		found := false
		for _, m := range req.Messages {
			if m.Role != llm.RoleTool || m.ToolCallID == nil || *m.ToolCallID != "recover" || m.Content.Text == nil {
				continue
			}
			var got builtin.ArtifactFetchOut
			if err := json.Unmarshal([]byte(*m.Content.Text), &got); err != nil {
				t.Fatalf("result encoding: %.600s: %v", *m.Content.Text, err)
			}
			if got.Content != payload[len(payload)-256:] || got.ReturnedBytes != 256 || got.Truncated || got.Error != "" {
				t.Fatalf("incorrect bounded recovery: %+v", got)
			}
			found = true
		}
		if !found {
			t.Error("fresh recovered bytes omitted from dependent request")
		}
		return llm.CompleteResponse{Content: "exact source recovered"}, nil
	}})
	if _, err := s.RunOnce(t.Context(), "recover the exact result", id, assemble.WithRunID("next")); err != nil {
		t.Fatal(err)
	}
	if modelCalls != 2 || originalCalls.Load() != 1 {
		t.Fatalf("model=%d original tool=%d", modelCalls, originalCalls.Load())
	}
}

func TestRunOnce_RetainedResults_DeletedBlobCannotReachInference(t *testing.T) {
	s, id, ref, _, _ := retainedLargeResult(t)
	if _, err := s.Artifacts.Delete(t.Context(), artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}, ref); err != nil {
		t.Fatal(err)
	}
	modelCalls := 0
	s.Planner = react.New(resultClient{fn: func(llm.CompleteRequest) (llm.CompleteResponse, error) {
		modelCalls++
		return llm.CompleteResponse{Content: "must not see stale source"}, nil
	}})
	_, err := s.RunOnce(t.Context(), "continue", id, assemble.WithRunID("deleted"))
	if !errors.Is(err, runctx.ErrRetainedContextUnavailable) || modelCalls != 0 {
		t.Fatalf("deleted evidence accepted: calls=%d err=%v", modelCalls, err)
	}
}

func TestRunOnce_RetainedResults_DeletionDuringInferenceFencesDispatch(t *testing.T) {
	s, id, ref, _, calls := retainedLargeResult(t)
	modelCalls := 0
	s.Planner = react.New(resultClient{fn: func(llm.CompleteRequest) (llm.CompleteResponse, error) {
		modelCalls++
		if _, err := s.Artifacts.Delete(t.Context(), artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}, ref); err != nil {
			t.Fatal(err)
		}
		return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{ID: "write-after-delete", Name: "large_read", Args: json.RawMessage(`{}`)}}}, nil
	}})
	_, err := s.RunOnce(t.Context(), "continue", id, assemble.WithRunID("deleted-during"))
	if !errors.Is(err, runctx.ErrRetainedContextUnavailable) || modelCalls != 1 || calls.Load() != 1 {
		t.Fatalf("deleted evidence permitted dispatch: calls=%d tools=%d err=%v", modelCalls, calls.Load(), err)
	}
}
