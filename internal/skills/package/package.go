// Package skillpkg owns Harbor's canonical complete-skill-package
// semantic core: the DTO, validator, deterministic serializer,
// versioned package hash, package support URI, and the archive /
// SKILL.md validation primitives.
//
// A complete skill package is the distributable unit that carries a
// skill's logical content (its root SKILL.md, parsed) PLUS an ordered
// normalized support manifest: the resource files (examples, assets,
// scripts) the skill body references, each recorded with its canonical
// path, MIME type, exact size, and digest. The package is the seam
// between a raw archive and any later resolver or materializer: it is
// resolver-neutral (carries no tenant, no resolver authority, no
// authorization material) and storage-free (this package defines no
// Protocol surface and performs no storage mutation).
//
// Identity model. The package DTO carries no identity fields. Identity
// is a property of the caller and the storage layer, never of the
// package bytes — the same package content hashes identically for
// every tenant, user, and session.
//
// Hashing model. `PackageHash` is the versioned content hash of the
// COMPLETE package: the logical canonical skill content PLUS the
// ordered normalized support manifest (canonical path, MIME, exact
// size, digest per entry). It is distinct from the legacy
// `skills.CanonicalContentHash` (the stored-row content hash), which
// covers only the skill body fields and carries no version and no
// manifest. The package hash is computed over the canonical
// serialization BEFORE a `skillpkg://` support URI is materialized;
// the URI embeds the hash verbatim so any authorized resolver can
// verify a package against its reference.
//
// Concurrency. Every function in this package is pure with respect to
// its arguments: no mutable package-level state, no caches, no
// goroutines. A single shared instance (or the package functions
// themselves) is safe for N concurrent goroutines under -race.
package skillpkg

import (
	"errors"
	"fmt"
	"strings"
)

// RootSkillFileName is the canonical root document of a complete
// skill package. A valid package archive contains EXACTLY ONE
// root-level, case-exact `SKILL.md`; any other spelling or placement
// (skill.md, SKILL.MD, docs/SKILL.md, two SKILL.md files) is
// rejected.
const RootSkillFileName = "SKILL.md"

// URI scheme constants. The package support URI is the bounded,
// immutable, resolver-neutral reference of ONE support file of a
// complete skill package:
// `skillpkg://<versioned-hash>/<encoded-canonical-support-path>`.
// The authority position carries the versioned PackageHash verbatim —
// never a resolver host, userinfo, port, or authorization material —
// because nothing is resolved over the network.
const (
	// URIScheme is the scheme prefix of every package support URI.
	URIScheme = "skillpkg"
	// HashVersionV1 is the current package-hash version. The
	// versioned hash string is "v1:<64-hex>"; a future v2 changes
	// the hash envelope without reusing the v1 namespace.
	HashVersionV1 = "v1"
)

// Bounds. Every bound is enforced at the DTO / archive / URI
// boundaries so an oversized or malformed package never reaches a
// downstream consumer. Rune bounds count runes (what the planner
// renders); byte bounds count bytes (what a decompressor produces).
const (
	// MaxPackageNameRunes bounds the canonical package name.
	MaxPackageNameRunes = 128
	// MaxPackageVersionRunes bounds the optional package version.
	MaxPackageVersionRunes = 64
	// MaxPackageTextRunes bounds each single logical text field
	// (title, description, trigger, task_type, one step, one
	// precondition, one failure mode, one annotation).
	MaxPackageTextRunes = 2000
	// MaxPackageSteps bounds the ordered procedural steps.
	MaxPackageSteps = 64
	// MaxPackageTags bounds the tags.
	MaxPackageTags = 32
	// MaxPackageAnnotations bounds each of RequiredTools /
	// RequiredNS / RequiredTags.
	MaxPackageAnnotations = 32
	// MaxPackageSections bounds each of Preconditions / FailureModes.
	// The two ordered text lists mirror the shape of Steps (ordered
	// text entries a planner renders), so the same cardinality bound
	// applies; an unbounded section list would let one package carry
	// an arbitrarily large logical body under the per-entry rune
	// bound.
	MaxPackageSections = 64
	// MaxPackageSupports bounds the support-manifest cardinality.
	MaxPackageSupports = 1024
	// MaxPackageTotalBytes bounds the sum of all support-file
	// decompressed sizes.
	MaxPackageTotalBytes = 64 << 20 // 64 MiB
	// MaxPackageSupportBytes bounds one support file's decompressed
	// size.
	MaxPackageSupportBytes = 16 << 20 // 16 MiB
	// MaxPackageSkillMDBytes bounds the root SKILL.md document.
	MaxPackageSkillMDBytes = 1 << 20 // 1 MiB
	// MaxPackagePathRunes bounds one canonical package path.
	MaxPackagePathRunes = 1024
	// MaxPackagePathSegmentRunes bounds one path segment.
	MaxPackagePathSegmentRunes = 255
	// MaxURIRunes bounds the whole `skillpkg://` support URI string.
	MaxURIRunes = 512
	// MaxPackageURIPathRunes bounds a canonical support path so that
	// EVERY path Package.Validate accepts is representable by the
	// exact bounded skillpkg URI constructor: the whole URI string
	// (`skillpkg://<v1:<64-hex>>/<path>`) must stay within
	// MaxURIRunes. The value is derived, not guessed:
	// MaxURIRunes - len("skillpkg://") - len("v1:") - 64 - len("/").
	MaxPackageURIPathRunes = MaxURIRunes - len(URIScheme) - len("://") - len(HashVersionV1) - 1 - 64 - 1
	// MaxCanonicalBytes bounds the canonical serialization input
	// accepted by FromCanonicalBytes, enforced BEFORE decoding so no
	// unbounded allocation can happen from an oversized or
	// pathological document. The bound is a closed constant with
	// headroom over the worst-case canonical form of a valid package
	// (the JSON-escaped sum of every bounded logical text field —
	// MaxPackageSteps / MaxPackageSections / MaxPackageTags /
	// MaxPackageAnnotations entries of up to MaxPackageTextRunes
	// runes — plus the support manifest of up to MaxPackageSupports
	// bounded paths + MIME + size + digest).
	MaxCanonicalBytes = 16 << 20 // 16 MiB

	// DefaultMaxArchiveEntries is the default archive entry-count
	// bound (zip-bomb count gate).
	DefaultMaxArchiveEntries = 1024
	// DefaultMaxArchiveEntryBytes is the default per-entry
	// decompressed byte bound (zip-bomb size gate).
	DefaultMaxArchiveEntryBytes = MaxPackageSupportBytes
	// DefaultMaxArchiveTotalBytes is the default total decompressed
	// byte bound.
	DefaultMaxArchiveTotalBytes = MaxPackageTotalBytes
	// DefaultMaxCompressionRatio is the default decompression
	// amplification gate (zip-bomb ratio gate). An entry whose
	// uncompressed size exceeds `compressed * ratio` is rejected.
	DefaultMaxCompressionRatio = 1000.0
)

// PackageSkill is the logical skill content a package carries. It is
// the canonical semantic core shared by the importer path and any
// later proposal path: the same field set both parse into and the
// hash covers. It deliberately carries no storage envelope fields
// (Origin, Scope, timestamps, ContentHash) — those are properties of
// the stored row, not of the package's logical content.
type PackageSkill struct {
	// Name is the canonical skill name. Required.
	Name string `json:"name"`
	// Title is the human-readable title (may be empty).
	Title string `json:"title,omitempty"`
	// Description is the free-form body text (may be empty).
	Description string `json:"description,omitempty"`
	// Trigger is the planner-visible match cue. Required.
	Trigger string `json:"trigger"`
	// TaskType is the planner-facing task class
	// (browser|api|code|domain|unknown).
	TaskType string `json:"task_type,omitempty"`
	// Tags are search/classification tags. Unordered; the canonical
	// serialization sorts them.
	Tags []string `json:"tags,omitempty"`
	// Steps are the ordered procedural steps. Required (>= 1).
	Steps []string `json:"steps"`
	// Preconditions are the optional ordered preconditions.
	Preconditions []string `json:"preconditions,omitempty"`
	// FailureModes are the optional ordered failure modes.
	FailureModes []string `json:"failure_modes,omitempty"`
	// RequiredTools are capability-annotation tool names. Metadata
	// only — NEVER a grant.
	RequiredTools []string `json:"required_tools,omitempty"`
	// RequiredNS are capability-annotation namespaces. Metadata only.
	RequiredNS []string `json:"required_ns,omitempty"`
	// RequiredTags are capability-annotation tags. Metadata only.
	RequiredTags []string `json:"required_tags,omitempty"`
}

// SupportFile is ONE entry of the ordered normalized support
// manifest. The four identity-bearing fields are exactly: canonical
// path, MIME, exact size, digest. `Data` is materialization-only and
// never participates in identity (the digest and size do).
type SupportFile struct {
	// Path is the canonical, root-relative, forward-slash path
	// (e.g. "examples/demo.json"). Never absolute, never a `..`
	// segment, never a backslash, ASCII-only (which closes the
	// Unicode-collision class by construction).
	Path string `json:"path"`
	// Mime is the canonical MIME type of the file, drawn from the
	// closed v1.28 support allowlist (mime.go). Entries outside the
	// allowlist are rejected before a SupportFile ever exists, and
	// when the entry carries materialized Data the MIME is CONTENT
	// TRUTH: the bytes must satisfy the MIME's bounded content check
	// (ValidateMimeContent), so the recorded MIME is never a bare
	// filename-suffix claim.
	Mime string `json:"mime"`
	// Size is the exact decompressed byte size.
	Size int64 `json:"size"`
	// Digest is the lowercase hex sha256 of the exact bytes.
	Digest string `json:"digest"`
	// Data is the materialized bytes. Excluded from the canonical
	// serialization and the hash. May be nil when only the manifest
	// is present (e.g. a resolver holding references).
	Data []byte `json:"-"`
}

// Package is the canonical complete-skill-package DTO: the logical
// skill content plus the ordered normalized support manifest.
//
// It is resolver-neutral (no tenant, resolver authority, or
// authorization material), storage-free (no Protocol surface, no
// storage mutation — this package only defines semantics), and
// versioned through `PackageHash`, never through mutable fields.
type Package struct {
	// Name is the canonical package name (lowercase, trimmed).
	Name string `json:"name"`
	// Version is the optional package version (may be empty).
	Version string `json:"version,omitempty"`
	// Skill is the logical canonical content of the root SKILL.md.
	Skill PackageSkill `json:"skill"`
	// Supports is the ordered normalized support manifest. The
	// canonical serialization orders it by canonical path, so the
	// hash is insensitive to caller-side ordering noise.
	Supports []SupportFile `json:"supports,omitempty"`
}

// CanonicalName returns the canonical (lowercase, trimmed) package /
// skill name form used for name identity and dedup.
func CanonicalName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}

// Sentinel errors. Callers compare via errors.Is. The archive,
// SKILL.md, URI, and hash checks each carry their own sentinel so a
// rejection is actionable; `ErrInvalidPackage` is the umbrella for
// DTO-level structural failures.
var (
	// ErrInvalidPackage — the DTO failed structural validation.
	ErrInvalidPackage = errors.New("skillpkg: invalid package")
	// ErrInvalidSkillContent — the logical skill content failed
	// validation (missing name / trigger / steps, or out-of-bounds).
	ErrInvalidSkillContent = errors.New("skillpkg: invalid skill content")
	// ErrInvalidSupport — a support-manifest entry failed validation.
	ErrInvalidSupport = errors.New("skillpkg: invalid support entry")
	// ErrSupportDigestMismatch — a support entry's Data bytes do not
	// hash to its declared Digest.
	ErrSupportDigestMismatch = errors.New("skillpkg: support digest mismatch")
	// ErrSupportSizeMismatch — a support entry's Data length does not
	// match its declared Size.
	ErrSupportSizeMismatch = errors.New("skillpkg: support size mismatch")
	// ErrUnsupportedMime — a support entry's MIME is outside the
	// canonical allowlist.
	ErrUnsupportedMime = errors.New("skillpkg: unsupported support MIME")
	// ErrHashMismatch — a package's hash does not match the
	// reference the caller supplied.
	ErrHashMismatch = errors.New("skillpkg: package hash mismatch")
	// ErrMalformedHash — a hash string is not a valid versioned
	// package hash.
	ErrMalformedHash = errors.New("skillpkg: malformed package hash")
)

// Validation helpers shared across the DTO checks.

func errInvalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidPackage, fmt.Sprintf(format, args...))
}

func errSupportf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidSupport, fmt.Sprintf(format, args...))
}
