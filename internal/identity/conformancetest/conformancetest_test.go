package conformancetest_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/identity/conformancetest"
)

// TestRun_SelfApplied is the smallest possible consumer of the
// conformance suite: a context.Background() factory. If this fails,
// the suite is broken before any downstream subsystem can rely on it.
func TestRun_SelfApplied(t *testing.T) {
	conformancetest.Run(t, func() context.Context { return context.Background() })
}
