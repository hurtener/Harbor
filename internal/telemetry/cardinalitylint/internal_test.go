package cardinalitylint

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// parseExpr is a small helper: parse a Go expression string to an
// ast.Expr so the unexported predicates can be exercised directly.
func parseExpr(t *testing.T, src string) ast.Expr {
	t.Helper()
	e, err := parser.ParseExpr(src)
	if err != nil {
		t.Fatalf("parser.ParseExpr(%q): %v", src, err)
	}
	return e
}

func TestIdentityFieldSelector_NestedSelector(t *testing.T) {
	// `ev.Identity.RunID` — a nested selector ending in a forbidden
	// field. The base must render as `ev.Identity`.
	field, base, ok := identityFieldSelector(parseExpr(t, "ev.Identity.RunID"))
	if !ok {
		t.Fatal("expected ev.Identity.RunID to be flagged")
	}
	if field != "RunID" {
		t.Errorf("field = %q, want RunID", field)
	}
	if base != "ev.Identity" {
		t.Errorf("base = %q, want ev.Identity", base)
	}
}

func TestIdentityFieldSelector_BareIdentSelector(t *testing.T) {
	// `id.TraceID` — base is a bare identifier.
	field, base, ok := identityFieldSelector(parseExpr(t, "id.TraceID"))
	if !ok {
		t.Fatal("expected id.TraceID to be flagged")
	}
	if field != "TraceID" || base != "id" {
		t.Errorf("got (%q, %q), want (TraceID, id)", field, base)
	}
}

func TestIdentityFieldSelector_NonForbiddenField(t *testing.T) {
	// `ev.Type` is not an identity field — must not be flagged.
	if _, _, ok := identityFieldSelector(parseExpr(t, "ev.Type")); ok {
		t.Error("ev.Type should not be flagged as an identity-sourced label")
	}
}

func TestIdentityFieldSelector_NonSelector(t *testing.T) {
	// A bare identifier is not a selector — must not be flagged.
	if _, _, ok := identityFieldSelector(parseExpr(t, "someVar")); ok {
		t.Error("a bare identifier should not be flagged")
	}
}

func TestRenderSelectorBase_ExoticFallback(t *testing.T) {
	// A call-expression base (`f().RunID`) is not an Ident or a
	// SelectorExpr base shape renderSelectorBase handles — it must fall
	// back to the placeholder rather than panic.
	got := renderSelectorBase(parseExpr(t, "f()"))
	if got != "<identity>" {
		t.Errorf("renderSelectorBase(f()) = %q, want <identity>", got)
	}
}

func TestIsMetricWithAttributes(t *testing.T) {
	cases := []struct {
		src  string
		want bool
	}{
		{"metric.WithAttributes(a)", true},
		{"trace.WithAttributes(a)", false}, // span attrs — not in scope
		{"metric.WithDescription(a)", false},
		{"WithAttributes(a)", false}, // no package selector
	}
	for _, c := range cases {
		call, ok := parseExpr(t, c.src).(*ast.CallExpr)
		if !ok {
			t.Fatalf("%q did not parse to a CallExpr", c.src)
		}
		if got := isMetricWithAttributes(call.Fun); got != c.want {
			t.Errorf("isMetricWithAttributes(%q) = %v, want %v", c.src, got, c.want)
		}
	}
}

func TestScanAttributeCall_NonAttributeCallIgnored(t *testing.T) {
	// A call that is not an attribute.* constructor yields no
	// violations even nested inside metric.WithAttributes.
	call := parseExpr(t, `somepkg.Thing("run_id", x)`).(*ast.CallExpr)
	if v := scanAttributeCall(token.NewFileSet(), call, "x.go"); len(v) != 0 {
		t.Errorf("non-attribute call produced violations: %v", v)
	}
}
