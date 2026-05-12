package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/governance/conformancetest"
	"github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

// TestGovernance_Conformance_SQLite — Wave 7b audit FAIL #5 closes:
// runs the canonical governance state-conformance suite against the
// SQLite StateStore driver. README + master-plan promise three-driver
// conformance for `Kind=governance.cost` and `Kind=governance.bucket`;
// before this test, only the in-mem leg ran the suite.
func TestGovernance_Conformance_SQLite(t *testing.T) {
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
		dsn := filepath.Join(t.TempDir(), "governance-conformance.sqlite")
		st, err := sqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
		if err != nil {
			_ = bus.Close(context.Background())
			t.Fatalf("sqlite.New: %v", err)
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
