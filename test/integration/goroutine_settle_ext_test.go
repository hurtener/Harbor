package integration_test

import (
	"runtime"
	"testing"
	"time"
)

// eventuallyGoroutinesSettleExt is the package integration_test twin of
// eventuallyGoroutinesSettle (goroutine_settle_test.go, package integration).
// The directory hosts both an internal (integration) and external
// (integration_test) test package, which are distinct compilation units that
// cannot share unexported helpers — hence the twin.
//
// It polls until the live goroutine count drains to within tol of base, or a
// bounded deadline elapses. It is a real-time eventually-assertion (AGENTS.md
// §17.4), NOT a fixed sleep-as-synchronisation: a single instant sampled right
// after the workers join can read transiently over tolerance because shared
// server/bus teardown goroutines (SSE keepalives, HTTP handler unwinds,
// response-body readers) are still draining. Each attempt runs runtime.GC() —
// reaping finished-but-unscheduled goroutines and parking the poller so the
// exiting goroutines get scheduler time under load — then re-samples. It fails
// only if the count never drains within the deadline, so a genuine leak still
// fails the test.
func eventuallyGoroutinesSettleExt(t *testing.T, base, tol int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var got int
	for {
		runtime.GC()
		got = runtime.NumGoroutine()
		if got <= base+tol || !time.Now().Before(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got > base+tol {
		t.Errorf("goroutine leak: base=%d after=%d (did not drain within 5s, tol=%d)", base, got, tol)
	}
}
