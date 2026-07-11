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
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/tasks"
)

// fakeTaskReg implements tasks.TaskRegistry but only List is exercised by
// RetentionProvider — the embedded interface satisfies the rest (any
// unexpected call nil-panics, surfacing a drift).
type fakeTaskReg struct {
	tasks.TaskRegistry
	summaries []tasks.TaskSummary
	err       error
}

func (f *fakeTaskReg) List(_ context.Context, _ identity.Identity, _ tasks.TaskFilter) ([]tasks.TaskSummary, error) {
	return f.summaries, f.err
}

// fakeLister implements sessions.SessionLister.
type fakeLister struct {
	snaps []sessions.SessionSnapshot
	err   error
}

func (f *fakeLister) ListSnapshots(_ context.Context, _ sessions.SessionListFilter) ([]sessions.SessionSnapshot, error) {
	return f.snaps, f.err
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

func TestRetentionProvider_NilDeps_ReturnsNil(t *testing.T) {
	provider := RetentionProvider(nil, nil, nil)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	if got := provider(context.Background(), id); got != nil {
		t.Fatalf("nil-dep retention = %+v, want nil", got)
	}
}

func TestRetentionProvider_EventsHorizon_FromBus(t *testing.T) {
	bus := ringBus(t)
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	ev := events.Event{Type: events.EventTypeRuntimeWarning, Identity: id, OccurredAt: base,
		Payload: events.RedactedMap{Data: map[string]any{"n": 1}}}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}

	provider := RetentionProvider(bus, nil, nil)
	out := provider(context.Background(), id.Identity)
	if len(out) != 1 || out[0].Surface != retentionSurfaceEvents {
		t.Fatalf("out = %+v, want a single events entry", out)
	}
	if !out[0].OldestRetainedAt.Equal(base) {
		t.Fatalf("events horizon = %v, want %v", out[0].OldestRetainedAt, base)
	}
}

func TestRetentionProvider_TasksAndSessions_OldestRow(t *testing.T) {
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
	out := provider(context.Background(), id)

	byS := map[string]time.Time{}
	for _, h := range out {
		byS[h.Surface] = h.OldestRetainedAt
	}
	if got, ok := byS[retentionSurfaceTasks]; !ok || !got.Equal(base) {
		t.Fatalf("tasks horizon = %v (ok=%v), want %v", got, ok, base)
	}
	if got, ok := byS[retentionSurfaceSessions]; !ok || !got.Equal(base.Add(time.Hour)) {
		t.Fatalf("sessions horizon = %v (ok=%v), want %v", got, ok, base.Add(time.Hour))
	}
}

func TestRetentionProvider_EmptyStores_OmitEntries(t *testing.T) {
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	// Empty task list + empty session list ⇒ no entries.
	provider := RetentionProvider(nil, &fakeTaskReg{}, &fakeLister{})
	if got := provider(context.Background(), id); got != nil {
		t.Fatalf("empty-store retention = %+v, want nil", got)
	}
}

func TestRetentionProvider_ReadError_DegradesThatSurface(t *testing.T) {
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	// tasks read errors; sessions still reports — one bad surface never
	// fails the whole retention read.
	taskReg := &fakeTaskReg{err: context.DeadlineExceeded}
	lister := &fakeLister{snaps: []sessions.SessionSnapshot{
		{Session: sessions.Session{ID: "s1", OpenedAt: base}},
	}}
	out := RetentionProvider(nil, taskReg, lister)(context.Background(), id)
	if len(out) != 1 || out[0].Surface != retentionSurfaceSessions {
		t.Fatalf("out = %+v, want only the sessions entry", out)
	}
}
