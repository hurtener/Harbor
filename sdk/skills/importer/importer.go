// Package importer is the public SDK facade over Harbor's
// internal/skills/importer package — the Skills.md ingest path
// (RFC §3.6, §6.7). Alias-based re-exports only: no
// behavior lives here. The
// use-memory-and-skills-from-go recipe documents ImportAndStore as the
// one ingest surface the `harbor skill import` verb wraps, so
// the conversion flushed the names out. The streaming Importer
// interface, the parser, and the export path are deliberately private
// — ImportAndStore is the supported one-call shape.
package importer

import (
	internal "github.com/hurtener/Harbor/internal/skills/importer"
)

// Ingest vocabulary — aliases of the internal types.
type (
	// Deps carries the importer's runtime dependencies (the artifact
	// store attachments upload through).
	Deps = internal.Deps
	// ImportReport summarises one successful ImportAndStore.
	ImportReport = internal.ImportReport
	// ImportStoreOption customises ImportAndStore (e.g. WithOverwrite).
	ImportStoreOption = internal.ImportStoreOption
)

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrDuplicateSkillName — a same-name skill exists and overwrite
	// was not requested.
	ErrDuplicateSkillName = internal.ErrDuplicateSkillName
	// ErrMissingFrontmatter — the source has no YAML frontmatter.
	ErrMissingFrontmatter = internal.ErrMissingFrontmatter
	// ErrMalformedYAML — the frontmatter did not parse.
	ErrMalformedYAML = internal.ErrMalformedYAML
	// ErrMissingTrigger — the mandatory planner match cue is absent.
	ErrMissingTrigger = internal.ErrMissingTrigger
	// ErrEmptySteps — Skills.md requires at least one step.
	ErrEmptySteps = internal.ErrEmptySteps
	// ErrUnknownSection — an unrecognised `## Heading` (fails closed).
	ErrUnknownSection = internal.ErrUnknownSection
	// ErrAttachmentOutsideRoot — an attachment path escaped the
	// allowed root (CLAUDE.md §7 path-traversal rule).
	ErrAttachmentOutsideRoot = internal.ErrAttachmentOutsideRoot
)

// ImportAndStore parses one Skills.md file, uploads its attachments,
// and persists the skill under the supplied identity — the same
// one-call path `harbor skill import` drives.
var ImportAndStore = internal.ImportAndStore

// WithOverwrite opts ImportAndStore into replacing an existing
// same-name skill (pack-origin rows stay overwrite-protected at the
// store).
var WithOverwrite = internal.WithOverwrite
