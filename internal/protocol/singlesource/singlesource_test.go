package singlesource_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/singlesource"
)

// protocolRoot is the internal/protocol directory, resolved relative to
// this test's location (internal/protocol/singlesource/).
func protocolRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve protocol root: %v", err)
	}
	return root
}

// TestSingleSource_ProtocolTreeIsClean is the binding Phase 58 lint: the
// internal/protocol tree carries NO single-source violation. A hardcoded
// Protocol method string outside internal/protocol/methods, a Protocol
// error code constant outside internal/protocol/errors, or a redeclared
// Protocol wire type outside internal/protocol/types fails this test —
// which gates the build via `make preflight` + CI (CLAUDE.md §8, §13;
// D-075).
//
// Every violation is reported in one run so an operator sees the full
// extent of any drift, not just the first breach.
func TestSingleSource_ProtocolTreeIsClean(t *testing.T) {
	violations, err := singlesource.ScanProtocolTree(protocolRoot(t))
	if err != nil {
		t.Fatalf("scan protocol tree: %v", err)
	}
	if len(violations) > 0 {
		lines := make([]string, len(violations))
		for i, v := range violations {
			lines[i] = v.String()
		}
		t.Fatalf("internal/protocol carries %d single-source violation(s) — "+
			"Protocol method names / error codes / wire types are single-sourced "+
			"(CLAUDE.md §8, §13; D-075):\n  %s",
			len(violations), strings.Join(lines, "\n  "))
	}
}

// TestSingleSource_ScannerReachesTheTree is a sanity gate: the scan MUST
// inspect a non-trivial number of files. A scan that walked an empty or
// moved tree would pass TestSingleSource_ProtocolTreeIsClean with zero
// files inspected — a silent pass is a drift signal, not a green light.
func TestSingleSource_ScannerReachesTheTree(t *testing.T) {
	root := protocolRoot(t)
	var goFiles int
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			if d.Name() == "vendor" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") {
			goFiles++
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk protocol tree: %v", walkErr)
	}
	// Phase 54 + Phase 58 ship at least: protocol.go, control.go,
	// errors.go, protocol_test.go, control_test.go, concurrent_test.go,
	// errors_internal_test.go, types/{version,control,types_test}.go,
	// methods/{methods,methods_test}.go, errors/{errors,errors_test}.go,
	// singlesource/{singlesource,singlesource_test}.go — 15+ files.
	if goFiles < 12 {
		t.Fatalf("protocol-tree walk found only %d .go files; expected at least 12 — "+
			"the clean-tree lint would silently pass on a moved or empty tree", goFiles)
	}
}

// TestSingleSource_DetectsMethodLiteral proves the checker actually
// catches a hardcoded method string — it scans a synthetic tree
// containing a method literal outside the methods package and asserts
// the violation surfaces with the right kind, file, and line.
func TestSingleSource_DetectsMethodLiteral(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "consumer/consumer.go", `package consumer

// Consumer hardcodes a Protocol method string — a §8 violation.
func Consumer() string {
	return "cancel"
}
`)
	violations, err := singlesource.ScanProtocolTree(dir)
	if err != nil {
		t.Fatalf("scan synthetic tree: %v", err)
	}
	requireOneViolation(t, violations, singlesource.KindMethodLiteral, "consumer/consumer.go")
}

// TestSingleSource_DetectsErrorCodeRedefinition proves the checker
// catches a Protocol error Code constant declared outside the errors
// package.
func TestSingleSource_DetectsErrorCodeRedefinition(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "shadow/shadow.go", `package shadow

import protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"

// CodeShadow is a forbidden second definition site for a Protocol error code.
const CodeShadow protoerrors.Code = "shadow"
`)
	violations, err := singlesource.ScanProtocolTree(dir)
	if err != nil {
		t.Fatalf("scan synthetic tree: %v", err)
	}
	requireOneViolation(t, violations, singlesource.KindErrorCode, "shadow/shadow.go")
}

// TestSingleSource_DetectsWireTypeRedefinition proves the checker
// catches a canonical Protocol wire type redeclared outside the types
// package.
func TestSingleSource_DetectsWireTypeRedefinition(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "shadow/shadow.go", `package shadow

// StartRequest redeclares a canonical Protocol wire type — a §13 violation.
type StartRequest struct {
	Spoofed string
}
`)
	violations, err := singlesource.ScanProtocolTree(dir)
	if err != nil {
		t.Fatalf("scan synthetic tree: %v", err)
	}
	requireOneViolation(t, violations, singlesource.KindWireType, "shadow/shadow.go")
}

// TestSingleSource_AllowsCanonicalPackages proves the checker does NOT
// flag the canonical definition sites themselves: a method literal in
// methods/, a Code const in errors/, a wire type in types/ are all
// permitted — that is the whole point of single-sourcing.
func TestSingleSource_AllowsCanonicalPackages(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "methods/methods.go", `package methods

type Method string

const MethodCancel Method = "cancel"
`)
	writeGo(t, dir, "errors/errors.go", `package errors

type Code string

const CodeNotFound Code = "not_found"
`)
	writeGo(t, dir, "types/types.go", `package types

type StartRequest struct {
	Query string
}
`)
	violations, err := singlesource.ScanProtocolTree(dir)
	if err != nil {
		t.Fatalf("scan synthetic tree: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("checker flagged the canonical definition sites — "+
			"single-sourcing means methods/ + errors/ + types/ ARE the homes; got: %v", violations)
	}
}

// TestSingleSource_DetectsMethodLiteralInTestFile proves the checker
// lints _test.go files too — a method string hardcoded in a test is the
// same drift as one hardcoded in production (the importgraph lint
// precedent treats test files identically).
func TestSingleSource_DetectsMethodLiteralInTestFile(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "consumer/consumer_test.go", `package consumer

import "testing"

func TestThing(t *testing.T) {
	if "redirect" == "" {
		t.Fatal("unreachable")
	}
}
`)
	violations, err := singlesource.ScanProtocolTree(dir)
	if err != nil {
		t.Fatalf("scan synthetic tree: %v", err)
	}
	requireOneViolation(t, violations, singlesource.KindMethodLiteral, "consumer/consumer_test.go")
}

// TestSingleSource_NoFalsePositiveOnNonProtocolCode proves the checker
// does not flag a non-method string that merely contains a canonical
// method name as a substring, nor a comment / struct tag mentioning a
// method name. Only an exact string-literal match is a method-literal
// violation.
func TestSingleSource_NoFalsePositiveOnNonProtocolCode(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "consumer/consumer.go", `package consumer

// This comment mentions cancel and pause and resume — not flagged.
type Thing struct {
	// The struct tag below contains "cancel" — a tag is not a string literal expression.
	Field string `+"`json:\"cancel_reason\"`"+`
}

// CancelishVerb returns a string that merely contains a method name as a substring.
func CancelishVerb() string {
	return "cancellation_requested"
}
`)
	violations, err := singlesource.ScanProtocolTree(dir)
	if err != nil {
		t.Fatalf("scan synthetic tree: %v", err)
	}
	if len(violations) != 0 {
		t.Fatalf("checker false-positived on non-Protocol code: %v", violations)
	}
}

// TestSingleSource_IgnoresNonGoFilesAndReportsSorted proves two
// properties in one synthetic tree: (a) non-.go files (a README, a JSON
// fixture) are skipped, not parsed; (b) when multiple violations exist
// across files, ScanProtocolTree returns them sorted by file then line —
// the deterministic ordering a test failure message relies on.
func TestSingleSource_IgnoresNonGoFilesAndReportsSorted(t *testing.T) {
	dir := t.TempDir()
	// Non-Go files — must be skipped silently (no parse, no violation).
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# mentions cancel and pause\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fixture.json"), []byte(`{"method":"resume"}`), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	// Two Go files, each with a method-literal violation — "b" sorts
	// after "a", so the scan must return a/... before b/...
	writeGo(t, dir, "b/b.go", "package b\n\nfunc B() string { return \"resume\" }\n")
	writeGo(t, dir, "a/a.go", "package a\n\nfunc A() string { return \"redirect\" }\n")

	violations, err := singlesource.ScanProtocolTree(dir)
	if err != nil {
		t.Fatalf("scan synthetic tree: %v", err)
	}
	if len(violations) != 2 {
		t.Fatalf("expected exactly 2 violations (the non-Go files must be skipped), got %d: %v", len(violations), violations)
	}
	if violations[0].File != "a/a.go" || violations[1].File != "b/b.go" {
		t.Fatalf("violations not sorted by file: got %q then %q", violations[0].File, violations[1].File)
	}
}

// TestSingleSource_ScanErrorsOnUnparseableSource proves a walk failure
// (an unparseable .go file) surfaces as an error, not a silent
// zero-violation pass — the checker fails loud (CLAUDE.md §5).
func TestSingleSource_ScanErrorsOnUnparseableSource(t *testing.T) {
	dir := t.TempDir()
	writeGo(t, dir, "broken/broken.go", "package broken\n\nthis is not valid go\n")
	_, err := singlesource.ScanProtocolTree(dir)
	if err == nil {
		t.Fatal("ScanProtocolTree returned nil error on an unparseable source file — must fail loud")
	}
	if !strings.Contains(err.Error(), "parse") {
		t.Fatalf("error %q does not mention the parse failure", err)
	}
}

// TestSingleSource_ViolationString pins the `file:line: kind: detail`
// rendering a test failure message joins.
func TestSingleSource_ViolationString(t *testing.T) {
	v := singlesource.Violation{
		File:   "consumer/consumer.go",
		Line:   42,
		Kind:   singlesource.KindMethodLiteral,
		Detail: "hardcoded Protocol method string \"cancel\"",
	}
	got := v.String()
	want := "consumer/consumer.go:42: method-literal: hardcoded Protocol method string \"cancel\""
	if got != want {
		t.Fatalf("Violation.String() = %q, want %q", got, want)
	}
}

// TestSingleSource_CanonicalMethodsInLockstep pins the duplication
// between singlesource.CanonicalMethods and internal/protocol/methods.
// The checker cannot import the methods package (it must be runnable
// against a tree where methods/ itself is under audit), so the set is
// duplicated — and this test fails the moment the two drift, which is
// exactly when a new Protocol method landed without updating the
// checker.
func TestSingleSource_CanonicalMethodsInLockstep(t *testing.T) {
	canonical := methods.Methods()
	canonicalSet := make(map[string]struct{}, len(canonical))
	for _, m := range canonical {
		canonicalSet[string(m)] = struct{}{}
	}

	// Missing in checker map: a method canonical in methods.Methods()
	// that has no entry in singlesource.CanonicalMethods.
	missing := make([]string, 0)
	for name := range canonicalSet {
		if _, ok := singlesource.CanonicalMethods[name]; !ok {
			missing = append(missing, name)
		}
	}

	// Extra in checker map: a method recorded in singlesource.CanonicalMethods
	// that is not in methods.Methods() — usually a stale entry left over
	// from a method rename or removal.
	extra := make([]string, 0)
	for name := range singlesource.CanonicalMethods {
		if _, ok := canonicalSet[name]; !ok {
			extra = append(extra, name)
		}
	}

	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("singlesource.CanonicalMethods drifted from internal/protocol/methods (D-075):\n"+
			"  missing in checker map: %v\n"+
			"  extra in checker map:   %v",
			missing, extra)
	}
}

// TestSingleSource_CanonicalWireTypesInLockstep pins the duplication
// between singlesource.CanonicalWireTypes and the wire types actually
// declared in internal/protocol/types + internal/protocol/errors. It
// parses the canonical packages and asserts every exported struct type
// they declare is in the checker's map under the RIGHT home package (and
// vice versa) — so a new wire type landing without updating the checker,
// or a wire type moving home, fails here.
func TestSingleSource_CanonicalWireTypesInLockstep(t *testing.T) {
	// declared maps each exported struct type name to the canonical
	// package directory that actually declares it.
	declared := map[string]string{}
	for name := range exportedStructTypes(t, filepath.Join(protocolRoot(t), "types")) {
		declared[name] = "types"
	}
	for name := range exportedStructTypes(t, filepath.Join(protocolRoot(t), "errors")) {
		declared[name] = "errors"
	}

	for name, home := range declared {
		recorded, ok := singlesource.CanonicalWireTypes[name]
		if !ok {
			t.Errorf("struct type %q is declared in internal/protocol/%s but missing from singlesource.CanonicalWireTypes (D-075)", name, home)
			continue
		}
		if recorded != home {
			t.Errorf("singlesource.CanonicalWireTypes records %q home as %q, but it is declared in internal/protocol/%s", name, recorded, home)
		}
	}
	for name := range singlesource.CanonicalWireTypes {
		if _, ok := declared[name]; !ok {
			t.Errorf("singlesource.CanonicalWireTypes lists %q but no canonical Protocol package declares it — stale checker entry", name)
		}
	}
}

// --- test helpers ---------------------------------------------------------

// writeGo writes src to rel under root, creating parent dirs. It is the
// fixture builder for the synthetic-tree detection tests.
func writeGo(t *testing.T, root, rel, src string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir for %q: %v", rel, err)
	}
	if err := os.WriteFile(full, []byte(src), 0o644); err != nil {
		t.Fatalf("write %q: %v", rel, err)
	}
}

// requireOneViolation asserts violations contains exactly one entry, of
// the given kind and file.
func requireOneViolation(t *testing.T, violations []singlesource.Violation, kind, file string) {
	t.Helper()
	if len(violations) != 1 {
		t.Fatalf("expected exactly 1 %s violation in %s, got %d: %v", kind, file, len(violations), violations)
	}
	v := violations[0]
	if v.Kind != kind {
		t.Errorf("violation kind = %q, want %q", v.Kind, kind)
	}
	if v.File != file {
		t.Errorf("violation file = %q, want %q", v.File, file)
	}
	if v.Line <= 0 {
		t.Errorf("violation line = %d, want a positive line number", v.Line)
	}
	if v.Detail == "" {
		t.Error("violation detail is empty")
	}
}

// exportedStructTypes parses every non-test .go file in dir and returns
// the set of exported struct type names it declares.
func exportedStructTypes(t *testing.T, dir string) map[string]struct{} {
	t.Helper()
	fset := token.NewFileSet()
	//nolint:staticcheck // SA1019: parser.ParseDir is adequate for this single-package, build-tag-agnostic AST scan of one source dir; the go/packages migration it points to is heavier than this test-helper warrants
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %q: %v", dir, err)
	}
	out := map[string]struct{}{}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				ts, ok := n.(*ast.TypeSpec)
				if !ok {
					return true
				}
				if !ts.Name.IsExported() {
					return true
				}
				if _, isStruct := ts.Type.(*ast.StructType); !isStruct {
					return true
				}
				out[ts.Name.Name] = struct{}{}
				return true
			})
		}
	}
	return out
}
