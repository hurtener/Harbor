package engine

import (
	"testing"

	"go.uber.org/goleak"
)

// TestMain runs the engine package's tests under a goroutine-leak
// detector. The engine starts per-run worker goroutines and TTL
// sweepers; every one MUST be joined on Stop. A leak here is a real
// shutdown bug (the concurrent-reuse contract requires goroutines be
// joined before an invocation returns — CLAUDE.md §5/D-025), so the
// check runs with no ignore options.
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}
