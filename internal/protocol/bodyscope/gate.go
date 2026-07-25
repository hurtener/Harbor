package bodyscope

// The lockstep gate.
//
// A single shared reconciler closes the instances of a body-identity
// drift; it does not close the way new ones arrive. New ones arrive by
// copy: a contributor writes the next handler, reaches for the nearest
// example, copies the comparison and the comment that explains it, and
// does not copy the entitlement check the comment describes. Three
// helpers arrived exactly that way.
//
// The gate is the answer. It is three mechanical scans, each modelled on
// an idiom the repository already trusts (the canonical-wire-type
// lockstep, the driver-conformance coverage gate, the import-graph AST
// lint):
//
//   - ScanWireTypes — COVERAGE. Every canonical Protocol request type
//     that carries an identity scope must join to a registered surface.
//     Bidirectional, so a deleted registration fails as loudly as a new
//     unregistered type. A new surface is therefore registered or the
//     build is red; there is no third state.
//   - ScanHandRolledGates — ENFORCEMENT. No file outside this package
//     may compare a request body's identity component against a verified
//     identity component. That comparison IS the copy-paste shape, and
//     the scan makes helper number thirteen fail `go test` on the commit
//     that writes it.
//   - ScanElevationSites — MINTING. The functions that seat a verified
//     identity or cross the tenant boundary have a closed, reviewed call
//     list. A new call site fails until it is registered with a reason.
//
// Each scan returns every violation it finds in one pass, plus the count
// of files it read so a test can prove the scan reached its tree — a
// scanner that silently matched nothing is not a gate.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Violation is one breach found by a gate scan: the offending file
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

// Violation kinds.
const (
	// KindUnregisteredRequest — a canonical Protocol request type carries
	// an identity scope but no surface governs it.
	KindUnregisteredRequest = "unregistered_request_type"
	// KindUnregisteredCarrier — a canonical wire type carries an identity
	// scope, is not a request, and is not a reviewed exempt carrier.
	KindUnregisteredCarrier = "unregistered_scope_carrier"
	// KindStaleRegistration — a registry row names a wire type the
	// canonical packages no longer declare.
	KindStaleRegistration = "stale_registration"
	// KindUnknownSurface — a registry row names a surface with no policy.
	KindUnknownSurface = "unknown_surface"
	// KindUnusedSurface — a surface declares a policy no request type
	// joins to.
	KindUnusedSurface = "unused_surface"
	// KindHandRolledGate — a body-identity comparison outside the shared
	// reconciler.
	KindHandRolledGate = "hand_rolled_body_scope_gate"
	// KindUnregisteredElevation — a call to a verified-identity or
	// elevation writer from a site that is not on the reviewed list.
	KindUnregisteredElevation = "unregistered_elevation_site"
)

// scopeFieldTypes are the canonical wire shapes that carry the isolation
// triple in a request body.
var scopeFieldTypes = map[string]struct{}{
	"IdentityScope": {},
	"ArtifactScope": {},
}

// tripleFields are the wire-shape component names.
var tripleFields = map[string]struct{}{
	"Tenant": {}, "User": {}, "Session": {},
}

// identityFields are the identity.Identity component names.
var identityFields = map[string]struct{}{
	"TenantID": {}, "UserID": {}, "SessionID": {},
}

// ScanWireTypes is the COVERAGE half of the gate. It parses the
// canonical wire-type tree at typesDir and reconciles what it finds
// against the surface registry, in both directions.
//
// # What it recognises, and what it does not
//
// A type is in scope when it carries a DIRECTLY-NAMED field of one of
// the canonical scope shapes (IdentityScope / ArtifactScope). Two
// shapes are deliberately NOT recognised, because Harbor's wire types
// do not use them today:
//
//   - a POINTER-typed scope field (*IdentityScope). The canonical
//     bodies carry the scope by value; the one pointer use is
//     IdentityScope's own Actor / Requester / Impersonating triplet,
//     which the impersonation gate owns rather than this one.
//   - a FLAT tenant/user/session triple declared inline on the request
//     instead of a named scope struct.
//
// A wire type introducing either shape would pass this scan without a
// registry row. If one lands, widen structCarriesScope in the same
// change — the boundary is stated here so the next author meets it as a
// documented limit rather than a silent hole.
//
// Returns the violations and the number of Go files read.
func ScanWireTypes(typesDir string) ([]Violation, int, error) {
	fset := token.NewFileSet()
	var out []Violation
	files := 0
	declared := map[string]bool{}

	err := filepath.WalkDir(typesDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		files++
		rel := relOrSelf(typesDir, path)
		ast.Inspect(f, func(n ast.Node) bool {
			ts, ok := n.(*ast.TypeSpec)
			if !ok || !ts.Name.IsExported() {
				return true
			}
			st, ok := ts.Type.(*ast.StructType)
			if !ok || st.Fields == nil {
				return true
			}
			if !structCarriesScope(st) {
				return true
			}
			name := ts.Name.Name
			declared[name] = true
			line := fset.Position(ts.Pos()).Line
			if strings.HasSuffix(name, "Request") {
				surface, ok := requestSurfaces[name]
				if !ok {
					out = append(out, Violation{rel, line, KindUnregisteredRequest, fmt.Sprintf(
						"%s carries a body identity scope but no surface governs it; add a requestSurfaces row naming the surface whose policy applies", name)})
					return true
				}
				if _, ok := policies[surface]; !ok {
					out = append(out, Violation{rel, line, KindUnknownSurface, fmt.Sprintf(
						"%s joins to surface %q, which declares no policy; add it to the policies table", name, surface)})
				}
				return true
			}
			why, exempt := nonRequestScopeCarriers[name]
			switch {
			case !exempt:
				out = append(out, Violation{rel, line, KindUnregisteredCarrier, fmt.Sprintf(
					"%s carries an identity scope and is not a request; if the runtime authors that scope, add it to nonRequestScopeCarriers with the reason, otherwise rename it to a Request and join it to a surface", name)})
			case strings.TrimSpace(why) == "":
				// An exemption REMOVES a type from the gate's universe, so a
				// reasonless one is the most consequential of the three
				// allow-lists: it is the only way to make the gate look at
				// less without anyone saying why.
				out = append(out, Violation{rel, line, KindUnregisteredCarrier, fmt.Sprintf(
					"%s is exempted from the join with no reason; an exemption narrows what the gate inspects and must say why", name)})
			}
			return true
		})
		return nil
	})
	if err != nil {
		return nil, files, err
	}

	// Reverse direction: a registry row whose wire type no longer exists.
	for _, name := range JoinedRequestTypes() {
		if !declared[name] {
			out = append(out, Violation{"registry", 0, KindStaleRegistration, fmt.Sprintf(
				"requestSurfaces names %q, which the canonical wire types no longer declare; remove the stale row", name)})
		}
	}
	exempt := make([]string, 0, len(nonRequestScopeCarriers))
	for name := range nonRequestScopeCarriers {
		exempt = append(exempt, name)
	}
	sort.Strings(exempt)
	for _, name := range exempt {
		if !declared[name] {
			out = append(out, Violation{"registry", 0, KindStaleRegistration, fmt.Sprintf(
				"nonRequestScopeCarriers names %q, which the canonical wire types no longer declare; remove the stale row", name)})
		}
	}

	// A surface nothing joins to is a posture nobody reads.
	used := map[Surface]bool{}
	for _, s := range requestSurfaces {
		used[s] = true
	}
	for _, s := range RegisteredSurfaces() {
		if !used[s] {
			out = append(out, Violation{"registry", 0, KindUnusedSurface, fmt.Sprintf(
				"surface %q declares a policy no request type joins to; either join its request types or remove the policy", s)})
		}
	}

	sortViolations(out)
	return out, files, nil
}

// structCarriesScope reports whether a struct has a field whose type is
// one of the canonical body identity-scope shapes.
func structCarriesScope(st *ast.StructType) bool {
	for _, fld := range st.Fields.List {
		id, ok := fld.Type.(*ast.Ident)
		if !ok {
			continue
		}
		if _, ok := scopeFieldTypes[id.Name]; ok {
			return true
		}
	}
	return false
}

// ScanHandRolledGates is the ENFORCEMENT half of the gate. It walks the
// Go source under root and flags every comparison that reconciles a
// request body's identity component against a verified identity — the
// shape a hand-written body-scope helper always takes:
//
//	body.Tenant != verified.TenantID       // wire component vs identity component
//	body.Session != verified.Session       // wire component vs wire component
//
// allow maps a repo-relative file path to the reason it is exempt. An
// exemption is a reviewed decision, so it carries prose, not a bare
// path; an empty reason is itself a violation.
//
// # Stated limits
//
// The scan reads through a ONE-level hoist and follows an aliased
// `strings` import, which covers the refactors a contributor performs by
// habit. It does NOT resolve a two-level hoist (`b := a; b != x`), a
// `strings.ToLower(a) != strings.ToLower(b)` pair, or a comparison
// assembled through a function value. Those are deliberate evasions
// rather than accidents, and this scan is a copy-paste tripwire, not the
// control: the control is checkTenantMove, which runs at every write and
// cannot be evaded by any spelling. Widen the scan if one of these ever
// appears honestly.
//
// Returns the violations and the number of Go files read.
func ScanHandRolledGates(root string, allow map[string]string) ([]Violation, int, error) {
	fset := token.NewFileSet()
	var out []Violation
	files := 0

	for path, reason := range allow {
		if strings.TrimSpace(reason) == "" {
			out = append(out, Violation{path, 0, KindHandRolledGate,
				"allow-list entry carries no reason; an exemption without a stated reason is the comment-linked contract this gate exists to replace"})
		}
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel := relOrSelf(root, path)
		// This package IS the reconciler; the comparison lives here by
		// design.
		if strings.HasPrefix(rel, "bodyscope"+string(os.PathSeparator)) {
			return nil
		}
		if _, ok := allow[rel]; ok {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", path, perr)
		}
		files++
		// Resolve the local name `strings` is bound to in this file, so an
		// aliased import is followed rather than missed — the same
		// import-path resolution the minting scan performs.
		stringsLocal := importLocalName(f, "strings")

		// Locals are collected per function so a hoisted component is
		// resolved within the scope that hoisted it.
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			locals := collectComponentLocals(fn)
			ast.Inspect(fn, func(inner ast.Node) bool {
				switch v := inner.(type) {
				case *ast.BinaryExpr:
					if v.Op != token.EQL && v.Op != token.NEQ {
						return true
					}
					if detail, bad := reconciliationShape(v, locals); bad {
						out = append(out, Violation{rel, fset.Position(v.Pos()).Line, KindHandRolledGate, detail})
					}
				case *ast.CallExpr:
					if detail, bad := foldedComparisonShape(v, stringsLocal); bad {
						out = append(out, Violation{rel, fset.Position(v.Pos()).Line, KindHandRolledGate, detail})
					}
				}
				return true
			})
			return true
		})
		return nil
	})
	if err != nil {
		return nil, files, err
	}
	sortViolations(out)
	return out, files, nil
}

// reconciliationShape reports whether a comparison reconciles a request
// body's identity component against a verified identity component, and
// returns the detail naming the fix.
//
// It reads through a local: hoisting `bodyTenant := body.Tenant` before
// comparing is an ordinary refactor, and a scan that only saw
// selector-vs-selector would wave it through. resolveComponent walks the
// enclosing function's assignments to recover the component a plain
// identifier stands for.
func reconciliationShape(be *ast.BinaryExpr, locals map[string]string) (string, bool) {
	l, lok := resolveComponent(be.X, locals)
	r, rok := resolveComponent(be.Y, locals)
	if !lok || !rok {
		return "", false
	}
	_, lWire := tripleFields[l]
	_, rWire := tripleFields[r]
	_, lID := identityFields[l]
	_, rID := identityFields[r]

	// Wire component compared against an identity.Identity component.
	if (lWire && rID) || (lID && rWire) {
		return fmt.Sprintf(
			"comparison of %s against %s reconciles a body identity scope by hand; route it through bodyscope.Reconcile with the surface's registered policy", l, r), true
	}
	// The same component named on both sides, from different expressions.
	if lWire && rWire && l == r && !sameOrigin(be.X, be.Y) {
		return fmt.Sprintf(
			"comparison of two %s components reconciles a body identity scope by hand; route it through bodyscope.Reconcile with the surface's registered policy", l), true
	}
	return "", false
}

// resolveComponent names the identity component an expression stands
// for: a selector reads its field name directly; a plain identifier is
// looked up in the enclosing function's local assignments, so a hoisted
// `t := body.Tenant` is still seen as a Tenant.
func resolveComponent(e ast.Expr, locals map[string]string) (string, bool) {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		return v.Sel.Name, true
	case *ast.Ident:
		name, ok := locals[v.Name]
		return name, ok
	}
	return "", false
}

// sameOrigin reports whether two expressions read the same thing —
// `x.Tenant != x.Tenant` is not a reconciliation, and neither is a local
// compared against the selector it was hoisted from.
func sameOrigin(a, b ast.Expr) bool {
	return exprOrigin(a) == exprOrigin(b) && exprOrigin(a) != ""
}

// exprOrigin renders the base an expression reads from, for the
// same-origin check.
func exprOrigin(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		if base, ok := v.X.(*ast.Ident); ok {
			return base.Name + "." + v.Sel.Name
		}
	case *ast.Ident:
		return v.Name
	}
	return ""
}

// collectComponentLocals maps each local variable in fn that was
// assigned an identity component to that component's name, so the
// comparison scan reads through a hoist.
func collectComponentLocals(fn ast.Node) map[string]string {
	locals := map[string]string{}
	record := func(lhs, rhs []ast.Expr) {
		for i, l := range lhs {
			if i >= len(rhs) {
				break
			}
			ident, ok := l.(*ast.Ident)
			if !ok {
				continue
			}
			sel, ok := rhs[i].(*ast.SelectorExpr)
			if !ok {
				continue
			}
			name := sel.Sel.Name
			_, wire := tripleFields[name]
			_, id := identityFields[name]
			if wire || id {
				locals[ident.Name] = name
			}
		}
	}
	ast.Inspect(fn, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			record(v.Lhs, v.Rhs)
		case *ast.ValueSpec:
			lhs := make([]ast.Expr, 0, len(v.Names))
			for _, nm := range v.Names {
				lhs = append(lhs, nm)
			}
			record(lhs, v.Values)
		}
		return true
	})
	return locals
}

// foldedComparisonShape flags a case-insensitive comparison of two
// identity components. strings.EqualFold over a tenant is not a
// reconciliation Harbor performs — identity components are compared
// exactly — so its presence is either a hand-rolled gate or a
// correctness bug, and both want the same answer.
func foldedComparisonShape(call *ast.CallExpr, stringsLocal string) (string, bool) {
	if stringsLocal == "" {
		return "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "EqualFold" {
		return "", false
	}
	if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != stringsLocal {
		return "", false
	}
	if len(call.Args) != 2 {
		return "", false
	}
	for _, arg := range call.Args {
		s, ok := arg.(*ast.SelectorExpr)
		if !ok {
			continue
		}
		_, wire := tripleFields[s.Sel.Name]
		_, id := identityFields[s.Sel.Name]
		if wire || id {
			return fmt.Sprintf(
				"case-insensitive comparison of the %s component: identity components are compared EXACTLY, so route this through bodyscope.Reconcile rather than folding case by hand", s.Sel.Name), true
		}
	}
	return "", false
}

// ScanElevationSites is the MINTING half of the gate. It walks the Go
// source under root and flags every call to a writer of the verified
// identity or of an audited tenant crossing that is not on the reviewed
// list.
//
// allow maps a repo-relative file path to the reason that file is
// permitted to mint. The list is short by construction: seating a
// verified identity is a request-edge act, and crossing a tenant is an
// authorization act.
//
// Returns the violations and the number of Go files read.
func ScanElevationSites(root string, allow map[string]string) ([]Violation, int, error) {
	minters := map[string]struct{}{"WithVerified": {}, "WithElevated": {}}
	fset := token.NewFileSet()
	var out []Violation
	files := 0
	used := map[string]bool{}

	for path, reason := range allow {
		if strings.TrimSpace(reason) == "" {
			out = append(out, Violation{path, 0, KindUnregisteredElevation,
				"allow-list entry carries no reason; a minting site nobody justified is a minting site nobody reviewed"})
		}
	}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "testdata", "node_modules", ".git", "web":
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
		// The identity package declares the writers; it is not a caller.
		if rel == filepath.Join("internal", "identity", "identity.go") {
			return nil
		}
		// Resolve the LOCAL NAME the identity package is bound to in this
		// file rather than assuming the conventional one: an alias is an
		// ordinary import, and a scan keyed on the literal qualifier would
		// stop seeing a file that renamed it.
		local, imported := identityImportName(f)
		if !imported {
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
			if _, ok := minters[sel.Sel.Name]; !ok {
				return true
			}
			if _, ok := allow[rel]; ok {
				used[rel] = true
				return true
			}
			out = append(out, Violation{rel, fset.Position(call.Pos()).Line, KindUnregisteredElevation, fmt.Sprintf(
				"identity.%s is called from a site that is not on the reviewed minting list; seating a verified identity or crossing a tenant is an authorization act — register the site with the reason, or re-scope through identity.With instead", sel.Sel.Name)})
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
		out = append(out, Violation{path, 0, KindUnregisteredElevation,
			"the minting list registers this file, but it calls no verified-identity or elevation writer; remove the stale registration"})
	}
	sortViolations(out)
	return out, files, nil
}

// identityPkgPath is the import path of Harbor's identity package — the
// home of the writers the minting scan holds to a reviewed list.
const identityPkgPath = "github.com/hurtener/Harbor/internal/identity"

// identityImportName returns the local name the identity package is
// bound to in f, and whether f imports it at all. A file that aliases
// the import is still scanned under its alias; a file that does not
// import it cannot call the writers and is skipped.
func identityImportName(f *ast.File) (string, bool) {
	name := importLocalName(f, identityPkgPath)
	return name, name != ""
}

// importLocalName returns the local name f binds importPath to, or ""
// when f does not import it in a callable form. An alias is followed; a
// blank import cannot call, and a dot import would need unqualified
// resolution the repo does not use.
//
// Both scans resolve by import PATH rather than by the conventional
// qualifier: renaming an import is an ordinary refactor, and a scan keyed
// on the spelling would quietly stop seeing the file that did it.
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
		// No alias: the local name is the path's last segment.
		if i := strings.LastIndex(path, "/"); i >= 0 {
			return path[i+1:]
		}
		return path
	}
	return ""
}

// relOrSelf renders path relative to path root, falling back to path.
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
