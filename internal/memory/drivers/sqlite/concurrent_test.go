package sqlite_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/memory/conformancetest"
	memorydriver "github.com/hurtener/Harbor/internal/memory/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

// TestSQLite_Memory_Concurrent_BusyTimeoutAbsorbsContention exercises the cumulative owner through a real sqlite
// StateStore. The shared suite preserves N=128 concurrent identity-isolated callers,
// eight mutation/read/deletion cycles each, cancellation and leak assertions.
func TestSQLite_Memory_Concurrent_BusyTimeoutAbsorbsContention(t *testing.T) {

	conformancetest.Run(t, func() conformancetest.Harness {
		dsn := filepath.Join(t.TempDir(), "cumulative.sqlite")
		st, err := state.Open(t.Context(), config.StateConfig{Driver: "sqlite", DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		bus := buildBus(t)
		mem, err := memorydriver.New(memory.ConfigSnapshot{Driver: "sqlite", DSN: dsn, Strategy: memory.StrategyRollingSummary}, memory.Deps{State: st, Bus: bus, Redactor: cumulativeRedactor(t)})
		if err != nil {
			_ = st.Close(context.Background())
			t.Fatal(err)
		}
		return conformancetest.Harness{Store: mem, Bus: bus, Strategy: memory.StrategyRollingSummary, Cleanup: func() { _ = mem.Close(context.Background()); _ = st.Close(context.Background()) }}
	})
}
