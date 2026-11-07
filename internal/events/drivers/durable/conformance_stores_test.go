package durable

import (
	"context"
	"testing"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/conformancetest"
	"github.com/hurtener/Harbor/internal/state"
)

// TestDurable_Conformance_SQLiteAndPostgresStores runs the canonical
// EventBus conformance suite against the durable driver over the SQLite and
// real Postgres StateStores (the inmem store runs in conformance_test.go), so
// the v2 index is exercised through every mandatory driver.
func TestDurable_Conformance_SQLiteAndPostgresStores(t *testing.T) {
	for _, name := range []string{"sqlite", "postgres"} {
		t.Run(name, func(t *testing.T) {
			if name == "postgres" && postgresTestDSN() == "" {
				t.Skip("HARBOR_PG_DSN not set")
			}
			conformancetest.Run(t, func() conformancetest.Harness {
				return conformancetest.Harness{
					NewBus: func(t *testing.T) events.EventBus {
						t.Helper()
						var store state.StateStore
						if name == "sqlite" {
							store = newSQLiteStore(t)
						} else {
							store = newPostgresStore(t)
						}
						bus, err := New(context.Background(), conformanceCfg(), auditpatterns.New(), store)
						if err != nil {
							t.Fatalf("durable.New: %v", err)
						}
						t.Cleanup(func() { _ = bus.Close(context.Background()) })
						return bus
					},
					NewReplayDisabledBus: func(t *testing.T) events.EventBus {
						t.Helper()
						cfg := conformanceCfg()
						cfg.ReplayBufferSize = 0
						bus, err := New(context.Background(), cfg, auditpatterns.New(), nil, WithLogger(discardTestLogger()))
						if err != nil {
							t.Fatalf("durable.New: %v", err)
						}
						t.Cleanup(func() { _ = bus.Close(context.Background()) })
						return bus
					},
					NewBoundedRetentionBus: func(t *testing.T, capacity int) events.EventBus {
						t.Helper()
						cfg := conformanceCfg()
						cfg.ReplayBufferSize = capacity
						bus, err := New(context.Background(), cfg, auditpatterns.New(), nil, WithLogger(discardTestLogger()))
						if err != nil {
							t.Fatalf("durable.New: %v", err)
						}
						t.Cleanup(func() { _ = bus.Close(context.Background()) })
						return bus
					},
				}
			})
		})
	}
}

func conformanceCfg() config.EventsConfig {
	cfg := metadataCfg()
	cfg.SubscriberBufferSize = 256
	cfg.MaxSubscribersPerSession = 16
	return cfg
}
