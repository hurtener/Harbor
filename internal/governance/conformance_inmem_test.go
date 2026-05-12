package governance_test

import (
	"context"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/governance/conformancetest"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// TestGovernance_Conformance_InMem runs the canonical governance
// conformance suite against the in-mem StateStore. SQLite + Postgres
// drivers run the same suite from their own test packages (per AGENTS.md
// §9 three-driver conformance rule) — those land via `make preflight`'s
// state-driver suites.
func TestGovernance_Conformance_InMem(t *testing.T) {
	conformancetest.Run(t, func() conformancetest.Harness {
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
		st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
		if err != nil {
			_ = bus.Close(context.Background())
			t.Fatalf("state.Open: %v", err)
		}
		return conformancetest.Harness{
			State: st,
			Bus:   bus,
			Cleanup: func() {
				_ = st.Close(context.Background())
				_ = bus.Close(context.Background())
			},
		}
	})
}
