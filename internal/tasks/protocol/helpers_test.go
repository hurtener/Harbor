package protocol_test

import (
	"context"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// newTestRegistry builds an in-process TaskRegistry over in-memory
// state + event drivers — the production-grade drivers, no mocks at the
// seam (CLAUDE.md §17.3). The returned bus is exposed so a test can
// subscribe to `audit.admin_scope_used` events; cleanup closes all
// three.
func newTestRegistry(t *testing.T) (tasks.TaskRegistry, events.EventBus) {
	t.Helper()
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem New: %v", err)
	}
	redactor := auditpatterns.New()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
		ReplayBufferSize:         1024,
	}, redactor)
	if err != nil {
		t.Fatalf("events inmem New: %v", err)
	}
	r, err := tasks.OpenDriver("inprocess", tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: redactor,
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("OpenDriver: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_ = r.Close(ctx)
		_ = bus.Close(ctx)
		_ = store.Close(ctx)
	})
	return r, bus
}

// idFor builds a complete identity triple for a tenant/user/session.
func idFor(tenant, user, session string) identity.Identity {
	return identity.Identity{TenantID: tenant, UserID: user, SessionID: session}
}

// ctxFor returns a context carrying the identity triple — the shape
// the TaskRegistry's identity-mandatory gate requires.
func ctxFor(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With(%v): %v", id, err)
	}
	return ctx
}

// seedTask spawns one task in the given identity and advances it to the
// requested status. It returns the minted TaskID.
func seedTask(t *testing.T, r tasks.TaskRegistry, id identity.Identity, kind tasks.TaskKind, status tasks.TaskStatus, desc, query string) tasks.TaskID {
	t.Helper()
	ctx := ctxFor(t, id)
	handle, err := r.Spawn(ctx, tasks.SpawnRequest{
		Identity:    identity.Quadruple{Identity: id},
		Kind:        kind,
		Description: desc,
		Query:       query,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	advance(t, r, ctx, handle.ID, status)
	return handle.ID
}

// advance drives a freshly-spawned (Pending) task to the target status
// through the FSM's legal transitions.
func advance(t *testing.T, r tasks.TaskRegistry, ctx context.Context, id tasks.TaskID, target tasks.TaskStatus) {
	t.Helper()
	switch target {
	case tasks.StatusPending:
		// already Pending
	case tasks.StatusRunning:
		mustMarkRunning(t, r, ctx, id)
	case tasks.StatusPaused:
		mustMarkRunning(t, r, ctx, id)
		if err := r.MarkPaused(ctx, id); err != nil {
			t.Fatalf("MarkPaused: %v", err)
		}
	case tasks.StatusComplete:
		mustMarkRunning(t, r, ctx, id)
		if err := r.MarkComplete(ctx, id, tasks.TaskResult{Value: []byte(`"done"`)}); err != nil {
			t.Fatalf("MarkComplete: %v", err)
		}
	case tasks.StatusFailed:
		mustMarkRunning(t, r, ctx, id)
		if err := r.MarkFailed(ctx, id, tasks.TaskError{Code: "tool_timeout", Message: "timed out"}); err != nil {
			t.Fatalf("MarkFailed: %v", err)
		}
	case tasks.StatusCancelled:
		if _, err := r.Cancel(ctx, id, "test cancel"); err != nil {
			t.Fatalf("Cancel: %v", err)
		}
	default:
		t.Fatalf("advance: unknown target status %q", target)
	}
}

func mustMarkRunning(t *testing.T, r tasks.TaskRegistry, ctx context.Context, id tasks.TaskID) {
	t.Helper()
	if err := r.MarkRunning(ctx, id); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
}
