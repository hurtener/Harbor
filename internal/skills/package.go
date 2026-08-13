package skills

// package.go — the complete-skill-package facade.
//
// The canonical complete-skill-package semantic core lives in the
// `skillpkg` subpackage (internal/skills/package): the DTO
// (`Package`, `PackageSkill`, `SupportFile`), the validator, the
// deterministic serializer, the versioned `PackageHash`, the bounded
// resolver-neutral `skillpkg:` URI, and the archive / SKILL.md
// validation primitives. This file re-exports the core surface on the
// `skills` package so consumers can address it without importing the
// subpackage directly; the subpackage remains the single definition
// site (one canonical implementation, no parallel copies).
//
// The package hash is VERSIONED and distinct from the legacy
// `CanonicalContentHash`: the stored-row content hash covers only the
// skill body fields; `PackageHash` covers the logical canonical
// content PLUS the ordered normalized support manifest (canonical
// path, MIME, exact size, digest per entry), and its string form
// carries a version prefix (`v1:<64-hex>`).
//
// This surface is semantic only: it defines no Protocol authority and
// performs no storage mutation. Identity is the caller's concern, as
// everywhere in the skills subsystem.

import (
	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// Complete-skill-package DTO types.
type (
	// Package is the canonical complete-skill-package DTO.
	Package = skillpkg.Package
	// PackageSkill is the logical skill content a package carries.
	PackageSkill = skillpkg.PackageSkill
	// SupportFile is one ordered normalized support-manifest entry.
	SupportFile = skillpkg.SupportFile
	// ArchiveEntry is one validated archive entry.
	ArchiveEntry = skillpkg.ArchiveEntry
	// PackageURI is the bounded resolver-neutral package reference.
	PackageURI = skillpkg.URI
	// PackageArchiveLimits bounds an archive scan.
	PackageArchiveLimits = skillpkg.ArchiveLimits
	// PackageMarkdownLimits bounds a SKILL.md scan.
	PackageMarkdownLimits = skillpkg.MarkdownLimits
)

// Package constants.
const (
	// RootSkillFileName is the canonical root document of a complete
	// skill package (exactly one, root-level, case-exact).
	RootSkillFileName = skillpkg.RootSkillFileName
	// PackageURIScheme is the `skillpkg` URI scheme.
	PackageURIScheme = skillpkg.URIScheme
	// PackageHashVersionV1 is the current package-hash version.
	PackageHashVersionV1 = skillpkg.HashVersionV1
)

// PackageHash returns the versioned content hash of the complete
// package: the logical canonical skill content plus the ordered
// normalized support manifest, hashed BEFORE skillpkg URI
// materialization. Distinct from the legacy `CanonicalContentHash`.
func PackageHash(p Package) (string, error) { return skillpkg.PackageHash(p) }

// VerifyPackageHash reports whether the package's computed hash
// matches the supplied versioned reference.
func VerifyPackageHash(p Package, want string) error { return skillpkg.VerifyPackageHash(p, want) }

// CanonicalPackageBytes returns the canonical identity-bearing
// serialization of the package (the input to PackageHash).
func CanonicalPackageBytes(p Package) ([]byte, error) { return skillpkg.CanonicalBytes(p) }

// PackageFromCanonicalBytes reconstructs a Package from the canonical
// serialization produced by CanonicalPackageBytes.
func PackageFromCanonicalBytes(b []byte) (Package, error) { return skillpkg.FromCanonicalBytes(b) }

// ParsePackageURI parses a bounded resolver-neutral `skillpkg:` URI.
func ParsePackageURI(s string) (PackageURI, error) { return skillpkg.ParseURI(s) }

// NewPackageURI builds a package URI from a versioned PackageHash and
// an optional canonical name hint.
func NewPackageURI(hash, name string) (PackageURI, error) { return skillpkg.NewURI(hash, name) }

// ValidatePackageArchive scans a zip archive into the ordered
// canonical entry list, rejecting traversal, absolute/case/Unicode
// collisions, links/devices, nested archives, unsupported MIME, and
// decompression + count/size/ratio violations.
func ValidatePackageArchive(b []byte, limits PackageArchiveLimits) ([]ArchiveEntry, error) {
	return skillpkg.ValidateArchive(b, limits)
}

// ValidateRootSkillEntries enforces the one-root-case-exact-SKILL.md
// invariant over a validated entry list.
func ValidateRootSkillEntries(entries []ArchiveEntry) error {
	return skillpkg.ValidateRootSkillEntries(entries)
}

// ValidateSkillMarkdown is the canonical document-level gate for the
// root SKILL.md bytes.
func ValidateSkillMarkdown(b []byte, limits PackageMarkdownLimits) error {
	return skillpkg.ValidateSkillMarkdown(b, limits)
}

// Complete-skill-package sentinel errors (compare via errors.Is).
var (
	ErrInvalidPackage            = skillpkg.ErrInvalidPackage
	ErrInvalidSkillContent       = skillpkg.ErrInvalidSkillContent
	ErrInvalidSupport            = skillpkg.ErrInvalidSupport
	ErrSupportDigestMismatch     = skillpkg.ErrSupportDigestMismatch
	ErrSupportSizeMismatch       = skillpkg.ErrSupportSizeMismatch
	ErrUnsupportedMime           = skillpkg.ErrUnsupportedMime
	ErrHashMismatch              = skillpkg.ErrHashMismatch
	ErrMalformedHash             = skillpkg.ErrMalformedHash
	ErrMalformedURI              = skillpkg.ErrMalformedURI
	ErrURITooLong                = skillpkg.ErrURITooLong
	ErrArchiveNotZip             = skillpkg.ErrArchiveNotZip
	ErrArchiveCorrupt            = skillpkg.ErrArchiveCorrupt
	ErrArchivePathInvalid        = skillpkg.ErrArchivePathInvalid
	ErrArchiveTraversal          = skillpkg.ErrArchiveTraversal
	ErrArchivePathCollision      = skillpkg.ErrArchivePathCollision
	ErrArchiveNonRegular         = skillpkg.ErrArchiveNonRegular
	ErrArchiveNested             = skillpkg.ErrArchiveNested
	ErrArchiveMimeUnsupported    = skillpkg.ErrArchiveMimeUnsupported
	ErrArchiveTooManyEntries     = skillpkg.ErrArchiveTooManyEntries
	ErrArchiveEntryTooLarge      = skillpkg.ErrArchiveEntryTooLarge
	ErrArchiveTotalTooLarge      = skillpkg.ErrArchiveTotalTooLarge
	ErrArchiveRatioTooHigh       = skillpkg.ErrArchiveRatioTooHigh
	ErrSkillMDMissing            = skillpkg.ErrSkillMDMissing
	ErrSkillMDMultiple           = skillpkg.ErrSkillMDMultiple
	ErrSkillMDNotRoot            = skillpkg.ErrSkillMDNotRoot
	ErrSkillMDCaseMismatch       = skillpkg.ErrSkillMDCaseMismatch
	ErrSkillMDTooLarge           = skillpkg.ErrSkillMDTooLarge
	ErrSkillMDFrontmatterMissing = skillpkg.ErrSkillMDFrontmatterMissing
	ErrSkillMDMalformedYAML      = skillpkg.ErrSkillMDMalformedYAML
	ErrSkillMDMissingTrigger     = skillpkg.ErrSkillMDMissingTrigger
	ErrSkillMDEmptySteps         = skillpkg.ErrSkillMDEmptySteps
)
