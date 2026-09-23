package postgres_test

import (
	"errors"
	"testing"

	"go.uber.org/goleak"

	"github.com/hurtener/Harbor/internal/memory"
	memorypostgres "github.com/hurtener/Harbor/internal/memory/drivers/postgres"
)

func TestPostgres_RejectedStrategyDoesNotLeakPool(t *testing.T) {
	bus, store := buildDeps(t)
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())
	// Documented dummy DSN: an invalid strategy must fail before allocating or
	// contacting a database. The pgx database/sql opener itself owns a goroutine.
	cfg := memory.ConfigSnapshot{Driver: "postgres", DSN: "postgres://unused.invalid/unused", Strategy: "truncation"}
	for range 128 {
		if _, err := memorypostgres.New(cfg, memory.Deps{State: store, Bus: bus}); !errors.Is(err, memory.ErrStrategyNotImplemented) {
			t.Fatalf("removed strategy: %v", err)
		}
	}
}
