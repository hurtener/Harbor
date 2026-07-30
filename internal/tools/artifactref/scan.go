package artifactref

// The substitution scan.
//
// The substitution invariant says where a resolved artifact value does
// NOT travel: not the trajectory, not the interleaved observation, not a
// canonical event payload, not an audit payload, not a log. An invariant
// of that shape is enforced most durably by bounding where the value is
// PRODUCED, because the set of places it might arrive grows with the
// runtime while the set of places it is created does not. Enumerating
// arrivals is the check that goes stale; bounding production is the
// check that stays true.
//
// So [Substitute] is the one call site, and this scan holds it to one.
// It resolves this package by IMPORT PATH rather than by the
// conventional qualifier, because renaming an import is an ordinary
// refactor and a scan keyed on the spelling would quietly stop seeing
// the file that did it. It carries a reasoned allow-list, and a list
// entry matching no call site is itself reported — so the list stays a
// description of the code rather than a wish about it.
//
// It also reports the writer named as a VALUE rather than called —
// assigned, passed, returned, stored — and reports it even from a file
// on the reviewed list. A bound counted in call positions is no bound
// at all once the function itself can travel: `var f =
// artifactref.Substitute` followed by `f(ctx, in)` is a second
// substitution site that a call-position scan reads as a call to `f`.
// The list registers a site that substitutes; it does not hand out the
// writer.
//
// The scan returns every violation it finds in one pass, plus the count
// of files it read, so a test can prove it reached its tree: a scanner
// that silently matched nothing is not a gate.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// PkgPath is the import path of this package — the home of the
// substitution writer the scan holds to a reviewed list.
const PkgPath = "github.com/hurtener/Harbor/internal/tools/artifactref"

// EgressPkgPath is the import path of the SIBLING package that holds
// the remote-egress encoder — the second writer that puts a resolved
// artifact value into a dispatched tool argument.
//
// It is a STRING here rather than an import for two reasons, and both
// are load-bearing:
//
//  1. This package must not import the egress package. The scan
//     resolves an import path textually, so a string costs nothing and
//     an import would create a dependency edge purely to name a
//     constant.
//  2. The scan returns early for a file that does not import the
//     package it resolves. A file INSIDE the scanned package is
//     therefore invisible to its own scan — so the egress encoder is
//     deliberately NOT housed here, where it would have been unscanned
//     by construction. The residual blind spot (a second call from
//     inside the egress package itself) is bounded by a test asserting
//     that package's non-test file set, so "the package is small
//     enough to read" is checked rather than asserted.
const EgressPkgPath = "github.com/hurtener/Harbor/internal/tools/artifactegress"

// KindUnregisteredEgress — a call to the remote-egress encoder from a
// site that is not on the reviewed list, or a list entry that names no
// such call.
const KindUnregisteredEgress = "unregistered_egress_site"

// KindEgressValueRef — the remote-egress encoder named as a VALUE
// rather than called. Same reasoning as [KindSubstitutionValueRef]: a
// bound counted in call positions is no bound once the function itself
// can travel.
const KindEgressValueRef = "egress_writer_value_reference"

// KindUnregisteredSubstitution — a call to the substitution writer from
// a site that is not on the reviewed list, or a list entry that names no
// such call.
const KindUnregisteredSubstitution = "unregistered_substitution_site"

// KindSubstitutionValueRef — the substitution writer named as a VALUE
// rather than called: assigned to a variable or field, passed as an
// argument, returned, or stored in a map. The bound this scan holds is
// on call sites, and a function value carries the call anywhere the
// value goes — so a reference outside call position is reported
// wherever it appears, INCLUDING from a file on the reviewed list. The
// list registers a site that substitutes; it does not hand out the
// writer.
const KindSubstitutionValueRef = "substitution_writer_value_reference"

// Violation is one breach found by the scan: the offending file
// (relative to the scanned root), the 1-based line, the kind of breach,
// and a detail that names the fix rather than only the mismatch.
type Violation struct {
	File   string
	Line   int
	Kind   string
	Detail string
}

// String renders a Violation as `file:line: kind: detail`.
func (v Violation) String() string {
	if v.Line == 0 {
		return fmt.Sprintf("%s: %s: %s", v.File, v.Kind, v.Detail)
	}
	return fmt.Sprintf("%s:%d: %s: %s", v.File, v.Line, v.Kind, v.Detail)
}

// ScanSubstitutionSites walks the Go source under root and flags every
// call to the substitution writer that is not on the reviewed list.
//
// allow maps a repo-relative file path to the reason that file is
// permitted to substitute. The list is short by construction: binding a
// resolved artifact value into a dispatched argument is the runtime's
// act at the tool boundary, and it happens in one place.
//
// Returns the violations and the number of Go files read.
func ScanSubstitutionSites(root string, allow map[string]string) ([]Violation, int, error) {
	return scanWriterSites(root, allow, writerScan{
		pkgPath:      PkgPath,
		pkgQualifier: "artifactref",
		writers:      map[string]struct{}{"Substitute": {}},
		unregistered: KindUnregisteredSubstitution,
		valueRef:     KindSubstitutionValueRef,
		blankReason:  "allow-list entry carries no reason; a substitution site nobody justified is a substitution site nobody reviewed",
		staleEntry:   "the substitution list registers this file, but it calls no substitution writer; remove the stale registration",
		valueDetail:  "%s.%s is referenced as a VALUE rather than called; the substitution bound is on call sites, and a function value carries the call past every reviewed site — call it directly at a registered site, or pass the reference id through instead",
		callDetail:   "%s.%s is called from a site that is not on the reviewed substitution list; a resolved artifact value entering a dispatched argument is dispatch-local by contract — register the site with the reason it is one, or pass the reference id through instead",
	})
}

// ScanEgressSites walks the Go source under root and flags every call to
// the remote-egress encoder that is not on the reviewed list.
//
// It is a SEPARATE scan from [ScanSubstitutionSites] rather than an
// extension of it, because the two writers live in two packages and the
// scan returns early for a file that imports neither. Widening the
// substitution scan's allow-list would have covered nothing on this
// path anyway: the MCP arm does not and cannot call [Substitute], which
// walks a Go type tree that a remote-authored input schema does not
// have.
//
// What this bounds, honestly: where the encoder is CALLED, not where
// its output travels. Downstream of the encoder the value is data, and
// an AST walk over call sites says nothing about data flow. It is one
// of three mechanisms — the others being the carrier's projection bound
// (a Payload serialises as a reference at every door but MarshalJSON)
// and an arrival check over every sink the raw arguments reach.
//
// allow maps a repo-relative file path to the reason that file is
// permitted to encode. Blank reasons and entries matching no call site
// are both reported, so the list stays a description of the code.
//
// Returns the violations and the number of Go files read.
func ScanEgressSites(root string, allow map[string]string) ([]Violation, int, error) {
	return scanWriterSites(root, allow, writerScan{
		pkgPath:      EgressPkgPath,
		pkgQualifier: "artifactegress",
		writers:      map[string]struct{}{"Encode": {}},
		unregistered: KindUnregisteredEgress,
		valueRef:     KindEgressValueRef,
		blankReason:  "allow-list entry carries no reason; an egress site nobody justified is an egress site nobody reviewed",
		staleEntry:   "the egress list registers this file, but it calls no egress writer; remove the stale registration",
		valueDetail:  "%s.%s is referenced as a VALUE rather than called; the egress bound is on call sites, and a function value carries the call past every reviewed site — call it directly at a registered site, or pass the artifact id through instead",
		callDetail:   "%s.%s is called from a site that is not on the reviewed egress list; resolving an artifact into an outbound request body is the runtime's act at ONE tool boundary — register the site with the reason it is one, or pass the artifact id through instead",
	})
}

// writerScan parameterises [scanWriterSites] over the two writers this
// package gates. One walker serves both because the mechanics are
// identical — resolve the package by import path, separate call
// positions from value references, reconcile against a reasoned
// allow-list — and only the vocabulary differs. A second copy of the
// walker would be a second thing to keep correct.
type writerScan struct {
	pkgPath      string
	pkgQualifier string
	writers      map[string]struct{}
	unregistered string
	valueRef     string
	blankReason  string
	staleEntry   string
	valueDetail  string
	callDetail   string
}

func scanWriterSites(root string, allow map[string]string, cfg writerScan) ([]Violation, int, error) {
	writers := cfg.writers
	fset := token.NewFileSet()
	var out []Violation
	files := 0
	used := map[string]bool{}

	for path, reason := range allow {
		if strings.TrimSpace(reason) == "" {
			out = append(out, Violation{path, 0, cfg.unregistered, cfg.blankReason})
		}
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			// `.claude` holds tooling state, including nested git
			// worktrees that carry a full copy of the module; scanning
			// those would report every call site once per checkout, so
			// the gate would fail on a machine that keeps one and pass in
			// a clean tree.
			case "testdata", "node_modules", ".git", ".claude", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := relOrSelf(root, path)
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		files++
		// Resolve the LOCAL NAME this package is bound to in the file
		// rather than assuming the conventional one: an alias is an
		// ordinary import, and a scan keyed on the literal qualifier
		// would stop seeing a file that renamed it. A file that does not
		// import the package cannot call the writer.
		local := importLocalName(f, cfg.pkgPath)
		if local == "" {
			return nil
		}
		// Collect the selectors that sit in CALL position first, so the
		// walk below can tell a call from a bare value reference. Keying
		// only on *ast.CallExpr saw calls and nothing else, which left
		// `var f = artifactref.Substitute` — a second substitution site
		// reachable through the value — invisible to a gate whose whole
		// job is to hold the count at one.
		calledSel := map[*ast.SelectorExpr]struct{}{}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
					calledSel[sel] = struct{}{}
				}
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != local {
				return true
			}
			if _, ok := writers[sel.Sel.Name]; !ok {
				return true
			}
			line := fset.Position(sel.Pos()).Line
			if _, isCall := calledSel[sel]; !isCall {
				// A value reference. Not excused by the allow-list: the
				// list authorises a call site, and a function value can
				// be called from anywhere it is handed to.
				out = append(out, Violation{rel, line, cfg.valueRef, fmt.Sprintf(
					cfg.valueDetail, cfg.pkgQualifier, sel.Sel.Name)})
				return true
			}
			if _, ok := allow[rel]; ok {
				used[rel] = true
				return true
			}
			out = append(out, Violation{rel, line, cfg.unregistered, fmt.Sprintf(
				cfg.callDetail, cfg.pkgQualifier, sel.Sel.Name)})
			return true
		})
		return nil
	})
	if err != nil {
		return nil, files, err
	}
	// A registration nobody exercises is a registration nobody reviews.
	stale := make([]string, 0, len(allow))
	for path := range allow {
		if !used[path] {
			stale = append(stale, path)
		}
	}
	sort.Strings(stale)
	for _, path := range stale {
		out = append(out, Violation{path, 0, cfg.unregistered, cfg.staleEntry})
	}
	sortViolations(out)
	return out, files, nil
}

// importLocalName returns the local name f binds importPath to, or ""
// when f does not import it in a callable form. An alias is followed; a
// blank import cannot call, and a dot import would need unqualified
// resolution the repository does not use.
func importLocalName(f *ast.File, importPath string) string {
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil || path != importPath {
			continue
		}
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				return ""
			}
			return imp.Name.Name
		}
		if i := strings.LastIndex(path, "/"); i >= 0 {
			return path[i+1:]
		}
		return path
	}
	return ""
}

// relOrSelf renders path relative to root, falling back to path.
func relOrSelf(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}

// sortViolations orders violations deterministically so a failure
// message reads the same on every run.
func sortViolations(v []Violation) {
	sort.Slice(v, func(i, j int) bool {
		if v[i].File != v[j].File {
			return v[i].File < v[j].File
		}
		if v[i].Line != v[j].Line {
			return v[i].Line < v[j].Line
		}
		return v[i].Detail < v[j].Detail
	})
}
