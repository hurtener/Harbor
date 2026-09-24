package assemble_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/react"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/tools/builtin"
)

func TestRunOnce_RetainedInputsSurviveCompaction(t *testing.T) {
	s, _, toolsCalled := retainedRecordingStack(t)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "inputs"}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	secretBody := strings.Repeat("SOURCE-BYTES-NOT-IN-JOURNAL-", 500)
	ref, err := s.Artifacts.PutText(t.Context(), scope, secretBody, artifacts.PutOpts{MimeType: "text/plain", Filename: "source.txt"})
	if err != nil {
		t.Fatal(err)
	}
	q := identity.Quadruple{Identity: id, RunID: "first"}
	base, err := runctx.NewRunContext(t.Context(), runctx.Sources{Artifacts: s.Artifacts}, q, "Use the attached source", runctx.WithInputArtifacts(ref.ID))
	if err != nil {
		t.Fatal(err)
	}
	r, err := sessionmemory.BeginRetainedRun(t.Context(), s.State, s.Redactor, q, 4, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Apply(&base); err != nil {
		t.Fatal(err)
	}
	if err = r.Start(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	base.Trajectory.Steps = append(base.Trajectory.Steps, planner.Step{LLMObservation: "older check"}, planner.Step{LLMObservation: "recent check"})
	// This fixture bypasses the run loop; model the earlier decision having
	// consumed the attachment and old result while the latest stays fresh.
	seen := len(base.Trajectory.Steps) - 1
	base.Trajectory.UnseenFrom = &seen
	base.Budget.TokenBudget = 1
	if err = planner.NewCompressionRunner(resultSummary{}).MaybeCompress(t.Context(), base, base.Trajectory); err != nil {
		t.Fatal(err)
	}
	if base.Trajectory.Summary == nil {
		t.Fatal("fixture did not compact")
	}
	if err = r.Finish(t.Context(), base.Trajectory, base.Query, "Done", "complete"); err != nil {
		t.Fatal(err)
	}
	if err := builtin.RegisterWith(builtin.RegistryContext{Catalog: s.Catalog, ArtifactStore: s.Artifacts}, []string{"artifact_fetch"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.Planner = react.New(resultClient{fn: func(req llm.CompleteRequest) (llm.CompleteResponse, error) {
		calls++
		if calls == 2 {
			found := false
			for _, msg := range req.Messages {
				if msg.Role == llm.RoleTool && msg.ToolCallID != nil && *msg.ToolCallID == "recover-input" && msg.Content.Text != nil && strings.Contains(*msg.Content.Text, secretBody) {
					found = true
				}
			}
			if !found {
				t.Error("authorized attachment fetch did not reach the next decision exactly")
			}
			return llm.CompleteResponse{Content: "Ready", FinishReason: "stop"}, nil
		}
		body, err := json.Marshal(req.Messages)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), ref.ID) || !strings.Contains(string(body), "retained input attachment") {
			t.Error("compacted attachment reference absent from next request")
		}
		if strings.Contains(string(body), secretBody) {
			t.Error("retained request duplicated source bytes")
		}
		args, err := json.Marshal(builtin.ArtifactFetchArgs{Ref: ref.ID, MaxBytes: len(secretBody) + 1})
		if err != nil {
			t.Fatal(err)
		}
		return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{ID: "recover-input", Name: "artifact_fetch", Args: args}}}, nil
	}})
	if _, err := s.RunOnce(t.Context(), "Revise the attached source from before", id, assemble.WithRunID("second")); err != nil {
		t.Fatal(err)
	}
	if calls != 2 || toolsCalled.Load() != 0 {
		t.Fatal("restoration repeated model or tool work")
	}
}

func TestRunOnce_RetainedInputsDeletionDuringInferenceFencesDispatch(t *testing.T) {
	s, _, toolsCalled := retainedRecordingStack(t)
	s.Cfg.Memory.RecentTurns = 2
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "delete-input"}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	ref, err := s.Artifacts.PutText(t.Context(), scope, "source", artifacts.PutOpts{MimeType: "text/plain"})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.Planner = react.New(resultClient{fn: func(llm.CompleteRequest) (llm.CompleteResponse, error) {
		calls++
		if calls == 1 {
			if _, err := s.Artifacts.Delete(t.Context(), scope, ref.ID); err != nil {
				t.Fatal(err)
			}
			return llm.CompleteResponse{ToolCalls: []llm.ToolCallStructured{{ID: "dependent", Name: "retained_read", Args: json.RawMessage(`{}`)}}}, nil
		}
		return llm.CompleteResponse{Content: "done"}, nil
	}})
	_, err = s.RunOnce(t.Context(), "Apply the source", id, assemble.WithInputArtifacts(ref.ID))
	if !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) || calls != 1 || toolsCalled.Load() != 0 {
		t.Fatalf("deleted input allowed dependent work: err=%v model=%d tools=%d", err, calls, toolsCalled.Load())
	}
}

func TestRunOnce_RetainedInputsMissingAdmissionRefused(t *testing.T) {
	s, _, _ := retainedRecordingStack(t)
	s.Cfg.Memory.RecentTurns = 2
	calls := 0
	s.Planner = react.New(resultClient{fn: func(llm.CompleteRequest) (llm.CompleteResponse, error) {
		calls++
		return llm.CompleteResponse{Content: "done"}, nil
	}})
	_, err := s.RunOnce(context.Background(), "Inspect", identity.Identity{TenantID: "t", UserID: "u", SessionID: "missing-input"}, assemble.WithInputArtifacts("missing"))
	if !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) || calls != 0 {
		t.Fatalf("missing input silently dropped: %v calls=%d", err, calls)
	}
}
