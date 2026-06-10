// Phase 111c — durable pauses + pause lifecycle integration tests
// (RFC §3.3 + §6.3 + §6.11; docs/plans/phase-111c-durable-pause-lifecycle.md;
// D-200).
//
// Real drivers everywhere on the seam (CLAUDE.md §17.3 #1): the real
// steering.RunLoop driving a RequestPause-emitting planner, the real
// pauseresume.Coordinator over a file-backed sqlite StateStore (and
// the full inmem/sqlite/postgres triad for the trajectory round-trip),
// the real inmem event bus, the real RunSweeper. It exercises:
//
//   - the production pause path checkpoints the run's LIVE trajectory
//     (the `Trajectory: nil` gap closed) with `format_version: 1`, and
//     the run continues after a wire-shaped APPROVE;
//   - the restart-shaped durability E2E: a NEW Coordinator over the
//     SAME store rehydrates the pause, the checkpointed trajectory
//     round-trips byte-stably, Resume succeeds, and the
//     destructive-Resume contract holds (resumed ⇒ checkpoint deleted
//     ⇒ ErrPauseNotFound on the next fresh Coordinator);
//   - the timeout E2E: the max-park sweeper reaps an expired pause
//     with `Decision: timeout` (DecisionTimeout's first producer), the
//     waiting run terminates as a constraints-conflict, and the
//     checkpoint is deleted (no orphan);
//   - cancel-while-paused no longer leaks forever: the orphaned record
//   - checkpoint are reaped at deadline;
//   - identity propagation on every pause event (CLAUDE.md §17.3 #2);
//   - failure modes (CLAUDE.md §17.3 #3): a non-serialisable
//     trajectory fails the run loud at Request time with
//     trajectory.ErrUnserializable (§11 mandatory — no silent nil, no
//     half-persisted checkpoint), and a lost tool-context handle on a
//     restart-shaped resume fails loud with ErrToolContextLost;
//   - N≥100 concurrent Request/Resume against ONE store-backed
//     Coordinator under -race (the D-025 suite extended to the
//     store-backed shape), across distinct sessions.
package integration

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
)

// phase111cID is a documented dummy identity triple — no secrets.
var phase111cID = identity.Identity{
	TenantID:  "tenant-111c",
	UserID:    "user-111c",
	SessionID: "session-111c",
}

func phase111cQuad(runID string) identity.Quadruple {
	return identity.Quadruple{Identity: phase111cID, RunID: runID}
}

func phase111cCtx(t *testing.T, runID string) context.Context {
	t.Helper()
	ctx, err := identity.WithRun(context.Background(), phase111cID, runID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

func phase111cBus(t *testing.T, red audit.Redactor) events.EventBus {
	t.Helper()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     128,
		IdleTimeout:              2 * time.Second,
		DropWindow:               50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("eventsInmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func phase111cSQLite(t *testing.T) state.StateStore {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "phase111c.sqlite")
	s, err := state.Open(context.Background(), config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatalf("state.Open(sqlite): %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

// phase111cPlanner emits RequestPause once (step 1), then Finish.
type phase111cPlanner struct {
	mu     sync.Mutex
	step   int
	reason planner.PauseReason
}

func (p *phase111cPlanner) Next(_ context.Context, _ planner.RunContext) (planner.Decision, error) {
	p.mu.Lock()
	p.step++
	step := p.step
	p.mu.Unlock()
	if step == 1 {
		reason := p.reason
		if reason == "" {
			reason = planner.PauseApprovalRequired
		}
		return planner.RequestPause{
			Reason:  reason,
			Payload: map[string]any{"phase": "111c"},
		}, nil
	}
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

func (p *phase111cPlanner) steps() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.step
}

// awaitPauseRequested waits (bounded) for the run's pause.requested
// event and returns the minted Token, asserting identity propagation
// on the event envelope.
func awaitPauseRequested(t *testing.T, sub events.Subscription, wantQuad identity.Quadruple) pauseresume.Token {
	t.Helper()
	select {
	case ev := <-sub.Events():
		payload, ok := ev.Payload.(pauseresume.PauseRequestedPayload)
		if !ok {
			t.Fatalf("pause.requested payload type = %T, want PauseRequestedPayload", ev.Payload)
		}
		if ev.Identity != wantQuad {
			t.Fatalf("pause.requested identity = %+v, want %+v (identity propagation)", ev.Identity, wantQuad)
		}
		return pauseresume.Token(payload.Token)
	case <-time.After(5 * time.Second):
		t.Fatal("no pause.requested event within 5s")
		return ""
	}
}

// TestE2E_Phase111c_RunLoopPause_CheckpointsTrajectory_RunContinues is
// the production-path half of the durability E2E: a real RunLoop
// drives a RequestPause-emitting planner with a live trajectory; the
// store-backed Coordinator checkpoints the trajectory (format_version
// 1, byte-stable round-trip, sane size); a NEW Coordinator over the
// SAME store sees the pause (restart-shaped rehydration); a wire-shaped
// APPROVE resumes the run, the planner re-enters and finishes; the
// destructive-Resume contract holds.
func TestE2E_Phase111c_RunLoopPause_CheckpointsTrajectory_RunContinues(t *testing.T) {
	red := patternsAudit.New()
	bus := phase111cBus(t, red)
	store := phase111cSQLite(t)
	coord := pauseresume.New(
		pauseresume.WithBus(bus),
		pauseresume.WithCheckpointStore(store),
	)
	reg := steering.NewRegistry()
	rl, err := steering.NewRunLoop(reg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}

	q := phase111cQuad("run-durable")
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: phase111cID.TenantID, User: phase111cID.UserID, Session: phase111cID.SessionID,
		Types: []events.EventType{pauseresume.EventTypePauseRequested},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	tr := &trajectory.Trajectory{
		Query:      "provision a database",
		LLMContext: map[string]any{"prior_summary": "user asked to provision a database"},
		ToolContext: trajectory.ToolContext{
			Serializable: map[string]any{"region": "eu-west-1"},
		},
	}
	wantBytes, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Trajectory.Serialize (fixture): %v", err)
	}

	p := &phase111cPlanner{}
	spec := steering.RunSpec{
		Planner:  p,
		Base:     planner.RunContext{Quadruple: q, Goal: "durable pause", Trajectory: tr},
		MaxSteps: 4,
	}
	done := make(chan struct {
		fin planner.Finish
		err error
	}, 1)
	go func() {
		fin, rerr := rl.Run(context.Background(), spec)
		done <- struct {
			fin planner.Finish
			err error
		}{fin, rerr}
	}()

	token := awaitPauseRequested(t, sub, q)

	// The checkpoint carries the serialized trajectory under
	// format_version 1, byte-stably.
	ctx := phase111cCtx(t, "run-durable")
	sr, err := store.LoadByEventID(ctx, state.EventID(token))
	if err != nil {
		t.Fatalf("LoadByEventID(checkpoint): %v — the production pause path did not checkpoint", err)
	}
	rec, err := pauseresume.DeserializeRecord(sr.Bytes)
	if err != nil {
		t.Fatalf("DeserializeRecord: %v", err)
	}
	if rec.FormatVersion != pauseresume.FormatVersion {
		t.Fatalf("checkpoint format_version = %d, want %d", rec.FormatVersion, pauseresume.FormatVersion)
	}
	if string(rec.TrajectoryBytes) != string(wantBytes) {
		t.Fatalf("checkpointed trajectory bytes diverge from Trajectory.Serialize output:\n got: %s\nwant: %s",
			rec.TrajectoryBytes, wantBytes)
	}
	if rec.Identity != phase111cID || rec.RunID != "run-durable" {
		t.Fatalf("checkpoint identity = %+v / %q, want %+v / run-durable", rec.Identity, rec.RunID, phase111cID)
	}
	// Sane upper bound for this fixture (plan §Risks — trajectory size
	// at pause time; 111e's compression bounds bigger ones).
	if len(sr.Bytes) > 64*1024 {
		t.Fatalf("checkpoint envelope = %d bytes, want < 64KiB for this fixture", len(sr.Bytes))
	}

	// Restart-shaped rehydration: a NEW Coordinator over the SAME store
	// sees the pause and the deserialized trajectory matches.
	restarted := pauseresume.New(pauseresume.WithCheckpointStore(store))
	st, err := restarted.Status(ctx, token)
	if err != nil {
		t.Fatalf("Status on restarted coordinator: %v", err)
	}
	if st.State != pauseresume.StatusPaused {
		t.Fatalf("restarted Status.State = %q, want paused", st.State)
	}
	restored, err := trajectory.Deserialize(rec.TrajectoryBytes)
	if err != nil {
		t.Fatalf("trajectory.Deserialize: %v", err)
	}
	if restored.Query != tr.Query {
		t.Fatalf("restored trajectory Query = %q, want %q", restored.Query, tr.Query)
	}

	// Wire-shaped APPROVE → the RunLoop drains it, Coordinator.Resume
	// fires, the planner re-enters, the run finishes.
	in, err := reg.Lookup(q)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if err := in.Enqueue(steering.ControlEvent{
		Type:         steering.ControlApprove,
		Identity:     q,
		CallerScope:  steering.ScopeOwnerUser,
		CallerTenant: q.TenantID,
	}); err != nil {
		t.Fatalf("Enqueue(APPROVE): %v", err)
	}
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("Run: %v", out.err)
		}
		if out.fin.Reason != planner.FinishGoal {
			t.Fatalf("Finish.Reason = %q, want %q (the resumed run must continue)", out.fin.Reason, planner.FinishGoal)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not finish within 5s of the APPROVE")
	}
	if got := p.steps(); got != 2 {
		t.Fatalf("planner steps = %d, want 2 (pause, then re-entry to Finish)", got)
	}

	// Destructive Resume: the checkpoint is gone — a fresh Coordinator
	// returns ErrPauseNotFound (asserted, not "fixed"; coordinator.go's
	// godoc contract).
	fresh := pauseresume.New(pauseresume.WithCheckpointStore(store))
	if _, err := fresh.Status(ctx, token); !errors.Is(err, pauseresume.ErrPauseNotFound) {
		t.Fatalf("post-resume fresh Status err = %v, want ErrPauseNotFound", err)
	}
}

// TestE2E_Phase111c_TrajectoryRoundTrip_AllStateDrivers asserts the
// checkpointed trajectory round-trips byte-stably on every V1
// StateStore driver (the store-agnostic save/load/delete parity the
// plan's conformance leg names). Reuses the Phase 50 driver cases —
// the postgres leg skips-with-reason when HARBOR_PG_DSN is unset.
func TestE2E_Phase111c_TrajectoryRoundTrip_AllStateDrivers(t *testing.T) {
	for _, tc := range phase50StoreCases() {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.open(t)
			ctx := phase111cCtx(t, "run-roundtrip")
			tr := &trajectory.Trajectory{
				Query:       "round trip",
				LLMContext:  map[string]any{"k": "v"},
				ToolContext: trajectory.ToolContext{Serializable: map[string]any{"n": float64(7)}},
			}
			wantBytes, err := tr.Serialize()
			if err != nil {
				t.Fatalf("Serialize: %v", err)
			}

			c1 := pauseresume.New(pauseresume.WithCheckpointStore(store))
			p, err := c1.Request(ctx, pauseresume.PauseRequest{
				Identity:   phase111cID,
				Reason:     pauseresume.ReasonExternalEvent,
				Trajectory: tr,
			})
			if err != nil {
				t.Fatalf("Request: %v", err)
			}

			sr, err := store.LoadByEventID(ctx, state.EventID(p.Token))
			if err != nil {
				t.Fatalf("LoadByEventID: %v", err)
			}
			rec, err := pauseresume.DeserializeRecord(sr.Bytes)
			if err != nil {
				t.Fatalf("DeserializeRecord: %v", err)
			}
			if string(rec.TrajectoryBytes) != string(wantBytes) {
				t.Fatalf("trajectory bytes did not round-trip on %s", tc.name)
			}

			// Restart-shaped resume + destructive contract per driver.
			c2 := pauseresume.New(pauseresume.WithCheckpointStore(store))
			if err := c2.Resume(ctx, p.Token, pauseresume.DecisionResume, nil); err != nil {
				t.Fatalf("Resume on restarted coordinator: %v", err)
			}
			c3 := pauseresume.New(pauseresume.WithCheckpointStore(store))
			if _, err := c3.Status(ctx, p.Token); !errors.Is(err, pauseresume.ErrPauseNotFound) {
				t.Fatalf("post-resume Status err = %v, want ErrPauseNotFound", err)
			}
		})
	}
}

// TestE2E_Phase111c_Timeout_RunTerminatesAndCheckpointReaped is the
// timeout E2E: the real RunSweeper over a store-backed Coordinator
// reaps an expired pause with `Decision: timeout` (DecisionTimeout's
// first producer, end-to-end), the waiting run terminates as a
// constraints-conflict (never silently continues), the `pause.resumed`
// event carries the typed marker under the run's quadruple, and the
// checkpoint is deleted.
func TestE2E_Phase111c_Timeout_RunTerminatesAndCheckpointReaped(t *testing.T) {
	red := patternsAudit.New()
	bus := phase111cBus(t, red)
	store := phase111cSQLite(t)
	coord := pauseresume.New(
		pauseresume.WithBus(bus),
		pauseresume.WithCheckpointStore(store),
		pauseresume.WithMaxParkDuration(50*time.Millisecond),
	)
	reg := steering.NewRegistry()
	rl, err := steering.NewRunLoop(reg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}

	sweepCtx, sweepCancel := context.WithCancel(context.Background())
	sweepDone := make(chan error, 1)
	go func() {
		sweepDone <- pauseresume.RunSweeper(sweepCtx, coord, pauseresume.WithSweepInterval(20*time.Millisecond))
	}()
	defer func() {
		sweepCancel()
		if err := <-sweepDone; !errors.Is(err, context.Canceled) {
			t.Errorf("RunSweeper exit err = %v, want context.Canceled", err)
		}
	}()

	q := phase111cQuad("run-timeout")
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: phase111cID.TenantID, User: phase111cID.UserID, Session: phase111cID.SessionID,
		Types: []events.EventType{pauseresume.EventTypePauseRequested, pauseresume.EventTypePauseResumed},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	p := &phase111cPlanner{reason: planner.PauseAwaitInput}
	done := make(chan struct {
		fin planner.Finish
		err error
	}, 1)
	go func() {
		fin, rerr := rl.Run(context.Background(), steering.RunSpec{
			Planner:  p,
			Base:     planner.RunContext{Quadruple: q, Goal: "park until timeout"},
			MaxSteps: 4,
		})
		done <- struct {
			fin planner.Finish
			err error
		}{fin, rerr}
	}()

	token := awaitPauseRequested(t, sub, q)

	// The run terminates as a constraints-conflict — never a silent
	// unpark-and-continue (the planner is not re-entered).
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("Run: %v", out.err)
		}
		if out.fin.Reason != planner.FinishConstraintsConflict {
			t.Fatalf("Finish.Reason = %q, want %q", out.fin.Reason, planner.FinishConstraintsConflict)
		}
		if out.fin.Metadata["steering_reason"] != "pause_timeout" {
			t.Fatalf("Finish.Metadata[steering_reason] = %v, want pause_timeout", out.fin.Metadata["steering_reason"])
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not terminate within 10s — the sweeper/timeout path is dead")
	}
	if got := p.steps(); got != 1 {
		t.Fatalf("planner steps = %d, want 1 (a timed-out pause must not re-enter the planner)", got)
	}

	// The pause.resumed event carries the typed timeout marker under
	// the run's quadruple.
	deadline := time.After(5 * time.Second)
	for {
		var ev events.Event
		select {
		case ev = <-sub.Events():
		case <-deadline:
			t.Fatal("no pause.resumed(timeout) event within 5s")
		}
		payload, ok := ev.Payload.(pauseresume.PauseResumedPayload)
		if !ok {
			continue
		}
		if payload.Token != string(token) {
			continue
		}
		if payload.Decision != pauseresume.DecisionTimeout {
			t.Fatalf("pause.resumed decision = %q, want %q", payload.Decision, pauseresume.DecisionTimeout)
		}
		if ev.Identity != q {
			t.Fatalf("pause.resumed identity = %+v, want %+v", ev.Identity, q)
		}
		break
	}

	// No orphan: the checkpoint is deleted.
	ctx := phase111cCtx(t, "run-timeout")
	fresh := pauseresume.New(pauseresume.WithCheckpointStore(store))
	if _, err := fresh.Status(ctx, token); !errors.Is(err, pauseresume.ErrPauseNotFound) {
		t.Fatalf("post-reap fresh Status err = %v, want ErrPauseNotFound (orphan checkpoint)", err)
	}
}

// TestE2E_Phase111c_CancelWhilePaused_SweeperReapsOrphan closes the
// audit's "cancel-while-paused orphans records forever" finding: the
// cancelled run leaves its pause record + checkpoint behind (asserted
// — the pre-111c leak), and the sweeper reaps them at deadline.
func TestE2E_Phase111c_CancelWhilePaused_SweeperReapsOrphan(t *testing.T) {
	red := patternsAudit.New()
	bus := phase111cBus(t, red)
	store := phase111cSQLite(t)
	coord := pauseresume.New(
		pauseresume.WithBus(bus),
		pauseresume.WithCheckpointStore(store),
		pauseresume.WithMaxParkDuration(50*time.Millisecond),
	)
	reg := steering.NewRegistry()
	rl, err := steering.NewRunLoop(reg, coord) // bus-less RunLoop: CANCEL must still terminate it
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}

	q := phase111cQuad("run-cancel")
	ctx := phase111cCtx(t, "run-cancel")
	p := &phase111cPlanner{}
	done := make(chan struct {
		fin planner.Finish
		err error
	}, 1)
	go func() {
		fin, rerr := rl.Run(context.Background(), steering.RunSpec{
			Planner:  p,
			Base:     planner.RunContext{Quadruple: q, Goal: "cancel while paused"},
			MaxSteps: 4,
		})
		done <- struct {
			fin planner.Finish
			err error
		}{fin, rerr}
	}()

	// Bounded wait for the pause record.
	var token pauseresume.Token
	waitDeadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(waitDeadline) {
		resp, lerr := coord.List(ctx, pauseresume.ListRequest{Identity: phase111cID})
		if lerr != nil {
			t.Fatalf("List: %v", lerr)
		}
		if len(resp.Snapshots) == 1 {
			token = resp.Snapshots[0].Token
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if token == "" {
		t.Fatal("pause record did not appear within 5s")
	}

	in, err := reg.Lookup(q)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if err := in.Enqueue(steering.ControlEvent{
		Type:         steering.ControlCancel,
		Identity:     q,
		CallerScope:  steering.ScopeOwnerUser,
		CallerTenant: q.TenantID,
	}); err != nil {
		t.Fatalf("Enqueue(CANCEL): %v", err)
	}
	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("Run: %v", out.err)
		}
		if out.fin.Reason != planner.FinishCancelled {
			t.Fatalf("Finish.Reason = %q, want %q", out.fin.Reason, planner.FinishCancelled)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not terminate within 5s of the CANCEL")
	}

	// The orphan: record still paused, checkpoint still in the store.
	st, err := coord.Status(ctx, token)
	if err != nil {
		t.Fatalf("Status(orphan): %v", err)
	}
	if st.State != pauseresume.StatusPaused {
		t.Fatalf("orphan state = %q, want paused (the leak this phase backstops)", st.State)
	}
	if _, err := store.LoadByEventID(ctx, state.EventID(token)); err != nil {
		t.Fatalf("orphan checkpoint missing before the sweep: %v", err)
	}

	// The sweeper is the backstop: start it AFTER the orphan is
	// asserted; it reaps at deadline.
	sweepCtx, sweepCancel := context.WithCancel(context.Background())
	sweepDone := make(chan error, 1)
	go func() {
		sweepDone <- pauseresume.RunSweeper(sweepCtx, coord, pauseresume.WithSweepInterval(20*time.Millisecond))
	}()
	defer func() {
		sweepCancel()
		<-sweepDone
	}()

	reapDeadline := time.Now().Add(5 * time.Second)
	for {
		st, err := coord.Status(ctx, token)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if st.State == pauseresume.StatusResumed {
			if st.Decision != pauseresume.DecisionTimeout {
				t.Fatalf("orphan reaped with Decision %q, want %q", st.Decision, pauseresume.DecisionTimeout)
			}
			break
		}
		if time.Now().After(reapDeadline) {
			t.Fatal("orphaned pause was not reaped within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	fresh := pauseresume.New(pauseresume.WithCheckpointStore(store))
	if _, err := fresh.Status(ctx, token); !errors.Is(err, pauseresume.ErrPauseNotFound) {
		t.Fatalf("orphan checkpoint survived the reap: err = %v, want ErrPauseNotFound", err)
	}
}

// TestE2E_Phase111c_UnserializableTrajectory_FailsLoudAtRequest is the
// §11 mandatory fail-loud test on the PRODUCTION path: the run's live
// trajectory carries a non-JSON-encodable leaf; the planner's
// RequestPause reaches Coordinator.Request through the RunLoop and the
// run fails loud with trajectory.ErrUnserializable — no silent nil, no
// silently-empty checkpoint, no pause recorded.
func TestE2E_Phase111c_UnserializableTrajectory_FailsLoudAtRequest(t *testing.T) {
	store := phase111cSQLite(t)
	coord := pauseresume.New(pauseresume.WithCheckpointStore(store))
	reg := steering.NewRegistry()
	rl, err := steering.NewRunLoop(reg, coord)
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}

	q := phase111cQuad("run-unserializable")
	poisoned := &trajectory.Trajectory{
		Query:      "poisoned",
		LLMContext: map[string]any{"oops": make(chan int)}, // non-JSON-encodable leaf
	}
	_, runErr := rl.Run(context.Background(), steering.RunSpec{
		Planner:  &phase111cPlanner{},
		Base:     planner.RunContext{Quadruple: q, Goal: "fail loud", Trajectory: poisoned},
		MaxSteps: 4,
	})
	var unser trajectory.ErrUnserializable
	if !errors.As(runErr, &unser) {
		t.Fatalf("Run err = %v, want trajectory.ErrUnserializable surfaced verbatim", runErr)
	}

	// Nothing was half-persisted: no pause record, no checkpoint.
	ctx := phase111cCtx(t, "run-unserializable")
	resp, err := coord.List(ctx, pauseresume.ListRequest{Identity: phase111cID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if resp.TotalRows != 0 {
		t.Fatalf("pause records after a rejected Request = %d, want 0 (half-persist)", resp.TotalRows)
	}
}

// TestE2E_Phase111c_LostHandle_FailsLoudOnRestartResume re-asserts the
// non-goal's loud edge under the 111c wiring: the serializable half
// survives the restart, the non-serialisable half does not — a resume
// needing a lost handle fails with ErrToolContextLost, never a nil
// tool context.
func TestE2E_Phase111c_LostHandle_FailsLoudOnRestartResume(t *testing.T) {
	store := phase111cSQLite(t)
	ctx := phase111cCtx(t, "run-lost-handle")

	liveReg := trajectory.NewProcessLocalRegistry()
	const handleID trajectory.HandleID = "handle-111c"
	liveReg.Set(handleID, "a-live-conn")
	c1 := pauseresume.New(
		pauseresume.WithCheckpointStore(store),
		pauseresume.WithHandleRegistry(liveReg),
		pauseresume.WithMaxParkDuration(time.Hour),
	)
	p, err := c1.Request(ctx, pauseresume.PauseRequest{
		Identity: phase111cID,
		Reason:   pauseresume.ReasonExternalEvent,
		Trajectory: &trajectory.Trajectory{
			ToolContext: trajectory.ToolContext{Handles: []trajectory.HandleID{handleID}},
		},
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}

	// Restarted Runtime: fresh registry — the handle is lost.
	c2 := pauseresume.New(
		pauseresume.WithCheckpointStore(store),
		pauseresume.WithHandleRegistry(trajectory.NewProcessLocalRegistry()),
	)
	err = c2.Resume(ctx, p.Token, pauseresume.DecisionResume, nil)
	var lost trajectory.ErrToolContextLost
	if !errors.As(err, &lost) {
		t.Fatalf("Resume err = %v, want trajectory.ErrToolContextLost", err)
	}
}

// TestE2E_Phase111c_ConcurrentRequestResume_StoreBacked extends the
// D-025 concurrent-reuse suite to the store-backed Coordinator shape:
// N=100 concurrent Request→Resume round-trips against ONE shared
// store-backed Coordinator under -race, across distinct sessions (no
// cross-talk: every goroutine resolves exactly its own token), with
// every checkpoint cleared afterwards.
func TestE2E_Phase111c_ConcurrentRequestResume_StoreBacked(t *testing.T) {
	store := phase111cSQLite(t)
	red := patternsAudit.New()
	bus := phase111cBus(t, red)
	coord := pauseresume.New(
		pauseresume.WithBus(bus),
		pauseresume.WithCheckpointStore(store),
		pauseresume.WithMaxParkDuration(time.Hour), // expiry stamped but never reached
	)

	const n = 100
	tokens := make([]pauseresume.Token, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  phase111cID.TenantID,
				UserID:    phase111cID.UserID,
				SessionID: fmt.Sprintf("session-conc-%03d", i),
			}
			ctx, err := identity.WithRun(context.Background(), id, fmt.Sprintf("run-conc-%03d", i))
			if err != nil {
				errs[i] = err
				return
			}
			p, err := coord.Request(ctx, pauseresume.PauseRequest{
				Identity:   id,
				Reason:     pauseresume.ReasonAwaitInput,
				Payload:    map[string]any{"i": i},
				Trajectory: &trajectory.Trajectory{Query: fmt.Sprintf("concurrent run %d", i)},
			})
			if err != nil {
				errs[i] = err
				return
			}
			tokens[i] = p.Token
			errs[i] = coord.Resume(ctx, p.Token, pauseresume.DecisionResume, nil)
		}()
	}
	wg.Wait()

	seen := make(map[pauseresume.Token]struct{}, n)
	fresh := pauseresume.New(pauseresume.WithCheckpointStore(store))
	for i := range n {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if _, dup := seen[tokens[i]]; dup {
			t.Fatalf("token %q minted twice (cross-talk)", tokens[i])
		}
		seen[tokens[i]] = struct{}{}
		// Destructive resume held for every token under contention.
		ctx := phase111cCtx(t, "run-verify")
		if _, err := fresh.Status(ctx, tokens[i]); !errors.Is(err, pauseresume.ErrPauseNotFound) {
			t.Fatalf("token %d: checkpoint survived its resume (err=%v)", i, err)
		}
	}
}
