package serve

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/sessions"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
)

// activityClock is a controllable sessions.Clock.
type activityClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *activityClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *activityClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// recordingActivity forwards to an inner marker (or fails) and records every
// identity it was asked to mark.
type recordingActivity struct {
	inner SessionActivityMarker
	fail  bool
	done  chan struct{} // one send per completed mark
	mu    sync.Mutex
	calls []identity.Identity
}

func (r *recordingActivity) MarkRunActivity(ctx context.Context, ident identity.Identity) error {
	r.mu.Lock()
	r.calls = append(r.calls, ident)
	r.mu.Unlock()
	err := errors.New("activity store unavailable")
	if !r.fail {
		err = r.inner.MarkRunActivity(ctx, ident)
	}
	r.done <- struct{}{}
	return err
}

func (r *recordingActivity) snapshot() []identity.Identity {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]identity.Identity(nil), r.calls...)
}

// activityPlanner reports the session's LastSeen as observed mid-run (after
// the start mark), advances the clock so the finish mark lands past the
// coalesce window, then finishes with `reason` or fails with `err`.
type activityPlanner struct {
	reg     *sessions.Registry
	owner   identity.Identity
	clock   *activityClock
	advance time.Duration
	reason  planner.FinishReason
	err     error
	midRun  chan time.Time
	// marked receives once per applied mark; Next waits for the start mark
	// (delivered asynchronously) before reading LastSeen.
	marked chan struct{}
}

func (p *activityPlanner) Next(ctx context.Context, _ planner.RunContext) (planner.Decision, error) {
	select {
	case <-p.marked:
	case <-time.After(3 * time.Second):
		return nil, errors.New("start mark never applied")
	}
	ownerCtx, _ := identity.With(context.Background(), p.owner)
	var seen time.Time
	if s, err := p.reg.Get(ownerCtx, p.owner.SessionID); err == nil {
		seen = s.LastSeen
	}
	select {
	case p.midRun <- seen:
	default:
	}
	p.clock.Advance(p.advance)
	if p.err != nil {
		return nil, p.err
	}
	return planner.Finish{Reason: p.reason}, nil
}

// The run lifecycle stamps the run's session LastSeen when the run starts and
// again when it reaches a terminal state — completed or failed — keyed by the
// run's storage partition, so a participant's turn in the owner's session
// refreshes the owner's session. A failing mark never fails the run.
func TestRunLoopDriver_SessionActivity_StampsStartAndFinish(t *testing.T) {
	for _, tc := range []struct {
		name        string
		participant *identity.Participant
		reason      planner.FinishReason
		planErr     error
		failMark    bool
		wantStatus  tasks.TaskStatus
	}{
		{name: "completed", reason: planner.FinishGoal, wantStatus: tasks.StatusComplete},
		{name: "participant", participant: &identity.Participant{UserID: "writer-b", Speaker: "Bruno"}, reason: planner.FinishGoal, wantStatus: tasks.StatusComplete},
		{name: "non_goal_finish", reason: planner.FinishNoPath, wantStatus: tasks.StatusFailed},
		{name: "planner_error", planErr: errors.New("boom"), wantStatus: tasks.StatusFailed},
		{name: "mark_failure_does_not_fail_run", reason: planner.FinishGoal, failMark: true, wantStatus: tasks.StatusComplete},
	} {
		t.Run(tc.name, func(t *testing.T) {
			red := auditpatterns.New()
			bus := mkDriverTestBus(t, red)
			taskReg := mkDriverTestTaskRegistry(t, bus, red)

			st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
			if err != nil {
				t.Fatalf("state.Open: %v", err)
			}
			t.Cleanup(func() { _ = st.Close(context.Background()) })
			t0 := time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC)
			clock := &activityClock{now: t0}
			sessReg, err := sessions.New(st, config.SessionsConfig{
				IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: time.Hour,
			}, bus, sessions.WithClock(clock))
			if err != nil {
				t.Fatalf("sessions.New: %v", err)
			}
			t.Cleanup(func() { _ = sessReg.CloseRegistry(context.Background()) })
			owner := runLoopDriverTestID
			if _, err := sessReg.Open(context.Background(), owner.SessionID, owner); err != nil {
				t.Fatalf("Open: %v", err)
			}
			// A second session of the same user, used more recently than the
			// owner's until the run: it lists first before the run, second after.
			clock.Advance(time.Minute)
			other := identity.Identity{TenantID: owner.TenantID, UserID: owner.UserID, SessionID: "session-other"}
			if _, err := sessReg.Open(context.Background(), other.SessionID, other); err != nil {
				t.Fatalf("Open other: %v", err)
			}
			listOrder := func() []string {
				t.Helper()
				projector, err := sessionsprotocol.NewListerProjector(sessReg)
				if err != nil {
					t.Fatalf("NewListerProjector: %v", err)
				}
				svc, err := sessionsprotocol.NewService(projector)
				if err != nil {
					t.Fatalf("NewService: %v", err)
				}
				resp, err := svc.List(context.Background(), prototypes.SessionsListRequest{
					Identity: prototypes.IdentityScope{Tenant: owner.TenantID, User: owner.UserID, Session: owner.SessionID},
					Sort:     prototypes.SessionSortLastActivityDesc,
				}, false)
				if err != nil {
					t.Fatalf("sessions List: %v", err)
				}
				ids := make([]string, 0, len(resp.Rows))
				for _, r := range resp.Rows {
					ids = append(ids, r.SessionID)
				}
				return ids
			}
			if got := listOrder(); len(got) != 2 || got[0] != other.SessionID {
				t.Fatalf("list order before run = %v, want %s first", got, other.SessionID)
			}

			marker := &recordingActivity{inner: sessReg, fail: tc.failMark, done: make(chan struct{}, 2)}
			p := &activityPlanner{reg: sessReg, owner: owner, clock: clock, advance: 10 * time.Minute,
				reason: tc.reason, err: tc.planErr, midRun: make(chan time.Time, 1), marked: marker.done}
			coord := pauseresume.New(pauseresume.WithBus(bus))
			rl, err := steering.NewRunLoop(steering.NewRegistry(), coord, steering.WithRunLoopBus(bus))
			if err != nil {
				t.Fatal(err)
			}
			driver, err := NewRunLoopDriver(RunLoopDriverOptions{SessionMemory: config.MemoryConfig{Strategy: "none"},
				Bus: bus, RunLoop: rl, Planner: p, Tasks: taskReg, AgentConfigID: "agent-activity",
				SessionActivity: marker})
			if err != nil {
				t.Fatal(err)
			}
			// The registry's fake clock drives the timeline; the queue's
			// wall-clock coalescing would merge the start and finish marks.
			driver.activity.coalesce = 0
			if err := driver.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = driver.Close(context.Background()) }()

			clock.Advance(9 * time.Minute) // the run starts at t0+10m
			spawnCtx, _ := identity.With(context.Background(), owner)
			h, err := taskReg.Spawn(spawnCtx, tasks.SpawnRequest{Identity: identity.Quadruple{Identity: owner},
				Kind: tasks.KindForeground, Query: "activity turn", Participant: tc.participant})
			if err != nil {
				t.Fatal(err)
			}

			var midRun time.Time
			select {
			case midRun = <-p.midRun:
			case <-time.After(3 * time.Second):
				t.Fatal("planner never ran")
			}
			if got := waitForTaskStatus(t, taskReg, h.ID, tc.wantStatus, 3*time.Second); got != tc.wantStatus {
				t.Fatalf("task status = %q, want %q", got, tc.wantStatus)
			}
			select { // the finish mark (the planner consumed the start mark)
			case <-marker.done:
			case <-time.After(3 * time.Second):
				t.Fatal("finish activity mark never completed")
			}
			calls := marker.snapshot()
			if len(calls) != 2 {
				t.Fatalf("activity marks = %d, want 2 (start + finish)", len(calls))
			}
			for _, c := range calls {
				if c != owner {
					t.Fatalf("activity marked %+v, want the session partition %+v", c, owner)
				}
			}

			s, err := sessReg.Get(spawnCtx, owner.SessionID)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if tc.failMark {
				if !s.LastSeen.Equal(t0) {
					t.Fatalf("LastSeen = %v, want unchanged %v when marks fail", s.LastSeen, t0)
				}
				return
			}
			if got := listOrder(); len(got) != 2 || got[0] != owner.SessionID {
				t.Fatalf("list order after run = %v, want %s first", got, owner.SessionID)
			}
			if want := t0.Add(10 * time.Minute); !midRun.Equal(want) {
				t.Fatalf("LastSeen mid-run = %v, want run start %v", midRun, want)
			}
			if want := t0.Add(20 * time.Minute); !s.LastSeen.Equal(want) {
				t.Fatalf("LastSeen after run = %v, want run finish %v", s.LastSeen, want)
			}
			if tc.participant != nil {
				writer := identity.Identity{TenantID: owner.TenantID, UserID: tc.participant.UserID, SessionID: owner.SessionID}
				writerCtx, _ := identity.With(context.Background(), writer)
				if _, err := sessReg.Get(writerCtx, writer.SessionID); !errors.Is(err, sessions.ErrSessionNotFound) {
					t.Fatalf("writer triple Get err = %v, want ErrSessionNotFound", err)
				}
			}
		})
	}
}

// blockingMarker never returns until released, ignoring its context — a
// store or registry lock that hangs.
type blockingMarker struct {
	calls   chan identity.Identity
	release chan struct{}
}

func (b *blockingMarker) MarkRunActivity(_ context.Context, ident identity.Identity) error {
	b.calls <- ident
	<-b.release
	return nil
}

// A hung session store never blocks the run path (enqueue never waits, a
// full queue drops) and shutdown waits for it only up to the drain bound.
func TestSessionActivityQueue_HungStoreNeverBlocksRunsOrShutdown(t *testing.T) {
	m := &blockingMarker{calls: make(chan identity.Identity, 1), release: make(chan struct{})}
	t.Cleanup(func() { close(m.release) })
	q := newSessionActivityQueue(m, slog.New(slog.NewTextHandler(io.Discard, nil)))
	q.drainTimeout = 50 * time.Millisecond
	q.start()

	first := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s-0"}
	q.enqueue(first)
	select {
	case <-m.calls: // the worker is now stuck inside the store call
	case <-time.After(3 * time.Second):
		t.Fatal("worker never picked up the first mark")
	}
	enqueued := make(chan struct{})
	go func() {
		defer close(enqueued)
		for i := range 4 * sessionActivityQueueSize {
			q.enqueue(identity.Identity{TenantID: "t", UserID: "u", SessionID: fmt.Sprintf("s-%d", i)})
		}
	}()
	select {
	case <-enqueued:
	case <-time.After(3 * time.Second):
		t.Fatal("enqueue blocked behind a hung store")
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		q.stop()
	}()
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown stalled on a hung store")
	}
}

// Marks for one session within the coalesce window reach the registry once;
// other sessions are unaffected.
func TestSessionActivityQueue_CoalescesBeforeTheRegistry(t *testing.T) {
	marker := &recordingActivity{inner: noopMarker{}, done: make(chan struct{}, 8)}
	q := newSessionActivityQueue(marker, slog.New(slog.NewTextHandler(io.Discard, nil)))
	q.coalesce = time.Hour
	q.start()
	a := identity.Identity{TenantID: "t", UserID: "u", SessionID: "a"}
	b := identity.Identity{TenantID: "t", UserID: "u", SessionID: "b"}
	q.enqueue(a)
	q.enqueue(a)
	q.enqueue(b)
	q.enqueue(a)
	q.stop()
	calls := marker.snapshot()
	if len(calls) != 2 || calls[0] != a || calls[1] != b {
		t.Fatalf("registry marks = %+v, want one for a then one for b", calls)
	}
}

type noopMarker struct{}

func (noopMarker) MarkRunActivity(context.Context, identity.Identity) error { return nil }
