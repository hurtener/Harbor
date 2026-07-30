package artifactref_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hurtener/Harbor/internal/tools/artifactref"
)

// egressSiteAllowList records the files permitted to resolve an artifact
// into an OUTBOUND request body, and why.
//
// The list is expected to stay at ONE entry, for the same reason its
// substitution sibling is: an artifact value entering a third party's
// request body is the runtime's act at one tool boundary. A second entry
// is not a formality — it is a second place content leaves the runtime,
// and it should be argued for on that basis.
//
// What this list bounds, stated honestly: where the encoder is CALLED.
// It says nothing about where the encoder's OUTPUT travels — downstream
// of the encoder the value is data, and an AST walk over call sites
// cannot follow data flow. Two further mechanisms cover that: the
// carrier projects a reference through every serialisation door but
// MarshalJSON, and the integration suite walks every sink the raw
// arguments reach.
var egressSiteAllowList = map[string]string{
	filepath.Join("internal", "tools", "drivers", "mcp", "egress.go"): "the MCP driver resolves each mapped artifact parameter ONCE per dispatched call, ahead of the reliability shell so a retry neither re-reads the store nor multiplies the memory ceiling, and writes only into the DECODED argument map — the raw argument JSON is never rewritten, so the trajectory, the observation, the per-invocation content hash and the durable app tool-context record all keep carrying the artifact id the model authored",
}

// TestEgress_EverySiteIsRegistered is the production-side bound on the
// egress encoder: a resolved artifact value enters an outbound request
// body in exactly the reviewed place and nowhere else.
func TestEgress_EverySiteIsRegistered(t *testing.T) {
	root := moduleRoot(t)

	violations, files, err := artifactref.ScanEgressSites(root, egressSiteAllowList)
	if err != nil {
		t.Fatalf("ScanEgressSites: %v", err)
	}
	if files < 200 {
		t.Fatalf("the egress scan read only %d files under %s; a scan that reaches nothing cannot gate anything", files, root)
	}
	for _, v := range violations {
		t.Errorf("%s", v)
	}
}

// egressFixture renders a file that calls the egress encoder under the
// supplied import spec, so a test can vary how the package is bound.
func egressFixture(importSpec, qualifier string) string {
	return `package worker

import (
	"context"

	` + importSpec + `
)

func send(ctx context.Context, args map[string]any, m ` + qualifier + `.Mapping) error {
	_, err := ` + qualifier + `.Encode(ctx, args, m, "t", 1)
	return err
}
`
}

// egressValueFixture renders a file that takes the encoder as a VALUE
// rather than calling it — a second egress site reachable through the
// function value, which a call-position-only scan would read as a call
// to `f`.
func egressValueFixture() string {
	return `package worker

import (
	"github.com/hurtener/Harbor/internal/tools/artifactegress"
)

var encoder = artifactegress.Encode
`
}

// TestEgress_ScanIsNonVacuous proves the scan bites, and that
// registering the site clears it. Without this pin a scan that matched
// nothing would look exactly like a codebase with one call site.
func TestEgress_ScanIsNonVacuous(t *testing.T) {
	dir := t.TempDir()
	src := egressFixture(`"github.com/hurtener/Harbor/internal/tools/artifactegress"`, "artifactegress")
	if err := os.WriteFile(filepath.Join(dir, "worker.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := artifactref.ScanEgressSites(dir, nil)
	if err != nil {
		t.Fatalf("ScanEgressSites: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %d, want 1; got %v", len(violations), violations)
	}
	if violations[0].Kind != artifactref.KindUnregisteredEgress {
		t.Fatalf("kind = %q, want %q", violations[0].Kind, artifactref.KindUnregisteredEgress)
	}
	violations, _, err = artifactref.ScanEgressSites(dir, map[string]string{"worker.go": "fixture"})
	if err != nil {
		t.Fatalf("ScanEgressSites: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("registering the site left %d violations: %v", len(violations), violations)
	}
}

// TestEgress_ASecondCallSiteFailsTheScan is the mutation the bound is
// actually defended against: an ALREADY-registered file stays clean
// while a NEW one does not.
func TestEgress_ASecondCallSiteFailsTheScan(t *testing.T) {
	dir := t.TempDir()
	src := egressFixture(`"github.com/hurtener/Harbor/internal/tools/artifactegress"`, "artifactegress")
	if err := os.WriteFile(filepath.Join(dir, "registered.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	allow := map[string]string{"registered.go": "the reviewed site"}

	violations, _, err := artifactref.ScanEgressSites(dir, allow)
	if err != nil {
		t.Fatalf("ScanEgressSites: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("the registered site alone produced %d violations: %v", len(violations), violations)
	}

	if err := os.WriteFile(filepath.Join(dir, "second.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write second fixture: %v", err)
	}
	violations, _, err = artifactref.ScanEgressSites(dir, allow)
	if err != nil {
		t.Fatalf("ScanEgressSites: %v", err)
	}
	if len(violations) != 1 || violations[0].File != "second.go" {
		t.Fatalf("a second egress site produced %v, want exactly one violation on second.go", violations)
	}
}

// TestEgress_StaleRegistrationIsReported keeps the allow-list a
// DESCRIPTION of the code rather than a wish about it — the direction
// that catches a removed call site as readily as an added one.
func TestEgress_StaleRegistrationIsReported(t *testing.T) {
	dir := t.TempDir()
	violations, _, err := artifactref.ScanEgressSites(dir, map[string]string{"gone.go": "a site that no longer exists"})
	if err != nil {
		t.Fatalf("ScanEgressSites: %v", err)
	}
	if len(violations) != 1 || violations[0].Kind != artifactref.KindUnregisteredEgress {
		t.Fatalf("violations = %v, want one stale-registration report", violations)
	}
}

// TestEgress_BlankReasonIsReported — an egress site nobody justified is
// an egress site nobody reviewed.
func TestEgress_BlankReasonIsReported(t *testing.T) {
	dir := t.TempDir()
	src := egressFixture(`"github.com/hurtener/Harbor/internal/tools/artifactegress"`, "artifactegress")
	if err := os.WriteFile(filepath.Join(dir, "worker.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := artifactref.ScanEgressSites(dir, map[string]string{"worker.go": "   "})
	if err != nil {
		t.Fatalf("ScanEgressSites: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %v, want the blank-reason report", violations)
	}
}

// TestEgress_ScanFollowsAnImportAlias — renaming an import is an
// ordinary refactor, and a scan keyed on the literal qualifier would
// quietly stop seeing the file that did it.
func TestEgress_ScanFollowsAnImportAlias(t *testing.T) {
	dir := t.TempDir()
	src := egressFixture(`eg "github.com/hurtener/Harbor/internal/tools/artifactegress"`, "eg")
	if err := os.WriteFile(filepath.Join(dir, "aliased.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := artifactref.ScanEgressSites(dir, nil)
	if err != nil {
		t.Fatalf("ScanEgressSites: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("an aliased import evaded the egress scan: %v", violations)
	}
}

// TestEgress_ValueReferenceIsReportedEvenWhenRegistered — a bound
// counted in call positions is no bound once the function value can
// travel, so the writer taken as a value is reported wherever it
// appears, INCLUDING from a file on the reviewed list.
func TestEgress_ValueReferenceIsReportedEvenWhenRegistered(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hoisted.go"), []byte(egressValueFixture()), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := artifactref.ScanEgressSites(dir, map[string]string{"hoisted.go": "registered, but that authorises a CALL, not the writer as a value"})
	if err != nil {
		t.Fatalf("ScanEgressSites: %v", err)
	}
	if len(violations) == 0 {
		t.Fatalf("a value reference from a registered file was excused; the list hands out the writer instead of registering a site")
	}
	found := false
	for _, v := range violations {
		if v.Kind == artifactref.KindEgressValueRef {
			found = true
		}
	}
	if !found {
		t.Fatalf("violations = %v, want a %s", violations, artifactref.KindEgressValueRef)
	}
}

// TestEgress_ScanIgnoresAFileThatImportsOnlyTheSubstitutionPackage
// pins the separation: the two writers live in two packages, and a file
// that imports one cannot call the other. It is also why the egress
// encoder does NOT live in this package — a file inside the scanned
// package is invisible to its own scan.
func TestEgress_ScanIgnoresAFileThatImportsOnlyTheSubstitutionPackage(t *testing.T) {
	dir := t.TempDir()
	src := substitutionFixture(`"github.com/hurtener/Harbor/internal/tools/artifactref"`, "artifactref")
	if err := os.WriteFile(filepath.Join(dir, "inproc.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := artifactref.ScanEgressSites(dir, nil)
	if err != nil {
		t.Fatalf("ScanEgressSites: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("the egress scan flagged a substitution-only file: %v", violations)
	}
}

// TestEgress_PkgPathIsTheEgressPackage guards the constant the whole
// scan hangs on. A typo here would make every scan return early on every
// file and report nothing — a gate that passes by seeing nothing.
func TestEgress_PkgPathIsTheEgressPackage(t *testing.T) {
	if artifactref.EgressPkgPath != "github.com/hurtener/Harbor/internal/tools/artifactegress" {
		t.Fatalf("EgressPkgPath = %q", artifactref.EgressPkgPath)
	}
	if artifactref.EgressPkgPath == artifactref.PkgPath {
		t.Fatalf("the two scans resolve the same package; they must bound two distinct writers")
	}
}
