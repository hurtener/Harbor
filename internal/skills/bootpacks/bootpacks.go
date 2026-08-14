// Package bootpacks owns the boot-declared, resource-free operator
// skill baseline loader: an eager, immutable, config-file-relative
// index of the per-agent SKILL.md pack directories declared in
// `skills.boot_agent_packs`.
//
// The index is built ONCE by [New] — which reads, validates, parses,
// and freezes every declared package directory before returning — and
// is then served read-only for the process lifetime. Lookups never
// touch the filesystem after construction, never consult a store,
// never write a row, and never watch for changes: the baseline is
// boot-declared read-only state, node-local, reconstructed at every
// boot, and removed only by building a new index from a new config
// (there is no tombstone and no delete verb — config removal is
// represented by the absence of the key in the next index).
//
// Each declaration binds to the exact (tenant_id, agent_id) pair.
// agent_id is a runtime/config entity, never an isolation principal:
// the run still starts from the caller's verified identity triple and
// its signed reach to the effective agent.
//
// Each include is ONE relative package directory holding exactly one
// case-sensitive top-level regular UTF-8 `SKILL.md`. v1.28 baselines
// are resource-free: support-file references and every additional file
// or subdirectory are rejected. Loading rejects symlinks, hardlinks
// (nlink > 1), devices/FIFOs/sockets/specials, path swaps between stat
// and open, duplicate normalized include paths, and duplicate canonical
// skill names, under declaration / item / per-file / aggregate-byte
// bounds. A relative (unresolved) declaration directory fails loud —
// the loader never resolves against the process CWD.
//
// Every entry parses through the ONE existing importer/validator — the
// pure single-document package ingest — injected as [Deps.Parser] so
// the loader itself holds no store dependency. Required-tool metadata
// is validated against the injected static-catalog compatibility
// reader ([Deps.Catalog]); it grants nothing, and required namespaces /
// tags are not validated in v1.28. The index exposes deterministic
// ordered keys, deep-copy immutable lookup data, an ownership
// predicate, and a deterministic `boot_pack_set_hash` over the
// canonical ordered name+semantic-hash pairs of each key, stable
// across restarts over identical files.
//
// Concurrency: [Index] is immutable after construction and safe for N
// concurrent goroutines under -race. Lookups return deep copies; the
// frozen index never shares mutable backing arrays with a caller.
package bootpacks

import (
	"context"
	"errors"

	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/importer"
)

// Key is the exact (tenant_id, agent_id) pair a boot pack declaration
// binds to. The index is keyed by the exact declared pair; lookups
// with a different tenant or a different agent never compose it, and
// no placeholder or wildcard identity is ever served.
type Key struct {
	TenantID string
	AgentID  string
}

// String returns the deterministic "<tenant>/<agent>" display form.
func (k Key) String() string {
	return k.TenantID + "/" + k.AgentID
}

// Entry is ONE loaded, frozen boot-pack item: the parsed skill in
// stored form plus its two hashes and its source directory. Entries
// are immutable; [Index.Lookup] returns deep copies.
type Entry struct {
	// Skill is the parsed stored-skill form (Origin=pack,
	// Scope=project — the envelope the pure package parser fixes).
	Skill skills.Skill
	// PackageHash is the versioned complete-package hash
	// (v1:<64-hex>) computed by the pure package hash over the
	// canonical package DTO.
	PackageHash string
	// SemanticHash is the canonical content hash of Skill
	// (skills.CanonicalContentHash) — the semantic identity the
	// boot pack set hash pairs with the canonical name.
	SemanticHash string
	// Source is the normalized absolute include directory
	// (filepath.Join(declaration.Directory, include)) the entry was
	// loaded from.
	Source string
}

// Parser is the injected pure single-document package-ingest seam.
// The importer's Importer satisfies it — the ONE existing
// importer/validator: the loader never reimplements frontmatter
// parsing, body normalization, or validation. Injection keeps the
// loader free of any store dependency.
type Parser interface {
	// ImportPackageMarkdown ingests ONE bounded UTF-8 SKILL.md
	// document as a resource-free complete skill package: the same
	// parser / canonical DTO / versioned hash as the archive ingest,
	// with every relative support-file reference rejected (no support
	// manifest exists to satisfy it).
	ImportPackageMarkdown(ctx context.Context, src importer.PackageMarkdownSource) (importer.PackageIngest, error)
}

// ToolCatalog is the injected static-catalog compatibility reader the
// loader validates required_tools against at boot. It is a pure
// predicate over tool names: the loader never queries a live registry
// and never grants capabilities — required_tools is metadata only.
type ToolCatalog interface {
	// Compatible reports whether the static tool catalog can satisfy
	// tool. A boot pack whose required_tools names a tool the catalog
	// cannot satisfy fails boot loud.
	Compatible(tool string) bool
}

// Limits bounds the cost of one eager load. Zero-valued fields fall
// back to the canonical defaults; there is no knob for relaxing the
// per-file or declaration/item bounds (those are the pure package core
// and config contracts respectively).
type Limits struct {
	// MaxAggregateBytes bounds the total SKILL.md bytes across every
	// declaration of one index (default MaxAggregateBytes).
	MaxAggregateBytes int64
}

// Normalize fills zero-valued limit fields with the canonical
// defaults.
func (l Limits) Normalize() Limits {
	if l.MaxAggregateBytes <= 0 {
		l.MaxAggregateBytes = MaxAggregateBytes
	}
	return l
}

// Deps carries the injected pure dependencies of [New].
type Deps struct {
	// Parser is the pure single-document package ingest. Required.
	Parser Parser
	// Catalog is the static-catalog compatibility reader used to
	// validate required_tools. Required (fail loud at boot when a
	// required dependency is missing — never a silent skip).
	Catalog ToolCatalog
	// Limits bounds the aggregate byte cost of the load. Optional
	// (zero values fall back to the canonical defaults).
	Limits Limits
}

// Bounds. Exported so the eventual boot resolver shares ONE
// deterministic contract with the loader.
const (
	// MaxAggregateBytes bounds the total SKILL.md bytes across every
	// declaration of one index. The index is eager and in-memory, so
	// the aggregate read cost must be closed even though each
	// declaration is individually bounded. Mirrors the complete-
	// package core's total-byte precedent (64 MiB).
	MaxAggregateBytes = 64 << 20 // 64 MiB
)

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrBootPackInvalid is the umbrella for every structural
	// rejection of a boot pack declaration (shape, bounds, duplicate
	// keys).
	ErrBootPackInvalid = errors.New("bootpacks: invalid boot pack declaration")
	// ErrDepsIncomplete — [New] received missing required
	// dependencies (nil Parser / Catalog).
	ErrDepsIncomplete = errors.New("bootpacks: incomplete dependencies")
	// ErrRelativeDirectory — a declaration directory is not absolute.
	// Config-file-relative directories must be resolved by the config
	// loader (against the config file's directory, never CWD); the
	// loader fails loud on an unresolved value.
	ErrRelativeDirectory = errors.New("bootpacks: relative directory unresolved (config-file-relative directories must be resolved before the loader runs; never resolve against CWD)")
	// ErrNotDirectory — an include path is not a real directory.
	ErrNotDirectory = errors.New("bootpacks: include path is not a directory")
	// ErrSymlink — a directory or file in the include tree is a
	// symlink.
	ErrSymlink = errors.New("bootpacks: symlink rejected")
	// ErrHardlink — a file has nlink > 1 (hardlinked) or its link
	// count cannot be verified.
	ErrHardlink = errors.New("bootpacks: hardlink rejected")
	// ErrSpecialFile — a non-regular file entry (device / FIFO /
	// socket / other special) appears in the include tree.
	ErrSpecialFile = errors.New("bootpacks: non-regular file entry rejected")
	// ErrPathSwap — a file's identity changed between stat and open.
	ErrPathSwap = errors.New("bootpacks: file identity changed between stat and open")
	// ErrSkillMDEntry — a package directory does not contain exactly
	// one case-sensitive top-level SKILL.md and nothing else.
	ErrSkillMDEntry = errors.New("bootpacks: package directory must contain exactly one case-sensitive top-level SKILL.md and nothing else")
	// ErrDuplicateInclude — the same normalized include path is
	// declared twice for one (tenant, agent) key.
	ErrDuplicateInclude = errors.New("bootpacks: duplicate normalized include path")
	// ErrDuplicateName — two entries of one (tenant, agent) key share
	// the same canonical skill name.
	ErrDuplicateName = errors.New("bootpacks: duplicate canonical skill name")
	// ErrBoundExceeded — a declaration/item/file/aggregate-byte bound
	// was exceeded.
	ErrBoundExceeded = errors.New("bootpacks: bound exceeded")
	// ErrRequiredTool — an entry's required_tools names a tool the
	// injected static catalog cannot satisfy.
	ErrRequiredTool = errors.New("bootpacks: required_tools not satisfied by the static catalog")
)
