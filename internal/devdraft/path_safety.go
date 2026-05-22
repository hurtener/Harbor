package devdraft

import (
	"fmt"
	"path/filepath"
	"strings"
)

// resolveSafe canonicalises relPath under allowedRoot and verifies
// the result lives within allowedRoot. Returns the absolute path on
// success or wrapped ErrUnsafePath on rejection. CLAUDE.md §7 rule 5.
//
// This helper mirrors `internal/skills/importer/path_safety.go`'s
// shape; the duplication is intentional because the importer's
// helper is unexported. If a future refactor lifts the importer's
// helper into a shared package, this file becomes a one-line
// re-export.
//
// Rejection cases:
//   - allowedRoot empty (the operator did not declare a safe root).
//   - relPath empty.
//   - relPath absolute (callers must supply path components relative
//     to the draft root).
//   - The constructed path lexically escapes the canonicalised root.
//   - The constructed path's symlink-evaluated form escapes the
//     symlink-evaluated root.
//
// On a non-existent target (the common case — the caller is probing
// before write), symlink evaluation is skipped and the lexical check
// carries the safety guarantee.
func resolveSafe(allowedRoot, relPath string) (string, error) {
	if allowedRoot == "" {
		return "", fmt.Errorf("%w: allowedRoot is empty", ErrUnsafePath)
	}
	if relPath == "" {
		return "", fmt.Errorf("%w: empty path", ErrUnsafePath)
	}
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("%w: %q is absolute; relative path required", ErrUnsafePath, relPath)
	}

	canonicalRoot, err := filepath.Abs(filepath.Clean(allowedRoot))
	if err != nil {
		return "", fmt.Errorf("%w: allowedRoot abs: %w", ErrUnsafePath, err)
	}

	joined := filepath.Clean(filepath.Join(canonicalRoot, relPath))
	if !pathHasPrefix(joined, canonicalRoot) {
		return "", fmt.Errorf("%w: %q escapes %q (lexical)", ErrUnsafePath, joined, canonicalRoot)
	}

	// Symlink pass — only fires when the constructed path already
	// exists. The lexical check above is the load-bearing one for
	// the probe-then-write pattern Store.WriteFile uses.
	evaluated, evalErr := filepath.EvalSymlinks(joined)
	if evalErr == nil {
		evaluatedRoot, rootErr := filepath.EvalSymlinks(canonicalRoot)
		if rootErr != nil {
			return "", fmt.Errorf("%w: allowedRoot eval: %w", ErrUnsafePath, rootErr)
		}
		if !pathHasPrefix(evaluated, evaluatedRoot) {
			return "", fmt.Errorf("%w: %q escapes %q (symlink)", ErrUnsafePath, evaluated, evaluatedRoot)
		}
	}

	return joined, nil
}

// pathHasPrefix is the canonical prefix check CLAUDE.md §7 rule 5
// mandates — `strings.HasPrefix(absPath, allowedRoot)` plus the OS
// separator to avoid the false-positive where allowedRoot=/a matches
// /abc.
func pathHasPrefix(p, root string) bool {
	if p == root {
		return true
	}
	rootWithSep := root + string(filepath.Separator)
	return strings.HasPrefix(p, rootWithSep)
}
