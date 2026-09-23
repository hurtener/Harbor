package postgres_test

import (
	"context"

	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/memory/conformancetest"
	memorydriver "github.com/hurtener/Harbor/internal/memory/drivers/postgres"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/postgres"
)

// TestPostgres_Memory_Concurrent exercises the cumulative owner through a real postgres
// StateStore. The shared suite preserves N=128 concurrent identity-isolated callers,
// eight mutation/read/deletion cycles each, cancellation and leak assertions.
func TestPostgres_Memory_Concurrent(t *testing.T) {
	baseDSN := requireDSN(t)
	conformancetest.Run(t, func() conformancetest.Harness {
		dsn := freshSchema(t, baseDSN)
		st, err := state.Open(t.Context(), config.StateConfig{Driver: "postgres", DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		bus := buildBus(t)
		mem, err := memorydriver.New(memory.ConfigSnapshot{Driver: "postgres", DSN: dsn, Strategy: memory.StrategyRollingSummary}, memory.Deps{State: st, Bus: bus, Redactor: cumulativeRedactor(t)})
		if err != nil {
			_ = st.Close(context.Background())
			t.Fatal(err)
		}
		return conformancetest.Harness{Store: mem, Bus: bus, Strategy: memory.StrategyRollingSummary, Cleanup: func() { _ = mem.Close(context.Background()); _ = st.Close(context.Background()) }}
	})
}
