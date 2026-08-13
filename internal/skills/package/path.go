package skillpkg

import (
	"fmt"
	"strings"
)

// pathViolation classifies a canonical-path rejection so callers can
// wrap it with the sentinel that best names the violation class
// (archive traversal vs. generic invalid path vs. DTO invalid
// support).
type pathViolation int

const (
	// violationInvalid — any structural rejection that is not a
	// traversal: empty path, backslash, absolute path, `.` segment,
	// empty segment, non-ASCII byte, out-of-charset rune, oversized
	// segment or path, NUL.
	violationInvalid pathViolation = iota
	// violationTraversal — a `..` segment: the path escapes the
	// package root.
	violationTraversal
)

// pathErr is the classified rejection canonicalizePath returns.
type pathErr struct {
	class pathViolation
	msg   string
}

func (e *pathErr) Error() string { return e.msg }

// canonicalizePath normalizes and validates ONE archive / manifest
// path into its canonical form, or returns a classified rejection.
//
// The canonical form is strict and closed:
//
//   - root-relative, forward-slash separated (`/` is the ONLY
//     separator; a literal backslash is rejected);
//   - no leading or trailing slash, no empty segments (no `//`);
//   - no `.` or `..` segments — `..` is the traversal class; an
//     absolute leading `/` and a drive-letter prefix (`C:`-style,
//     whose `:` is outside the charset) are rejected as invalid;
//   - ASCII graphic characters only (`[A-Za-z0-9._-]` plus `/`):
//     any byte >= 0x80 rejects the path as non-ASCII, which closes
//     the Unicode-normalization collision class by construction —
//     canonical package paths cannot collide under NFC/NFD because
//     they cannot carry combining marks;
//   - segments bounded (MaxPackagePathSegmentRunes), whole path
//     bounded (MaxPackagePathRunes).
//
// Collision detection (case-folded duplicates) is the caller's job —
// this function returns the canonical path, not a uniqueness verdict.
func canonicalizePath(path string) (string, error) {
	if path == "" {
		return "", &pathErr{violationInvalid, "empty path"}
	}
	if strings.ContainsRune(path, '\x00') {
		return "", &pathErr{violationInvalid, "path contains NUL"}
	}
	if strings.ContainsRune(path, '\\') {
		return "", &pathErr{violationInvalid, fmt.Sprintf("%q contains a backslash (use forward slashes)", path)}
	}
	if strings.HasPrefix(path, "/") {
		return "", &pathErr{violationInvalid, fmt.Sprintf("%q is absolute (root-relative required)", path)}
	}
	if strings.HasPrefix(path, "./") || strings.HasSuffix(path, "/") {
		return "", &pathErr{violationInvalid, fmt.Sprintf("%q has a leading `./` or trailing slash", path)}
	}

	segments := strings.Split(path, "/")
	for _, seg := range segments {
		switch seg {
		case "":
			return "", &pathErr{violationInvalid, fmt.Sprintf("%q has an empty segment (double slash)", path)}
		case ".":
			return "", &pathErr{violationInvalid, fmt.Sprintf("%q contains a `.` segment", path)}
		case "..":
			return "", &pathErr{violationTraversal, fmt.Sprintf("%q escapes the package root (`..` traversal)", path)}
		}
		if rl := len([]rune(seg)); rl > MaxPackagePathSegmentRunes {
			return "", &pathErr{violationInvalid, fmt.Sprintf("%q segment exceeds %d runes (%d)", path, MaxPackagePathSegmentRunes, rl)}
		}
		for _, r := range seg {
			if r > 0x7f {
				return "", &pathErr{violationInvalid, fmt.Sprintf("%q is not ASCII (canonical package paths are ASCII-only)", path)}
			}
			if !isPathChar(r) {
				return "", &pathErr{violationInvalid, fmt.Sprintf("%q contains unsupported character %q", path, r)}
			}
		}
	}
	if rl := len([]rune(path)); rl > MaxPackagePathRunes {
		return "", &pathErr{violationInvalid, fmt.Sprintf("%q exceeds %d runes (%d)", path, MaxPackagePathRunes, rl)}
	}
	return path, nil
}

// isPathChar reports whether r is allowed inside a canonical path
// segment: ASCII letters, digits, `.`, `_`, `-`. `/` is the separator
// (handled by splitting) and is not allowed inside a segment.
func isPathChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= 'A' && r <= 'Z':
		return true
	case r >= '0' && r <= '9':
		return true
	case r == '.', r == '_', r == '-':
		return true
	default:
		return false
	}
}

// foldPath returns the case-folded comparison key for a canonical
// path. Two paths that fold identically are a case-collision; the
// requirement is that "SKILL.md" and "skill.md" may not coexist.
func foldPath(path string) string {
	return strings.ToLower(path)
}
