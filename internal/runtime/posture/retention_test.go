package posture

import (
	"context"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/tasks"
)

// fakeTaskReg implements tasks.TaskRegistry but only List is exercised by
// the non-widened RetentionProvider path — the embedded interface
// satisfies the rest (any unexpected call nil-panics, surfacing a drift).
// It deliberately does NOT implement the optional identity-free
// oldestRetainedReader, so a widened read finds no runtime-wide reader.
type fakeTaskReg struct {
	tasks.TaskRegistry
	summaries []tasks.TaskSummary
	err       error
}

func (f *fakeTaskReg) List(_ context.Context, _ identity.Identity, _ tasks.TaskFilter) ([]tasks.TaskSummary, error) {
	return f.summaries, f.err
}

// fakeLister implements sessions.SessionLister only (no runtime-wide
// reader), so a widened read finds no runtime-wide reader on it.
type fakeLister struct {
	snaps []sessions.SessionSnapshot
	err   error
}

func (f *fakeLister) ListSnapshots(_ context.Context, _ sessions.SessionListFilter) ([]sessions.SessionSnapshot, error) {
	return f.snaps, f.err
}

// fakeRuntimeTaskReg additionally implements oldestRetainedReader — the
// runtime-wide identity-free horizon reader a widened read discovers by
// type assertion.
type fakeRuntimeTaskReg struct {
	fakeTaskReg
	runtimeOldest  time.Time
	runtimePresent bool
	runtimeErr     error
}

func (f *fakeRuntimeTaskReg) OldestRetainedAt(_ context.Context) (time.Time, bool, error) {
	return f.runtimeOldest, f.runtimePresent, f.runtimeErr
}

// fakeRuntimeLister additionally implements oldestRetainedReader.
type fakeRuntimeLister struct {
	fakeLister
	runtimeOldest  time.Time
	runtimePresent bool
	runtimeErr     error
}

func (f *fakeRuntimeLister) OldestRetainedAt(_ context.Context) (time.Time, bool, error) {
	return f.runtimeOldest, f.runtimePresent, f.runtimeErr
}

func ringBus(t *testing.T) events.EventBus {
	t.Helper()
	cfg := config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     8,
		IdleTimeout:              200 * time.Millisecond,
		DropWindow:               50 * time.Millisecond,
		ReplayBufferSize:         8,
	}
	bus, err := inmem.New(cfg, auditpatterns.New())
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func horizonBySurface(hs []types.RetentionHorizon) map[string]types.RetentionHorizon {
	m := make(map[string]types.RetentionHorizon, len(hs))
	for _, h := range hs {
		m[h.Surface] = h
	}
	return m
}

// hasTS reports whether h carries a timestamp equal to want.
func hasTS(h types.RetentionHorizon, want time.Time) bool {
	return h.OldestRetainedAt != nil && h.OldestRetainedAt.Equal(want)
}

// noTS reports whether h omits its timestamp (the absence-representable
// "no rows at this scope" signal).
func noTS(h types.RetentionHorizon) bool {
	return h.OldestRetainedAt == nil
}

func TestRetentionProvider_NilDeps_ReturnsNil(t *testing.T) {
	provider := RetentionProvider(nil, nil, nil)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	if got := provider(context.Background(), id, false); got != nil {
		t.Fatalf("nil-dep retention = %+v, want nil", got)
	}
	// A nil-everything seam omits the whole block even when widened.
	if got := provider(context.Background(), id, true); got != nil {
		t.Fatalf("nil-dep widened retention = %+v, want nil", got)
	}
}

func TestRetentionProvider_EventsHorizon_FromBus_ScopeRuntime(t *testing.T) {
	bus := ringBus(t)
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	ev := events.Event{Type: events.EventTypeRuntimeWarning, Identity: id, OccurredAt: base,
		Payload: events.RedactedMap{Data: map[string]any{"n": 1}}}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	provider := RetentionProvider(bus, nil, nil)
	out := provider(context.Background(), id.Identity, false)
	if len(out) != 1 || out[0].Surface != retentionSurfaceEvents {
		t.Fatalf("out = %+v, want a single events entry", out)
	}
	if out[0].Scope != types.RetentionScopeRuntime {
		t.Fatalf("events scope = %q, want %q", out[0].Scope, types.RetentionScopeRuntime)
	}
	if !hasTS(out[0], base) {
		t.Fatalf("events horizon = %v, want %v", out[0].OldestRetainedAt, base)
	}
}

func TestRetentionProvider_EventsHorizon_EmptyBus_EmitsRuntimeScopeNoTimestamp(t *testing.T) {
	// A wired bus that implements RetentionReporter but retains no events
	// emits a runtime-scoped entry with NO timestamp — a trustworthy empty
	// (distinguishable from "unobservable at your scope").
	bus := ringBus(t)
	provider := RetentionProvider(bus, nil, nil)
	out := provider(context.Background(), identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, false)
	if len(out) != 1 || out[0].Surface != retentionSurfaceEvents {
		t.Fatalf("out = %+v, want a single events entry", out)
	}
	if out[0].Scope != types.RetentionScopeRuntime {
		t.Fatalf("events scope = %q, want %q", out[0].Scope, types.RetentionScopeRuntime)
	}
	if !noTS(out[0]) {
		t.Fatalf("events horizon = %v, want omitted (empty bus)", out[0].OldestRetainedAt)
	}
}

func TestRetentionProvider_TasksAndSessions_OldestRow_ScopedLabels(t *testing.T) {
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	taskReg := &fakeTaskReg{summaries: []tasks.TaskSummary{
		{ID: "a", CreatedAt: base.Add(2 * time.Hour).UnixNano()},
		{ID: "b", CreatedAt: base.UnixNano()}, // oldest
		{ID: "c", CreatedAt: base.Add(time.Hour).UnixNano()},
	}}
	lister := &fakeLister{snaps: []sessions.SessionSnapshot{
		{Session: sessions.Session{ID: "s1", OpenedAt: base.Add(3 * time.Hour)}},
		{Session: sessions.Session{ID: "s2", OpenedAt: base.Add(time.Hour)}}, // oldest
	}}

	provider := RetentionProvider(nil, taskReg, lister)
	out := provider(context.Background(), id, false)

	by := horizonBySurface(out)
	tsk, ok := by[retentionSurfaceTasks]
	if !ok || !hasTS(tsk, base) || tsk.Scope != types.RetentionScopeSession {
		t.Fatalf("tasks horizon = %+v (ok=%v), want oldest=%v scope=session", tsk, ok, base)
	}
	ses, ok := by[retentionSurfaceSessions]
	if !ok || !hasTS(ses, base.Add(time.Hour)) || ses.Scope != types.RetentionScopeTenant {
		t.Fatalf("sessions horizon = %+v (ok=%v), want oldest=%v scope=tenant", ses, ok, base.Add(time.Hour))
	}
}

func TestRetentionProvider_EmptyStores_EmitScopedEntriesNoTimestamp(t *testing.T) {
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	// A wired-but-empty-at-scope surface emits an entry carrying its scope
	// with the timestamp OMITTED — the absence is representable, never a
	// silently-dropped entry (D-311).
	provider := RetentionProvider(nil, &fakeTaskReg{}, &fakeLister{})
	out := provider(context.Background(), id, false)
	by := horizonBySurface(out)
	tsk, ok := by[retentionSurfaceTasks]
	if !ok || tsk.Scope != types.RetentionScopeSession || !noTS(tsk) {
		t.Fatalf("tasks entry = %+v (ok=%v), want scope=session no-timestamp", tsk, ok)
	}
	ses, ok := by[retentionSurfaceSessions]
	if !ok || ses.Scope != types.RetentionScopeTenant || !noTS(ses) {
		t.Fatalf("sessions entry = %+v (ok=%v), want scope=tenant no-timestamp", ses, ok)
	}
}

func TestRetentionProvider_ReadError_DegradesThatSurface(t *testing.T) {
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	// tasks read errors → that surface is absent; sessions still reports —
	// one bad surface never fails the whole retention read.
	taskReg := &fakeTaskReg{err: context.DeadlineExceeded}
	lister := &fakeLister{snaps: []sessions.SessionSnapshot{
		{Session: sessions.Session{ID: "s1", OpenedAt: base}},
	}}
	out := RetentionProvider(nil, taskReg, lister)(context.Background(), id, false)
	if len(out) != 1 || out[0].Surface != retentionSurfaceSessions {
		t.Fatalf("out = %+v, want only the sessions entry", out)
	}
}

func TestRetentionProvider_Widened_ReadsRuntimeWideHorizons(t *testing.T) {
	id := identity.Identity{TenantID: "svc", UserID: "coordinator", SessionID: "fleet"}
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)

	// The `svc:` caller owns NO tasks/sessions in its own scope (List /
	// ListSnapshots return empty), but the runtime-wide readers report the
	// other tenants' oldest rows.
	taskReg := &fakeRuntimeTaskReg{
		runtimeOldest:  base,
		runtimePresent: true,
	}
	lister := &fakeRuntimeLister{
		runtimeOldest:  base.Add(time.Hour),
		runtimePresent: true,
	}

	out := RetentionProvider(nil, taskReg, lister)(context.Background(), id, true)
	by := horizonBySurface(out)
	tsk, ok := by[retentionSurfaceTasks]
	if !ok || tsk.Scope != types.RetentionScopeRuntime || !hasTS(tsk, base) {
		t.Fatalf("tasks horizon = %+v (ok=%v), want scope=runtime oldest=%v", tsk, ok, base)
	}
	ses, ok := by[retentionSurfaceSessions]
	if !ok || ses.Scope != types.RetentionScopeRuntime || !hasTS(ses, base.Add(time.Hour)) {
		t.Fatalf("sessions horizon = %+v (ok=%v), want scope=runtime oldest=%v", ses, ok, base.Add(time.Hour))
	}
}

func TestRetentionProvider_Widened_NoRuntimeReader_OmitsEntry(t *testing.T) {
	id := identity.Identity{TenantID: "svc", UserID: "coordinator", SessionID: "fleet"}
	// A registry / lister that does NOT implement the optional
	// identity-free reader contributes NO runtime-wide entry — its horizon
	// is honestly absent, never fabricated from the (empty) svc: scope.
	out := RetentionProvider(nil, &fakeTaskReg{}, &fakeLister{})(context.Background(), id, true)
	if out != nil {
		t.Fatalf("widened read against reader-less stores = %+v, want nil (honest absence)", out)
	}
}

func TestRetentionProvider_Widened_RuntimeReaderEmpty_EmitsRuntimeScopeNoTimestamp(t *testing.T) {
	id := identity.Identity{TenantID: "svc", UserID: "coordinator", SessionID: "fleet"}
	taskReg := &fakeRuntimeTaskReg{runtimePresent: false}
	lister := &fakeRuntimeLister{runtimePresent: false}
	out := RetentionProvider(nil, taskReg, lister)(context.Background(), id, true)
	by := horizonBySurface(out)
	tsk, ok := by[retentionSurfaceTasks]
	if !ok || tsk.Scope != types.RetentionScopeRuntime || !noTS(tsk) {
		t.Fatalf("tasks entry = %+v (ok=%v), want scope=runtime no-timestamp", tsk, ok)
	}
	ses, ok := by[retentionSurfaceSessions]
	if !ok || ses.Scope != types.RetentionScopeRuntime || !noTS(ses) {
		t.Fatalf("sessions entry = %+v (ok=%v), want scope=runtime no-timestamp", ses, ok)
	}
}

func TestRetentionProvider_Widened_RuntimeReaderError_DegradesThatSurface(t *testing.T) {
	id := identity.Identity{TenantID: "svc", UserID: "coordinator", SessionID: "fleet"}
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	taskReg := &fakeRuntimeTaskReg{runtimeErr: context.DeadlineExceeded}
	lister := &fakeRuntimeLister{runtimeOldest: base, runtimePresent: true}
	out := RetentionProvider(nil, taskReg, lister)(context.Background(), id, true)
	if len(out) != 1 || out[0].Surface != retentionSurfaceSessions || out[0].Scope != types.RetentionScopeRuntime {
		t.Fatalf("out = %+v, want only the runtime-scoped sessions entry", out)
	}
}
