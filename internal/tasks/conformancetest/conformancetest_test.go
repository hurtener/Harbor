package conformancetest_test

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/conformancetest"

	// Side-effect: register the inprocess driver so OpenDriver works.
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"

	// Production-shape dependency drivers (per AGENTS.md §17.3 —
	// no mocks at the seam). Imported as named packages because we
	// instantiate them directly (the registry seam takes a
	// `config.<X>Config`, not a registry name).
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// TestRun_SelfApplied is the smallest possible consumer of the
// conformance suite: drives the inprocess driver. If this fails,
// the suite is broken before any downstream driver can rely on it.
func TestRun_SelfApplied(t *testing.T) {
	conformancetest.Run(t, func() (tasks.TaskRegistry, func()) {
		store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
		if err != nil {
			t.Fatalf("state inmem New: %v", err)
		}
		// Use the production patterns redactor with its canonical
		// rule set — the conformance suite also implicitly verifies
		// that the redactor's deep walk doesn't break Description /
		// Query / Result round-trips.
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
		return r, func() {
			ctx := context.Background()
			_ = r.Close(ctx)
			_ = bus.Close(ctx)
			_ = store.Close(ctx)
		}
	})
}
