package artifactref_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hurtener/Harbor/internal/tools/artifactref"
)

// substitutionSiteAllowList records the files permitted to bind a
// resolved artifact value into a dispatched tool argument, and why.
//
// The list is expected to stay at ONE entry. A resolved artifact value
// is dispatch-local: it enters the argument at the tool boundary and
// ends there. A second entry is not a formality — it is a second place a
// value that must not travel is created, and it should be argued for on
// that basis.
var substitutionSiteAllowList = map[string]string{
	filepath.Join("internal", "tools", "drivers", "inproc", "inproc.go"): "the in-process tool driver binds the resolved value onto the decoded argument value at the moment of invocation, inside the policy shell so a retried attempt re-resolves rather than reusing a prior attempt's bytes; the argument JSON is not rewritten, so the trajectory, the observation and every event payload keep carrying the reference id the model authored",
}

// moduleRoot walks up from the test's working directory to the module
// root so the scan reads the real tree rather than a fixture.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for range 8 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("module root not found above the test working directory")
	return ""
}

// TestSubstitution_EverySiteIsRegistered is the production-side bound on
// the substitution invariant: a resolved artifact value is produced in
// exactly the reviewed places and nowhere else.
func TestSubstitution_EverySiteIsRegistered(t *testing.T) {
	root := moduleRoot(t)

	violations, files, err := artifactref.ScanSubstitutionSites(root, substitutionSiteAllowList)
	if err != nil {
		t.Fatalf("ScanSubstitutionSites: %v", err)
	}
	if files < 200 {
		t.Fatalf("the substitution scan read only %d files under %s; a scan that reaches nothing cannot gate anything", files, root)
	}
	for _, v := range violations {
		t.Errorf("%s", v)
	}
}

// substitutionFixture renders a file that calls the substitution writer
// under the supplied import spec, so a test can vary how the package is
// bound.
func substitutionFixture(importSpec, qualifier string) string {
	return `package worker

import (
	"context"

	` + importSpec + `
)

type args struct {
	Doc ` + qualifier + `.Ref ` + "`json:\"doc\"`" + `
}

func bind(ctx context.Context, in *args) error {
	return ` + qualifier + `.Substitute(ctx, in)
}
`
}

// TestSubstitution_ScanIsNonVacuous proves the scan bites, and that
// registering the site clears it. Without this pin a scan that matched
// nothing would look exactly like a codebase with one call site.
func TestSubstitution_ScanIsNonVacuous(t *testing.T) {
	dir := t.TempDir()
	src := substitutionFixture(`"github.com/hurtener/Harbor/internal/tools/artifactref"`, "artifactref")
	if err := os.WriteFile(filepath.Join(dir, "worker.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := artifactref.ScanSubstitutionSites(dir, nil)
	if err != nil {
		t.Fatalf("ScanSubstitutionSites: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("violations = %d, want 1; got %v", len(violations), violations)
	}
	if violations[0].Kind != artifactref.KindUnregisteredSubstitution {
		t.Fatalf("kind = %q, want %q", violations[0].Kind, artifactref.KindUnregisteredSubstitution)
	}
	// Registering the site clears it.
	violations, _, err = artifactref.ScanSubstitutionSites(dir, map[string]string{"worker.go": "fixture"})
	if err != nil {
		t.Fatalf("ScanSubstitutionSites: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("registering the site left %d violations: %v", len(violations), violations)
	}
}

// TestSubstitution_ASecondCallSiteFailsTheScan is the mutation the
// invariant is actually defended against: an ALREADY-registered file
// stays clean while a NEW one does not, so the gate distinguishes "one
// site" from "any number of sites".
func TestSubstitution_ASecondCallSiteFailsTheScan(t *testing.T) {
	dir := t.TempDir()
	first := substitutionFixture(`"github.com/hurtener/Harbor/internal/tools/artifactref"`, "artifactref")
	if err := os.WriteFile(filepath.Join(dir, "registered.go"), []byte(first), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	allow := map[string]string{"registered.go": "the reviewed site"}

	violations, _, err := artifactref.ScanSubstitutionSites(dir, allow)
	if err != nil {
		t.Fatalf("ScanSubstitutionSites: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("the registered site alone produced %d violations: %v", len(violations), violations)
	}

	second := substitutionFixture(`"github.com/hurtener/Harbor/internal/tools/artifactref"`, "artifactref")
	if err := os.WriteFile(filepath.Join(dir, "second.go"), []byte(second), 0o600); err != nil {
		t.Fatalf("write second fixture: %v", err)
	}
	violations, _, err = artifactref.ScanSubstitutionSites(dir, allow)
	if err != nil {
		t.Fatalf("ScanSubstitutionSites: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("a second substitution site produced %d violations, want 1; got %v", len(violations), violations)
	}
	if violations[0].File != "second.go" {
		t.Fatalf("violation named %q, want second.go", violations[0].File)
	}
}

// TestSubstitution_AValueReferenceDoesNotEvadeTheScan — this is the
// exact evasion the call-position-only scan admitted: take the writer as
// a VALUE and call it through the variable, and a scan that keyed on
// *ast.CallExpr saw a call to `sneak`, not to `artifactref.Substitute`.
// A second, unregistered substitution site compiled and the gate stayed
// green — a bound on call sites that a function value walks straight
// past.
func TestSubstitution_AValueReferenceDoesNotEvadeTheScan(t *testing.T) {
	dir := t.TempDir()
	src := `package worker

import (
	"context"

	"github.com/hurtener/Harbor/internal/tools/artifactref"
)

var sneak = artifactref.Substitute

func Bind(ctx context.Context, in any) error {
	return sneak(ctx, in)
}
`
	if err := os.WriteFile(filepath.Join(dir, "sneak.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := artifactref.ScanSubstitutionSites(dir, nil)
	if err != nil {
		t.Fatalf("ScanSubstitutionSites: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("a value reference produced %d violations, want 1; got %v", len(violations), violations)
	}
	if violations[0].Kind != artifactref.KindSubstitutionValueRef {
		t.Fatalf("kind = %q, want %q", violations[0].Kind, artifactref.KindSubstitutionValueRef)
	}
	if violations[0].File != "sneak.go" {
		t.Fatalf("violation named %q, want sneak.go", violations[0].File)
	}
}

// TestSubstitution_AValueReferenceIsNotExcusedByTheAllowList — the
// reviewed list registers a SITE THAT SUBSTITUTES; it does not hand out
// the writer. A registered file that takes the writer as a value can
// pass it anywhere, so the bound the list is meant to hold would leave
// with it.
func TestSubstitution_AValueReferenceIsNotExcusedByTheAllowList(t *testing.T) {
	dir := t.TempDir()
	src := `package worker

import (
	"context"

	"github.com/hurtener/Harbor/internal/tools/artifactref"
)

type args struct {
	Doc artifactref.Ref ` + "`json:\"doc\"`" + `
}

// A legitimate registered call ...
func bind(ctx context.Context, in *args) error {
	return artifactref.Substitute(ctx, in)
}

// ... and a hand-off of the writer from the very same file.
func Escape() func(context.Context, any) error {
	return artifactref.Substitute
}
`
	if err := os.WriteFile(filepath.Join(dir, "registered.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := artifactref.ScanSubstitutionSites(dir,
		map[string]string{"registered.go": "the reviewed site"})
	if err != nil {
		t.Fatalf("ScanSubstitutionSites: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("a value hand-off from the registered file produced %d violations, want 1; got %v",
			len(violations), violations)
	}
	if violations[0].Kind != artifactref.KindSubstitutionValueRef {
		t.Fatalf("kind = %q, want %q", violations[0].Kind, artifactref.KindSubstitutionValueRef)
	}
}

// TestSubstitution_AnImportAliasDoesNotEvadeTheScan — renaming an import
// is an ordinary refactor, so a scan keyed on the conventional package
// qualifier would quietly stop seeing the file that did it.
func TestSubstitution_AnImportAliasDoesNotEvadeTheScan(t *testing.T) {
	dir := t.TempDir()
	src := substitutionFixture(`aref "github.com/hurtener/Harbor/internal/tools/artifactref"`, "aref")
	if err := os.WriteFile(filepath.Join(dir, "aliased.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := artifactref.ScanSubstitutionSites(dir, nil)
	if err != nil {
		t.Fatalf("ScanSubstitutionSites: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("an aliased import produced %d violations, want 1; got %v", len(violations), violations)
	}
}

// TestSubstitution_ALookalikeQualifierIsNotFlagged — the scan keys on the
// import path, so a local symbol that merely happens to be spelled
// `artifactref` is not a substitution site.
func TestSubstitution_ALookalikeQualifierIsNotFlagged(t *testing.T) {
	dir := t.TempDir()
	src := `package worker

import "context"

var artifactref struct {
	Substitute func(context.Context, any) error
}

func bind(ctx context.Context, in any) error {
	return artifactref.Substitute(ctx, in)
}
`
	if err := os.WriteFile(filepath.Join(dir, "lookalike.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := artifactref.ScanSubstitutionSites(dir, nil)
	if err != nil {
		t.Fatalf("ScanSubstitutionSites: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("a lookalike local symbol was flagged: %v", violations)
	}
}

// TestSubstitution_AStaleRegistrationIsReported — an allow-list entry
// that matches no call site is itself a violation, so the list stays a
// description of the code rather than a wish about it.
func TestSubstitution_AStaleRegistrationIsReported(t *testing.T) {
	dir := t.TempDir()
	src := `package worker

func nothing() {}
`
	if err := os.WriteFile(filepath.Join(dir, "quiet.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := artifactref.ScanSubstitutionSites(dir, map[string]string{
		"gone.go": "a site that used to substitute",
	})
	if err != nil {
		t.Fatalf("ScanSubstitutionSites: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("a stale registration produced %d violations, want 1; got %v", len(violations), violations)
	}
	if violations[0].File != "gone.go" || violations[0].Line != 0 {
		t.Fatalf("stale violation = %v, want gone.go with no line", violations[0])
	}
}

// TestSubstitution_AReasonlessRegistrationIsAViolation — a site nobody
// justified is a site nobody reviewed.
func TestSubstitution_AReasonlessRegistrationIsAViolation(t *testing.T) {
	dir := t.TempDir()
	src := substitutionFixture(`"github.com/hurtener/Harbor/internal/tools/artifactref"`, "artifactref")
	if err := os.WriteFile(filepath.Join(dir, "worker.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := artifactref.ScanSubstitutionSites(dir, map[string]string{"worker.go": "   "})
	if err != nil {
		t.Fatalf("ScanSubstitutionSites: %v", err)
	}
	if len(violations) != 1 {
		t.Fatalf("a reasonless registration produced %d violations, want 1; got %v", len(violations), violations)
	}
}

// TestSubstitution_TestFilesAreNotScanned — a test may drive the writer
// directly to exercise it; the invariant is about production dispatch.
func TestSubstitution_TestFilesAreNotScanned(t *testing.T) {
	dir := t.TempDir()
	src := substitutionFixture(`"github.com/hurtener/Harbor/internal/tools/artifactref"`, "artifactref")
	if err := os.WriteFile(filepath.Join(dir, "worker_test.go"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	violations, _, err := artifactref.ScanSubstitutionSites(dir, nil)
	if err != nil {
		t.Fatalf("ScanSubstitutionSites: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("a _test.go call site was flagged: %v", violations)
	}
}
