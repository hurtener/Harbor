package conformance_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/conformance"
)

// TestProtocol_Conformance is the package-local consumer of the
// conformance suite. It builds the default factory (which wires real
// drivers everywhere on the seam — see conformance.NewDefaultFactory)
// and runs every scenario as a subtest.
//
// This is the Phase 62 acceptance gate (master-plan §62: "go test
// ./internal/protocol/conformance/... exits 0"). A new Protocol
// method, error code, or capability constant landing without a
// corresponding scenario surfaces here as a t.Fatal — never silently.
func TestProtocol_Conformance(t *testing.T) {
	// Empty testdataRoot uses the package-relative default
	// `../auth/testdata`.
	conformance.RunSuite(t, conformance.NewDefaultFactory(""))
}
