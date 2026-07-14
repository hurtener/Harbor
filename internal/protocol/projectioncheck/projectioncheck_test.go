package projectioncheck_test

import (
	"reflect"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/projectioncheck"

	// Blank-import every projection surface so its init() self-registers
	// its ProjectionContract. The registered set must reflect the REAL
	// shipped surfaces — this is the §4.4/D-305 registry-gate shape applied
	// to projectors: a surface that ships without registering fails the
	// coverage half below.
	_ "github.com/hurtener/Harbor/internal/memory/protocol"
	_ "github.com/hurtener/Harbor/internal/runtime/flow/protocol"
	_ "github.com/hurtener/Harbor/internal/sessions/protocol"
	_ "github.com/hurtener/Harbor/internal/tasks/protocol"
	_ "github.com/hurtener/Harbor/internal/tools/protocol"
)

// TestGate_HalfA_NeverAssigned_AllSurfacesPass is the build-gating
// conformance test: every registered surface's probe, reflected, assigns
// every operated wire field (or allow-lists it with a reason). A regression
// that drops an assignment fails here.
func TestGate_HalfA_NeverAssigned_AllSurfacesPass(t *testing.T) {
	contracts := projectioncheck.Contracts()
	if len(contracts) == 0 {
		t.Fatal("no projection surfaces registered — the blank imports did not run init()")
	}
	for _, c := range contracts {
		if err := projectioncheck.ValidateContract(c); err != nil {
			t.Errorf("surface %q: contract invalid: %v", c.Surface, err)
			continue
		}
		violations, err := projectioncheck.CheckProbe(c)
		if err != nil {
			t.Errorf("surface %q: probe check errored: %v", c.Surface, err)
			continue
		}
		for _, v := range violations {
			t.Errorf("Half A FAIL: %s", v)
		}
	}
}

// TestGate_Coverage_EveryKnownSurfaceRegistered asserts the registered set
// covers the known set (mirroring events.RegisteredDrivers()/conformance
// parity, D-305). A new projection surface that ships without registering
// fails the build here.
func TestGate_Coverage_EveryKnownSurfaceRegistered(t *testing.T) {
	got := projectioncheck.RegisteredSurfaces()
	want := append([]string(nil), projectioncheck.KnownSurfaces...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("surface coverage mismatch:\n  registered: %v\n  known:      %v\n"+
			"a known surface that ships without registering, or a registered surface not in KnownSurfaces, fails the build", got, want)
	}
}

// TestGate_HalfB_EveryContractNamesProdWiringTest asserts Half-B coverage:
// every registered surface names a prod-wiring test. A registered surface
// with no prod-wiring test fails the build (the never-wired variant could
// otherwise pass Half A with a fake and ship zeros in prod).
func TestGate_HalfB_EveryContractNamesProdWiringTest(t *testing.T) {
	for _, c := range projectioncheck.Contracts() {
		if c.ProdWiringTest == "" {
			t.Errorf("Half B FAIL: surface %q declares no prod-wiring test", c.Surface)
		}
	}
}

// --- Non-vacuity self-tests (§17.8): a gate that cannot fail is a rubber
// stamp. Each synthetic surface proves a specific gate rule bites. ---

// probeRow is a synthetic wire row the non-vacuity probes reflect over.
type probeRow struct {
	Assigned   int64 `json:"assigned"`
	Unassigned int64 `json:"unassigned"`
}

func TestGate_HalfA_NonVacuous_UnassignedFieldFails(t *testing.T) {
	// A synthetic surface that declares a filter over `unassigned` but whose
	// projector never assigns it MUST produce a Half-A violation.
	c := projectioncheck.ProjectionContract{
		Surface:        "synthetic-unassigned",
		Probe:          func() any { return probeRow{Assigned: 7} }, // Unassigned left zero
		OperatedFields: []string{"assigned", "unassigned"},
		ProdWiringTest: "TestSynthetic",
	}
	violations, err := projectioncheck.CheckProbe(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(violations) != 1 || violations[0].Field != "unassigned" {
		t.Fatalf("expected exactly one violation on `unassigned`, got %+v", violations)
	}
}

func TestGate_HalfA_AllowListSuppressesReasonedOmission(t *testing.T) {
	// The same unassigned field, allow-listed with a reason, must NOT fail —
	// the allow-list is the honest escape hatch.
	c := projectioncheck.ProjectionContract{
		Surface:         "synthetic-allowlisted",
		Probe:           func() any { return probeRow{Assigned: 7} },
		OperatedFields:  []string{"assigned", "unassigned"},
		HonestOmissions: map[string]string{"unassigned": "backend lands in a later phase"},
		ProdWiringTest:  "TestSynthetic",
	}
	if err := projectioncheck.ValidateContract(c); err != nil {
		t.Fatalf("valid allow-listed contract rejected: %v", err)
	}
	violations, err := projectioncheck.CheckProbe(c)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("allow-listed omission still flagged: %+v", violations)
	}
}

func TestGate_EmptyReason_Fails(t *testing.T) {
	// An honest-omission entry with an empty reason is itself a gate failure
	// (anti-theater).
	c := projectioncheck.ProjectionContract{
		Surface:         "synthetic-empty-reason",
		Probe:           func() any { return probeRow{} },
		OperatedFields:  []string{"unassigned"},
		HonestOmissions: map[string]string{"unassigned": "   "},
		ProdWiringTest:  "TestSynthetic",
	}
	if err := projectioncheck.ValidateContract(c); err == nil {
		t.Fatal("expected ValidateContract to FAIL on an empty honest-omission reason")
	}
}

func TestGate_Bloat_OmissionForNonOperatedField_Fails(t *testing.T) {
	// An honest-omission for a field not in OperatedFields is bloat and fails.
	c := projectioncheck.ProjectionContract{
		Surface:         "synthetic-bloat",
		Probe:           func() any { return probeRow{Assigned: 1} },
		OperatedFields:  []string{"assigned"},
		HonestOmissions: map[string]string{"unassigned": "some reason"},
		ProdWiringTest:  "TestSynthetic",
	}
	if err := projectioncheck.ValidateContract(c); err == nil {
		t.Fatal("expected ValidateContract to FAIL on an honest-omission for a non-operated field (bloat)")
	}
}

func TestGate_HalfB_MissingProdWiringTest_Fails(t *testing.T) {
	c := projectioncheck.ProjectionContract{
		Surface:        "synthetic-no-wiring-test",
		Probe:          func() any { return probeRow{Assigned: 1} },
		OperatedFields: []string{"assigned"},
		ProdWiringTest: "",
	}
	if err := projectioncheck.ValidateContract(c); err == nil {
		t.Fatal("expected ValidateContract to FAIL on an empty ProdWiringTest (Half B coverage)")
	}
}

// enrichedRow models a read-time-enriched wire row: `count` is populated
// only when the enricher is wired.
type enrichedRow struct {
	Count int64 `json:"count"`
}

// assembleProjector models a production assembly: `withEnricher` toggles
// whether the `WithX` enricher is installed (as mux.go would). A forgotten
// WithX ships a zero `count`.
func assembleProjector(withEnricher bool) func() any {
	return func() any {
		row := enrichedRow{}
		if withEnricher {
			row.Count = 42
		}
		return row
	}
}

func TestGate_HalfB_NonVacuous_OmittedWithX_ShipsZero_Fails(t *testing.T) {
	// Half B closes the never-wired variant: when a prod-wiring test runs the
	// REAL assembly (assembleProjector), a build that WIRES the enricher
	// passes CheckProbe, but one whose assembly OMITS the WithX enricher ships
	// a zero field the same reflection catches — the exact tools bug.
	wired := projectioncheck.ProjectionContract{
		Surface:        "synthetic-wired",
		Probe:          assembleProjector(true),
		OperatedFields: []string{"count"},
		ProdWiringTest: "TestSynthetic",
	}
	unwired := projectioncheck.ProjectionContract{
		Surface:        "synthetic-unwired",
		Probe:          assembleProjector(false), // mux forgot WithX
		OperatedFields: []string{"count"},
		ProdWiringTest: "TestSynthetic",
	}
	if v, err := projectioncheck.CheckProbe(wired); err != nil || len(v) != 0 {
		t.Fatalf("prod-assembled projector WITH enricher should pass: violations=%+v err=%v", v, err)
	}
	v, err := projectioncheck.CheckProbe(unwired)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(v) != 1 || v[0].Field != "count" {
		t.Fatalf("prod assembly that omits WithX must FAIL Half B, got %+v", v)
	}
}
