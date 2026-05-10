package inprocess_test

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/conformancetest"

	// Production dependency drivers (per AGENTS.md §17.3).
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"

	// Side-effect: register the inprocess driver so OpenDriver works.
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// TestInprocess_Conformance runs the canonical conformance suite
// against the inprocess driver. Mirrors
// `internal/state/drivers/inmem/inmem_test.go`'s pattern — drivers
// other than the suite-self-applied test ALSO run the suite to lock
// down the contract from the consumer's side.
func TestInprocess_Conformance(t *testing.T) {
	conformancetest.Run(t, func() (tasks.TaskRegistry, func()) {
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
		return r, func() {
			ctx := context.Background()
			_ = r.Close(ctx)
			_ = bus.Close(ctx)
			_ = store.Close(ctx)
		}
	})
}
