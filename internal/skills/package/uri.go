package skillpkg

import (
	"errors"
	"fmt"
	"strings"
)

// uri.go — the bounded, authorized-resolver-neutral package URI.
//
// A package URI is the reference form of a package's identity: it
// carries the versioned PackageHash verbatim, plus an optional bounded
// name hint. It is deliberately resolver-neutral:
//
//   - NO authority component (`//host`) — nothing in the URI names a
//     resolver, a registry, or a network endpoint;
//   - NO tenant / user / session / project — identity is a property
//     of the caller and the storage layer, never of the package
//     reference;
//   - NO authorization material — no tokens, no scopes, no signed
//     claims. Any authorized resolver may materialize the same URI.
//
// Canonical form: `skillpkg:v1:<64-hex>` with an optional
// `/name` hint: `skillpkg:v1:<64-hex>/<name>`. The URI is bounded
// (MaxURIRunes for the whole string, MaxURINameRunes for the hint).

// URI errors. Compare via errors.Is.
var (
	// ErrMalformedURI — the string is not a valid package URI.
	ErrMalformedURI = errors.New("skillpkg: malformed package URI")
	// ErrURITooLong — the URI exceeds MaxURIRunes.
	ErrURITooLong = errors.New("skillpkg: package URI too long")
)

// URI is the parsed, canonical form of a package reference.
type URI struct {
	// Hash is the versioned PackageHash ("v1:<64-hex>"). Required.
	Hash string
	// Name is the optional canonical package-name hint (bounded,
	// lowercase `[a-z0-9._-]`). Empty means no hint. The hint is
	// display/selection metadata only — the hash is the identity.
	Name string
}

// NewURI builds a URI from a versioned package hash and an optional
// name hint. The hash must be structurally valid (it is NOT re-derived
// from a Package — callers compute PackageHash first, per the
// "hash before URI materialization" contract).
func NewURI(hash, name string) (URI, error) {
	u := URI{Hash: hash, Name: name}
	if err := u.Validate(); err != nil {
		return URI{}, err
	}
	return u, nil
}

// ParseURI parses the canonical string form. Accepted shapes:
//
//	skillpkg:v1:<64-hex>
//	skillpkg:v1:<64-hex>/<name>
//
// The scheme is case-sensitive; the hash must be a versioned package
// hash; the name hint, when present, is bounded and ASCII. Rejects
// with wrapped ErrMalformedURI / ErrURITooLong.
func ParseURI(s string) (URI, error) {
	if len([]rune(s)) > MaxURIRunes {
		return URI{}, fmt.Errorf("%w: %d runes exceeds %d", ErrURITooLong, len([]rune(s)), MaxURIRunes)
	}
	prefix := URIScheme + ":"
	if !strings.HasPrefix(s, prefix) {
		return URI{}, fmt.Errorf("%w: must start with %q", ErrMalformedURI, prefix)
	}
	rest := s[len(prefix):]
	hash, name := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		hash, name = rest[:i], rest[i+1:]
		if name == "" {
			return URI{}, fmt.Errorf("%w: empty name hint after %q", ErrMalformedURI, rest[:i])
		}
		if strings.ContainsRune(name, '/') {
			return URI{}, fmt.Errorf("%w: name hint %q contains '/'", ErrMalformedURI, name)
		}
	}
	if _, _, ok := splitHash(hash); !ok {
		return URI{}, fmt.Errorf("%w: hash segment %q is not a versioned package hash", ErrMalformedURI, hash)
	}
	u := URI{Hash: hash, Name: name}
	if err := u.validateName(); err != nil {
		return URI{}, err
	}
	return u, nil
}

// Validate checks the URI's closed shape.
func (u URI) Validate() error {
	if _, _, ok := splitHash(u.Hash); !ok {
		return fmt.Errorf("%w: Hash %q is not a versioned package hash", ErrMalformedURI, u.Hash)
	}
	if err := u.validateName(); err != nil {
		return err
	}
	if rl := len([]rune(u.String())); rl > MaxURIRunes {
		return fmt.Errorf("%w: %d runes exceeds %d", ErrURITooLong, rl, MaxURIRunes)
	}
	return nil
}

func (u URI) validateName() error {
	if u.Name == "" {
		return nil
	}
	if rl := len([]rune(u.Name)); rl > MaxURINameRunes {
		return fmt.Errorf("%w: Name hint exceeds %d runes (%d)", ErrMalformedURI, MaxURINameRunes, rl)
	}
	if CanonicalName(u.Name) != u.Name {
		return fmt.Errorf("%w: Name hint %q is not canonical (lowercase, trimmed)", ErrMalformedURI, u.Name)
	}
	for _, r := range u.Name {
		if r > 0x7f {
			return fmt.Errorf("%w: Name hint %q is not ASCII", ErrMalformedURI, u.Name)
		}
		if !isURINameChar(r) {
			return fmt.Errorf("%w: Name hint %q contains unsupported character %q", ErrMalformedURI, u.Name, r)
		}
	}
	return nil
}

func isURINameChar(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z':
		return true
	case r >= '0' && r <= '9':
		return true
	case r == '.', r == '_', r == '-':
		return true
	default:
		return false
	}
}

// String returns the canonical string form of the URI. It is the
// exact inverse of ParseURI: ParseURI(u.String()) == u.
func (u URI) String() string {
	if u.Name == "" {
		return URIScheme + ":" + u.Hash
	}
	return URIScheme + ":" + u.Hash + "/" + u.Name
}
