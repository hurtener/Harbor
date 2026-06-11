// Package skills is the public SDK facade over Harbor's
// internal/skills package — the token-savvy skill store and the
// Directory view that projects ranked skills into planner context
// (RFC §3.6, §6.7; D-204). Alias-based re-exports only: no behavior
// lives here. The importer, generator, capability filter, event
// emission helpers, and wire projections are deliberately private.
package skills

import (
	internal "github.com/hurtener/Harbor/internal/skills"
)

// Store + directory vocabulary — aliases of the internal types.
type (
	// SkillStore is the identity-mandatory skill store interface.
	SkillStore = internal.SkillStore
	// ConfigSnapshot is the resolved skills configuration.
	ConfigSnapshot = internal.ConfigSnapshot
	// Deps carries shared dependencies (bus, logger).
	Deps = internal.Deps
	// Skill is one stored skill.
	Skill = internal.Skill
	// SkillView is the directory's compact skill projection.
	SkillView = internal.SkillView
	// RankedSkill is one search hit with its rank score.
	RankedSkill = internal.RankedSkill
	// ListFilter scopes a List call.
	ListFilter = internal.ListFilter
	// Directory is the curated skills view for planner context.
	Directory = internal.Directory
	// DirectoryConfig configures a Directory.
	DirectoryConfig = internal.DirectoryConfig
	// DirectoryCapability gates capability-filtered directory entries.
	DirectoryCapability = internal.DirectoryCapability
	// Origin records where a skill came from (pack / generated).
	Origin = internal.Origin
	// Scope is the skill's visibility scope.
	Scope = internal.Scope
	// Selection names the directory selection strategy.
	Selection = internal.Selection
	// RetrievalMode names the opt-in Search ranking shape.
	RetrievalMode = internal.RetrievalMode
	// Embedder is the injectable text→vector callable the semantic
	// retrieval mode consumes (any embeddings.Embedder satisfies it).
	Embedder = internal.Embedder
)

// DefaultDriver is the driver name Open resolves when the config
// names none.
const DefaultDriver = internal.DefaultDriver

// Origin values.
const (
	// OriginPack — imported from a skill pack.
	OriginPack = internal.OriginPack
	// OriginGenerated — produced by the in-runtime generator.
	OriginGenerated = internal.OriginGenerated
)

// Scope values.
const (
	// ScopeSession — visible to one session (the default).
	ScopeSession = internal.ScopeSession
	// ScopeProject — visible to a project.
	ScopeProject = internal.ScopeProject
	// ScopeTenant — visible to a tenant.
	ScopeTenant = internal.ScopeTenant
	// ScopeGlobal — visible globally (explicit policy required).
	ScopeGlobal = internal.ScopeGlobal
)

// Selection values.
const (
	// SelectionPinnedThenRecent — pinned skills, then most recent.
	SelectionPinnedThenRecent = internal.SelectionPinnedThenRecent
	// SelectionPinnedThenTop — pinned skills, then top-ranked.
	SelectionPinnedThenTop = internal.SelectionPinnedThenTop
)

// RetrievalMode values.
const (
	// RetrievalDefault — the token-savvy FTS5 → regex → exact ladder.
	RetrievalDefault = internal.RetrievalDefault
	// RetrievalSemantic — embedding-similarity ranking (requires
	// Deps.Embedder).
	RetrievalSemantic = internal.RetrievalSemantic
)

// PathSemantic is the Search result path the semantic retrieval mode
// reports.
const PathSemantic = internal.PathSemantic

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrSkillNotFound — no skill under that ID.
	ErrSkillNotFound = internal.ErrSkillNotFound
	// ErrPackOverwriteRefused — pack-origin skills are overwrite-protected.
	ErrPackOverwriteRefused = internal.ErrPackOverwriteRefused
	// ErrStoreClosed — the store has been closed.
	ErrStoreClosed = internal.ErrStoreClosed
	// ErrInvalidSkill — the skill failed validation.
	ErrInvalidSkill = internal.ErrInvalidSkill
	// ErrUnknownDriver — the named skill driver is not registered.
	ErrUnknownDriver = internal.ErrUnknownDriver
	// ErrIdentityRequired — the identity triple is incomplete.
	ErrIdentityRequired = internal.ErrIdentityRequired
	// ErrInvalidConfig — the directory configuration is invalid.
	ErrInvalidConfig = internal.ErrInvalidConfig
)

// Open resolves the configured skill driver and opens it.
var Open = internal.Open

// OpenDriver opens a skill driver by explicit name.
var OpenDriver = internal.OpenDriver

// SnapshotFromConfig projects the operator config block into the
// resolved ConfigSnapshot Open consumes.
var SnapshotFromConfig = internal.SnapshotFromConfig

// NewDirectory constructs the curated Directory view over a store.
var NewDirectory = internal.NewDirectory

// DirectoryFromConfig projects the operator config block into a
// DirectoryConfig.
var DirectoryFromConfig = internal.DirectoryFromConfig

// RegisteredDrivers lists the seated skill driver names (blank-import
// sdk/drivers/prod to seat the production set).
var RegisteredDrivers = internal.RegisteredDrivers
