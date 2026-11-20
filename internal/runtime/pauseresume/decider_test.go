package pauseresume_test

import (
	"context"
	"errors"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

func deciderBus(t *testing.T) events.EventBus {
	t.Helper()
	b, err := eventsInmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32,
		IdleTimeout: time.Second, DropWindow: 50 * time.Millisecond,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("eventsInmem.New: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b
}

func resumedEvents(t *testing.T, bus events.EventBus) events.Subscription {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant: testID.TenantID, User: testID.UserID, Session: testID.SessionID,
		Types: []events.EventType{pauseresume.EventTypePauseResumed},
	})
	if err != nil {
		cancel()
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(func() { sub.Cancel(); cancel() })
	return sub
}

func nextResumed(t *testing.T, sub events.Subscription) pauseresume.PauseResumedPayload {
	t.Helper()
	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(pauseresume.PauseResumedPayload)
		if !ok {
			t.Fatalf("payload %T", ev.Payload)
		}
		return p
	case <-time.After(2 * time.Second):
		t.Fatal("no pause.resumed")
		return pauseresume.PauseResumedPayload{}
	}
}

// The deciding principal seated on the resuming ctx is stamped on
// pause.resumed with the resolution time; an unseated resume carries
// no principal.
func TestResume_StampsDeciderAndDecidedAt(t *testing.T) {
	t.Parallel()
	bus := deciderBus(t)
	sub := resumedEvents(t, bus)
	now := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	c := pauseresume.New(pauseresume.WithBus(bus), pauseresume.WithClock(func() time.Time { return now }))
	ctx := runCtx(t, testID, "run-decider")

	p, err := c.Request(ctx, pauseresume.PauseRequest{Identity: testID, Reason: pauseresume.ReasonApprovalRequired})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	d := pauseresume.Decider{TenantID: "t1", UserID: "approver", SessionID: "s1", Speaker: "Approver"}
	if err := c.Resume(pauseresume.WithDecider(ctx, d), p.Token, pauseresume.DecisionApprove, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	got := nextResumed(t, sub)
	if got.DecidedByTenant != d.TenantID || got.DecidedByUser != d.UserID || got.DecidedBySession != d.SessionID ||
		got.DecidedBySpeaker != d.Speaker || !got.DecidedAt.Equal(now) || got.Decision != pauseresume.DecisionApprove {
		t.Fatalf("pause.resumed = %+v", got)
	}

	p2, err := c.Request(ctx, pauseresume.PauseRequest{Identity: testID, Reason: pauseresume.ReasonAwaitInput})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	if err := c.Resume(ctx, p2.Token, pauseresume.DecisionResume, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := nextResumed(t, sub); got.DecidedByUser != "" || got.DecidedBySpeaker != "" {
		t.Fatalf("unattributed resume carried a principal: %+v", got)
	}
}

// Resolved fires for an out-of-band resolution and is closed for an
// already-resolved pause; an unknown token yields nil.
func TestResolved_SignalsResolution(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	ctx := runCtx(t, testID, "run-resolved")
	p, err := c.Request(ctx, pauseresume.PauseRequest{Identity: testID, Reason: pauseresume.ReasonApprovalRequired})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	ch := pauseresume.Resolved(c, p.Token)
	select {
	case <-ch:
		t.Fatal("signal fired before resolution")
	default:
	}
	if err := c.Resume(ctx, p.Token, pauseresume.DecisionTimeout, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("signal did not fire on resolution")
	}
	select {
	case <-pauseresume.Resolved(c, p.Token):
	default:
		t.Fatal("already-resolved pause must yield a closed channel")
	}
	if pauseresume.Resolved(c, "unknown") != nil {
		t.Fatal("unknown token must yield nil")
	}
}

// After a restart, ResolveInterrupted resolves matching process-bound
// checkpoints with the typed `interrupted` Decision, deletes them, and
// leaves pauses with a trajectory (or not matching) untouched.
func TestResolveInterrupted_ResolvesProcessBoundCheckpoints(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	ctx := runCtx(t, testID, "run-interrupted")
	before := pauseresume.New(pauseresume.WithCheckpointStore(store))
	gated, err := before.Request(ctx, pauseresume.PauseRequest{
		Identity: testID, Reason: pauseresume.ReasonApprovalRequired,
		Payload: map[string]any{"tool": "delete_doc"},
	})
	if err != nil {
		t.Fatalf("Request gated: %v", err)
	}
	other, err := before.Request(ctx, pauseresume.PauseRequest{
		Identity: testID, Reason: pauseresume.ReasonAwaitInput,
	})
	if err != nil {
		t.Fatalf("Request other: %v", err)
	}
	withTrajectory, err := before.Request(ctx, pauseresume.PauseRequest{
		Identity: testID, Reason: pauseresume.ReasonApprovalRequired,
		Payload:    map[string]any{"tool": "delete_doc"},
		Trajectory: &trajectory.Trajectory{},
	})
	if err != nil {
		t.Fatalf("Request trajectory: %v", err)
	}

	// A fresh Coordinator over the same store is the restarted process.
	bus := deciderBus(t)
	sub := resumedEvents(t, bus)
	after := pauseresume.New(pauseresume.WithCheckpointStore(store), pauseresume.WithBus(bus))
	match := func(p pauseresume.Pause) bool {
		_, ok := p.Payload["tool"].(string)
		return p.Reason == pauseresume.ReasonApprovalRequired && ok
	}
	resolved, err := pauseresume.ResolveInterrupted(context.Background(), after, match, nil)
	if err != nil {
		t.Fatalf("ResolveInterrupted: %v", err)
	}
	if len(resolved) != 1 || resolved[0].Token != gated.Token || resolved[0].Payload["tool"] != "delete_doc" {
		t.Fatalf("resolved = %+v, want only the gated pause", resolved)
	}
	if got := nextResumed(t, sub); got.Token != string(gated.Token) || got.Decision != pauseresume.DecisionInterrupted {
		t.Fatalf("pause.resumed = %+v", got)
	}

	// The checkpoint is gone for a later process too.
	third := pauseresume.New(pauseresume.WithCheckpointStore(store))
	if _, err := third.Status(ctx, gated.Token); !errors.Is(err, pauseresume.ErrPauseNotFound) {
		t.Fatalf("Status(gated) after resolution = %v, want ErrPauseNotFound", err)
	}
	for _, tok := range []pauseresume.Token{other.Token, withTrajectory.Token} {
		if st, err := third.Status(ctx, tok); err != nil || st.State != pauseresume.StatusPaused {
			t.Fatalf("Status(%s) = %+v, %v; want still paused", tok, st, err)
		}
	}
	// A second pass is a no-op.
	again, err := pauseresume.ResolveInterrupted(context.Background(), after, match, nil)
	if err != nil || len(again) != 0 {
		t.Fatalf("second pass = %+v, %v", again, err)
	}
}

func TestResolveInterrupted_NoStoreIsNoop(t *testing.T) {
	t.Parallel()
	got, err := pauseresume.ResolveInterrupted(context.Background(), pauseresume.New(), func(pauseresume.Pause) bool { return true }, nil)
	if err != nil || got != nil {
		t.Fatalf("no-store pass = %+v, %v", got, err)
	}
}

// The resolution recorder is write-once and never gating: a recorder
// failure does not block, park or make the decision retryable; a success
// gets the Coordinator's decision time and the pre-merge pause.
func TestResume_RecorderFailureNeverBlocksTheDecision(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	fail := true
	var got []pauseresume.Resolution
	c := pauseresume.New(pauseresume.WithCheckpointStore(store), pauseresume.WithClock(func() time.Time { return now }),
		pauseresume.WithResolutionRecorder(func(_ context.Context, r pauseresume.Resolution) error {
			got = append(got, r)
			if fail {
				return errors.New("store down")
			}
			return nil
		}))
	ctx := runCtx(t, testID, "run-rec")
	p, err := c.Request(ctx, pauseresume.PauseRequest{Identity: testID, Reason: pauseresume.ReasonApprovalRequired, Payload: map[string]any{"tool": "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if p.RunID != "run-rec" {
		t.Fatalf("Pause.RunID = %q", p.RunID)
	}
	if err := c.Resume(ctx, p.Token, pauseresume.DecisionApprove, map[string]any{"reason": "ok"}); err != nil {
		t.Fatalf("a record failure must not fail the decision: %v", err)
	}
	if st, _ := c.Status(ctx, p.Token); st.State != pauseresume.StatusResumed || !st.ResumedAt.Equal(now) {
		t.Fatalf("status = %+v", st)
	}
	if _, err := pauseresume.New(pauseresume.WithCheckpointStore(store)).Status(ctx, p.Token); !errors.Is(err, pauseresume.ErrPauseNotFound) {
		t.Fatalf("checkpoint not retired: %v", err)
	}
	if len(got) != 1 || got[0].Decision != pauseresume.DecisionApprove || !got[0].DecidedAt.Equal(now) ||
		got[0].ResumePayload["reason"] != "ok" || got[0].Pause.RunID != "run-rec" || got[0].Pause.Payload["reason"] != nil {
		t.Fatalf("recorded = %+v", got)
	}
}

// pause.list truncation is judged within the caller's scope: a resumed
// pause in another session does not flip it.
func TestList_TruncatedWithinScope(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	other := testID
	other.SessionID = "other-session"
	otherCtx := runCtx(t, other, "run-other")
	p, err := c.Request(otherCtx, pauseresume.PauseRequest{Identity: other, Reason: pauseresume.ReasonAwaitInput})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Resume(otherCtx, p.Token, pauseresume.DecisionResume, nil); err != nil {
		t.Fatal(err)
	}
	resp, err := c.List(context.Background(), pauseresume.ListRequest{
		Identity: testID,
		Filter:   pauseresume.ListFilter{States: []pauseresume.State{pauseresume.StatusResumed}, SessionIDs: []string{testID.SessionID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Snapshots) != 0 || !resp.Truncated {
		t.Fatalf("scoped resumed listing = %d rows, truncated=%v; want 0 rows, truncated (nothing visible in scope)", len(resp.Snapshots), resp.Truncated)
	}
}

// A decision at or after the park deadline commits the typed timeout
// inside the Coordinator and reports ErrPauseExpired.
func TestResume_DeadlineEnforcedUnderDecisionLock(t *testing.T) {
	t.Parallel()
	bus := deciderBus(t)
	sub := resumedEvents(t, bus)
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	c := pauseresume.New(pauseresume.WithBus(bus), pauseresume.WithMaxParkDuration(time.Minute),
		pauseresume.WithClock(func() time.Time { return now }))
	ctx := runCtx(t, testID, "run-deadline")
	p, err := c.Request(ctx, pauseresume.PauseRequest{Identity: testID, Reason: pauseresume.ReasonApprovalRequired})
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	err = c.Resume(pauseresume.WithDecider(ctx, pauseresume.Decider{TenantID: "t1", UserID: "late"}), p.Token, pauseresume.DecisionApprove, nil)
	if !errors.Is(err, pauseresume.ErrPauseExpired) || !errors.Is(err, pauseresume.ErrAlreadyResumed) {
		t.Fatalf("late approve = %v", err)
	}
	if ev := nextResumed(t, sub); ev.Decision != pauseresume.DecisionTimeout || ev.DecidedByUser != "" {
		t.Fatalf("pause.resumed = %+v", ev)
	}
}
