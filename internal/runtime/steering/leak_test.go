package steering

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs the steering package's tests under a goroutine-leak
// detector. The RunLoop and per-task drivers start goroutines that must
// be joined on shutdown; a leak is a real lifecycle bug, so the check
// runs with no ignore options.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
