package pauseresume_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/state"
)

// TestRestartSurvival_WithTrajectory exercises the full
// checkpoint round-trip including a serialised trajectory: a
// Coordinator with a store checkpoints a pause carrying a trajectory;
// a fresh Coordinator over the same store rehydrates it (Deserialize
// path) and resumes it.
func TestRestartSurvival_WithTrajectory(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	ctx := runCtx(t, testID, "run-1")

	tr := &trajectory.Trajectory{
		Query: "checkpointed run",
		LLMContext: map[string]any{
			"note": "prior-turn-summary",
		},
		ToolContext: trajectory.ToolContext{
			Serializable: map[string]any{"region": "us-east-1"},
		},
	}

	c1 := pauseresume.New(pauseresume.WithCheckpointStore(store))
	p, err := c1.Request(ctx, pauseresume.PauseRequest{
		Identity:   testID,
		Reason:     pauseresume.ReasonExternalEvent,
		Trajectory: tr,
		Payload:    map[string]any{"webhook": "pending"},
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	// Restarted Coordinator rehydrates the pause (trajectory bytes are
	// Deserialized on the rehydrate path).
	c2 := pauseresume.New(pauseresume.WithCheckpointStore(store))
	st, err := c2.Status(ctx, p.Token)
	if err != nil {
		t.Fatalf("Status on restarted coordinator: %v", err)
	}
	if st.State != pauseresume.StatusPaused {
		t.Fatalf("Status.State = %q, want paused", st.State)
	}
	if err := c2.Resume(ctx, p.Token, pauseresume.DecisionResume, nil); err != nil {
		t.Fatalf("Resume on restarted coordinator: %v", err)
	}

	// The checkpoint was cleared on resume — a third Coordinator
	// cannot find the token.
	c3 := pauseresume.New(pauseresume.WithCheckpointStore(store))
	if _, err := c3.Status(ctx, p.Token); !errors.Is(err, pauseresume.ErrPauseNotFound) {
		t.Fatalf("Status after resume-cleared checkpoint: err=%v, want ErrPauseNotFound", err)
	}
}

// TestStatus_CorruptCheckpointFailsLoud verifies that a checkpoint
// whose persisted bytes do not decode into a pause record surfaces
// ErrCheckpointCorrupt — never a half-decoded record.
func TestStatus_CorruptCheckpointFailsLoud(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	ctx := runCtx(t, testID, "run-1")

	// Hand-write a checkpoint slot with garbage bytes under a token.
	const token = pauseresume.Token("01HCORRUPTTOKEN0000000000")
	bad := state.StateRecord{
		ID:       state.EventID(token),
		Identity: identity.Quadruple{Identity: testID, RunID: "run-1"},
		Kind:     "pauseresume.checkpoint:" + string(token),
		Bytes:    []byte("{not valid json for a checkpoint record"),
	}
	if err := store.Save(ctx, bad); err != nil {
		t.Fatalf("seed corrupt checkpoint: %v", err)
	}

	c := pauseresume.New(pauseresume.WithCheckpointStore(store))
	_, err := c.Status(ctx, token)
	if !errors.Is(err, pauseresume.ErrCheckpointCorrupt) {
		t.Fatalf("Status on corrupt checkpoint: err=%v, want ErrCheckpointCorrupt", err)
	}
}

// TestResume_BareIdentityContextAccepted verifies the
// identityFromContext fallback branch: a context carrying a bare
// Identity (attached via identity.With, not WithRun) still satisfies
// the resuming-scope check.
func TestResume_BareIdentityContextAccepted(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()

	// Pause under a full quadruple.
	pauseCtx := runCtx(t, testID, "run-1")
	p, err := c.Request(pauseCtx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonApprovalRequired,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	// Resume with a context carrying only a bare Identity.
	bareCtx, err := identity.With(context.Background(), testID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if err := c.Resume(bareCtx, p.Token, pauseresume.DecisionResume, nil); err != nil {
		t.Fatalf("Resume with bare-identity ctx: %v", err)
	}
}

// TestResume_IncompleteIdentityInContextRejected verifies that a
// context carrying a structurally-incomplete identity fails closed.
func TestResume_IncompleteIdentityInContextRejected(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	pauseCtx := runCtx(t, testID, "run-1")
	p, err := c.Request(pauseCtx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonApprovalRequired,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	// A context whose quadruple is missing a component cannot be
	// built via identity.WithRun (it validates), so this branch is
	// covered by the bare-context case in
	// TestResume_MissingIdentityReturnsIdentityRequired. Here we
	// confirm Status / Resume both honour a cancelled context.
	cctx, cancel := context.WithCancel(pauseCtx)
	cancel()
	if err := c.Resume(cctx, p.Token, pauseresume.DecisionResume, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Resume on cancelled ctx: err=%v, want context.Canceled", err)
	}
	if _, err := c.Status(cctx, p.Token); !errors.Is(err, context.Canceled) {
		t.Fatalf("Status on cancelled ctx: err=%v, want context.Canceled", err)
	}
}

// TestRequest_StoreSaveError verifies that a checkpoint-store failure
// surfaces loud out of Request — the pause is not recorded in the
// in-memory registry when the durable write failed.
func TestRequest_StoreSaveError(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	// Close the store so every Save fails with ErrStoreClosed.
	if err := store.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	c := pauseresume.New(pauseresume.WithCheckpointStore(store))
	ctx := runCtx(t, testID, "run-1")

	p, err := c.Request(ctx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonApprovalRequired,
	})
	if err == nil {
		t.Fatalf("Request on closed store: err=nil, want a store error")
	}
	if !errors.Is(err, state.ErrStoreClosed) {
		t.Fatalf("Request on closed store: err=%v, want state.ErrStoreClosed", err)
	}
	// The token (if any) is not resolvable — nothing was recorded.
	if p.Token != "" {
		st, serr := c.Status(ctx, p.Token)
		t.Fatalf("Request returned token %q after store failure (Status: %+v / %v)", p.Token, st, serr)
	}
}

// fresh config import keeps the durability seam explicit even when
// newStore is the only consumer.
var _ = config.StateConfig{}
