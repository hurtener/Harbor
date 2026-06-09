package sessions_test

import (
	"context"
	"errors"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// mustStateStore opens a fresh in-memory StateStore for the task
// registry side of the probe tests — real drivers at the seam per
// §17.3, no mocks.
func mustStateStore(t *testing.T) state.StateStore {
	t.Helper()
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

// TestTaskRunningProbe_ReportsRunningTask exercises the adapter
// directly: a session with a task in StatusRunning probes true; once
// the task completes, the probe flips to false. PENDING tasks do not
// count as running (RFC §6.9 names RUNNING specifically).
func TestTaskRunningProbe_ReportsRunningTask(t *testing.T) {
	t.Parallel()
	_, bus := testWiring(t)
	store := mustStateStore(t)
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: auditpatterns.New(),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	t.Cleanup(func() { _ = taskReg.Close(context.Background()) })

	id := ident("t1", "u1", "s-probe")
	q := identity.Quadruple{Identity: id}
	ctx := ctxFor(id)
	probe := sessions.TaskRunningProbe(taskReg)

	// No tasks at all → not running.
	running, err := probe(ctx, q)
	if err != nil {
		t.Fatalf("probe (no tasks): %v", err)
	}
	if running {
		t.Fatal("probe = true with no tasks, want false")
	}

	handle, err := taskReg.Spawn(ctx, tasks.SpawnRequest{
		Identity: q,
		Kind:     tasks.KindForeground,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	// PENDING task → still not running.
	running, err = probe(ctx, q)
	if err != nil {
		t.Fatalf("probe (pending): %v", err)
	}
	if running {
		t.Fatal("probe = true with only a PENDING task, want false")
	}

	if err := taskReg.MarkRunning(ctx, handle.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	running, err = probe(ctx, q)
	if err != nil {
		t.Fatalf("probe (running): %v", err)
	}
	if !running {
		t.Fatal("probe = false with a RUNNING task, want true")
	}

	if err := taskReg.MarkComplete(ctx, handle.ID, tasks.TaskResult{}); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
	running, err = probe(ctx, q)
	if err != nil {
		t.Fatalf("probe (complete): %v", err)
	}
	if running {
		t.Fatal("probe = true after task completed, want false")
	}
}

// TestTaskRunningProbe_NilRegistryFailsLoud pins the fail-loud
// contract (§13 — no silent degradation): a probe built over a nil
// registry errors instead of silently reporting "not running."
func TestTaskRunningProbe_NilRegistryFailsLoud(t *testing.T) {
	t.Parallel()
	probe := sessions.TaskRunningProbe(nil)
	id := ident("t1", "u1", "s-nil")
	_, err := probe(ctxFor(id), identity.Quadruple{Identity: id})
	if err == nil {
		t.Fatal("probe over nil registry returned nil error, want fail-loud error")
	}
}

// TestGC_RunningSessionSurvives_IdleSessionReaped is the end-to-end
// wiring test for the RFC §6.9 invariant across the sessions↔tasks
// seam with real drivers: a session whose task is RUNNING survives a
// GC pass that simultaneously reaps an idle-expired sibling. Time is
// driven by the controllable clock (§17.4 — no time.Sleep).
func TestGC_RunningSessionSurvives_IdleSessionReaped(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Now())

	// Shared bus + state via the standard wiring; the task registry
	// uses its own state store keyspace (Kind = task.lifecycle), so
	// sharing the store is exactly what production does.
	reg, bus := testWiring(t, sessions.WithClock(clock))
	store := mustStateStore(t)
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: auditpatterns.New(),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	t.Cleanup(func() { _ = taskReg.Close(context.Background()) })

	busyID := ident("t1", "u1", "s-busy")
	idleID := ident("t1", "u1", "s-idle")
	if _, err := reg.Open(ctxFor(busyID), busyID.SessionID, busyID); err != nil {
		t.Fatalf("Open busy: %v", err)
	}
	if _, err := reg.Open(ctxFor(idleID), idleID.SessionID, idleID); err != nil {
		t.Fatalf("Open idle: %v", err)
	}

	// Put a RUNNING task in the busy session.
	handle, err := taskReg.Spawn(ctxFor(busyID), tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: busyID},
		Kind:     tasks.KindForeground,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := taskReg.MarkRunning(ctxFor(busyID), handle.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}

	// Advance past the IdleTTL so BOTH sessions are idle-expired by
	// wall-clock math; only the probe should save the busy one.
	policy := sessions.GCPolicy{
		IdleTTL:       time.Hour,
		HardCap:       720 * time.Hour,
		SweepInterval: time.Hour,
		RunningProbe:  sessions.TaskRunningProbe(taskReg),
	}
	clock.Advance(2 * time.Hour)

	reaped, err := reg.GC(context.Background(), policy)
	if err != nil {
		t.Fatalf("GC: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("GC reaped = %d, want 1 (idle only)", reaped)
	}

	// Busy session is still open.
	busy, err := reg.Get(ctxFor(busyID), busyID.SessionID)
	if err != nil {
		t.Fatalf("Get busy: %v", err)
	}
	if busy.Closed {
		t.Fatalf("busy session was reaped despite RUNNING task (ClosedReason=%q)", busy.ClosedReason)
	}

	// Idle session was reaped with the idle reason.
	idle, err := reg.Get(ctxFor(idleID), idleID.SessionID)
	if err != nil {
		t.Fatalf("Get idle: %v", err)
	}
	if !idle.Closed || idle.ClosedReason != "gc:idle" {
		t.Fatalf("idle session: Closed=%v ClosedReason=%q, want Closed=true reason=gc:idle", idle.Closed, idle.ClosedReason)
	}

	// Once the task completes, the next sweep reaps the (still idle)
	// busy session too — the probe is consulted per pass, not cached.
	if err := taskReg.MarkComplete(ctxFor(busyID), handle.ID, tasks.TaskResult{}); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
	reaped, err = reg.GC(context.Background(), policy)
	if err != nil {
		t.Fatalf("GC #2: %v", err)
	}
	if reaped != 1 {
		t.Fatalf("GC #2 reaped = %d, want 1", reaped)
	}
	busy, err = reg.Get(ctxFor(busyID), busyID.SessionID)
	if err != nil {
		t.Fatalf("Get busy after #2: %v", err)
	}
	if !busy.Closed {
		t.Fatal("busy session still open after its RUNNING task completed and a second GC pass ran")
	}
}

// TestGC_ProbeErrorSurfaces asserts probe errors propagate as GC
// errors (the sweep continues but reports firstErr) rather than being
// swallowed — and that an erroring probe fails closed (no reap).
func TestGC_ProbeErrorSurfaces(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(time.Now())
	reg, _ := testWiring(t, sessions.WithClock(clock))
	id := ident("t1", "u1", "s-err")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	probeErr := errors.New("probe boom")
	policy := sessions.GCPolicy{
		IdleTTL:       time.Hour,
		HardCap:       720 * time.Hour,
		SweepInterval: time.Hour,
		RunningProbe: func(context.Context, identity.Quadruple) (bool, error) {
			return false, probeErr
		},
	}
	clock.Advance(2 * time.Hour)
	reaped, err := reg.GC(context.Background(), policy)
	if !errors.Is(err, probeErr) {
		t.Fatalf("GC err = %v, want wrapped probe error", err)
	}
	if reaped != 0 {
		t.Fatalf("GC reaped = %d on probe error, want 0 (fail-closed)", reaped)
	}
}
