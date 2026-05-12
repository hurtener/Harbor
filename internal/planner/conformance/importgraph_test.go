package conformance_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// TestImportGraph_PlannerDoesNotImportRuntime is the binding §13
// import-graph lint. The planner package and every sub-package
// (concretes, conformance harness, helpers) MUST NOT import
// `internal/runtime/...`.
//
// Why this lives in the conformance package:
//
//   - The conformance package is the natural home for cross-cutting
//     assertions every planner concrete inherits.
//   - Phase 45 (ReAct) and Phase 48 (Deterministic) add their code
//     under `internal/planner/...`; this test runs against the
//     entire subtree, so every concrete is gated by this lint for
//     free.
//   - Skipping the test on a missing planner subtree would be a
//     drift signal — the test asserts a presence invariant, not a
//     value invariant.
//
// Walks the planner subtree starting from the conformance package's
// own location (relative path `../`). Uses `go/parser` (not `go list`
// or `goimports`) so the test has zero external-tool dependencies.
//
// Failure shape: a single line per offending file naming the
// importer + the runtime-package it imported. Multiple failures
// surface in one run so the operator sees the full extent of the
// violation.
func TestImportGraph_PlannerDoesNotImportRuntime(t *testing.T) {
	// Anchor the walk at the planner package root. The test lives
	// at internal/planner/conformance/; the planner root is one
	// level up.
	plannerRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve planner root: %v", err)
	}

	fset := token.NewFileSet()
	var violations []string

	walkErr := filepath.WalkDir(plannerRoot, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			// Skip vendored / build artefacts if any sneak in.
			name := d.Name()
			if name == "vendor" || name == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		// Only .go files; skip test files when checking imports
		// would be too lax — a _test.go that imports runtime is
		// equally a violation (the lint exists to gate the
		// production AND test surface).
		if !strings.HasSuffix(path, ".go") {
			return nil
		}

		file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(plannerRoot, path)
		for _, imp := range file.Imports {
			// imp.Path.Value is the quoted import string.
			raw := imp.Path.Value
			// Strip the surrounding quotes.
			if len(raw) < 2 {
				continue
			}
			pkg := raw[1 : len(raw)-1]
			if strings.HasPrefix(pkg, "github.com/hurtener/Harbor/internal/runtime/") || pkg == "github.com/hurtener/Harbor/internal/runtime" {
				violations = append(violations, rel+" imports "+pkg)
			}
		}
		return nil
	})

	if walkErr != nil {
		t.Fatalf("walk planner subtree: %v", walkErr)
	}

	if len(violations) > 0 {
		t.Fatalf("planner package imports runtime internals — §13 violation (%d files):\n  %s", len(violations), strings.Join(violations, "\n  "))
	}
}

// TestImportGraph_PlannerSubtreeIsReachable is a sanity gate: the
// walk above MUST find at least one .go file. A subtree that's gone
// missing (file moved, build tag elided everything) would otherwise
// silently pass the lint above with zero files inspected.
func TestImportGraph_PlannerSubtreeIsReachable(t *testing.T) {
	plannerRoot, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve planner root: %v", err)
	}

	var goFileCount int
	walkErr := filepath.WalkDir(plannerRoot, func(path string, d fs.DirEntry, walkErr error) error {
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
			goFileCount++
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk planner subtree: %v", walkErr)
	}

	// Phase 42 ships at least: planner.go, decision.go, trajectory.go,
	// errors.go, events.go, wake.go, planner_test.go, concurrent_test.go,
	// finish/finish.go, finish/finish_test.go, conformance/conformance.go,
	// conformance/importgraph_test.go — 12 files minimum.
	if goFileCount < 8 {
		t.Fatalf("planner subtree walk found only %d .go files; expected at least 8 — the lint above would silently pass on an empty subtree", goFileCount)
	}
}
