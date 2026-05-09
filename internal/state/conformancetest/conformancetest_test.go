package conformancetest_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/conformancetest"

	// Side-effect: register the inmem driver so OpenDriver works.
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// TestRun_SelfApplied is the smallest possible consumer of the
// conformance suite: drives the inmem driver. If this fails, the
// suite is broken before any downstream driver can rely on it.
func TestRun_SelfApplied(t *testing.T) {
	conformancetest.Run(t, func() (state.StateStore, func()) {
		s, err := state.OpenDriver("inmem", config.StateConfig{Driver: "inmem"})
		if err != nil {
			t.Fatalf("OpenDriver: %v", err)
		}
		return s, func() { _ = s.Close(context.Background()) }
	})
}
