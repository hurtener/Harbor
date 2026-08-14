package skillpkg

import (
	"errors"
	"fmt"
	"strings"
)

// uri.go — the bounded, immutable support URI.
//
// A support URI is the authoritative immutable reference of ONE
// support file of a complete skill package: it carries the versioned
// PackageHash verbatim in the authority position plus the encoded
// canonical path of the referenced support file. It is deliberately
// resolver-neutral:
//
//   - The authority position holds ONLY the versioned PackageHash —
//     nothing in the URI names a resolver, a registry, or a network
//     endpoint, and no userinfo / port / host is ever admitted;
//   - NO tenant / user / session / project — identity is a property
//     of the caller and the storage layer, never of the package
//     reference;
//   - NO authorization material — no tokens, no scopes, no signed
//     claims. Any authorized resolver may materialize the same URI.
//
// Canonical form:
//
//	skillpkg://<PackageHash>/<encoded-canonical-support-path>
//
// e.g. `skillpkg://v1:<64-hex>/assets/logo.png`. The URI identifies
// ONE support file, never a package-name hint. The path is the
// percent-encoded canonical support path (deterministic encoding of
// the canonical path segments; for every canonical path — ASCII
// `[A-Za-z0-9._-]` plus `/` separators — the encoded form is the
// identity). The URI is bounded (MaxURIRunes for the whole string).

// URI errors. Compare via errors.Is.
var (
	// ErrMalformedURI — the string is not a valid package support URI.
	ErrMalformedURI = errors.New("skillpkg: malformed package support URI")
	// ErrURITooLong — the URI exceeds MaxURIRunes.
	ErrURITooLong = errors.New("skillpkg: package support URI too long")
)

// URI is the parsed, canonical form of a package support reference.
type URI struct {
	// Hash is the versioned PackageHash ("v1:<64-hex>"). Required.
	Hash string
	// Path is the canonical support-file path ("assets/logo.png")
	// of ONE support file of the package. Required — a support URI
	// never names the package or its root document, only a support
	// file.
	Path string
}

// NewURI builds a support URI from a versioned package hash and the
// canonical path of one support file of that package. The hash must
// be structurally valid (it is NOT re-derived from a Package —
// callers compute PackageHash first, per the "hash before URI
// materialization" contract) and the path must be a canonical,
// non-root support path.
func NewURI(hash, path string) (URI, error) {
	u := URI{Hash: hash, Path: path}
	if err := u.Validate(); err != nil {
		return URI{}, err
	}
	return u, nil
}

// ParseURI parses the canonical string form:
//
//	skillpkg://<PackageHash>/<encoded-canonical-support-path>
//
// The scheme is case-sensitive; the authority position must be
// EXACTLY a versioned package hash (no userinfo, no host, no port,
// no identity/token material); the path must be non-empty, strictly
// percent-decoded, and canonical. Rejects with wrapped
// ErrMalformedURI / ErrURITooLong. The parser is custom and strict —
// it never delegates to net/url, whose authority semantics would
// reinterpret the versioned hash (`v1:<hex>`) as userinfo/host
// material.
func ParseURI(s string) (URI, error) {
	if len([]rune(s)) > MaxURIRunes {
		return URI{}, fmt.Errorf("%w: %d runes exceeds %d", ErrURITooLong, len([]rune(s)), MaxURIRunes)
	}
	const prefix = URIScheme + "://"
	if !strings.HasPrefix(s, prefix) {
		return URI{}, fmt.Errorf("%w: must start with %q", ErrMalformedURI, prefix)
	}
	// Userinfo / identity-token (`@`), query (`?`), fragment (`#`)
	// are never part of a package support URI.
	if strings.ContainsAny(s, "@?#") {
		return URI{}, fmt.Errorf("%w: URI carries userinfo, query, fragment, or identity material", ErrMalformedURI)
	}
	rest := s[len(prefix):]
	sep := strings.IndexByte(rest, '/')
	if sep < 0 {
		return URI{}, fmt.Errorf("%w: missing support path", ErrMalformedURI)
	}
	authority, pathPart := rest[:sep], rest[sep+1:]
	if _, ok := splitHash(authority); !ok {
		return URI{}, fmt.Errorf("%w: hash segment %q is not a versioned package hash", ErrMalformedURI, authority)
	}
	if pathPart == "" {
		return URI{}, fmt.Errorf("%w: missing support path", ErrMalformedURI)
	}
	path, err := decodePath(pathPart)
	if err != nil {
		return URI{}, err
	}
	if _, err := canonicalizePath(path); err != nil {
		return URI{}, fmt.Errorf("%w: %s", ErrMalformedURI, err.Error())
	}
	if path == RootSkillFileName {
		return URI{}, fmt.Errorf("%w: %q is the root skill document, not a support file", ErrMalformedURI, path)
	}
	return URI{Hash: authority, Path: path}, nil
}

// Validate checks the URI's closed shape: a versioned package hash,
// a canonical non-root support path, and the whole-string bound.
func (u URI) Validate() error {
	if _, ok := splitHash(u.Hash); !ok {
		return fmt.Errorf("%w: Hash %q is not a versioned package hash", ErrMalformedURI, u.Hash)
	}
	if _, err := canonicalizePath(u.Path); err != nil {
		return fmt.Errorf("%w: %s", ErrMalformedURI, err.Error())
	}
	if u.Path == RootSkillFileName {
		return fmt.Errorf("%w: Path %q is the root skill document, not a support file", ErrMalformedURI, u.Path)
	}
	if rl := len([]rune(u.String())); rl > MaxURIRunes {
		return fmt.Errorf("%w: %d runes exceeds %d", ErrURITooLong, rl, MaxURIRunes)
	}
	return nil
}

// String returns the canonical string form of the URI:
//
//	skillpkg://<Hash>/<encoded path>
//
// For a valid URI (NewURI / Validate) it is the exact inverse of
// ParseURI: ParseURI(u.String()) == u.
func (u URI) String() string {
	return URIScheme + "://" + u.Hash + "/" + encodePath(u.Path)
}

// encodePath percent-encodes each path segment deterministically and
// rejoins with `/`. Canonical path segments carry only
// `[A-Za-z0-9._-]`, so the encoded form is the identity for every
// canonical path; the encoder is total (any out-of-charset byte is
// emitted as uppercase `%XX`) so String() is well-defined even for a
// malformed URI value (which Validate rejects).
func encodePath(path string) string {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = encodeSegment(seg)
	}
	return strings.Join(segments, "/")
}

func encodeSegment(seg string) string {
	needs := false
	for i := range len(seg) {
		if !isPathChar(rune(seg[i])) {
			needs = true
			break
		}
	}
	if !needs {
		return seg
	}
	var b strings.Builder
	for i := range len(seg) {
		c := seg[i]
		if isPathChar(rune(c)) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hexDigitsUpper[c>>4])
		b.WriteByte(hexDigitsUpper[c&0xf])
	}
	return b.String()
}

// decodePath strictly percent-decodes an encoded URI path. Every
// percent-escape that is not provably redundant is rejected: a `%`
// without two hex digits (malformed), an escape that decodes to an
// unreserved character that must stay literal (ambiguous encoding,
// including encoded `.` / `..` dot segments), an encoded `/`
// separator, or an encoded `\` backslash. Any other decoded byte is
// outside the canonical path charset and is rejected by the caller's
// canonicalization.
func decodePath(encoded string) (string, error) {
	rawSegs := strings.Split(encoded, "/")
	decoded := make([]string, len(rawSegs))
	for i, seg := range rawSegs {
		d, err := decodeSegment(seg)
		if err != nil {
			return "", err
		}
		decoded[i] = d
	}
	return strings.Join(decoded, "/"), nil
}

func decodeSegment(seg string) (string, error) {
	if !strings.Contains(seg, "%") {
		return seg, nil
	}
	var b strings.Builder
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		if i+2 >= len(seg) {
			return "", fmt.Errorf("%w: malformed percent-encoding in %q", ErrMalformedURI, seg)
		}
		hi, ok1 := unhexDigit(seg[i+1])
		lo, ok2 := unhexDigit(seg[i+2])
		if !ok1 || !ok2 {
			return "", fmt.Errorf("%w: malformed percent-encoding in %q", ErrMalformedURI, seg)
		}
		decoded := hi<<4 | lo
		switch {
		case isPathChar(rune(decoded)):
			return "", fmt.Errorf("%w: ambiguous percent-encoding in %q (character %q must stay literal)", ErrMalformedURI, seg, decoded)
		case decoded == '/':
			return "", fmt.Errorf("%w: encoded slash in %q (path separators must be literal '/')", ErrMalformedURI, seg)
		case decoded == '\\':
			return "", fmt.Errorf("%w: encoded backslash in %q", ErrMalformedURI, seg)
		}
		b.WriteByte(decoded)
		i += 2
	}
	return b.String(), nil
}

func unhexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

// hexDigitsUpper renders one byte as two uppercase hex digits.
const hexDigitsUpper = "0123456789ABCDEF"
