// Package importer ships Harbor's Skills.md importer — the
// predecessor's gap-closer (RFC §6.7, brief 04 §4.7). The importer
// is a YAML-frontmatter + line-based Markdown-body parser that
// round-trips a Skills.md source through the Phase 37
// `skills.Skill` envelope (Origin=PackImport) and back, byte-stable.
//
// The byte-stable round-trip is the gate: `Export(Import(b)) == b`
// for any spec-compliant `b`, asserted via `bytes.Equal` on the
// golden corpus under `testdata/golden`. Authors hand-order YAML
// keys; the importer preserves their ordering by carrying the raw
// frontmatter bytes verbatim between the `---` fences and only
// parsing the YAML for field extraction (D-053).
//
// Attachments — inline `![alt](path)` references — resolve to
// `artifacts.ArtifactRef` via the injected `Deps.Store` (option (b)
// per RFC §6.7). The body keeps an `artifact://<ID>` URI in place
// of the relative path; `ImportArtifacts.PathToRef` carries the
// reverse mapping so `Export` materialises the original path
// verbatim. Path-traversal is blocked at `path_safety.go` per
// CLAUDE.md §7 rule 5.
//
// Concurrent reuse (D-025): one `*Importer` is safe to share across
// N goroutines under `-race`. The struct has no per-call mutable
// state on itself; everything lives in `ctx` + `Import` / `Export`
// args. The injected `ArtifactStore` is itself D-025 safe per
// Phase 17's conformance suite.
//
// Identity-mandatory contract: the importer does NOT take an
// `identity.Quadruple` directly — its responsibility is parse +
// upload + canonicalise. The caller threads the identity through
// when they hand the produced `Skill` to `skills.SkillStore.Upsert`;
// Phase 37's contract owns the identity-rejection emit there.
//
// Failure modes (sentinel-typed; compare via errors.Is):
//
//   - ErrMissingFrontmatter      — no `---` fence found at line 1
//   - ErrMalformedYAML           — `---` fence found but YAML parse failed
//   - ErrMissingTrigger          — frontmatter parsed but `trigger:` empty (wraps skills.ErrInvalidSkill)
//   - ErrEmptySteps              — body parsed but `## Steps` absent or empty (wraps skills.ErrInvalidSkill)
//   - ErrUnknownSection          — body section heading not in the canonical set
//   - ErrAttachmentOutsideRoot   — relative attachment path escapes AllowedRoot
//   - ErrInvalidAttachmentRef    — Export saw a body ref not in ImportArtifacts
//   - ErrRoundTripDrift          — bytes.Equal(src, Export(Import(src))) failed (used in tests)
//   - ErrImporterClosed          — method called after Close
//
// No silent-degradation path (CLAUDE.md §13).
package importer

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/skills"
)

// ImportSource carries the bytes to parse and the operator-declared
// AllowedRoot for attachment resolution. PathHint is metadata only;
// the importer never reads from it. AllowedRoot MUST be a directory
// the operator trusts to host attachment payloads; the importer
// canonicalises every relative attachment path under it via
// path_safety.go.
type ImportSource struct {
	Scope       artifacts.ArtifactScope
	PathHint    string
	AllowedRoot string
	Bytes       []byte
}

// ImportArtifacts captures the round-trip-load-bearing mapping
// between inline attachment paths and the ArtifactRefs they
// resolved to. Export consults this slice to materialise each
// ArtifactRef.ID back to its source-side path verbatim.
//
// PathToRef preserves insertion order so Export emits attachments
// in the same order the parser encountered them. The importer
// rejects duplicate paths in one source at Import time
// (ErrInvalidAttachmentRef) so the Export injectivity invariant
// is preserved.
type ImportArtifacts struct {
	PathToRef []AttachmentMapping
}

// AttachmentMapping is one entry in ImportArtifacts.PathToRef.
type AttachmentMapping struct {
	Ref  artifacts.ArtifactRef
	Path string
}

// Importer is the importer entry point. Safe to share across N
// concurrent goroutines (D-025); per-call state lives in the
// Import / Export arguments.
type Importer interface {
	// Import parses Skills.md bytes into a skills.Skill record.
	// Returns wrapped sentinels on fail-loud failure modes. The
	// ImportArtifacts return value carries the path->ref mapping
	// Export consults; the caller passes it back to Export
	// verbatim for round-trip.
	Import(ctx context.Context, src ImportSource) (skills.Skill, ImportArtifacts, error)

	// Export reverses Import. Given a (Skill, ImportArtifacts)
	// pair produced by a prior Import, returns the original source
	// bytes. `Export(Import(b)) == b` for any spec-compliant b
	// (the round-trip invariant).
	Export(ctx context.Context, skill skills.Skill, attachments ImportArtifacts) ([]byte, error)

	// Close releases resources. Idempotent. Currently a no-op
	// (the importer holds no long-lived resources), but included
	// for symmetry with SkillStore / ArtifactStore.
	Close(ctx context.Context) error
}

// Deps carries the runtime dependencies the Importer needs. Store
// is mandatory — the attachment resolver uploads bytes through it
// per RFC §6.7 option (b).
type Deps struct {
	Store artifacts.ArtifactStore
}

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrMissingFrontmatter — no `---` fence at the start of the
	// source. Skills.md REQUIRES YAML frontmatter.
	ErrMissingFrontmatter = errors.New("importer: missing YAML frontmatter")
	// ErrMalformedYAML — `---` fence found but the YAML body did
	// not parse. Truncated fences (no closing `---`), or YAML
	// syntax errors land here.
	ErrMalformedYAML = errors.New("importer: malformed YAML frontmatter")
	// ErrMissingTrigger — frontmatter parsed but `trigger:` was
	// empty. Wraps skills.ErrInvalidSkill — the planner match cue
	// is mandatory.
	ErrMissingTrigger = errors.New("importer: missing trigger (planner match cue mandatory)")
	// ErrEmptySteps — body parsed but `## Steps` was absent or
	// contained zero list items. Wraps skills.ErrInvalidSkill —
	// Skills.md requires >= 1 step.
	ErrEmptySteps = errors.New("importer: empty steps (Skills.md requires >= 1 step)")
	// ErrUnknownSection — body contained a `## Heading` that does
	// not normalise to one of {Steps, Preconditions, Failure
	// modes}. V1 fails closed (no lenient flag — CLAUDE.md §13).
	ErrUnknownSection = errors.New("importer: unknown section heading")
	// ErrAttachmentOutsideRoot — an `![alt](path)` reference
	// resolved to a path outside AllowedRoot, or AllowedRoot is
	// empty and the source contains attachments. CLAUDE.md §7 #5.
	ErrAttachmentOutsideRoot = errors.New("importer: attachment path outside allowed root")
	// ErrInvalidAttachmentRef — Export saw an artifact:// reference
	// in the body that doesn't have a matching entry in
	// ImportArtifacts.PathToRef.
	ErrInvalidAttachmentRef = errors.New("importer: attachment ref not in ImportArtifacts mapping")
	// ErrRoundTripDrift — used in tests; surfaced when an
	// `Export(Import(b))` returns bytes that differ from b.
	ErrRoundTripDrift = errors.New("importer: Export(Import(b)) != b (caller bug or corruption)")
	// ErrImporterClosed — any method called after Close.
	ErrImporterClosed = errors.New("importer: closed")
)

// importer is the concrete Importer implementation. It has no
// mutable per-call state on itself; everything lives in ctx + args.
// `closed` is an atomic flag flipped by Close so subsequent calls
// fail loudly.
type importerImpl struct {
	store  artifacts.ArtifactStore
	closed atomic.Bool
}

// New constructs an Importer. Returns wrapped errors if Deps is
// incomplete (Store is mandatory — attachment resolution depends
// on it).
func New(deps Deps) (Importer, error) {
	if deps.Store == nil {
		return nil, fmt.Errorf("importer: Deps.Store is required (artifacts.ArtifactStore)")
	}
	return &importerImpl{store: deps.Store}, nil
}

// Import implements Importer.Import. The pipeline is:
//
//  1. Detect + extract frontmatter via the line-based scanner.
//  2. Parse the YAML body for field extraction (raw bytes preserved
//     verbatim for Export).
//  3. Walk the body line-by-line: capture the description (prose
//     before the first `## ` heading), then normalise each section
//     by canonical heading.
//  4. Resolve inline `![alt](path)` references — read the file
//     under AllowedRoot, upload via Deps.Store, replace the path in
//     the body with `artifact://<ID>`.
//  5. Synthesise a skills.Skill record (Origin=OriginPack,
//     Scope=ScopeProject default) and validate via skills.Skill.Validate.
func (i *importerImpl) Import(ctx context.Context, src ImportSource) (skills.Skill, ImportArtifacts, error) {
	if i.closed.Load() {
		return skills.Skill{}, ImportArtifacts{}, ErrImporterClosed
	}
	if err := ctx.Err(); err != nil {
		return skills.Skill{}, ImportArtifacts{}, err
	}
	return doImport(ctx, i.store, src)
}

// Export implements Importer.Export. The pipeline is the inverse:
//
//  1. Emit the frontmatter raw bytes verbatim between `---` fences.
//  2. Emit the description (canonicalised — paragraphs separated by
//     a blank line).
//  3. Emit each section in canonical order (Steps, Preconditions,
//     Failure modes) skipping any that are nil/empty.
//  4. Walk the body for `artifact://<ID>` markers and substitute
//     each ID back to its source-side path via PathToRef.
//
// The result is byte-stable for spec-compliant sources produced by
// Import — golden corpus assertion via bytes.Equal.
func (i *importerImpl) Export(ctx context.Context, skill skills.Skill, attachments ImportArtifacts) ([]byte, error) {
	if i.closed.Load() {
		return nil, ErrImporterClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return doExport(skill, attachments)
}

// Close releases resources. Currently no-op; the importer holds no
// long-lived state. Idempotent.
func (i *importerImpl) Close(_ context.Context) error {
	i.closed.Store(true)
	return nil
}
