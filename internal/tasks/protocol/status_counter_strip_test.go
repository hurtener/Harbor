package protocol_test

import (
	"context"
	"sync"
	"testing"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestList_StatusCounterStrip_OptInProjection asserts the Phase 73b
// (D-126) status-counter-strip aggregate is nil unless the request opts
// in, and carries the five canonical counts over the FULL identity-
// scoped task set (not the filtered view) when it does.
func TestList_StatusCounterStrip_OptInProjection(t *testing.T) {
	svc, reg, _ := newListService(t)
	id := idFor("t1", "u1", "s1")

	// Seed a known mix: 2 running, 1 pending, 1 paused, 1 complete,
	// 1 failed, 1 cancelled.
	seedTask(t, reg, id, tasks.KindForeground, tasks.StatusRunning, "r1", "q")
	seedTask(t, reg, id, tasks.KindForeground, tasks.StatusRunning, "r2", "q")
	seedTask(t, reg, id, tasks.KindBackground, tasks.StatusPending, "p1", "q")
	seedTask(t, reg, id, tasks.KindForeground, tasks.StatusPaused, "pa1", "q")
	seedTask(t, reg, id, tasks.KindBackground, tasks.StatusComplete, "c1", "q")
	seedTask(t, reg, id, tasks.KindForeground, tasks.StatusFailed, "f1", "q")
	seedTask(t, reg, id, tasks.KindBackground, tasks.StatusCancelled, "x1", "q")

	ctx := context.Background()
	scope := scopeOf("t1", "u1", "s1")

	t.Run("not opted in → strip is nil", func(t *testing.T) {
		resp, err := svc.List(ctx, prototypes.TaskListRequest{Identity: scope}, false)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if resp.StatusCounterStrip != nil {
			t.Fatalf("StatusCounterStrip = %+v, want nil when IncludeStatusCounterStrip is false", resp.StatusCounterStrip)
		}
	})

	t.Run("opted in → five counts over the full scoped set", func(t *testing.T) {
		resp, err := svc.List(ctx, prototypes.TaskListRequest{
			Identity:                  scope,
			IncludeStatusCounterStrip: true,
		}, false)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		s := resp.StatusCounterStrip
		if s == nil {
			t.Fatal("StatusCounterStrip is nil, want non-nil when opted in")
		}
		if s.Pending != 1 || s.Running != 2 || s.Completed != 1 || s.Paused != 1 || s.Failed != 1 {
			t.Fatalf("strip = %+v, want {Pending:1 Running:2 Completed:1 Paused:1 Failed:1}", *s)
		}
	})

	t.Run("strip ignores the facet filter (full-set posture)", func(t *testing.T) {
		// A filter that narrows rows to Running only must NOT narrow the
		// strip — the Live Runtime header strip is session-wide posture.
		resp, err := svc.List(ctx, prototypes.TaskListRequest{
			Identity:                  scope,
			Filter:                    prototypes.TaskFilter{Statuses: []prototypes.TaskStatus{prototypes.TaskStatusRunning}},
			IncludeStatusCounterStrip: true,
		}, false)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		// Rows narrowed to 2 running; the filtered Aggregates reflects
		// that; the strip stays full-set.
		if len(resp.Rows) != 2 {
			t.Fatalf("rows = %d, want 2 (filtered to running)", len(resp.Rows))
		}
		s := resp.StatusCounterStrip
		if s == nil || s.Pending != 1 || s.Running != 2 || s.Completed != 1 || s.Paused != 1 || s.Failed != 1 {
			t.Fatalf("strip = %+v, want full-set counts despite the running-only filter", s)
		}
	})
}

// TestList_StatusCounterStrip_IdentityScoped asserts the strip is
// identity-scoped (CLAUDE.md §6 rule 2): a second session never sees
// the first session's counts. The counter never crosses the isolation
// boundary.
func TestList_StatusCounterStrip_IdentityScoped(t *testing.T) {
	svc, reg, _ := newListService(t)

	idA := idFor("t1", "u1", "sA")
	idB := idFor("t1", "u1", "sB")

	// Session A: 3 running tasks. Session B: 1 failed task.
	seedTask(t, reg, idA, tasks.KindForeground, tasks.StatusRunning, "a1", "q")
	seedTask(t, reg, idA, tasks.KindForeground, tasks.StatusRunning, "a2", "q")
	seedTask(t, reg, idA, tasks.KindForeground, tasks.StatusRunning, "a3", "q")
	seedTask(t, reg, idB, tasks.KindForeground, tasks.StatusFailed, "b1", "q")

	ctx := context.Background()

	respA, err := svc.List(ctx, prototypes.TaskListRequest{
		Identity:                  scopeOf("t1", "u1", "sA"),
		IncludeStatusCounterStrip: true,
	}, false)
	if err != nil {
		t.Fatalf("List(A): %v", err)
	}
	if respA.StatusCounterStrip == nil || respA.StatusCounterStrip.Running != 3 {
		t.Fatalf("session A strip = %+v, want Running:3", respA.StatusCounterStrip)
	}
	if respA.StatusCounterStrip.Failed != 0 {
		t.Fatalf("session A strip Failed = %d, want 0 — session B's failed task must not leak",
			respA.StatusCounterStrip.Failed)
	}

	respB, err := svc.List(ctx, prototypes.TaskListRequest{
		Identity:                  scopeOf("t1", "u1", "sB"),
		IncludeStatusCounterStrip: true,
	}, false)
	if err != nil {
		t.Fatalf("List(B): %v", err)
	}
	if respB.StatusCounterStrip == nil || respB.StatusCounterStrip.Failed != 1 {
		t.Fatalf("session B strip = %+v, want Failed:1", respB.StatusCounterStrip)
	}
	if respB.StatusCounterStrip.Running != 0 {
		t.Fatalf("session B strip Running = %d, want 0 — session A's running tasks must not leak",
			respB.StatusCounterStrip.Running)
	}
}

// TestList_StatusCounterStrip_ConcurrentReuse exercises the
// status-counter-strip aggregate path under N≥100 concurrent callers
// against ONE shared *Service (D-025). The race detector is the gate;
// every call asserts its own session's counts so context bleed is
// caught.
func TestList_StatusCounterStrip_ConcurrentReuse(t *testing.T) {
	svc, reg, _ := newListService(t)

	// Two sessions with distinct, known postures.
	idA := idFor("t1", "u1", "sA")
	idB := idFor("t2", "u2", "sB")
	seedTask(t, reg, idA, tasks.KindForeground, tasks.StatusRunning, "a1", "q")
	seedTask(t, reg, idA, tasks.KindForeground, tasks.StatusRunning, "a2", "q")
	seedTask(t, reg, idB, tasks.KindForeground, tasks.StatusFailed, "b1", "q")

	const n = 120
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Alternate the two sessions so the shared Service is
			// exercised with interleaved identities.
			var scope prototypes.IdentityScope
			var wantRunning, wantFailed int
			if i%2 == 0 {
				scope = scopeOf("t1", "u1", "sA")
				wantRunning, wantFailed = 2, 0
			} else {
				scope = scopeOf("t2", "u2", "sB")
				wantRunning, wantFailed = 0, 1
			}
			resp, err := svc.List(ctx, prototypes.TaskListRequest{
				Identity:                  scope,
				IncludeStatusCounterStrip: true,
			}, false)
			if err != nil {
				errs <- err
				return
			}
			s := resp.StatusCounterStrip
			if s == nil || s.Running != wantRunning || s.Failed != wantFailed {
				errs <- errStripMismatch(i, s, wantRunning, wantFailed)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent strip call: %v", err)
	}
}

// errStripMismatch builds a descriptive error for a concurrent-call
// mismatch — context bleed surfaces as a wrong-session strip.
func errStripMismatch(i int, got *prototypes.TasksListStatusCounterStrip, wantRunning, wantFailed int) error {
	return &stripMismatchError{i: i, got: got, wantRunning: wantRunning, wantFailed: wantFailed}
}

type stripMismatchError struct {
	i           int
	got         *prototypes.TasksListStatusCounterStrip
	wantRunning int
	wantFailed  int
}

func (e *stripMismatchError) Error() string {
	return "goroutine " + itoa(e.i) + ": strip mismatch — context bleed"
}

// itoa is a tiny strconv.Itoa to keep this test file import-light.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
