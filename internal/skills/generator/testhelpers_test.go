package generator_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	"github.com/hurtener/Harbor/internal/skills/generator"
	tcat "github.com/hurtener/Harbor/internal/tools"
)

// newTestBus opens an inmem events.EventBus with the canonical
// patterns redactor. Cleanup is registered on t.
func newTestBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// newTestStore opens a localdb SkillStore bound to bus, backed by a
// t.TempDir-scoped SQLite file so parallel tests don't share the
// process-wide `:memory:` cache. Cleanup is registered on t.
func newTestStore(t *testing.T, bus events.EventBus) skills.SkillStore {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "skills.sqlite")
	store, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: dsn},
		skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("localdb.New: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

// newTestDeps returns a generator.Deps with the inmem bus + canonical
// audit redactor.
func newTestDeps(t *testing.T, bus events.EventBus) generator.Deps {
	t.Helper()
	return generator.Deps{
		Bus:      bus,
		Redactor: auditpatterns.New(),
	}
}

// testIdentity returns a populated Quadruple. The defaults match
// Phase 38's helper for cross-suite consistency.
func testIdentity() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  "t-gen",
			UserID:    "u-gen",
			SessionID: "s-gen",
		},
		RunID: "r-gen",
	}
}

// ctxWithIdentity returns ctx carrying testIdentity().
func ctxWithIdentity(t *testing.T) context.Context {
	t.Helper()
	q := testIdentity()
	ctx, err := identity.WithRun(context.Background(), q.Identity, q.RunID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	return ctx
}

// validDraft returns a SkillDraft that passes Skill.Validate.
func validDraft(name string) generator.SkillDraft {
	return generator.SkillDraft{
		Name:    name,
		Title:   "Title for " + name,
		Trigger: "trigger-" + name,
		Steps:   []string{"step one", "step two"},
		Tags:    []string{"generated", "test"},
	}
}

// newToolCatalog returns a fresh tools.ToolCatalog for test wiring.
func newToolCatalog() tcat.ToolCatalog {
	return tcat.NewCatalog()
}

// eventsFilterAllForIdentityRejected returns an Admin filter so the
// missing-identity emit path's event lands on the subscription
// regardless of the triple. The Phase 37 EmitIdentityRejected helper
// substitutes the missing-component sentinel for absent triple parts
// so the bus's ValidateEvent triple check passes; the Admin filter
// here matches across any identity.
func eventsFilterAllForIdentityRejected() events.Filter {
	return events.Filter{
		Admin: true,
		Types: []events.EventType{skills.EventTypeSkillIdentityRejected},
	}
}

// collectProposedEvents starts a subscription against bus filtered to
// the supplied identity triple AND the `skill.proposed` event type,
// then returns a function that drains and returns whatever has been
// delivered so far. The returned slice is a snapshot — call drain()
// again to observe additional events.
//
// Closes the subscription on test cleanup.
func collectProposedEvents(t *testing.T, bus events.EventBus, q identity.Quadruple) func() []events.Event {
	t.Helper()
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant:  q.TenantID,
		User:    q.UserID,
		Session: q.SessionID,
		Types:   []events.EventType{skills.EventTypeSkillProposed},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)
	return func() []events.Event {
		// Drain non-blockingly. The inmem bus's fan-out is
		// synchronous to publish, so by the time the caller's
		// Publish returns, the event is in the subscriber's
		// channel.
		out := make([]events.Event, 0)
		for {
			select {
			case ev, ok := <-sub.Events():
				if !ok {
					return out
				}
				out = append(out, ev)
			default:
				return out
			}
		}
	}
}
