package pauseresume

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs the pauseresume package's tests under a goroutine-leak
// detector. The Coordinator's TTL sweeper goroutine must be joined on
// shutdown; a leak is a real lifecycle bug, so the check runs with no
// ignore options.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
