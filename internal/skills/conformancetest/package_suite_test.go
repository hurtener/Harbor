package conformancetest

import (
	"testing"
)

// TestRunPackageSemanticsSuite pins the canonical complete-skill-
// package semantic suite. The suite is the shared assertion set
// future package consumers inherit; running it here keeps it honest
// against the one canonical implementation.
func TestRunPackageSemanticsSuite(t *testing.T) {
	RunPackageSemanticsSuite(t)
}
