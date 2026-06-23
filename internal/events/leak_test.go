package events

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs the events package's tests under a goroutine-leak
// detector. The in-memory bus starts a reaper goroutine and per-test
// subscriptions; every goroutine MUST be joined on Close. A leak here is
// a real shutdown bug, so the check runs with no ignore options.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

// TestGoleak_CatchesADeliberateLeak proves the leak detector is not
// inert: against a baseline snapshot, a goroutine parked on an
// unsignalled channel MUST be reported by goleak.Find. Without this, a
// VerifyTestMain that never trips would give false assurance. The leaked
// goroutine is unblocked and joined before the test returns so it does
// not trip the package-level VerifyTestMain.
func TestGoleak_CatchesADeliberateLeak(t *testing.T) {
	baseline := goleak.IgnoreCurrent()
	block := make(chan struct{})
	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		<-block
		close(done)
	}()
	<-started
	if err := goleak.Find(baseline); err == nil {
		t.Error("goleak.Find did not flag the deliberately leaked goroutine — the detector is inert")
	}
	close(block)
	<-done
}
