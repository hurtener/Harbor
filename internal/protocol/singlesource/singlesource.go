// Package singlesource is the Harbor Protocol single-source enforcement
// checker (CLAUDE.md §8, §13; RFC §5). It is the Phase 58 formalisation
// of the single-source discipline Phase 54 (D-072) laid the foundation
// for: the canonical Protocol packages are the ONLY definition sites,
// and a hardcoded Protocol method string / error code / wire-type
// redefinition anywhere else is a build-gating lint failure.
//
// # What is single-sourced, and where
//
//   - Protocol method names — internal/protocol/methods. Every method
//     wire string is a constant there; no method string literal appears
//     anywhere else under internal/protocol/ (CLAUDE.md §8: "No
//     hardcoded method strings elsewhere").
//   - Protocol error codes — internal/protocol/errors. Every Code
//     constant is declared there; no other package declares a
//     protocol/errors.Code constant (CLAUDE.md §8: "Add new codes there
//     and only there").
//   - Protocol wire types — internal/protocol/types. Every canonical
//     Protocol message struct is declared there; no other package
//     re-declares one (CLAUDE.md §13: "Adding a third place to define
//     Protocol message types" is rejection-on-sight).
//
// # Why a go/parser checker, not a golangci-lint analyzer or a script
//
// The repo already proves the pattern: internal/planner/conformance/
// importgraph_test.go is a go/parser AST walk that gates the §13
// planner-does-not-import-runtime invariant with zero external-tool
// dependencies. Phase 58 reuses that shape — a custom golangci-lint
// analyzer would need a plugin build and a .golangci.yml entry (a new
// linter needs a PR rationale per CLAUDE.md §5), and a shell script
// could not parse Go reliably (a method string inside a comment or a
// struct-tag is not a violation; only a real string-literal expression
// is). go/parser sees the AST, so the checker is precise: it flags a
// BasicLit STRING whose unquoted value is a canonical method name, not
// a substring match. The checker is plain Go, runs as a `go test`, and
// is gated by CI + the preflight smoke exactly like the importgraph
// lint.
//
// # The checker is a reusable artifact (D-025)
//
// ScanProtocolTree and its helpers are pure functions over a filesystem
// root — no package-level mutable state, safe to call concurrently. The
// Phase 58 test is the first consumer; a later phase (e.g. a `harbor
// lint` subcommand, or Phase 59's versioning discipline) can call the
// same checker without a second implementation.
package singlesource

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

// Violation is a single single-source breach found by a scan. It names
// the offending file (repo-relative), the 1-based line, the kind of
// breach, and a human-readable detail. A scan returns every Violation it
// finds in one pass so an operator sees the full extent of the drift,
// not just the first breach.
type Violation struct {
	// File is the offending file, relative to the scanned root.
	File string
	// Line is the 1-based line number of the offending token.
	Line int
	// Kind classifies the breach — one of the Kind* constants.
	Kind string
	// Detail is a human-readable explanation naming the offending
	// identifier / literal and the canonical package it belongs in.
	Detail string
}

// String renders a Violation as a single `file:line: kind: detail`
// line, the shape a test failure message joins.
func (v Violation) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", v.File, v.Line, v.Kind, v.Detail)
}

// The Kind* constants classify a Violation. They are stable strings so a
// test (or a future `harbor lint` subcommand) can branch on the kind.
const (
	// KindMethodLiteral — a Protocol method wire string appears as a
	// string literal outside internal/protocol/methods.
	KindMethodLiteral = "method-literal"
	// KindErrorCode — a protocol/errors.Code constant is declared
	// outside internal/protocol/errors.
	KindErrorCode = "error-code"
	// KindWireType — a canonical Protocol message struct type is
	// declared outside internal/protocol/types.
	KindWireType = "wire-type"
)

// CanonicalMethods is the set of Protocol method wire strings that must
// only ever appear as constants in internal/protocol/methods. It is
// kept in lockstep with internal/protocol/methods by
// TestSingleSource_CanonicalMethodsInLockstep — the checker does NOT
// import the methods package (the checker must be runnable against a
// tree where methods/ itself is the thing under audit), so the set is
// duplicated here and the test pins the duplication.
var CanonicalMethods = map[string]struct{}{
	"start":              {},
	"cancel":             {},
	"pause":              {},
	"resume":             {},
	"redirect":           {},
	"inject_context":     {},
	"approve":            {},
	"reject":             {},
	"prioritize":         {},
	"user_message":       {},
	"events.subscribe":   {}, // Phase 72 / D-105
	"events.aggregate":   {}, // Phase 72a / D-106
	"search.query":       {}, // Phase 72c / D-108
	"search.sessions":    {}, // Phase 72c / D-108
	"search.tasks":       {}, // Phase 72c / D-108
	"search.events":      {}, // Phase 72c / D-108
	"search.artifacts":   {}, // Phase 72c / D-108
	"runtime.info":       {}, // Phase 72f / D-111
	"runtime.health":     {}, // Phase 72f / D-111
	"runtime.counters":   {}, // Phase 72f / D-111
	"runtime.drivers":    {}, // Phase 72f / D-111
	"metrics.snapshot":   {}, // Phase 72f / D-111
	"governance.posture": {}, // Phase 72g / D-112
	"llm.posture":        {}, // Phase 72g / D-112
	"pause.list":         {}, // Phase 72e / D-110
	"topology.snapshot":  {}, // Phase 74 / D-114
}

// CanonicalWireTypes maps each canonical Protocol message struct type
// name to the single package directory (relative to the protocol-tree
// root) that is allowed to declare it. A declaration of one of these
// type names in ANY other package is a KindWireType violation.
//
// Almost every wire type lives in internal/protocol/types; the one
// exception is Error, the Protocol error wire type, which lives in
// internal/protocol/errors alongside the Code constants it carries
// (D-072 §1: "internal/protocol/errors/errors.go ... the Error wire
// type"). Single-sourcing means "exactly one home", not "all in the
// same directory" — the map records the home per type.
//
// Version, Deprecation, and VersionHandshake are the Phase 59 (D-077)
// versioning-discipline wire types — all in internal/protocol/types
// alongside the ProtocolVersion pin.
//
// Kept in lockstep with the canonical packages by
// TestSingleSource_CanonicalWireTypesInLockstep.
var CanonicalWireTypes = map[string]string{
	"IdentityScope":          "types",
	"StartRequest":           "types",
	"StartResponse":          "types",
	"ControlRequest":         "types",
	"ControlResponse":        "types",
	"Version":                "types",
	"Deprecation":            "types",
	"VersionHandshake":       "types",
	"EventFilter":            "types",
	"EventBucket":            "types",
	"EventAggregateRequest":  "types",
	"EventAggregateResponse": "types",
	"Error":                  "errors",
	// Phase 72c (D-108) search cluster wire types — all live in
	// internal/protocol/types alongside the rest of the Protocol shape.
	"SearchRequest":     "types",
	"SearchResponse":    "types",
	"SearchResultRow":   "types",
	"SearchFilter":      "types",
	"SearchFacet":       "types",
	"SearchArtifactRef": "types",
	// Phase 72f (D-111) runtime-posture wire types — all live in
	// internal/protocol/types (internal/protocol/types/posture.go).
	"RuntimeInfoRequest": "types",
	"RuntimeInfo":        "types",
	"SubsystemHealth":    "types",
	"RuntimeHealth":      "types",
	"RuntimeCounters":    "types",
	"SubsystemDriver":    "types",
	"RuntimeDrivers":     "types",
	"NamedCounter":       "types",
	"HistogramBucket":    "types",
	"NamedHistogram":     "types",
	"NamedGauge":         "types",
	"MetricsSnapshot":    "types",
	// Phase 72g (D-112) posture-pair wire types — all live in
	// internal/protocol/types alongside the rest of the Protocol shape.
	"GovernancePostureRequest":  "types",
	"GovernancePostureResponse": "types",
	"IdentityTierView":          "types",
	"RateLimitView":             "types",
	"LLMPostureRequest":         "types",
	"LLMPostureResponse":        "types",
	// Phase 72e (D-110) pause-list snapshot wire types — all live in
	// internal/protocol/types alongside the rest of the Protocol shape.
	"PauseListRequest":  "types",
	"PauseListResponse": "types",
	"PauseSnapshot":     "types",
	"PauseFilter":       "types",
	"PauseArtifactRef":  "types",
	// Phase 74 (D-114) topology-projection wire types — all live in
	// internal/protocol/types alongside the rest of the Protocol shape.
	"TopologyProjection":      "types",
	"TopologyNode":            "types",
	"TopologyEdge":            "types",
	"TopologySnapshotRequest": "types",
}

// dirAllowsKind reports whether the package directory dir (a path
// relative to the protocol-tree root, slash-separated) is the canonical
// home for the given Violation kind — i.e. the kind is permitted there.
// It covers the kinds with a single fixed home (method literals,
// error codes); KindWireType has a per-type home and is gated by
// CanonicalWireTypes directly.
func dirAllowsKind(dir, kind string) bool {
	switch kind {
	case KindMethodLiteral:
		return dir == "methods"
	case KindErrorCode:
		return dir == "errors"
	default:
		return false
	}
}

// ScanProtocolTree walks the Go source tree rooted at protocolRoot
// (expected to be the internal/protocol directory) and returns every
// single-source Violation it finds. It parses .go files — including
// _test.go files, because a method string hardcoded in a test is the
// same drift as one hardcoded in production — with go/parser, so the
// check is precise: a method name inside a comment, a doc string, or a
// struct tag is NOT flagged; only a real string-literal expression, a
// real const declaration, or a real type declaration is.
//
// protocolRoot may be absolute or relative; reported Violation.File
// paths are slash-separated and relative to protocolRoot either way.
//
// The scan is exhaustive (it returns ALL violations, not the first) and
// deterministic (violations are sorted by file then line). It has no
// package-level mutable state and is safe for concurrent use (D-025).
//
// A returned error means the walk itself failed (an unreadable file, an
// unparseable source file) — that is distinct from a Violation, which
// is a successful scan finding drift.
func ScanProtocolTree(protocolRoot string) ([]Violation, error) {
	fset := token.NewFileSet()
	var violations []Violation

	walkErr := filepath.WalkDir(protocolRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			name := d.Name()
			// Skip vendored / build artefacts and the checker's own
			// package — singlesource.go necessarily mentions the
			// canonical method strings (in CanonicalMethods) and the
			// canonical type names (in CanonicalWireTypes); it is the
			// audit tool, not a Protocol-definition site.
			if name == "vendor" || name == "testdata" || name == "singlesource" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		rel, relErr := filepath.Rel(protocolRoot, path)
		if relErr != nil {
			return fmt.Errorf("relativise %q: %w", path, relErr)
		}
		rel = filepath.ToSlash(rel)
		pkgDir := filepath.ToSlash(filepath.Dir(rel))
		if pkgDir == "." {
			pkgDir = ""
		}

		file, parseErr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if parseErr != nil {
			return fmt.Errorf("parse %q: %w", rel, parseErr)
		}

		violations = append(violations, scanFile(fset, file, rel, pkgDir)...)
		return nil
	})
	if walkErr != nil {
		return nil, fmt.Errorf("walk protocol tree %q: %w", protocolRoot, walkErr)
	}

	sort.Slice(violations, func(i, j int) bool {
		if violations[i].File != violations[j].File {
			return violations[i].File < violations[j].File
		}
		return violations[i].Line < violations[j].Line
	})
	return violations, nil
}

// scanFile walks a single parsed file's AST and collects the three
// single-source violation kinds. pkgDir is the file's package directory
// relative to the protocol-tree root ("" for the root package,
// "methods" / "errors" / "types" / ... for sub-packages).
func scanFile(fset *token.FileSet, file *ast.File, rel, pkgDir string) []Violation {
	var out []Violation

	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.BasicLit:
			// A STRING literal whose unquoted value is a canonical
			// Protocol method name is a hardcoded method string. Allowed
			// only inside internal/protocol/methods.
			if node.Kind != token.STRING {
				return true
			}
			if dirAllowsKind(pkgDir, KindMethodLiteral) {
				return true
			}
			val, uErr := strconv.Unquote(node.Value)
			if uErr != nil {
				return true
			}
			if _, isMethod := CanonicalMethods[val]; isMethod {
				out = append(out, Violation{
					File: rel,
					Line: fset.Position(node.Pos()).Line,
					Kind: KindMethodLiteral,
					Detail: fmt.Sprintf(
						"hardcoded Protocol method string %q — method names are single-sourced in internal/protocol/methods (use the methods.Method* constant)",
						val),
				})
			}

		case *ast.TypeSpec:
			// A `type Error ...` / `type StartRequest ...` declaration
			// of a canonical Protocol wire type outside its single home
			// package redefines a single-sourced type.
			name := node.Name.Name
			home, isWireType := CanonicalWireTypes[name]
			if !isWireType {
				return true
			}
			if pkgDir == home {
				return true
			}
			out = append(out, Violation{
				File: rel,
				Line: fset.Position(node.Name.Pos()).Line,
				Kind: KindWireType,
				Detail: fmt.Sprintf(
					"redeclared canonical Protocol wire type %q — it is single-sourced in internal/protocol/%s",
					name, home),
			})

		case *ast.GenDecl:
			// A const declaration whose type is the protocol/errors.Code
			// type, made outside internal/protocol/errors, is a second
			// definition site for Protocol error codes.
			if node.Tok != token.CONST {
				return true
			}
			if dirAllowsKind(pkgDir, KindErrorCode) {
				return true
			}
			for _, spec := range node.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || vs.Type == nil {
					continue
				}
				if !isProtocolErrorsCodeType(vs.Type) {
					continue
				}
				for _, ident := range vs.Names {
					out = append(out, Violation{
						File: rel,
						Line: fset.Position(ident.Pos()).Line,
						Kind: KindErrorCode,
						Detail: fmt.Sprintf(
							"declared Protocol error code constant %q of type protocol/errors.Code — error codes are single-sourced in internal/protocol/errors",
							ident.Name),
					})
				}
			}
		}
		return true
	})

	return out
}

// isProtocolErrorsCodeType reports whether the type expression names the
// protocol/errors.Code type — either the bare `Code` identifier (inside
// the errors package itself, which dirAllowsKind already excludes from
// the scan) or a `<pkg>.Code` selector where the selector base resolves
// to the protocol errors package. The check is intentionally
// conservative: it matches a `.Code` selector on ANY package alias, then
// the scan's pkgDir gate ensures the errors package itself is never
// flagged. A const of an unrelated `Code` type in a non-errors package
// would be a false positive, so the detail message names the type to
// make a false positive obvious in review — but no such type exists in
// the Protocol tree, and TestSingleSource_NoFalsePositiveOnNonProtocolCode
// pins that.
func isProtocolErrorsCodeType(expr ast.Expr) bool {
	switch t := expr.(type) {
	case *ast.SelectorExpr:
		// `protoerrors.Code`, `errors.Code`, etc. — a selector ending in
		// the identifier `Code`.
		return t.Sel.Name == "Code"
	case *ast.Ident:
		// A bare `Code` — only reachable inside the errors package,
		// which dirAllowsKind already excludes before this is called.
		return t.Name == "Code"
	default:
		return false
	}
}
