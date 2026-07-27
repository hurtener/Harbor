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

// KindUnregisteredSubstitution — a call to the substitution writer from
// a site that is not on the reviewed list, or a list entry that names no
// such call.
const KindUnregisteredSubstitution = "unregistered_substitution_site"

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
	writers := map[string]struct{}{"Substitute": {}}
	fset := token.NewFileSet()
	var out []Violation
	files := 0
	used := map[string]bool{}

	for path, reason := range allow {
		if strings.TrimSpace(reason) == "" {
			out = append(out, Violation{path, 0, KindUnregisteredSubstitution,
				"allow-list entry carries no reason; a substitution site nobody justified is a substitution site nobody reviewed"})
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
		local := importLocalName(f, PkgPath)
		if local == "" {
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
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
			if _, ok := allow[rel]; ok {
				used[rel] = true
				return true
			}
			out = append(out, Violation{rel, fset.Position(call.Pos()).Line, KindUnregisteredSubstitution, fmt.Sprintf(
				"artifactref.%s is called from a site that is not on the reviewed substitution list; a resolved artifact value entering a dispatched argument is dispatch-local by contract — register the site with the reason it is one, or pass the reference id through instead", sel.Sel.Name)})
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
		out = append(out, Violation{path, 0, KindUnregisteredSubstitution,
			"the substitution list registers this file, but it calls no substitution writer; remove the stale registration"})
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
