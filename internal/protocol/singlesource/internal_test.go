package singlesource

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// In-package tests for the unexported helpers — dirAllowsKind and
// isProtocolErrorsCodeType. They are small predicate functions whose
// every branch matters (a wrong answer either silences a real violation
// or false-positives a clean file), so each branch is pinned here.

func TestDirAllowsKind_AllBranches(t *testing.T) {
	cases := []struct {
		dir  string
		kind string
		want bool
	}{
		// Method literals: only methods/ is the canonical home.
		{"methods", KindMethodLiteral, true},
		{"types", KindMethodLiteral, false},
		{"", KindMethodLiteral, false},
		// Error codes: only errors/ is the canonical home.
		{"errors", KindErrorCode, true},
		{"methods", KindErrorCode, false},
		// KindWireType has a per-type home (CanonicalWireTypes), so
		// dirAllowsKind never permits it — the default branch.
		{"types", KindWireType, false},
		{"errors", KindWireType, false},
		// An unknown kind hits the default branch.
		{"methods", "not-a-kind", false},
	}
	for _, tc := range cases {
		if got := dirAllowsKind(tc.dir, tc.kind); got != tc.want {
			t.Errorf("dirAllowsKind(%q, %q) = %v, want %v", tc.dir, tc.kind, got, tc.want)
		}
	}
}

func TestIsProtocolErrorsCodeType_AllBranches(t *testing.T) {
	cases := []struct {
		name string
		src  string // a type expression, parsed as the type of a var
		want bool
	}{
		// A `pkg.Code` selector — the production shape (a const of type
		// protoerrors.Code outside the errors package).
		{"selector .Code", "var x protoerrors.Code", true},
		// A selector ending in something else is not a Code type.
		{"selector .Method", "var x methods.Method", false},
		// A bare `Code` ident — only reachable inside the errors package
		// itself, which the scan excludes before this is called, but the
		// branch is pinned regardless.
		{"bare Code ident", "var x Code", true},
		// A bare non-Code ident is not a Code type.
		{"bare non-Code ident", "var x string", false},
		// A composite type (slice, map, struct) hits the default branch.
		{"composite type", "var x []string", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			expr := parseVarType(t, tc.src)
			if got := isProtocolErrorsCodeType(expr); got != tc.want {
				t.Errorf("isProtocolErrorsCodeType(%s) = %v, want %v", tc.src, got, tc.want)
			}
		})
	}
}

// parseVarType parses a single `var x <type>` declaration and returns
// the type expression node.
func parseVarType(t *testing.T, decl string) ast.Expr {
	t.Helper()
	src := "package p\n" + decl + "\n"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "synthetic.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse %q: %v", decl, err)
	}
	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR {
			continue
		}
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if ok && vs.Type != nil {
				return vs.Type
			}
		}
	}
	t.Fatalf("no var-with-type declaration found in %q", decl)
	return nil
}
