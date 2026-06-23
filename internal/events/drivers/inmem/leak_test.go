package inmem

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs the in-memory bus driver's tests under a goroutine-leak
// detector. This is where the bus goroutines actually live: the
// idle-subscription reaper and the per-subscriber drain paths. Every one
// MUST be joined on Close (the reaper via reaperWG). A leak here is a
// real shutdown bug, so the check runs with no ignore options.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
