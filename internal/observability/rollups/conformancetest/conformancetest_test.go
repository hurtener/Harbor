package conformancetest_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/observability/rollups"
	"github.com/hurtener/Harbor/internal/observability/rollups/conformancetest"
	"github.com/hurtener/Harbor/internal/observability/rollups/memstore"
)

// TestRun_SelfApplied is the smallest possible consumer of the conformance
// suite: it drives the in-memory reference implementation. If this fails,
// the suite itself is broken before any downstream driver (SQLite,
// Postgres) can rely on it — the same self-application precedent as the
// state and events conformance suites.
func TestRun_SelfApplied(t *testing.T) {
	conformancetest.Run(t, func() (rollups.Store, func()) {
		s := memstore.New()
		return s, func() { _ = s.Close(context.Background()) }
	})
}
