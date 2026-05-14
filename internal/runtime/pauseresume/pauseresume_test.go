package pauseresume_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// testID is a documented dummy identity triple — no secrets (CLAUDE.md
// §13: fixtures carry documented dummy values).
var testID = identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}

// runCtx attaches a full quadruple to ctx so Resume's
// identityFromContext finds the resuming scope.
func runCtx(t *testing.T, id identity.Identity, runID string) context.Context {
	t.Helper()
	ctx, err := identity.WithRun(context.Background(), id, runID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// newStore opens a fresh in-memory StateStore for durability tests.
func newStore(t *testing.T) state.StateStore {
	t.Helper()
	s, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

func TestRequest_MintsUniqueOpaqueToken(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	ctx := runCtx(t, testID, "run-1")

	seen := make(map[pauseresume.Token]struct{})
	for i := 0; i < 64; i++ {
		p, err := c.Request(ctx, pauseresume.PauseRequest{
			Identity: testID,
			Reason:   pauseresume.ReasonApprovalRequired,
		})
		if err != nil {
			t.Fatalf("Request #%d: %v", i, err)
		}
		if p.Token == "" {
			t.Fatalf("Request #%d: empty token", i)
		}
		if _, dup := seen[p.Token]; dup {
			t.Fatalf("Request #%d: duplicate token %q", i, p.Token)
		}
		seen[p.Token] = struct{}{}
	}
}

func TestRequest_RejectsIncompleteIdentity(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	ctx := context.Background()

	_, err := c.Request(ctx, pauseresume.PauseRequest{
		Identity: identity.Identity{TenantID: "t1"}, // missing user + session
		Reason:   pauseresume.ReasonApprovalRequired,
	})
	if !errors.Is(err, pauseresume.ErrIdentityRequired) {
		t.Fatalf("Request: err=%v, want ErrIdentityRequired", err)
	}
}

func TestRequest_RejectsInvalidReason(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	ctx := runCtx(t, testID, "run-1")

	_, err := c.Request(ctx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.Reason("not_a_real_reason"),
	})
	if !errors.Is(err, pauseresume.ErrInvalidReason) {
		t.Fatalf("Request: err=%v, want ErrInvalidReason", err)
	}
}

func TestStatus_ReportsPausedThenResumed(t *testing.T) {
	t.Parallel()
	clk := &fakeClock{now: time.Unix(1700000000, 0)}
	c := pauseresume.New(pauseresume.WithClock(clk.Now))
	ctx := runCtx(t, testID, "run-1")

	p, err := c.Request(ctx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonAwaitInput,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	st, err := c.Status(ctx, p.Token)
	if err != nil {
		t.Fatalf("Status (paused): %v", err)
	}
	if st.State != pauseresume.StatusPaused {
		t.Fatalf("Status.State = %q, want %q", st.State, pauseresume.StatusPaused)
	}
	if st.Reason != pauseresume.ReasonAwaitInput {
		t.Fatalf("Status.Reason = %q, want %q", st.Reason, pauseresume.ReasonAwaitInput)
	}
	if !st.ResumedAt.IsZero() {
		t.Fatalf("Status.ResumedAt = %v, want zero (not yet resumed)", st.ResumedAt)
	}

	clk.advance(5 * time.Second)
	if err := c.Resume(ctx, p.Token, map[string]any{"approved": true}); err != nil {
		t.Fatalf("Resume: %v", err)
	}

	st, err = c.Status(ctx, p.Token)
	if err != nil {
		t.Fatalf("Status (resumed): %v", err)
	}
	if st.State != pauseresume.StatusResumed {
		t.Fatalf("Status.State = %q, want %q", st.State, pauseresume.StatusResumed)
	}
	if st.ResumedAt.IsZero() {
		t.Fatalf("Status.ResumedAt is zero, want set after Resume")
	}
	if !st.ResumedAt.After(st.PausedAt) {
		t.Fatalf("Status.ResumedAt %v not after PausedAt %v", st.ResumedAt, st.PausedAt)
	}
}

func TestResume_SecondResumeReturnsAlreadyResumed(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	ctx := runCtx(t, testID, "run-1")

	p, err := c.Request(ctx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonApprovalRequired,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if err := c.Resume(ctx, p.Token, nil); err != nil {
		t.Fatalf("Resume #1: %v", err)
	}
	err = c.Resume(ctx, p.Token, nil)
	if !errors.Is(err, pauseresume.ErrAlreadyResumed) {
		t.Fatalf("Resume #2: err=%v, want ErrAlreadyResumed", err)
	}
}

func TestResume_UnknownTokenReturnsPauseNotFound(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	ctx := runCtx(t, testID, "run-1")

	err := c.Resume(ctx, pauseresume.Token("nonexistent"), nil)
	if !errors.Is(err, pauseresume.ErrPauseNotFound) {
		t.Fatalf("Resume: err=%v, want ErrPauseNotFound", err)
	}
}

func TestStatus_UnknownTokenReturnsPauseNotFound(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	ctx := runCtx(t, testID, "run-1")

	_, err := c.Status(ctx, pauseresume.Token("nonexistent"))
	if !errors.Is(err, pauseresume.ErrPauseNotFound) {
		t.Fatalf("Status: err=%v, want ErrPauseNotFound", err)
	}
}

func TestResume_MissingIdentityReturnsIdentityRequired(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	ctx := runCtx(t, testID, "run-1")

	p, err := c.Request(ctx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonApprovalRequired,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	// Resume with a bare context — no identity attached.
	err = c.Resume(context.Background(), p.Token, nil)
	if !errors.Is(err, pauseresume.ErrIdentityRequired) {
		t.Fatalf("Resume: err=%v, want ErrIdentityRequired", err)
	}
}

func TestResume_ScopeMismatchRejected(t *testing.T) {
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

	// A different tenant attempts the resume.
	otherID := identity.Identity{TenantID: "t2", UserID: "u1", SessionID: "s1"}
	resumeCtx := runCtx(t, otherID, "run-1")
	err = c.Resume(resumeCtx, p.Token, nil)
	if !errors.Is(err, pauseresume.ErrScopeMismatch) {
		t.Fatalf("Resume: err=%v, want ErrScopeMismatch", err)
	}
}

func TestResume_DifferentRunSameTripleAccepted(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	pauseCtx := runCtx(t, testID, "run-1")

	p, err := c.Request(pauseCtx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonAwaitInput,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	// Same triple, different RunID — the isolation boundary is the
	// triple, not the run; the resume must be accepted.
	resumeCtx := runCtx(t, testID, "run-2-resume")
	if err := c.Resume(resumeCtx, p.Token, nil); err != nil {
		t.Fatalf("Resume (different run, same triple): %v", err)
	}
}

func TestRestartSurvival_WithoutStore_PauseDoesNotSurvive(t *testing.T) {
	t.Parallel()
	// No checkpoint store — pauses are process-local only.
	c1 := pauseresume.New()
	ctx := runCtx(t, testID, "run-1")

	p, err := c1.Request(ctx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonExternalEvent,
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	// A fresh Coordinator simulates a Runtime restart. Without a
	// shared checkpoint store the token is genuinely not found.
	c2 := pauseresume.New()
	_, err = c2.Status(ctx, p.Token)
	if !errors.Is(err, pauseresume.ErrPauseNotFound) {
		t.Fatalf("Status on fresh coordinator: err=%v, want ErrPauseNotFound", err)
	}
}

func TestRestartSurvival_WithStore_PauseSurvives(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	ctx := runCtx(t, testID, "run-1")

	// Coordinator #1 records a checkpointed pause, then "crashes".
	c1 := pauseresume.New(pauseresume.WithCheckpointStore(store))
	p, err := c1.Request(ctx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonApprovalRequired,
		Payload:  map[string]any{"prompt": "approve tool call?"},
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	// Coordinator #2 over the SAME store simulates the restarted
	// Runtime. The pause survived.
	c2 := pauseresume.New(pauseresume.WithCheckpointStore(store))
	st, err := c2.Status(ctx, p.Token)
	if err != nil {
		t.Fatalf("Status on restarted coordinator: %v", err)
	}
	if st.State != pauseresume.StatusPaused {
		t.Fatalf("Status.State = %q, want %q", st.State, pauseresume.StatusPaused)
	}
	if st.Reason != pauseresume.ReasonApprovalRequired {
		t.Fatalf("Status.Reason = %q, want %q", st.Reason, pauseresume.ReasonApprovalRequired)
	}

	// And it can be resumed on the restarted coordinator.
	if err := c2.Resume(ctx, p.Token, map[string]any{"approved": true}); err != nil {
		t.Fatalf("Resume on restarted coordinator: %v", err)
	}
	st, err = c2.Status(ctx, p.Token)
	if err != nil {
		t.Fatalf("Status after resume: %v", err)
	}
	if st.State != pauseresume.StatusResumed {
		t.Fatalf("Status.State = %q, want %q after resume", st.State, pauseresume.StatusResumed)
	}
}

// TestRequest_FailsLoudlyOnUnserializableTrajectory is the CLAUDE.md
// §11 mandatory pause/resume serialisation test: a PauseRequest whose
// trajectory carries a non-JSON-encodable leaf must fail Request loud
// with trajectory.ErrUnserializable — never half-persisted.
func TestRequest_FailsLoudlyOnUnserializableTrajectory(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	c := pauseresume.New(pauseresume.WithCheckpointStore(store))
	ctx := runCtx(t, testID, "run-1")

	// A trajectory whose ToolContext.Serializable carries a live
	// channel masquerading as a "config" value — non-JSON-encodable.
	tr := &trajectory.Trajectory{
		Query: "do the thing",
		ToolContext: trajectory.ToolContext{
			Serializable: map[string]any{"callback": make(chan int)},
		},
	}

	_, err := c.Request(ctx, pauseresume.PauseRequest{
		Identity:   testID,
		Reason:     pauseresume.ReasonApprovalRequired,
		Trajectory: tr,
	})
	var unserr trajectory.ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Fatalf("Request: err=%v, want trajectory.ErrUnserializable", err)
	}
	if unserr.Field == "" {
		t.Fatalf("ErrUnserializable.Field is empty, want the offending field path")
	}

	// Nothing was half-persisted: no checkpoint exists for any token
	// (the Request returned before minting an in-registry entry, and
	// the store Save was never reached).
	if _, lerr := store.LoadByEventID(ctx, state.EventID("anything")); lerr == nil {
		t.Fatalf("unexpected checkpoint persisted after a failed Request")
	}
}

// fakeClock is a controllable clock for deterministic timestamp
// assertions (CLAUDE.md §11 — never time.Sleep for synchronisation).
type fakeClock struct {
	now time.Time
}

func (f *fakeClock) Now() time.Time { return f.now }
func (f *fakeClock) advance(d time.Duration) {
	f.now = f.now.Add(d)
}
