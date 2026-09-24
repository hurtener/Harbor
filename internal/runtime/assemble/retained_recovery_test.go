package assemble_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	sdk "github.com/hurtener/Harbor/sdk/assemble"
)

func TestRunOnce_RetainedRecoveryActualRequest(t *testing.T) {
	stack, client, toolCalls := retainedRecordingStack(t)
	stack.Cfg.Memory.Strategy = "rolling_summary"
	stack.Cfg.Memory.RecentTurns = 4
	sink := registerHookSink(t, stack, "recovery_sink")
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "recovery-sdk"}
	q := identity.Quadruple{Identity: id, RunID: "lost-process"}
	base := planner.RunContext{Quadruple: q, Query: "edit the existing document", Trajectory: &planner.Trajectory{Query: "edit the existing document"}}
	owner, err := sessionmemory.BeginRetainedRun(t.Context(), stack.State, stack.Redactor, q, 4, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Apply(&base); err != nil {
		t.Fatal(err)
	}
	if err := owner.Start(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	step := planner.Step{Action: planner.CallTool{Tool: "retained_read", CallID: "prior-read", Args: json.RawMessage(`{}`)}}
	if err := owner.BeforeDispatch(t.Context(), base, step); err != nil {
		t.Fatal(err)
	}
	step.LLMObservation = json.RawMessage(`{"source":"` + strings.Repeat("x", 14585) + `","resource_id":"doc-a","version":9007199254740993,"more":false}`)
	if err := owner.AfterDispatch(t.Context(), base, step); err != nil {
		t.Fatal(err)
	}
	// Simulate process loss after settlement, before a terminal write. The SDK
	// method explicitly fences and publishes evidence without running any agent.
	public := stack
	if err := public.ReconcileRetainedContext(t.Context(), id, q.RunID); err != nil {
		t.Fatal(err)
	}
	if err := public.ReconcileRetainedContext(t.Context(), id, q.RunID); err != nil {
		t.Fatal(err)
	}
	if toolCalls.Load() != 0 {
		t.Fatal("reconciliation executed a tool")
	}
	client.mu.Lock()
	calls := len(client.requests)
	client.mu.Unlock()
	if calls != 0 {
		t.Fatal("reconciliation invoked inference")
	}
	if _, ok := sink.get(q.RunID); ok {
		t.Fatal("reconciliation invoked trusted completion hook")
	}
	if _, err := stack.RunOnce(t.Context(), "Continue with the committed source", id, sdk.WithRunID("next"), assemble.WithCompletionHook(&steering.CompletionHookSpec{Tool: "recovery_sink"})); err != nil {
		t.Fatal(err)
	}
	body := client.body(t, id.SessionID+"/next")
	for _, want := range []string{"doc-a", "9007199254740993", `"more":false`, strings.Repeat("x", 14585), `"historical_run_outcome":"interrupted"`} {
		if !strings.Contains(body, want) {
			t.Fatalf("next request lost %q", want[:min(len(want), 50)])
		}
	}
	if toolCalls.Load() != 0 {
		t.Fatal("historical action was dispatched on continuation")
	}
	if err := owner.BeforeDispatch(t.Context(), base, step); !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) {
		t.Fatalf("old admission still active: %v", err)
	}
}

func TestRunOnce_RetainedRecoveryDisabledIdentityAndCancellation(t *testing.T) {
	stack, _, _ := retainedRecordingStack(t)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	if err := stack.ReconcileRetainedContext(t.Context(), id, "source"); !errors.Is(err, sdk.ErrRetainedContextUnavailable) {
		t.Fatalf("disabled retention accepted: %v", err)
	}
	var absent *sdk.Stack
	if err := absent.ReconcileRetainedContext(t.Context(), id, "source"); !errors.Is(err, sdk.ErrRetainedContextUnavailable) {
		t.Fatal("nil stack accepted")
	}
	stack.Cfg.Memory.Strategy = "rolling_summary"
	stack.Cfg.Memory.RecentTurns = 4
	if err := stack.ReconcileRetainedContext(t.Context(), identity.Identity{}, "source"); err == nil {
		t.Fatal("missing identity accepted")
	}
	if err := stack.ReconcileRetainedContext(t.Context(), id, ""); err == nil {
		t.Fatal("missing source accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := stack.ReconcileRetainedContext(ctx, id, "source"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled reconciliation accepted: %v", err)
	}
}
