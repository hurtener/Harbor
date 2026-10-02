package telemetry_test

import (
	"fmt"
	"os"
	"testing"
)

// TestMain keeps the in-memory recorder fixtures independent of an enclosing
// host's sampling ratio. Production construction still honors operator settings;
// only this test process requests the SDK's default parent-based always-on policy.
func TestMain(m *testing.M) {
	if err := os.Setenv("OTEL_TRACES_SAMPLER", "parentbased_always_on"); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "configure deterministic test sampler:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
