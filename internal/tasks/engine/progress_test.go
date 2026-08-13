package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/engine"
)

// fakeClock is a controllable wall-clock source for the progress
// coalescing tests — AGENTS.md §11 forbids time.Sleep as a
// synchronisation primitive, so the rate window is driven by clock
// advancement instead.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{t: start} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// failingBus wraps a real inmem bus and fails Publish while failPublish
// is set — the publication-failure injector for ReportProgress (and
// only for it: Subscribe / Close delegate to the wrapped bus).
type failingBus struct {
	events.EventBus
	mu           sync.Mutex
	failPublish  bool
	failedEvents int
}

func (b *failingBus) Publish(ctx context.Context, ev events.Event) error {
	b.mu.Lock()
	fail := b.failPublish
	if fail {
		b.failedEvents++
	}
	b.mu.Unlock()
	if fail {
		return errInjectedPublish
	}
	return b.EventBus.Publish(ctx, ev)
}

func (b *failingBus) setFail(f bool) {
	b.mu.Lock()
	b.failPublish = f
	b.mu.Unlock()
}

var errInjectedPublish = errors.New("engine_test: injected publish failure")

// progressSub subscribes a test to the task.progress events for the
// shared test identity and returns a drain helper. Publish is
// synchronous on the inmem bus, so every event an accepted report
// emitted is already buffered when ReportProgress returns; the drain
// is a non-blocking select loop, never a sleep.
func progressSub(t *testing.T, bus events.EventBus, id identity.Quadruple) func() []tasks.TaskProgressPayload {
	t.Helper()
	ctx, err := identity.With(context.Background(), id.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	sub, err := bus.Subscribe(ctx, events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Types:   []events.EventType{tasks.EventTypeTaskProgress},
	})
	if err != nil {
		t.Fatalf("Subscribe(task.progress): %v", err)
	}
	t.Cleanup(sub.Cancel)
	return func() []tasks.TaskProgressPayload {
		var out []tasks.TaskProgressPayload
		for {
			select {
			case ev, ok := <-sub.Events():
				if !ok {
					return out
				}
				p, ok := ev.Payload.(tasks.TaskProgressPayload)
				if !ok {
					t.Fatalf("task.progress payload type=%T, want TaskProgressPayload", ev.Payload)
				}
				out = append(out, p)
			default:
				return out
			}
		}
	}
}

// newProgressEngine builds an engine over the production inmem event
// bus + a no-op mem backend, with a controllable clock and the given
// rate policy. It returns the engine, the bus, the clock, and the
// shared test identity ctx.
func newProgressEngine(t *testing.T, policy tasks.ProgressPolicy) (*engine.Engine, events.EventBus, *fakeClock, context.Context) {
	t.Helper()
	bus := mkBus(t)
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	clock := newFakeClock(time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC))
	eng, err := engine.New(bus, auditpatterns.New(), &memBackend{},
		engine.WithClock(clock.Now), engine.WithProgressPolicy(policy))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { _ = eng.Close(context.Background()) })
	id := idQuad()
	ctx, err := identity.With(context.Background(), id.Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return eng, bus, clock, ctx
}

// spawnProgressTask spawns a task (optionally under a parent) on the
// engine and advances it to Running so progress reporting is legal.
func spawnProgressTask(t *testing.T, eng tasks.TaskRegistry, ctx context.Context, id identity.Quadruple, parent *tasks.TaskID) tasks.TaskID {
	t.Helper()
	h, err := eng.Spawn(ctx, tasks.SpawnRequest{
		Identity:     id,
		Kind:         tasks.KindBackground,
		ParentTaskID: parent,
		Description:  "progress-test task",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := eng.MarkRunning(ctx, h.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	return h.ID
}

func frac(v float64) *float64 { return &v }

// TestProgress_FirstReport_RecordsEmitsAndCarriesParent asserts a
// background child task's first report is durably recorded (Get sees
// it) and emitted with task + parent ids and the redacted snapshot.
func TestProgress_FirstReport_RecordsEmitsAndCarriesParent(t *testing.T) {
	eng, bus, clock, ctx := newProgressEngine(t, tasks.DefaultProgressPolicy())
	drain := progressSub(t, bus, idQuad())

	parentID := spawnProgressTask(t, eng, ctx, idQuad(), nil)
	childID := spawnProgressTask(t, eng, ctx, idQuad(), &parentID)

	// The report carries an inline bearer credential to prove redaction
	// runs before persistence + publication (the patterns redactor's
	// bearer_in_value rule rewrites `Bearer <token>` to `Bearer ***`).
	res, err := eng.ReportProgress(ctx, childID, tasks.ReportProgressRequest{
		Fraction: frac(0.5),
		Phase:    "indexing",
		Message:  "token Bearer abc123xyz",
		Tags:     []string{"batch", " batch ", "batch"},
	})
	if err != nil {
		t.Fatalf("ReportProgress: %v", err)
	}
	if !res.Recorded || !res.Emitted {
		t.Fatalf("first report result=%+v, want Recorded+Emitted", res)
	}

	got, err := eng.Get(ctx, childID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Progress == nil {
		t.Fatal("Get returned nil Progress after a recorded report")
	}
	if got.Progress.Fraction == nil || *got.Progress.Fraction != 0.5 {
		t.Errorf("persisted fraction=%v, want 0.5", got.Progress.Fraction)
	}
	if got.Progress.Phase != "indexing" {
		t.Errorf("persisted phase=%q, want indexing", got.Progress.Phase)
	}
	if got.Progress.ReportedAt != clock.Now().UnixNano() {
		t.Errorf("ReportedAt not stamped by the engine clock")
	}
	// Tags are normalized (trimmed + deduped) before persistence.
	if len(got.Progress.Tags) != 1 || got.Progress.Tags[0] != "batch" {
		t.Errorf("persisted tags=%v, want [batch]", got.Progress.Tags)
	}
	// The message was redacted before persistence: the raw credential
	// token must not appear in the stored snapshot.
	if contains(got.Progress.Message, "abc123xyz") {
		t.Errorf("persisted message %q leaked the credential token", got.Progress.Message)
	}
	// Progress is activity: UpdatedAt advanced (a freshly-spawned task's
	// UpdatedAt was its CreatedAt; the report moved it later).
	if got.UpdatedAt < got.CreatedAt {
		t.Errorf("UpdatedAt=%d did not advance past CreatedAt=%d", got.UpdatedAt, got.CreatedAt)
	}

	events := drain()
	if len(events) != 1 {
		t.Fatalf("drained %d task.progress events, want 1", len(events))
	}
	ev := events[0]
	if ev.TaskID != childID {
		t.Errorf("event TaskID=%q, want %q", ev.TaskID, childID)
	}
	if ev.ParentTaskID != parentID {
		t.Errorf("event ParentTaskID=%q, want %q", ev.ParentTaskID, parentID)
	}
	if ev.Fraction == nil || *ev.Fraction != 0.5 {
		t.Errorf("event fraction=%v, want 0.5", ev.Fraction)
	}
	if contains(ev.Message, "abc123xyz") {
		t.Errorf("event message %q leaked the credential token", ev.Message)
	}
	if len(ev.Tags) != 1 || ev.Tags[0] != "batch" {
		t.Errorf("event tags=%v, want [batch]", ev.Tags)
	}
	if ev.ReportedAt == 0 {
		t.Errorf("event ReportedAt=0 — the runtime must stamp it")
	}
}

// TestProgress_NoParent_EventCarriesEmptyParentID locks the payload
// shape for a task without a parent.
func TestProgress_NoParent_EventCarriesEmptyParentID(t *testing.T) {
	eng, bus, _, ctx := newProgressEngine(t, tasks.DefaultProgressPolicy())
	drain := progressSub(t, bus, idQuad())
	id := spawnProgressTask(t, eng, ctx, idQuad(), nil)

	if _, err := eng.ReportProgress(ctx, id, tasks.ReportProgressRequest{Phase: "x"}); err != nil {
		t.Fatalf("ReportProgress: %v", err)
	}
	evs := drain()
	if len(evs) != 1 {
		t.Fatalf("drained %d events, want 1", len(evs))
	}
	if evs[0].ParentTaskID != "" {
		t.Errorf("parentless event carried ParentTaskID=%q, want empty", evs[0].ParentTaskID)
	}
}

// TestProgress_IdentityMismatch_Rejected asserts cross-tenant /
// cross-session / cross-user reports are rejected with ErrNotFound
// (existence-without-access), and a ctx without identity fails closed
// with ErrIdentityRequired.
func TestProgress_IdentityMismatch_Rejected(t *testing.T) {
	eng, _, _, ctxA := newProgressEngine(t, tasks.DefaultProgressPolicy())
	id := spawnProgressTask(t, eng, ctxA, idQuad(), nil)

	cases := []struct {
		name string
		id   identity.Identity
	}{
		{"cross-tenant", identity.Identity{TenantID: "other-tenant", UserID: "u", SessionID: "s"}},
		{"cross-user", identity.Identity{TenantID: "t", UserID: "other-user", SessionID: "s"}},
		{"cross-session", identity.Identity{TenantID: "t", UserID: "u", SessionID: "other-session"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, err := identity.With(context.Background(), tc.id)
			if err != nil {
				t.Fatalf("identity.With: %v", err)
			}
			_, err = eng.ReportProgress(ctx, id, tasks.ReportProgressRequest{Phase: "x"})
			if !errors.Is(err, tasks.ErrNotFound) {
				t.Fatalf("ReportProgress(%s): err=%v, want ErrNotFound", tc.name, err)
			}
		})
	}

	// No identity on ctx → ErrIdentityRequired.
	if _, err := eng.ReportProgress(context.Background(), id, tasks.ReportProgressRequest{Phase: "x"}); !errors.Is(err, tasks.ErrIdentityRequired) {
		t.Errorf("ReportProgress without ctx identity: err=%v, want ErrIdentityRequired", err)
	}

	// Unknown task id → ErrNotFound.
	if _, err := eng.ReportProgress(ctxA, tasks.TaskID("nope"), tasks.ReportProgressRequest{Phase: "x"}); !errors.Is(err, tasks.ErrNotFound) {
		t.Errorf("ReportProgress on unknown id: err=%v, want ErrNotFound", err)
	}
}

// TestProgress_Terminal_RejectedAndOrdered asserts every terminal
// status rejects a progress report with ErrInvalidTransition, records
// nothing, and emits no event — so a progress event can never follow
// the task's terminal event.
func TestProgress_Terminal_RejectedAndOrdered(t *testing.T) {
	eng, _, _, ctx := newProgressEngine(t, tasks.DefaultProgressPolicy())
	id := spawnProgressTask(t, eng, ctx, idQuad(), nil)

	// A real report first — the progress event MUST precede the
	// terminal event.
	if _, err := eng.ReportProgress(ctx, id, tasks.ReportProgressRequest{Fraction: frac(0.9), Phase: "finishing"}); err != nil {
		t.Fatalf("ReportProgress: %v", err)
	}
	if err := eng.MarkComplete(ctx, id, tasks.TaskResult{Value: []byte(`"done"`)}); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}

	if _, err := eng.ReportProgress(ctx, id, tasks.ReportProgressRequest{Fraction: frac(1.0), Phase: "done"}); !errors.Is(err, tasks.ErrInvalidTransition) {
		t.Fatalf("ReportProgress after Complete: err=%v, want ErrInvalidTransition", err)
	}

	got, err := eng.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	// The rejected report must not have replaced the snapshot.
	if got.Progress == nil || got.Progress.Phase != "finishing" {
		t.Errorf("post-terminal snapshot=%+v, want the pre-terminal {phase: finishing}", got.Progress)
	}
	if got.Status != tasks.StatusComplete {
		t.Errorf("status=%q, want complete", got.Status)
	}

	// Failed + Cancelled reject identically.
	failedID := spawnProgressTask(t, eng, ctx, idQuad(), nil)
	if err := eng.MarkFailed(ctx, failedID, tasks.TaskError{Code: "boom"}); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if _, err := eng.ReportProgress(ctx, failedID, tasks.ReportProgressRequest{Phase: "x"}); !errors.Is(err, tasks.ErrInvalidTransition) {
		t.Errorf("ReportProgress after Failed: err=%v, want ErrInvalidTransition", err)
	}
	cancelledID := spawnProgressTask(t, eng, ctx, idQuad(), nil)
	if _, err := eng.Cancel(ctx, cancelledID, "stop"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if _, err := eng.ReportProgress(ctx, cancelledID, tasks.ReportProgressRequest{Phase: "x"}); !errors.Is(err, tasks.ErrInvalidTransition) {
		t.Errorf("ReportProgress after Cancelled: err=%v, want ErrInvalidTransition", err)
	}
}

// TestProgress_Validation_RejectsBounds asserts ValidateProgressRequest
// (reached before any state is touched) rejects every out-of-bound
// input and accepts the empty request.
func TestProgress_Validation_RejectsBounds(t *testing.T) {
	good := tasks.ReportProgressRequest{Phase: "p", Message: "m", Tags: []string{"a"}}
	if err := tasks.ValidateProgressRequest(good); err != nil {
		t.Fatalf("good request rejected: %v", err)
	}
	if err := tasks.ValidateProgressRequest(tasks.ReportProgressRequest{}); err != nil {
		t.Fatalf("empty request rejected: %v", err)
	}

	tooLongPhase := string(make([]byte, tasks.ProgressPhaseMaxLen+1))
	tooLongMsg := string(make([]byte, tasks.ProgressMessageMaxLen+1))
	tooLongTag := string(make([]byte, tasks.ProgressTagMaxLen+1))
	tooManyTags := make([]string, tasks.ProgressMaxTags+1)

	cases := []struct {
		name string
		req  tasks.ReportProgressRequest
	}{
		{"fraction below 0", tasks.ReportProgressRequest{Fraction: frac(-0.01)}},
		{"fraction above 1", tasks.ReportProgressRequest{Fraction: frac(1.01)}},
		{"fraction NaN", tasks.ReportProgressRequest{Fraction: frac(math.NaN())}},
		{"fraction +Inf", tasks.ReportProgressRequest{Fraction: frac(math.Inf(1))}},
		{"fraction -Inf", tasks.ReportProgressRequest{Fraction: frac(math.Inf(-1))}},
		{"phase too long", tasks.ReportProgressRequest{Phase: tooLongPhase}},
		{"message too long", tasks.ReportProgressRequest{Message: tooLongMsg}},
		{"too many tags", tasks.ReportProgressRequest{Tags: tooManyTags}},
		{"tag too long", tasks.ReportProgressRequest{Tags: []string{tooLongTag}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tasks.ValidateProgressRequest(tc.req); !errors.Is(err, tasks.ErrInvalidRequest) {
				t.Fatalf("ValidateProgressRequest(%s): err=%v, want ErrInvalidRequest", tc.name, err)
			}
		})
	}

	// The registry also rejects before touching state: nothing recorded,
	// nothing emitted.
	eng, bus, _, ctx := newProgressEngine(t, tasks.DefaultProgressPolicy())
	drain := progressSub(t, bus, idQuad())
	id := spawnProgressTask(t, eng, ctx, idQuad(), nil)
	if _, err := eng.ReportProgress(ctx, id, tasks.ReportProgressRequest{Fraction: frac(2.0)}); !errors.Is(err, tasks.ErrInvalidRequest) {
		t.Fatalf("engine ReportProgress with out-of-range fraction: err=%v, want ErrInvalidRequest", err)
	}
	got, err := eng.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Progress != nil {
		t.Errorf("invalid report left a snapshot behind: %+v", got.Progress)
	}
	if evs := drain(); len(evs) != 0 {
		t.Errorf("invalid report emitted %d events", len(evs))
	}
}

// TestProgress_NormalizationUniqueness_NoOp asserts a re-report whose
// only difference is tag reordering / duplication / whitespace is an
// idempotent no-op — nothing recorded, nothing emitted.
func TestProgress_NormalizationUniqueness_NoOp(t *testing.T) {
	eng, bus, _, ctx := newProgressEngine(t, tasks.DefaultProgressPolicy())
	drain := progressSub(t, bus, idQuad())
	id := spawnProgressTask(t, eng, ctx, idQuad(), nil)

	first := tasks.ReportProgressRequest{
		Fraction: frac(0.3),
		Phase:    "searching",
		Message:  "querying",
		Tags:     []string{"alpha", "beta"},
	}
	res, err := eng.ReportProgress(ctx, id, first)
	if err != nil {
		t.Fatalf("ReportProgress 1: %v", err)
	}
	if !res.Recorded || !res.Emitted {
		t.Fatalf("first report result=%+v, want Recorded+Emitted", res)
	}
	if n := drain(); len(n) != 1 {
		t.Fatalf("first report drained %d events, want 1", len(n))
	}

	// Same content modulo tag order/duplication/whitespace → no-op.
	dup := tasks.ReportProgressRequest{
		Fraction: frac(0.3),
		Phase:    "searching",
		Message:  "querying",
		Tags:     []string{" beta ", "alpha", "alpha", "beta"},
	}
	res, err = eng.ReportProgress(ctx, id, dup)
	if err != nil {
		t.Fatalf("ReportProgress (normalized duplicate): %v", err)
	}
	if res.Recorded || res.Emitted {
		t.Fatalf("normalized duplicate result=%+v, want a (false,false) no-op", res)
	}
	if n := drain(); len(n) != 0 {
		t.Fatalf("no-op emitted %d events", len(n))
	}
	got, err := eng.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Progress.Tags) != 2 {
		t.Errorf("persisted tags=%v, want [alpha beta]", got.Progress.Tags)
	}
}

// TestProgress_Coalescing_RateWindow asserts the bounded rate policy:
// a message-only update inside the window is recorded but not emitted;
// a real fraction/phase change bypasses the window; a message-only
// update after the window elapses emits. The clock is advanced
// explicitly — no sleeps.
func TestProgress_Coalescing_RateWindow(t *testing.T) {
	eng, bus, clock, ctx := newProgressEngine(t, tasks.ProgressPolicy{
		MinInterval:     time.Second,
		FractionEpsilon: 0.01,
	})
	drain := progressSub(t, bus, idQuad())
	id := spawnProgressTask(t, eng, ctx, idQuad(), nil)

	// t0: first report — recorded + emitted.
	res, err := eng.ReportProgress(ctx, id, tasks.ReportProgressRequest{
		Fraction: frac(0.1), Phase: "indexing", Message: "started",
	})
	if err != nil {
		t.Fatalf("ReportProgress t0: %v", err)
	}
	if !res.Recorded || !res.Emitted {
		t.Fatalf("t0 result=%+v, want Recorded+Emitted", res)
	}
	if n := drain(); len(n) != 1 {
		t.Fatalf("t0 drained %d events, want 1", len(n))
	}

	// t0+100ms: message-only update inside the window → recorded, not
	// emitted (coalesced).
	clock.Advance(100 * time.Millisecond)
	res, err = eng.ReportProgress(ctx, id, tasks.ReportProgressRequest{
		Fraction: frac(0.1), Phase: "indexing", Message: "still indexing",
	})
	if err != nil {
		t.Fatalf("ReportProgress t0+100ms: %v", err)
	}
	if !res.Recorded || res.Emitted {
		t.Fatalf("coalesced update result=%+v, want Recorded only", res)
	}
	if n := drain(); len(n) != 0 {
		t.Fatalf("coalesced update emitted %d events", len(n))
	}
	// The coalesced update IS the latest snapshot on Get.
	got, err := eng.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Progress.Message != "still indexing" {
		t.Errorf("latest snapshot message=%q, want the coalesced update", got.Progress.Message)
	}

	// t0+200ms: real fraction change (0.1 → 0.15) bypasses the window.
	clock.Advance(100 * time.Millisecond)
	res, err = eng.ReportProgress(ctx, id, tasks.ReportProgressRequest{
		Fraction: frac(0.15), Phase: "indexing", Message: "deeper",
	})
	if err != nil {
		t.Fatalf("ReportProgress t0+200ms: %v", err)
	}
	if !res.Recorded || !res.Emitted {
		t.Fatalf("real fraction change result=%+v, want Recorded+Emitted", res)
	}
	if n := drain(); len(n) != 1 {
		t.Fatalf("real change drained %d events, want 1", len(n))
	}

	// t0+400ms: fraction wobble below epsilon is NOT a real change and
	// sits inside the window → recorded, coalesced.
	clock.Advance(200 * time.Millisecond)
	res, err = eng.ReportProgress(ctx, id, tasks.ReportProgressRequest{
		Fraction: frac(0.151), Phase: "indexing", Message: "wobble",
	})
	if err != nil {
		t.Fatalf("ReportProgress t0+400ms: %v", err)
	}
	if res.Emitted {
		t.Fatalf("sub-epsilon fraction wobble was emitted; want coalesced")
	}

	// t0+1300ms: message-only update; 1100ms elapsed since the last
	// emission (t0+200ms) → window elapsed → recorded + emitted.
	clock.Advance(900 * time.Millisecond)
	res, err = eng.ReportProgress(ctx, id, tasks.ReportProgressRequest{
		Fraction: frac(0.151), Phase: "indexing", Message: "window elapsed",
	})
	if err != nil {
		t.Fatalf("ReportProgress t0+1300ms: %v", err)
	}
	if !res.Recorded || !res.Emitted {
		t.Fatalf("post-window update result=%+v, want Recorded+Emitted", res)
	}
	if n := drain(); len(n) != 1 {
		t.Fatalf("post-window update drained %d events, want 1", len(n))
	}

	// A phase change is always real.
	clock.Advance(100 * time.Millisecond)
	res, err = eng.ReportProgress(ctx, id, tasks.ReportProgressRequest{
		Fraction: frac(0.151), Phase: "indexing", Message: "same phase",
	})
	if err != nil {
		t.Fatalf("ReportProgress phase-only: %v", err)
	}
	if res.Emitted {
		t.Fatalf("same-phase message update inside window was emitted; want coalesced")
	}
	clock.Advance(100 * time.Millisecond)
	res, err = eng.ReportProgress(ctx, id, tasks.ReportProgressRequest{
		Fraction: frac(0.151), Phase: "verifying", Message: "new phase",
	})
	if err != nil {
		t.Fatalf("ReportProgress phase change: %v", err)
	}
	if !res.Recorded || !res.Emitted {
		t.Fatalf("phase change result=%+v, want Recorded+Emitted", res)
	}
}

// TestProgress_PersistFailure_RollsBackAndClaimsNoSuccess asserts a
// persistence failure returns the error, leaves the prior snapshot
// (and UpdatedAt) untouched, and emits nothing — a failed report never
// claims success.
func TestProgress_PersistFailure_RollsBackAndClaimsNoSuccess(t *testing.T) {
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	backend := &memBackend{}
	eng, err := engine.New(bus, auditpatterns.New(), backend)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer func() { _ = eng.Close(context.Background()) }()
	id := idQuad()
	ctx, _ := identity.With(context.Background(), id.Identity)

	// First a successful report so there is a prior snapshot to roll
	// back to.
	h, err := eng.Spawn(ctx, tasks.SpawnRequest{Identity: id, Kind: tasks.KindBackground})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := eng.MarkRunning(ctx, h.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if _, err := eng.ReportProgress(ctx, h.ID, tasks.ReportProgressRequest{Fraction: frac(0.2), Phase: "a"}); err != nil {
		t.Fatalf("ReportProgress (good): %v", err)
	}
	before, err := eng.Get(ctx, h.ID)
	if err != nil {
		t.Fatalf("Get(before): %v", err)
	}

	// Inject a SaveTask failure.
	backend.saveErr = errors.New("injected disk failure")

	_, err = eng.ReportProgress(ctx, h.ID, tasks.ReportProgressRequest{Fraction: frac(0.9), Phase: "b"})
	if err == nil {
		t.Fatal("ReportProgress with failing backend: want error, got nil")
	}

	after, err := eng.Get(ctx, h.ID)
	if err != nil {
		t.Fatalf("Get(after): %v", err)
	}
	if after.Progress == nil || after.Progress.Phase != "a" || after.Progress.Fraction == nil || *after.Progress.Fraction != 0.2 {
		t.Errorf("snapshot after failed report=%+v, want the prior {0.2, a}", after.Progress)
	}
	if after.UpdatedAt != before.UpdatedAt {
		t.Errorf("UpdatedAt changed on a failed report: before=%d after=%d", before.UpdatedAt, after.UpdatedAt)
	}
}

// TestProgress_PublishFailure_ClaimsNoSuccessButSnapshotStands asserts
// a publication failure returns the error (the caller cannot claim
// success) while the durable snapshot stands — the at-most-once
// contract Mark* publish failures carry. A re-report of the identical
// content is then an idempotent no-op (the snapshot already records
// it), exactly like a retried Mark* on an already-transitioned task.
func TestProgress_PublishFailure_ClaimsNoSuccessButSnapshotStands(t *testing.T) {
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	fb := &failingBus{EventBus: bus}
	eng, err := engine.New(fb, auditpatterns.New(), &memBackend{})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	defer func() { _ = eng.Close(context.Background()) }()
	id := idQuad()
	ctx, _ := identity.With(context.Background(), id.Identity)
	h, err := eng.Spawn(ctx, tasks.SpawnRequest{Identity: id, Kind: tasks.KindBackground})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if err := eng.MarkRunning(ctx, h.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}

	fb.setFail(true)
	req := tasks.ReportProgressRequest{Fraction: frac(0.4), Phase: "p", Message: "m"}
	_, err = eng.ReportProgress(ctx, h.ID, req)
	if err == nil {
		t.Fatal("ReportProgress with failing publish: want error, got nil")
	}

	// The snapshot stands (persist happened before publish).
	got, err := eng.Get(ctx, h.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Progress == nil || *got.Progress.Fraction != 0.4 {
		t.Errorf("snapshot after failed publish=%+v, want the recorded {0.4}", got.Progress)
	}

	// Identical re-report → idempotent no-op, never a false success.
	fb.setFail(false)
	res, err := eng.ReportProgress(ctx, h.ID, req)
	if err != nil {
		t.Fatalf("identical re-report: %v", err)
	}
	if res.Recorded || res.Emitted {
		t.Errorf("identical re-report result=%+v, want a (false,false) no-op", res)
	}
}

// TestProgress_ConcurrentReports_NoRace is the D-025 concurrent-reuse
// gate for ReportProgress: N concurrent reports against ONE shared
// task instance under -race must yield a consistent final snapshot
// (one of the reported values — last-write-wins under the FSM lock),
// exactly N emissions when every report is a real phase change, and no
// torn writes.
func TestProgress_ConcurrentReports_NoRace(t *testing.T) {
	eng, bus, _, ctx := newProgressEngine(t, tasks.ProgressPolicy{
		MinInterval:     time.Hour, // every report is a real phase change, so the window never matters
		FractionEpsilon: 0.01,
	})
	drain := progressSub(t, bus, idQuad())
	id := spawnProgressTask(t, eng, ctx, idQuad(), nil)

	const N = 128
	var wg sync.WaitGroup
	wg.Add(N)
	for i := range N {
		go func(n int) {
			defer wg.Done()
			_, err := eng.ReportProgress(ctx, id, tasks.ReportProgressRequest{
				Fraction: frac(float64(n%101) / 100),
				Phase:    fmt.Sprintf("phase-%d", n),
			})
			if err != nil {
				t.Errorf("concurrent ReportProgress %d: %v", n, err)
			}
		}(i)
	}
	wg.Wait()

	if evs := drain(); len(evs) != N {
		t.Errorf("drained %d task.progress events, want %d (every real phase change must emit)", len(evs), N)
	}

	got, err := eng.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Progress == nil || got.Progress.Fraction == nil {
		t.Fatal("final snapshot missing after concurrent reports")
	}
	if got.Progress.Fraction == nil || *got.Progress.Fraction < 0 || *got.Progress.Fraction > 1 {
		t.Errorf("final fraction %v outside [0,1] — torn write", got.Progress.Fraction)
	}
	found := false
	for i := range N {
		if got.Progress.Phase == fmt.Sprintf("phase-%d", i) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("final phase %q is not one of the reported phases", got.Progress.Phase)
	}
}

// TestProgress_ConcurrentReportAndComplete_Race asserts a report
// racing MarkComplete either lands before the terminal transition (and
// is therefore visible on Get) or is rejected with
// ErrInvalidTransition — never a success that violates terminal
// ordering.
func TestProgress_ConcurrentReportAndComplete_Race(t *testing.T) {
	eng, _, _, ctx := newProgressEngine(t, tasks.DefaultProgressPolicy())
	id := spawnProgressTask(t, eng, ctx, idQuad(), nil)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = eng.ReportProgress(ctx, id, tasks.ReportProgressRequest{Fraction: frac(0.8), Phase: "racing"})
	}()
	go func() {
		defer wg.Done()
		_ = eng.MarkComplete(ctx, id, tasks.TaskResult{Value: []byte(`"done"`)})
	}()
	wg.Wait()

	got, err := eng.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != tasks.StatusComplete {
		t.Fatalf("status=%q, want complete", got.Status)
	}
	// A report that won the race left its snapshot; one that lost left
	// the pre-terminal nil. Both are consistent; neither fabricated a
	// completion value.
	if got.Progress != nil && got.Progress.Phase != "racing" {
		t.Errorf("racing snapshot phase=%q, want racing", got.Progress.Phase)
	}
	// And after the race, reports are rejected outright.
	if _, err := eng.ReportProgress(ctx, id, tasks.ReportProgressRequest{Phase: "late"}); !errors.Is(err, tasks.ErrInvalidTransition) {
		t.Errorf("post-race report: err=%v, want ErrInvalidTransition", err)
	}
}

// TestProgress_OldFormatTask_RoundTrips asserts old-client / old-record
// compatibility: a Task whose JSON predates the Progress field
// hydrates with nil Progress (no fabrication), and a task with a
// snapshot round-trips it intact.
func TestProgress_OldFormatTask_RoundTrips(t *testing.T) {
	id := idQuad()
	old := tasks.Task{
		ID:        "01ARZ3NDEKTSV4RRFFQ69G5FAV",
		Identity:  id,
		Kind:      tasks.KindBackground,
		Status:    tasks.StatusRunning,
		CreatedAt: 1000,
		UpdatedAt: 1000,
	}
	oldBytes, err := json.Marshal(old)
	if err != nil {
		t.Fatalf("marshal old-format task: %v", err)
	}
	var hydrated tasks.Task
	if err := json.Unmarshal(oldBytes, &hydrated); err != nil {
		t.Fatalf("unmarshal old-format task: %v", err)
	}
	if hydrated.Progress != nil {
		t.Fatalf("old-format task hydrated with Progress=%+v; want nil (no fabrication)", hydrated.Progress)
	}

	f := 0.75
	withProgress := tasks.Task{
		ID:       "01ARZ3NDEKTSV4RRFFQ69G5FAW",
		Identity: id,
		Kind:     tasks.KindBackground,
		Status:   tasks.StatusRunning,
		Progress: &tasks.TaskProgressSnapshot{
			Fraction:   &f,
			Phase:      "p",
			Message:    "m",
			Tags:       []string{"a"},
			ReportedAt: 2000,
		},
		CreatedAt: 1000,
		UpdatedAt: 2000,
	}
	withBytes, err := json.Marshal(withProgress)
	if err != nil {
		t.Fatalf("marshal with-progress task: %v", err)
	}
	var hydrated2 tasks.Task
	if err := json.Unmarshal(withBytes, &hydrated2); err != nil {
		t.Fatalf("unmarshal with-progress task: %v", err)
	}
	if hydrated2.Progress == nil {
		t.Fatal("with-progress task hydrated with nil Progress")
	}
	if hydrated2.Progress.Fraction == nil || *hydrated2.Progress.Fraction != 0.75 {
		t.Errorf("round-tripped fraction=%v, want 0.75", hydrated2.Progress.Fraction)
	}
	if hydrated2.Progress.ReportedAt != 2000 {
		t.Errorf("round-tripped ReportedAt=%d, want 2000", hydrated2.Progress.ReportedAt)
	}
}

// --- small helpers -------------------------------------------------

// clockReportedAt is the unix-nano instant newFakeClock starts at;
// TestProgress_FirstReport checks the snapshot's ReportedAt equals the
// engine clock's stamp.
var clockReportedAt = time.Date(2026, 8, 13, 0, 0, 0, 0, time.UTC).UnixNano()

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
