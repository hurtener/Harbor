// Package cardinalitylint is the Harbor metrics-cardinality enforcement
// checker (RFC §6.14; brief 06 "metrics cardinality footgun"). It is
// the Phase 56 mechanical gate behind the rule the predecessor learned
// the hard way and brief 06 calls out explicitly: a metric label must
// NEVER derive from a high-cardinality source — `run_id`, `trace_id`,
// `span_id`, `task_id`, or any value pulled off an `events.Event`'s
// `Identity` field. Tagging a metric by `trace_id` blows up the time
// series cardinality and takes down the metrics backend.
//
// # Two layers of defence
//
// The production boundary is closed by construction: telemetry's
// MetricsRegistry.RegisterEvent reads only `ev.Type` and the bounded
// `ev.Extra` keys — it has no code path that touches `ev.Identity`. This
// checker is the SECOND layer: it gates any FUTURE hand-rolled
// instrument (a per-subsystem latency histogram, a queue-depth gauge)
// so a contributor cannot add a `run_id`-labelled metric even if they
// bypass the RegisterEvent bridge.
//
// # Why a go/parser checker, not a golangci-lint analyzer or a script
//
// The repo already proves the pattern twice: internal/planner/conformance/
// importgraph_test.go and internal/protocol/singlesource/ are both
// go/parser AST walks that gate an invariant with zero external-tool
// dependency. Phase 56 reuses that shape. A golangci-lint plugin would
// need a separate build + a .golangci.yml entry (a new linter needs a
// PR rationale per CLAUDE.md §5). A shell grep could not be precise — a
// forbidden word inside a comment or an unrelated string is not a
// violation; only a real metric-label expression is. go/parser sees the
// AST, so the checker flags:
//
//   - KindForbiddenLabelKey — an attribute.String / attribute.Int / …
//     constructor call whose KEY argument (the first arg) is a string
//     literal matching a forbidden label name.
//   - KindIdentitySourcedLabel — an attribute.* constructor call whose
//     VALUE argument is a selector ending in a known Identity field
//     (`.RunID`, `.TraceID`, `.SpanID`, `.TaskID`, `.TenantID`,
//     `.UserID`, `.SessionID`) — i.e. a label value pulled off the run
//     quadruple.
//
// # Metric labels only — span attributes are NOT in scope
//
// A span attribute legitimately carries `run_id` (Phase 55 / D-073
// stamps the run quadruple onto event-derived spans on purpose — that
// is correct trace correlation). The cardinality rule is about METRIC
// labels, where a high-cardinality value blows up the time series
// count. Both spans and metrics build their attribute lists with the
// same `attribute.String(...)` constructors, so the checker
// distinguishes them by CONTEXT: it only flags an `attribute.*` call
// that is lexically nested inside a `metric.WithAttributes(...)` call.
// A span's `attribute.String("run_id", ...)` inside
// `trace.WithAttributes` is left alone; a metric's is a build-gating
// violation.
//
// # The checker is a reusable artifact (D-025)
//
// ScanMetricsTree and its helpers are pure functions over a filesystem
// root — no package-level mutable state, safe to call concurrently. The
// Phase 56 test is the first consumer; a later phase (a `harbor lint`
// subcommand) can call the same checker without a second implementation.
package cardinalitylint

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

// Violation is a single metrics-cardinality breach found by a scan. It
// names the offending file (root-relative), the 1-based line, the kind
// of breach, and a human-readable detail. A scan returns every
// Violation it finds in one pass so an operator sees the full extent of
// the drift, not just the first breach.
type Violation struct {
	// File is the offending file, relative to the scanned root.
	File string
	// Line is the 1-based line number of the offending token.
	Line int
	// Kind classifies the breach — one of the Kind* constants.
	Kind string
	// Detail is a human-readable explanation naming the offending
	// label / identifier and why it is a cardinality breach.
	Detail string
}

// String renders a Violation as a single `file:line: kind: detail`
// line, the shape a test failure message joins.
func (v Violation) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", v.File, v.Line, v.Kind, v.Detail)
}

// The Kind* constants classify a Violation. They are stable strings so
// a test (or a future `harbor lint` subcommand) can branch on the kind.
const (
	// KindForbiddenLabelKey — a metric label constructor uses a string
	// literal key matching a forbidden high-cardinality label name.
	KindForbiddenLabelKey = "forbidden-label-key"
	// KindIdentitySourcedLabel — a metric label constructor uses a
	// value pulled off an events.Event Identity field (the run
	// quadruple), which is high-cardinality by nature.
	KindIdentitySourcedLabel = "identity-sourced-label"
)

// forbiddenLabelKeys is the set of label-name string literals that must
// never appear as a metric label key. These are the high-cardinality
// identifiers brief 06 names: a metric tagged by any of them produces
// effectively-unbounded time series. The set is matched
// case-insensitively against the unquoted literal value.
//
// tenant_id / user_id / session_id are included alongside run_id /
// trace_id / span_id / task_id: the run quadruple as a whole is
// unbounded over a fleet's lifetime, and metric isolation across
// tenants is the metrics backend's job (a separate scrape target /
// recording rule), not a per-series label.
var forbiddenLabelKeys = map[string]struct{}{
	"run_id":     {},
	"trace_id":   {},
	"span_id":    {},
	"task_id":    {},
	"tenant_id":  {},
	"user_id":    {},
	"session_id": {},
	"runid":      {},
	"traceid":    {},
	"spanid":     {},
	"taskid":     {},
}

// forbiddenIdentityFields is the set of struct field names on
// identity.Quadruple (the type of events.Event.Identity). A metric
// label VALUE that is a selector ending in one of these — `ev.Identity.RunID`,
// `id.TraceID`, … — is pulling a high-cardinality value onto a label.
var forbiddenIdentityFields = map[string]struct{}{
	"RunID":     {},
	"TraceID":   {},
	"SpanID":    {},
	"TaskID":    {},
	"TenantID":  {},
	"UserID":    {},
	"SessionID": {},
}

// attributeConstructors is the set of go.opentelemetry.io/otel/attribute
// constructor function names whose FIRST argument is the label key and
// SECOND argument is the label value. A `attribute.String("run_id", …)`
// or `attribute.String("ok", ev.Identity.RunID)` call is what the
// checker inspects.
var attributeConstructors = map[string]struct{}{
	"String":      {},
	"Int":         {},
	"Int64":       {},
	"Float64":     {},
	"Bool":        {},
	"StringSlice": {},
}

// ScanMetricsTree walks the Go source tree rooted at telemetryRoot
// (expected to be the internal/telemetry directory) and returns every
// metrics-cardinality Violation it finds. It parses .go files —
// including _test.go files, because a forbidden label hardcoded in a
// test is the same drift as one in production — with go/parser, so the
// check is precise: a forbidden word inside a comment, a doc string, or
// an unrelated string is NOT flagged; only a real metric-label
// constructor argument is.
//
// telemetryRoot may be absolute or relative; reported Violation.File
// paths are slash-separated and relative to telemetryRoot either way.
//
// The scan is exhaustive (it returns ALL violations, not the first) and
// deterministic (violations are sorted by file then line). It has no
// package-level mutable state and is safe for concurrent use (D-025).
//
// A returned error means the walk itself failed (an unreadable file, an
// unparseable source file) — that is distinct from a Violation, which
// is a successful scan finding drift.
func ScanMetricsTree(telemetryRoot string) ([]Violation, error) {
	fset := token.NewFileSet()
	var violations []Violation

	walkErr := filepath.WalkDir(telemetryRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			name := d.Name()
			// Skip vendored / build artefacts and the checker's own
			// package — cardinalitylint.go necessarily names the
			// forbidden label strings (in forbiddenLabelKeys) and the
			// forbidden field names; it is the audit tool, not a
			// metric-label site.
			if name == "vendor" || name == "testdata" || name == "cardinalitylint" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		rel, relErr := filepath.Rel(telemetryRoot, path)
		if relErr != nil {
			return fmt.Errorf("relativise %q: %w", path, relErr)
		}
		rel = filepath.ToSlash(rel)

		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return fmt.Errorf("parse %q: %w", rel, parseErr)
		}

		violations = append(violations, scanFile(fset, file, rel)...)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk telemetry tree %q: %w", telemetryRoot, walkErr)
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].File != violations[j].File {
			return violations[i].File < violations[j].File
		}
		return violations[i].Line < violations[j].Line
	})
	return violations, nil
}

// scanFile walks a single parsed file's AST and collects the two
// cardinality violation kinds. It looks for `metric.WithAttributes(...)`
// calls — the OTel idiom for attaching labels to a metric instrument's
// Add/Record — and inspects the `attribute.*` constructor calls passed
// to them. A span's attribute list (`trace.WithAttributes(...)`) is
// deliberately NOT inspected: a span legitimately carries the run
// quadruple (Phase 55 / D-073), and the cardinality rule is about
// metric labels only.
func scanFile(fset *token.FileSet, file *ast.File, rel string) []Violation {
	var out []Violation

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !isMetricWithAttributes(call.Fun) {
			return true
		}
		// Every argument to metric.WithAttributes is an
		// attribute.KeyValue — inspect each that is an attribute.*
		// constructor call.
		for _, arg := range call.Args {
			argCall, ok := arg.(*ast.CallExpr)
			if !ok {
				continue
			}
			out = append(out, scanAttributeCall(fset, argCall, rel)...)
		}
		return true
	})

	return out
}

// isMetricWithAttributes reports whether fun is the selector
// `metric.WithAttributes` — the OTel option that attaches a label set
// to a metric instrument operation. The selector base must be the
// canonical import identifier `metric`; the checker is intentionally
// conservative and matches the canonical name.
func isMetricWithAttributes(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "WithAttributes" {
		return false
	}
	base, ok := sel.X.(*ast.Ident)
	return ok && base.Name == "metric"
}

// scanAttributeCall inspects a single `attribute.<Ctor>(key, value)`
// call that appeared inside a metric.WithAttributes(...) and returns
// any cardinality violations: a forbidden literal label key, or a label
// value sourced from an events.Event Identity field.
func scanAttributeCall(fset *token.FileSet, call *ast.CallExpr, rel string) []Violation {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	if _, isCtor := attributeConstructors[sel.Sel.Name]; !isCtor {
		return nil
	}
	base, ok := sel.X.(*ast.Ident)
	if !ok || base.Name != "attribute" {
		return nil
	}
	if len(call.Args) < 1 {
		return nil
	}

	var out []Violation

	// Layer 1 — forbidden literal label KEY (first argument).
	if keyLit, ok := call.Args[0].(*ast.BasicLit); ok && keyLit.Kind == token.STRING {
		if val, uErr := strconv.Unquote(keyLit.Value); uErr == nil {
			if _, forbidden := forbiddenLabelKeys[strings.ToLower(val)]; forbidden {
				out = append(out, Violation{
					File: rel,
					Line: fset.Position(keyLit.Pos()).Line,
					Kind: KindForbiddenLabelKey,
					Detail: fmt.Sprintf(
						"metric label key %q is a high-cardinality identifier - metrics MUST NOT be tagged by run/trace/span/task id or the identity triple (brief 06 metrics-cardinality footgun)",
						val),
				})
			}
		}
	}

	// Layer 2 — label VALUE sourced from an events.Event Identity field
	// (second argument, when present). A selector ending in `.RunID` /
	// `.TraceID` / ... is pulling the run quadruple onto a label.
	if len(call.Args) >= 2 {
		if field, sourcedFrom, ok := identityFieldSelector(call.Args[1]); ok {
			out = append(out, Violation{
				File: rel,
				Line: fset.Position(call.Args[1].Pos()).Line,
				Kind: KindIdentitySourcedLabel,
				Detail: fmt.Sprintf(
					"metric label value is sourced from %s.%s - the identity quadruple is high-cardinality and MUST NOT reach a metric label (brief 06 metrics-cardinality footgun)",
					sourcedFrom, field),
			})
		}
	}

	return out
}

// identityFieldSelector reports whether expr is a selector expression
// ending in a known identity.Quadruple field name — `x.RunID`,
// `ev.Identity.TraceID`, `rc.Identity.SessionID`, etc. It returns the
// field name and a short rendering of the selector base for the
// Violation detail. The check is intentionally conservative: it matches
// ANY selector ending in one of the forbidden field names, then relies
// on the detail message + review to catch a false positive (a struct
// in some unrelated package that happens to have a `RunID` field). No
// such collision exists in internal/telemetry, and
// TestCardinalityLint_TelemetryTreeIsClean pins that.
func identityFieldSelector(expr ast.Expr) (field, base string, ok bool) {
	sel, isSel := expr.(*ast.SelectorExpr)
	if !isSel {
		return "", "", false
	}
	if _, forbidden := forbiddenIdentityFields[sel.Sel.Name]; !forbidden {
		return "", "", false
	}
	return sel.Sel.Name, renderSelectorBase(sel.X), true
}

// renderSelectorBase renders the base of a selector expression for a
// Violation detail message — `ev`, `ev.Identity`, `rc.Identity`, etc.
// It handles the common shapes (a bare identifier, a nested selector)
// and falls back to a placeholder for anything more exotic so the
// checker never panics on an unexpected AST shape.
func renderSelectorBase(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return renderSelectorBase(e.X) + "." + e.Sel.Name
	default:
		return "<identity>"
	}
}
