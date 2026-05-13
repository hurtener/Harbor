package importer

import (
	"fmt"
	"path/filepath"
	"strings"
)

// resolveSafePath canonicalises `relPath` under `allowedRoot` and
// verifies the result lives within `allowedRoot`. Returns the
// absolute, evaluated path on success or wrapped
// `ErrAttachmentOutsideRoot` on rejection. Per CLAUDE.md §7 rule 5.
//
// Rejection cases:
//
//   - `allowedRoot` empty (the operator did not declare a safe root).
//   - `relPath` empty (no path component to resolve).
//   - `relPath` absolute (callers must supply relative paths;
//     the source-side Skills.md format is path-relative-to-source).
//   - `filepath.Clean(filepath.Join(allowedRoot, relPath))` does not
//     have `allowedRoot` (canonicalised) as a prefix — the standard
//     traversal check.
//   - The constructed path, after `filepath.EvalSymlinks` if it
//     exists, escapes the canonicalised root via symlink chain.
//
// The canonical root is computed once via `filepath.Abs` +
// `filepath.Clean`; symlinks INSIDE the root are followed but never
// crossed to a destination outside it. On a non-existent path
// (which is fine — the caller may be probing before write),
// symlink evaluation is skipped and the lexical check carries.
func resolveSafePath(allowedRoot, relPath string) (string, error) {
	if allowedRoot == "" {
		return "", fmt.Errorf("%w: AllowedRoot is empty", ErrAttachmentOutsideRoot)
	}
	if relPath == "" {
		return "", fmt.Errorf("%w: empty path", ErrAttachmentOutsideRoot)
	}
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("%w: %q is absolute; relative path required",
			ErrAttachmentOutsideRoot, relPath)
	}

	canonicalRoot, err := filepath.Abs(filepath.Clean(allowedRoot))
	if err != nil {
		return "", fmt.Errorf("%w: AllowedRoot abs: %v", ErrAttachmentOutsideRoot, err)
	}

	joined := filepath.Clean(filepath.Join(canonicalRoot, relPath))
	if !pathHasPrefix(joined, canonicalRoot) {
		return "", fmt.Errorf("%w: %q escapes %q (lexical)",
			ErrAttachmentOutsideRoot, joined, canonicalRoot)
	}

	// Symlink-evaluation pass. `filepath.EvalSymlinks` returns an
	// error if any component does not exist; that's fine — the
	// caller may probe before write. In that case the lexical check
	// already carried the safety guarantee.
	evaluated, evalErr := filepath.EvalSymlinks(joined)
	if evalErr == nil {
		evaluatedRoot, rootErr := filepath.EvalSymlinks(canonicalRoot)
		if rootErr != nil {
			// Root must exist if any attachment was probed
			// successfully under it; surface the root-eval error
			// rather than silently falling back.
			return "", fmt.Errorf("%w: AllowedRoot eval: %v",
				ErrAttachmentOutsideRoot, rootErr)
		}
		if !pathHasPrefix(evaluated, evaluatedRoot) {
			return "", fmt.Errorf("%w: %q escapes %q (symlink)",
				ErrAttachmentOutsideRoot, evaluated, evaluatedRoot)
		}
	}

	return joined, nil
}

// pathHasPrefix is the canonical prefix-check the AGENTS.md §7 #5
// rule mandates: `strings.HasPrefix(absPath, allowedRoot)` but
// avoiding the false-positive where `allowedRoot=/a` matches
// `/abc`. The check appends the OS separator to the root.
func pathHasPrefix(p, root string) bool {
	if p == root {
		return true
	}
	rootWithSep := root + string(filepath.Separator)
	return strings.HasPrefix(p, rootWithSep)
}
