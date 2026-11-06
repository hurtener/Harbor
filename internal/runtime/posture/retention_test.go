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

// fakeTaskReg deliberately omits the runtime-wide retention reader. Embedding
// the registry makes an unexpected fallback to session listing fail loudly.
type fakeTaskReg struct {
	tasks.TaskRegistry
	summaries []tasks.TaskSummary
	err       error
}

func (f *fakeTaskReg) List(_ context.Context, _ identity.Identity, _ tasks.TaskFilter) ([]tasks.TaskSummary, error) {
	return f.summaries, f.err
}

// fakeLister implements sessions.SessionLister only (no runtime-wide
// reader), so diagnostics omit this unwired surface.
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

func TestEventsHorizonProvider_ReportsOnlyCachedEvents(t *testing.T) {
	bus := ringBus(t)
	at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	provider := EventsHorizonProvider(bus)
	empty := provider(t.Context())
	if len(empty) != 1 || empty[0].OldestRetainedAt != nil || empty[0].Scope != types.RetentionScopeRuntime {
		t.Fatalf("empty=%+v", empty)
	}
	if err := bus.Publish(t.Context(), events.Event{Type: events.EventTypeRuntimeWarning, Identity: id, OccurredAt: at, Payload: events.RedactedMap{Data: map[string]any{"n": 1}}}); err != nil {
		t.Fatal(err)
	}
	got := provider(t.Context())
	if len(got) != 1 || got[0].Surface != "events" || !hasTS(got[0], at) {
		t.Fatalf("horizon=%+v", got)
	}
	if got := EventsHorizonProvider(nil)(t.Context()); got != nil {
		t.Fatalf("nil bus=%+v", got)
	}
}

func TestRuntimeRetentionProvider_RuntimeOnly(t *testing.T) {
	at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	taskReg := &fakeRuntimeTaskReg{runtimeOldest: at, runtimePresent: true}
	lister := &fakeRuntimeLister{runtimeOldest: at.Add(time.Hour), runtimePresent: true}
	got, err := RuntimeRetentionProvider(ringBus(t), taskReg, lister)(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("horizons=%+v", got)
	}
	by := horizonBySurface(got)
	if !hasTS(by["tasks"], at) || !hasTS(by["sessions"], at.Add(time.Hour)) {
		t.Fatalf("horizons=%+v", got)
	}
	for _, h := range got {
		if h.Scope != types.RetentionScopeRuntime {
			t.Fatalf("scoped entry=%+v", h)
		}
	}
}

func TestRuntimeRetentionProvider_UnwiredOrEmpty(t *testing.T) {
	got, err := RuntimeRetentionProvider(nil, &fakeTaskReg{}, &fakeLister{})(t.Context())
	if err != nil || got != nil {
		t.Fatalf("unwired=%+v err=%v", got, err)
	}
	got, err = RuntimeRetentionProvider(nil, &fakeRuntimeTaskReg{}, &fakeRuntimeLister{})(t.Context())
	if err != nil || len(got) != 2 {
		t.Fatalf("empty=%+v err=%v", got, err)
	}
	for _, h := range got {
		if h.OldestRetainedAt != nil || h.Scope != types.RetentionScopeRuntime {
			t.Fatalf("empty=%+v", h)
		}
	}
}

func TestRuntimeRetentionProvider_ReadFailureFailsSnapshot(t *testing.T) {
	for _, tasksFail := range []bool{false, true} {
		taskReg := &fakeRuntimeTaskReg{}
		lister := &fakeRuntimeLister{}
		if tasksFail {
			taskReg.runtimeErr = context.DeadlineExceeded
		} else {
			lister.runtimeErr = context.DeadlineExceeded
		}
		got, err := RuntimeRetentionProvider(nil, taskReg, lister)(t.Context())
		if err == nil || got != nil {
			t.Fatalf("got=%+v err=%v", got, err)
		}
	}
}
