package harbortest

// TestingT is the subset of *testing.T the kit's assertions need.
// Mirroring the standard testing.TB-ish shape lets callers pass
// either *testing.T or *testing.B; the kit's own self-tests use
// *testing.T directly.
//
// The kit deliberately avoids the full testing.TB interface
// because TB is sealed by the Go standard library — callers that
// want to drive the assertions from non-test code (e.g. a
// programmatic verification harness) can implement TestingT
// themselves with their own Helper / Errorf semantics.
type TestingT interface {
	// Helper marks the calling function as a helper so the test
	// runtime's failure reporting points at the call site, not at
	// the helper internals.
	Helper()
	// Errorf reports a non-fatal test failure. The assertion
	// helpers in this kit use Errorf (not Fatalf) so multiple
	// failures in one test surface together — this matches the
	// convention every Harbor integration test uses.
	Errorf(format string, args ...any)
}
