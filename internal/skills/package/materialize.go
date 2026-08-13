package skillpkg

import (
	"fmt"
	"sort"
	"strings"
)

// materialize.go — the pure bounded materialize / dematerialize /
// resolve helpers for package attachment references.
//
// MaterializeSupportRefs rewrites the validated logical relative
// support references of a package body to their exact immutable
// `skillpkg://<PackageHash>/<encoded-canonical-support-path>` URIs.
// DematerializeSupportRefs is the exact inverse: it rewrites ONLY the
// URIs of the EXACT package/hash back to their relative canonical
// paths, refusing foreign, malformed, and dangling URIs.
// ResolveSupportURI resolves one support URI against the exact
// package into the bounded manifest entry (bytes, MIME, size,
// digest).
//
// These helpers are PURE: they perform no store, authority,
// lifecycle, or filesystem side effects, and they are safe for N
// concurrent goroutines under -race. Boundedness: the input body is
// bounded by the SKILL.md bound at ingest; the scan is linear, each
// rewritten destination is replaced by a URI bounded by MaxURIRunes
// (NewURI enforces the bound), and no recursive or exponential
// expansion exists.

// rewrite is one splice: replace body[start:end] with replacement.
type rewrite struct {
	start, end  int
	replacement string
}

// MaterializeSupportRefs rewrites the validated logical relative
// support references in the package body (the description + section
// text) to their exact support URIs: `skillpkg://<hash>/<encoded
// canonical path>`. The package is validated and hashed first (the
// hash-before-URI materialization contract); every relative
// destination must canonicalize to a manifest entry
// (ErrSupportRefDangling otherwise). Scheme destinations (remote
// links), fragment-only anchors, and absolute paths are left verbatim
// — they are not support references.
func MaterializeSupportRefs(body string, pkg Package) (string, error) {
	hash, err := PackageHash(pkg)
	if err != nil {
		return "", err
	}
	manifest := make(map[string]struct{}, len(pkg.Supports))
	for _, f := range pkg.Supports {
		manifest[f.Path] = struct{}{}
	}

	refs := ScanSupportRefs(body)
	var rewrites []rewrite
	for _, r := range refs {
		if r.Start == r.End {
			continue // reference-style usage; the definition carries the span
		}
		kind, pathPart, _ := SplitDest(r.Dest)
		if kind != DestRelative {
			continue // schemes / anchors / absolute paths are not support refs
		}
		canonical, err := CanonicalizeSupportDest(pathPart)
		if err != nil {
			return "", fmt.Errorf("%w: %q: %v", ErrSupportRefDangling, r.Dest, err)
		}
		if _, ok := manifest[canonical]; !ok {
			return "", fmt.Errorf("%w: %q canonicalizes to %q", ErrSupportRefDangling, r.Dest, canonical)
		}
		u, err := NewURI(hash, canonical)
		if err != nil {
			return "", err
		}
		// The rewrite covers only the PATH portion of the destination;
		// a `#fragment` document anchor is left in place after the URI
		// so the dematerialization round-trip is exact.
		rewrites = append(rewrites, rewrite{start: r.Start, end: r.Start + len(pathPart), replacement: u.String()})
	}
	if len(rewrites) == 0 {
		return body, nil
	}
	sort.Slice(rewrites, func(i, j int) bool { return rewrites[i].start < rewrites[j].start })
	for i := 1; i < len(rewrites); i++ {
		if rewrites[i].start < rewrites[i-1].end {
			return "", fmt.Errorf("%w: overlapping rewrite spans in body (ambiguous reference structure)", ErrSupportRefDangling)
		}
	}
	return applyRewrites(body, rewrites), nil
}

// DematerializeSupportRefs reverses MaterializeSupportRefs for the
// EXACT package: every `skillpkg://` token in the body is parsed, and
// only tokens whose URI belongs to this package's hash and names a
// manifest entry are rewritten back to their relative canonical path.
// A token that is not a parseable package support URI
// (ErrSupportRefMalformedURI), carries a different package's hash
// (ErrSupportRefForeignURI), or names a path outside the manifest
// (ErrSupportRefDangling) refuses loudly — no partial dematerialization.
func DematerializeSupportRefs(body string, pkg Package) (string, error) {
	hash, err := PackageHash(pkg)
	if err != nil {
		return "", err
	}
	manifest := make(map[string]struct{}, len(pkg.Supports))
	for _, f := range pkg.Supports {
		manifest[f.Path] = struct{}{}
	}

	var rewrites []rewrite
	idx := 0
	for {
		i := strings.Index(body[idx:], URIScheme+"://")
		if i < 0 {
			break
		}
		start := idx + i
		end := start + len(URIScheme) + len("://")
		for end < len(body) {
			switch body[end] {
			// The URI token ends at a markdown construct boundary, a
			// fragment anchor, or whitespace.
			case ')', ']', '"', '\'', '#', '>', ' ', '\t', '\n', '\r':
				goto tokenEnd
			}
			end++
		}
	tokenEnd:
		tok := body[start:end]
		u, perr := ParseURI(tok)
		if perr != nil {
			return "", fmt.Errorf("%w: %q: %v", ErrSupportRefMalformedURI, tok, perr)
		}
		if u.Hash != hash {
			return "", fmt.Errorf("%w: %q carries hash %q, want %q", ErrSupportRefForeignURI, tok, u.Hash, hash)
		}
		if _, ok := manifest[u.Path]; !ok {
			return "", fmt.Errorf("%w: %q names %q", ErrSupportRefDangling, tok, u.Path)
		}
		rewrites = append(rewrites, rewrite{start: start, end: end, replacement: u.Path})
		idx = end
	}
	if len(rewrites) == 0 {
		return body, nil
	}
	return applyRewrites(body, rewrites), nil
}

// ResolveSupportURI resolves ONE support URI against the exact
// package: the URI's versioned hash must equal the package's hash
// (ErrSupportRefForeignURI otherwise) and its canonical path must
// name a manifest entry (ErrSupportRefDangling otherwise). The
// bounded manifest entry — bytes, canonical MIME, exact size, digest —
// is returned.
func ResolveSupportURI(u URI, pkg Package) (SupportFile, error) {
	hash, err := PackageHash(pkg)
	if err != nil {
		return SupportFile{}, err
	}
	if u.Hash != hash {
		return SupportFile{}, fmt.Errorf("%w: URI hash %q != package hash %q", ErrSupportRefForeignURI, u.Hash, hash)
	}
	for _, f := range pkg.Supports {
		if f.Path == u.Path {
			return f, nil
		}
	}
	return SupportFile{}, fmt.Errorf("%w: %q is not in the package manifest", ErrSupportRefDangling, u.Path)
}

// applyRewrites splices the rewrites into the body in ascending start
// order; the spans are disjoint, so a single left-to-right pass with a
// cursor is exact.
func applyRewrites(body string, rewrites []rewrite) string {
	sort.Slice(rewrites, func(i, j int) bool { return rewrites[i].start < rewrites[j].start })
	var b strings.Builder
	b.Grow(len(body) + 512*len(rewrites))
	cursor := 0
	for _, w := range rewrites {
		b.WriteString(body[cursor:w.start])
		b.WriteString(w.replacement)
		cursor = w.end
	}
	b.WriteString(body[cursor:])
	return b.String()
}
