package skillpkg

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
)

// hash.go — the versioned package hash.
//
// PackageHash is the identity of a COMPLETE skill package: the
// logical canonical skill content PLUS the ordered normalized support
// manifest (canonical path, MIME, exact size, digest per entry). It
// is computed over the canonical serialization BEFORE the `skillpkg:`
// URI is materialized — the URI embeds the hash verbatim, so the hash
// never depends on the URI form and any authorized resolver can
// verify a package against its reference.
//
// The hash is VERSIONED and distinct from the legacy
// `skills.CanonicalContentHash`: the stored-row content hash covers
// only the skill body fields, carries no version, and knows nothing
// about support files. The package hash string is `v1:<64-hex>`; a
// future envelope change bumps the version prefix without reusing the
// v1 namespace.

// hashEnvelopeV1 is the fixed prefix mixed into the hash input so the
// v1 envelope cannot collide with a bare canonical-serialization hash.
const hashEnvelopeV1 = "skillpkg-hash-v1\x00"

// PackageHash returns the versioned content hash of the complete
// package: sha256(hashEnvelopeV1 || CanonicalBytes(p)), rendered as
// "v1:<64-hex>". The package is validated before hashing — a
// structurally invalid package has no identity.
func PackageHash(p Package) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}
	cb, err := CanonicalBytes(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(hashEnvelopeV1), cb...))
	return hashString(HashVersionV1, sum[:]), nil
}

// Hash returns the versioned package hash as a method for callers
// that already hold a Package value.
func (p Package) Hash() (string, error) {
	return PackageHash(p)
}

func hashString(version string, digest []byte) string {
	return version + ":" + hex.EncodeToString(digest)
}

// HashVersion returns the version prefix of a versioned hash string
// ("v1") and whether the string is structurally well-formed.
func HashVersion(hash string) (string, bool) {
	version, _, ok := splitHash(hash)
	return version, ok
}

func splitHash(hash string) (version, hexPart string, ok bool) {
	i := strings.IndexByte(hash, ':')
	if i < 0 {
		return "", "", false
	}
	version, hexPart = hash[:i], hash[i+1:]
	// The version prefix must be `v` followed by digits (v1, v2, ...)
	// so a hash string cannot smuggle a non-versioned segment.
	if !validHashVersion(version) || len(hexPart) != 64 {
		return "", "", false
	}
	for _, r := range hexPart {
		if !isHex(r) {
			return "", "", false
		}
	}
	return version, hexPart, true
}

func validHashVersion(version string) bool {
	if len(version) < 2 || version[0] != 'v' {
		return false
	}
	for _, r := range version[1:] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func isHex(r rune) bool {
	switch {
	case r >= '0' && r <= '9':
		return true
	case r >= 'a' && r <= 'f':
		return true
	default:
		return false
	}
}

// VerifyPackageHash reports whether the package's computed hash
// matches the supplied versioned reference. Returns wrapped
// ErrHashMismatch (or ErrMalformedHash for a structurally invalid
// reference).
func VerifyPackageHash(p Package, want string) error {
	if _, _, ok := splitHash(want); !ok {
		return fmt.Errorf("%w: %q", ErrMalformedHash, want)
	}
	got, err := PackageHash(p)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("%w: got %q want %q", ErrHashMismatch, got, want)
	}
	return nil
}
