package cardinalitylint

import (
	"strings"
	"testing"
)

// TestCardinalityLint_TelemetryTreeIsClean is the BUILD GATE: it runs
// ScanMetricsTree against the real internal/telemetry tree and fails CI
// on any violation. A future hand-rolled instrument that tags a metric
// by run_id / trace_id / the identity quadruple turns this test red.
//
// The scanned root is the parent of this package's directory —
// internal/telemetry — reached via "..". ScanMetricsTree skips its own
// cardinalitylint/ package (which necessarily names the forbidden
// strings) and testdata/.
func TestCardinalityLint_TelemetryTreeIsClean(t *testing.T) {
	violations, err := ScanMetricsTree("..")
	if err != nil {
		t.Fatalf("ScanMetricsTree(internal/telemetry): %v", err)
	}
	if len(violations) != 0 {
		var b strings.Builder
		b.WriteString("internal/telemetry has metrics-cardinality violations:\n")
		for _, v := range violations {
			b.WriteString("  ")
			b.WriteString(v.String())
			b.WriteString("\n")
		}
		t.Fatal(b.String())
	}
}

// TestScanMetricsTree_DetectsForbiddenLabelKey proves the checker
// catches a forbidden literal label KEY inside metric.WithAttributes.
func TestScanMetricsTree_DetectsForbiddenLabelKey(t *testing.T) {
	violations, err := ScanMetricsTree("testdata/badmetric")
	if err != nil {
		t.Fatalf("ScanMetricsTree(testdata/badmetric): %v", err)
	}
	if !hasKind(violations, KindForbiddenLabelKey) {
		t.Fatalf("expected a %s violation in the fixture, got: %v", KindForbiddenLabelKey, violations)
	}
}

// TestScanMetricsTree_DetectsIdentitySourcedLabel proves the checker
// catches a label VALUE pulled off an Identity field.
func TestScanMetricsTree_DetectsIdentitySourcedLabel(t *testing.T) {
	violations, err := ScanMetricsTree("testdata/badmetric")
	if err != nil {
		t.Fatalf("ScanMetricsTree(testdata/badmetric): %v", err)
	}
	if !hasKind(violations, KindIdentitySourcedLabel) {
		t.Fatalf("expected a %s violation in the fixture, got: %v", KindIdentitySourcedLabel, violations)
	}
}

// TestScanMetricsTree_FixtureHasExactlyTwoViolations pins that the
// checker flags ONLY the two metric.WithAttributes violations and does
// NOT flag the span-like attribute.KeyValue slice in the fixture (a
// non-metric attribute list legitimately carries run_id).
func TestScanMetricsTree_FixtureHasExactlyTwoViolations(t *testing.T) {
	violations, err := ScanMetricsTree("testdata/badmetric")
	if err != nil {
		t.Fatalf("ScanMetricsTree(testdata/badmetric): %v", err)
	}
	if len(violations) != 2 {
		t.Fatalf("expected exactly 2 violations (span-like attribute lists must NOT be flagged), got %d: %v",
			len(violations), violations)
	}
}

// TestScanMetricsTree_ViolationStringShape pins the file:line:kind:detail
// rendering — a test failure message joins these.
func TestScanMetricsTree_ViolationStringShape(t *testing.T) {
	violations, err := ScanMetricsTree("testdata/badmetric")
	if err != nil {
		t.Fatalf("ScanMetricsTree(testdata/badmetric): %v", err)
	}
	for _, v := range violations {
		s := v.String()
		if !strings.Contains(s, "badmetric.go:") {
			t.Errorf("violation string %q missing file:line prefix", s)
		}
		if !strings.Contains(s, v.Kind) {
			t.Errorf("violation string %q missing kind %q", s, v.Kind)
		}
	}
}

// TestScanMetricsTree_WalkErrorOnMissingRoot proves a bad root is a
// walk ERROR, distinct from a clean scan with zero violations.
func TestScanMetricsTree_WalkErrorOnMissingRoot(t *testing.T) {
	_, err := ScanMetricsTree("testdata/does-not-exist")
	if err == nil {
		t.Fatal("expected a walk error for a missing root, got nil")
	}
}

func hasKind(vs []Violation, kind string) bool {
	for _, v := range vs {
		if v.Kind == kind {
			return true
		}
	}
	return false
}
