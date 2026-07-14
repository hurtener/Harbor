package projectioncheck_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
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

// testFuncDeclRE matches a top-level Go test-function declaration so the
// name-resolution gate can prove a named prod-wiring test EXISTS.
var testFuncDeclRE = regexp.MustCompile(`(?m)^func\s+(\w+)\s*\(`)

// TestGate_HalfB_ProdWiringTestNamesResolve closes the NIT the wave-audit
// found: ValidateContract only asserts ProdWiringTest is NON-EMPTY, so a
// rename or delete of a named prod-wiring test leaves the contract green
// while its Half-B coverage silently evaporates. This gate mechanically
// resolves every registered ProdWiringTest name to a real `func <name>(`
// declaration in some `_test.go` under the module — a stale/renamed name
// fails the build here. (Existence, not invocation: whether the named test
// still ASSERTS the wiring is the per-surface test's own job; this gate
// guarantees the name is not a dangling reference.)
func TestGate_HalfB_ProdWiringTestNamesResolve(t *testing.T) {
	decls := testFuncDeclsInModule(t)
	for _, c := range projectioncheck.Contracts() {
		name := strings.TrimSpace(c.ProdWiringTest)
		if name == "" {
			continue // the non-empty rule is TestGate_HalfB_EveryContractNamesProdWiringTest
		}
		if !decls[name] {
			t.Errorf("Half B FAIL: surface %q names prod-wiring test %q, but no `func %s(` "+
				"declaration exists in any _test.go under the module — a renamed/deleted prod-wiring "+
				"test leaves the contract green while its never-wired coverage evaporates", c.Surface, name, name)
		}
	}
}

// TestGate_HalfB_NameResolution_NonVacuous proves the name-resolution gate
// BITES: a dangling prod-wiring-test name (one with no `func <name>(` in the
// tree) is absent from the declaration set, so the gate would flag it. A gate
// that cannot fail is a rubber stamp (§17.8).
func TestGate_HalfB_NameResolution_NonVacuous(t *testing.T) {
	decls := testFuncDeclsInModule(t)
	const bogus = "TestProdWiring_DoesNotExist_ ThisNameIsNeverDeclared"
	if decls[bogus] {
		t.Fatalf("a name that is never declared resolved to a func — the gate cannot bite")
	}
	// A real prod-wiring test name DOES resolve (proving the walk works).
	if !decls["TestProdWiring_TasksListThroughBuildMux"] {
		t.Fatal("the tasks prod-wiring test did not resolve — the module walk is broken")
	}
}

// testFuncDeclsInModule walks the module tree and returns the set of every
// top-level function name declared in a `_test.go` file. It is used by the
// Half-B name-resolution gate to prove each registered ProdWiringTest name
// is a live reference.
func testFuncDeclsInModule(t *testing.T) map[string]bool {
	t.Helper()
	root := moduleRoot(t)
	decls := make(map[string]bool, 4096)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Skip vendored/VCS/build dirs — no Harbor test funcs live there.
			switch d.Name() {
			case ".git", "vendor", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, m := range testFuncDeclRE.FindAllSubmatch(src, -1) {
			decls[string(m[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module tree for test-func declarations: %v", err)
	}
	if len(decls) == 0 {
		t.Fatal("no _test.go function declarations found under the module root — the name-resolution gate cannot run")
	}
	return decls
}

// moduleRoot walks up from the test's working directory to the directory
// holding go.mod (the module root).
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("reached filesystem root without finding go.mod — cannot locate the module root")
		}
		dir = parent
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
