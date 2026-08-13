package pauseresume_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

// TestTrancheExceededPayload_MapRoundTrip pins the typed
// {cause, max_steps, steps_observed} shape: the typed payload renders
// to the pause-record map form with the canonical keys and decodes back
// losslessly.
func TestTrancheExceededPayload_MapRoundTrip(t *testing.T) {
	t.Parallel()
	in := pauseresume.NewTrancheExceededPayload(12, 34)
	m := in.Map()
	if got := m[pauseresume.TranchePayloadCauseKey]; got != "max_steps_exceeded" {
		t.Errorf("Map()[cause] = %v, want max_steps_exceeded", got)
	}
	if got := m[pauseresume.TranchePayloadMaxStepsKey]; got != 12 {
		t.Errorf("Map()[max_steps] = %v, want 12", got)
	}
	if got := m[pauseresume.TranchePayloadStepsKey]; got != 34 {
		t.Errorf("Map()[steps_observed] = %v, want 34", got)
	}
	out, ok := pauseresume.TrancheExceededFromMap(m)
	if !ok {
		t.Fatal("TrancheExceededFromMap(Map()) ok=false, want true")
	}
	if out.Cause != pauseresume.TrancheCauseMaxStepsExceeded || out.MaxSteps != 12 || out.StepsObserved != 34 {
		t.Errorf("round-trip = %+v, want {max_steps_exceeded 12 34}", out)
	}
}

// TestTrancheExceededFromMap_RejectsForeignShapes asserts the decoder
// fails closed on anything that is not a well-formed step-tranche
// payload: a foreign pause payload (an OAuth auth-url shape), a wrong
// cause, a missing key, or a malformed number. Never a half-decoded
// value.
func TestTrancheExceededFromMap_RejectsForeignShapes(t *testing.T) {
	t.Parallel()
	cases := map[string]map[string]any{
		"nil":                   nil,
		"foreign-payload":       {"auth_url": "https://example.com/oauth", "scopes": []string{"a"}},
		"wrong-cause":           {"cause": "max_tokens_exceeded", "max_steps": 12, "steps_observed": 12},
		"missing-max-steps":     {"cause": "max_steps_exceeded", "steps_observed": 12},
		"missing-steps":         {"cause": "max_steps_exceeded", "max_steps": 12},
		"non-integral-number":   {"cause": "max_steps_exceeded", "max_steps": 12.5, "steps_observed": 12},
		"non-numeric-max-steps": {"cause": "max_steps_exceeded", "max_steps": "twelve", "steps_observed": 12},
	}
	for name, m := range cases {
		if _, ok := pauseresume.TrancheExceededFromMap(m); ok {
			t.Errorf("%s: TrancheExceededFromMap ok=true, want false", name)
		}
	}
}

// TestStatus_Continuable renders the truthful "Continue" projection: a
// paused record is continuable by an authorised resume; a resumed
// record is terminal. The decision never depends on planner state —
// the pause record alone answers it.
func TestStatus_Continuable(t *testing.T) {
	t.Parallel()
	if got := (pauseresume.Status{State: pauseresume.StatusPaused, Available: true}).Continuable(); !got {
		t.Error("Status{paused}.Continuable() = false, want true")
	}
	if got := (pauseresume.Status{State: pauseresume.StatusResumed}).Continuable(); got {
		t.Error("Status{resumed}.Continuable() = true, want false (terminal)")
	}
}

// TestTranchePause_Restart_TypedUnavailable proves a checkpointed
// step-tranche pause is inspectable after restart but cannot be resumed
// without an exact durable run-loop redrive implementation.
func TestTranchePause_Restart_DurableRedrive(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	ctx := runCtx(t, testID, "run-tranche-1")

	tr := &trajectory.Trajectory{
		Query: "tranche run",
		Steps: []trajectory.Step{
			{Action: map[string]any{"tool": "first"}, Observation: "step-1"},
			{Action: map[string]any{"tool": "second"}, Observation: "step-2"},
		},
	}

	c1 := pauseresume.New(pauseresume.WithCheckpointStore(store))
	p, err := c1.Request(ctx, pauseresume.PauseRequest{
		Identity:   testID,
		Reason:     pauseresume.ReasonConstraintsConflict,
		Payload:    pauseresume.NewTrancheExceededPayload(2, 2).Map(),
		Trajectory: tr,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	// "Restart": a fresh Coordinator over the same store.
	c2 := pauseresume.New(pauseresume.WithCheckpointStore(store))
	st, err := c2.Status(ctx, p.Token)
	if err != nil {
		t.Fatalf("Status on restarted coordinator: %v", err)
	}
	if st.Available {
		t.Fatalf("rehydrated Status.Available = true, want false (restart redrive unavailable)")
	}
	if st.Continuable() {
		t.Fatalf("rehydrated Status.Continuable() = true, want false (restart redrive unavailable)")
	}
	// The typed payload survives the restart on the List projection.
	listed, err := c2.List(ctx, pauseresume.ListRequest{Identity: testID, PageSize: 50})
	if err != nil {
		t.Fatalf("List on restarted coordinator: %v", err)
	}
	if len(listed.Snapshots) != 1 || listed.Snapshots[0].Token != p.Token {
		t.Fatalf("restarted List snapshots = %+v, want exactly the original pause (no new-task masquerade)", listed.Snapshots)
	}
	got, ok := pauseresume.TrancheExceededFromMap(listed.Snapshots[0].Payload)
	if !ok {
		t.Fatalf("rehydrated payload %v is not a TrancheExceededPayload", listed.Snapshots[0].Payload)
	}
	if got.MaxSteps != 2 || got.StepsObserved != 2 {
		t.Errorf("rehydrated payload = %+v, want max_steps=2 steps_observed=2", got)
	}

	if err := c2.Resume(ctx, p.Token, pauseresume.DecisionResume, nil); !errors.Is(err, pauseresume.ErrRestartUnavailable) {
		t.Fatalf("Resume on restarted coordinator: err=%v, want ErrRestartUnavailable", err)
	}
}

// TestTranchePause_Restart_NoStore_TypedUnavailable proves the
// no-checkpoint-store restart path is explicitly typed unavailable —
// never a new-task masquerade. A pause recorded process-locally is not
// visible to a fresh Coordinator (ErrPauseNotFound); the original
// Coordinator still owns the same token, and a fresh pause mints a
// DIFFERENT token.
func TestTranchePause_Restart_NoStore_TypedUnavailable(t *testing.T) {
	t.Parallel()
	ctx := runCtx(t, testID, "run-tranche-2")

	c1 := pauseresume.New()
	p, err := c1.Request(ctx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonConstraintsConflict,
		Payload:  pauseresume.NewTrancheExceededPayload(2, 2).Map(),
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	// A fresh Coordinator (a "restart" with no store) cannot see the
	// process-local pause — typed ErrPauseNotFound, never a fabricated
	// record.
	c2 := pauseresume.New()
	if _, err := c2.Status(context.Background(), p.Token); !errors.Is(err, pauseresume.ErrPauseNotFound) {
		t.Fatalf("fresh Coordinator Status(err=%v), want ErrPauseNotFound", err)
	}

	// The original Coordinator still owns the pause; the token is not
	// recycled — a fresh Request mints a distinct one.
	st, err := c1.Status(ctx, p.Token)
	if err != nil {
		t.Fatalf("original Coordinator Status: %v", err)
	}
	if !st.Continuable() {
		t.Error("original Coordinator pause not continuable")
	}
	p2, err := c1.Request(ctx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonConstraintsConflict,
		Payload:  pauseresume.NewTrancheExceededPayload(2, 4).Map(),
	})
	if err != nil {
		t.Fatalf("second Request: %v", err)
	}
	if p2.Token == p.Token {
		t.Fatal("second tranche pause reuses the first token — new-task masquerade")
	}
}

// TestTranchePause_CancelResume_RaceHasOneWinner pins the atomic token
// arbitration: cancellation and resume cannot both consume a live tranche.
func TestTranchePause_CancelResume_RaceHasOneWinner(t *testing.T) {
	t.Parallel()
	ctx := runCtx(t, testID, "run-tranche-race")
	c := pauseresume.New()
	p, err := c.Request(ctx, pauseresume.PauseRequest{Identity: testID, Reason: pauseresume.ReasonConstraintsConflict, Payload: pauseresume.NewTrancheExceededPayload(2, 2).Map()})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	results := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); results <- pauseresume.CancelTranche(ctx, c, p.Token) }()
	go func() { defer wg.Done(); results <- c.Resume(ctx, p.Token, pauseresume.DecisionResume, nil) }()
	wg.Wait()
	close(results)
	var successes, losers int
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		if errors.Is(err, pauseresume.ErrAlreadyResumed) {
			losers++
			continue
		}
		t.Fatalf("race loser error = %v, want ErrAlreadyResumed", err)
	}
	if successes != 1 || losers != 1 {
		t.Fatalf("successes=%d losers=%d, want one each", successes, losers)
	}
	st, err := c.Status(ctx, p.Token)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Continuable() {
		t.Fatal("terminal race winner left a reusable tranche token")
	}
}

func TestCoordinator_Resume_RejectsCancellationDecision(t *testing.T) {
	t.Parallel()
	ctx := runCtx(t, testID, "run-cancelled-decision")
	c := pauseresume.New()
	p, err := c.Request(ctx, pauseresume.PauseRequest{
		Identity: testID, Reason: pauseresume.ReasonAwaitInput,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if err := c.Resume(ctx, p.Token, pauseresume.DecisionCancelled, nil); !errors.Is(err, pauseresume.ErrInvalidDecision) {
		t.Fatalf("Resume(cancelled): err=%v, want ErrInvalidDecision", err)
	}
}
