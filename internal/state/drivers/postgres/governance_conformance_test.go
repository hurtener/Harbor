package postgres_test

import (
	"context"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/governance/conformancetest"
	"github.com/hurtener/Harbor/internal/state/drivers/postgres"
)

// TestGovernance_Conformance_Postgres — Wave 7b audit FAIL #5 closes:
// runs the canonical governance state-conformance suite against the
// Postgres StateStore driver. Same gating shape as
// `TestPostgres_Conformance` — skips locally when `HARBOR_PG_DSN` is
// unset; CI provides a postgres:16 service container.
//
// IMPORTANT — concurrency isolation: the governance conformance
// subtests run with `t.Parallel()` and the RestartSurvival subtest
// writes a cost record, closes the accumulator, then re-opens against
// the SAME state store to read the record back. Sharing one schema
// across all parallel subtests would let one subtest's TRUNCATE wipe
// another subtest's mid-test data. The factory therefore creates a
// FRESH schema per harness (per subtest), and `freshSchema`'s
// t.Cleanup drops it on subtest completion.
func TestGovernance_Conformance_Postgres(t *testing.T) {
	baseDSN := requireDSN(t)

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
		// Per-subtest schema — isolates each subtest's state writes
		// from sibling subtests running concurrently.
		dsn := freshSchema(t, baseDSN)
		st, err := postgres.New(config.StateConfig{Driver: "postgres", DSN: dsn})
		if err != nil {
			_ = bus.Close(context.Background())
			t.Fatalf("postgres.New: %v", err)
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
