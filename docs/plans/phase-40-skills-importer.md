# Phase 40 — Skills.md importer (gap-closer)

## Summary

Phase 40 lands Harbor's spec-compliant Skills.md importer — a deterministic YAML-frontmatter + line-based body parser that maps a Skills.md file into the Phase 37 `SkillStore` `Skill` envelope with `Origin=PackImport`. The importer is the predecessor's gap-closer: it ships the byte-stable round-trip invariant (`Export(Import(bytes)) == bytes`) so a Skills.md authored once survives parse + normalize + re-emit without source edits, and ships the attachment resolver that uploads inline `![alt](path)` references to the `ArtifactStore` (option (b) per RFC §6.7) returning `ArtifactRef.ID` for body-side reference.

## RFC anchor

- RFC §6.7

## Briefs informing this phase

- brief 04

## Brief findings incorporated

- brief 04 §4.7: parse YAML frontmatter + markdown body via a deterministic parser; normalize headings `## Steps` / `## Preconditions` / `## Failure modes` into structured fields; absent sections default to empty; body narrative becomes `Description`; `Name` from frontmatter or filename slug; resolve sibling resource files referenced by relative path and record as attachments. **Round-trip test: any spec-compliant Skills.md imports without source edits and re-exports byte-stable** — this is the gate.
- brief 04 §5 (sharp edges): per-skill adaptation is the real gap; the predecessor has no native Skills.md path. Closing this is a Harbor-defining feature.
- brief 04 §6 (tests required): Skills.md round-trip — parse → normalize → re-emit byte-stable. Negative tests for missing `trigger` / empty `steps` / non-CommonMark input.
- brief 04 §6 (integration): Skills.md import golden corpus — ship N spec-compliant fixtures (with attachments) and assert byte-stable normalization.
- brief 04 §4.7 step 4 ("Validate + index via the same `Skill` validator the loader uses; fail loud on a non-empty `trigger` and non-empty `steps`") — Phase 40 calls the Phase 37 `Skill.Validate()` at the importer boundary; missing `trigger` and empty `steps` surface as wrapped `ErrInvalidSkill` (`skills: invalid skill: Trigger empty (planner match cue is mandatory)`). No silent-degradation path (CLAUDE.md §13).

## Findings I'm departing from (if any)

- **CommonMark parser scope.** Brief 04 §4.7 step 1 says "CommonMark-only parser." Phase 40 ships a **line-based deterministic parser** rather than introducing a full CommonMark dependency (`goldmark`). Rationale: byte-stable round-trip is the gate; full-AST CommonMark parsers do not have a reliable AST-to-source emitter (the surface they expose is rendering to HTML or visiting nodes), so a round-trip through one would either (a) require carrying the original source text alongside the AST and re-emitting from it (which is what we do directly) or (b) require a brand-new dependency with a custom emitter. The Skills.md format is line-structured (YAML frontmatter delimited by `---`, body sections delimited by `## Heading` lines, list items prefixed by a literal dash plus space), so a line-based parser is **stricter** than CommonMark and produces deterministic byte output by construction. The format diverges from full CommonMark only by rejecting (a) setext-style headings (we accept only the ATX two-hash form), (b) tabs as indentation (we accept spaces only), (c) lazy-continuation list items (one line = one list item). The departure is documented in D-053 and recorded here.
- **YAML field-ordering preservation.** Brief 04 §4.7 does not name a YAML library. Phase 40 uses `goccy/go-yaml` (already in the module graph, used by `internal/config/loader.go`) but does **not** rely on its emission for round-trip — instead, the importer captures the raw frontmatter bytes verbatim between the `---` fences and emits them back as-is. The parsed values feed validation + canonicalisation; the raw bytes feed the round-trip. This is the only design that survives the round-trip invariant when an author hand-orders frontmatter keys (which Skills.md authors routinely do — `name` first, `description` second).

## Goals

- Define the `Importer` surface at `internal/skills/importer/` with two operations: `Import(ctx, src ImportSource) (skills.Skill, ImportArtifacts, error)` and `Export(ctx, skill skills.Skill, artifacts ImportArtifacts) ([]byte, error)`.
- Parse YAML frontmatter + structured Markdown body via a deterministic line-based parser; fail loudly on missing frontmatter / missing `trigger` / empty `steps`.
- Normalize the canonical body sections — `## Steps`, `## Preconditions`, `## Failure modes` — accepting common heading variations (case, plural / singular) and emitting canonical headings on Export.
- Resolve inline attachment references (`![alt](path)`) as `ArtifactRef`s via an injected `ArtifactStore` (option (b) per RFC §6.7); on Import, the bytes upload and the body retains the `ArtifactRef.ID`; on Export, the `ArtifactRef.ID` materializes back to the original path. The mapping is captured in `ImportArtifacts.PathToRef`.
- Path-traversal-protected attachment resolution: every relative path is normalized via `filepath.Clean` and verified against the operator-supplied `AllowedRoot` (CLAUDE.md §7 rule 5). The helper lives at `internal/skills/importer/path_safety.go`.
- Byte-stable round-trip invariant: `Export(Import(bytes)) == bytes` for the golden corpus, asserted via `bytes.Equal`.
- D-025 concurrent-reuse: one `*Importer` is safe to share across N >= 128 concurrent goroutines under `-race`.
- Identity is mandatory at the importer's persistence boundary (`Import -> SkillStore.Upsert` path): the writer stamps `Origin=PackImport`, `OriginRef`, identity quadruple.
- 90% coverage on `internal/skills/importer` — the highest-stakes phase in Stage D.

## Non-goals

- Generator persistence (`skill_propose(persist=true)`) — Phase 41.
- Planner-facing tools (`skill_search` / `skill_get` / `skill_list`) — already landed Phase 38.
- Virtual directory subsystem (`Directory(cfg)`) — Phase 39 (parallel dispatch).
- Multi-pack manifest parsing — V1 ships per-file imports; pack-level manifests (`skill-pack.yaml`) are a post-V1 follow-up.
- Markdown rendering / HTML emission — the importer does not render; it round-trips the source bytes.
- Inline-image fetch over HTTP — only filesystem-relative attachments resolve; remote URLs (http://, https://, data:) are recorded as `Extra.attachments` metadata WITHOUT bytes upload (no network in the importer; Phase 40 stays offline).
- LLM-driven section extraction — the parser is fully deterministic.

## Acceptance criteria

- [ ] `Importer` exposed at `internal/skills/importer/importer.go`; sentinels (`ErrMissingFrontmatter`, `ErrMissingTrigger`, `ErrEmptySteps`, `ErrMalformedYAML`, `ErrUnknownSection`, `ErrAttachmentOutsideRoot`, `ErrRoundTripDrift`, `ErrInvalidAttachmentRef`, `ErrImporterClosed`) compare via `errors.Is`.
- [ ] Golden corpus of **>= 5** Skills.md fixtures under `internal/skills/importer/testdata/golden/` — `minimal.md` (trigger + steps only), `full.md` (all sections), `preconditions-only.md`, `failure-modes-only.md`, `with-attachments.md`. Each ships with a `.want.json` mirror that the importer's `Skill` output must match.
- [ ] **Byte-stable round-trip** asserted via `bytes.Equal(src, Export(Import(src), artifacts))` for every golden fixture.
- [ ] Negative tests pass: empty file -> `ErrMissingFrontmatter`; missing `trigger` -> `ErrMissingTrigger` (wrapped `skills.ErrInvalidSkill`); empty `steps` -> `ErrEmptySteps` (wrapped `skills.ErrInvalidSkill`); malformed YAML -> `ErrMalformedYAML`; unknown section heading -> `ErrUnknownSection`; attachment path outside `AllowedRoot` -> `ErrAttachmentOutsideRoot`.
- [ ] Path-safety helper at `internal/skills/importer/path_safety.go` rejects `../`, absolute paths, symlink escapes, and empty path components via `filepath.Clean` + `strings.HasPrefix(absPath, allowedRoot)`. CLAUDE.md §7 rule 5 satisfied.
- [ ] Attachment resolution: inline `![alt](path)` uploads via injected `artifacts.ArtifactStore` and returns the `ArtifactRef.ID`; Export materializes `ArtifactRef.ID` back to the captured path verbatim. The mapping survives a `Close -> reopen` cycle (because the path mapping lives in `ImportArtifacts`, not in the importer).
- [ ] Identity-mandatory: callers persist via `skills.SkillStore.Upsert(ctx, identity.Quadruple{...}, skill)` with `Origin=OriginPack`; missing identity returns wrapped `skills.ErrIdentityRequired` AND emits `skill.identity_rejected` on the bus. (Phase 37's contract; the importer call site enforces.)
- [ ] **D-025 concurrent-reuse** holds: N >= 128 goroutines importing distinct in-memory Skills.md payloads against one shared `*Importer` under `-race`; no data races, no context bleed, no cross-cancellation, no goroutine leak (baseline-restored after teardown).
- [ ] `internal/skills/importer` coverage >= **90%** (highest-stakes Stage D phase).
- [ ] D-053 entry filed in `docs/decisions.md`.
- [ ] Glossary entries for `Skills.md`, `Importer`, `ArtifactRef` (skills context), and the round-trip invariant filed.
- [ ] `docs/plans/README.md` row for Phase 40 flipped from `Pending` to `Shipped`.
- [ ] Root `README.md` Status table updated to list Phase 40 as Shipped with a one-line pointer.

## Files added or changed

```text
internal/skills/importer/
├── importer.go                        # Importer iface + Deps + sentinels + Open
├── parser.go                          # Line-based parser (frontmatter + body sections)
├── exporter.go                        # Reverse: Skill + ImportArtifacts -> original bytes
├── path_safety.go                     # CLAUDE.md §7 rule 5 helper
├── attachments.go                     # ArtifactRef resolution (option (b))
├── importer_test.go                   # Unit + golden corpus + round-trip
├── parser_test.go                     # Internal parser unit tests
├── exporter_test.go                   # Export-side determinism
├── path_safety_test.go                # Traversal / absolute / empty / symlink rejection
├── attachments_test.go                # Path -> ArtifactRef -> path round-trip
├── concurrent_test.go                 # D-025 N=128 under -race
├── negative_test.go                   # Empty file / missing trigger / empty steps / malformed YAML
└── testdata/
    └── golden/
        ├── minimal.md                 # trigger + steps only
        ├── minimal.want.json
        ├── full.md                    # all sections populated
        ├── full.want.json
        ├── preconditions-only.md
        ├── preconditions-only.want.json
        ├── failure-modes-only.md
        ├── failure-modes-only.want.json
        ├── with-attachments.md
        ├── with-attachments.want.json
        └── attachments/
            └── example.txt            # The sample attachment payload
docs/plans/phase-40-skills-importer.md # this file
docs/plans/README.md                   # flip Phase 40 row to Shipped
README.md                              # flip Phase 40 row to Shipped + one-line pointer
docs/glossary.md                       # Skills.md, Importer, ArtifactRef (skills), round-trip invariant
docs/decisions.md                      # D-053 entry
scripts/smoke/phase-40.sh              # `go test -race ./internal/skills/importer/...` + golden corpus dir check
```

## Public API surface

```go
// internal/skills/importer/importer.go

// ImportSource carries the bytes to parse + the operator-declared
// AllowedRoot for attachment resolution. PathHint names the source
// file (used for the slugified-name fallback when frontmatter lacks
// `name`); the importer never reads from PathHint — it is metadata
// only.
type ImportSource struct {
    Bytes       []byte
    PathHint    string // e.g. "skills/foo.md" — used for slugified-name fallback
    AllowedRoot string // operator-declared safe filesystem root for attachments
    Scope       artifacts.ArtifactScope // scope under which attachments upload
}

// ImportArtifacts captures the round-trip-load-bearing mapping
// between inline attachment paths and the ArtifactRef IDs they
// resolved to. Export consults this slice to materialize each ID
// back to its source-side path verbatim.
type ImportArtifacts struct {
    // PathToRef preserves insertion order so Export emits attachments
    // in the same order the parser encountered them.
    PathToRef []AttachmentMapping
}

// AttachmentMapping is one entry in ImportArtifacts.PathToRef.
type AttachmentMapping struct {
    Path string // verbatim path as it appeared in the source
    Ref  artifacts.ArtifactRef
}

// Importer is the importer entry point. Safe to share across N
// concurrent goroutines (D-025); per-call state lives in the
// Import / Export arguments.
type Importer interface {
    Import(ctx context.Context, src ImportSource) (skills.Skill, ImportArtifacts, error)
    Export(ctx context.Context, skill skills.Skill, attachments ImportArtifacts) ([]byte, error)
    Close(ctx context.Context) error
}

type Deps struct {
    Store artifacts.ArtifactStore // mandatory — attachments resolve to ArtifactRef
}

// New constructs an Importer.
func New(deps Deps) (Importer, error)

// Sentinel errors. Compare via errors.Is.
var (
    ErrMissingFrontmatter    = errors.New("importer: missing YAML frontmatter")
    ErrMissingTrigger        = errors.New("importer: missing trigger (planner match cue mandatory)")
    ErrEmptySteps            = errors.New("importer: empty steps (Skills.md requires >= 1 step)")
    ErrMalformedYAML         = errors.New("importer: malformed YAML frontmatter")
    ErrUnknownSection        = errors.New("importer: unknown section heading")
    ErrAttachmentOutsideRoot = errors.New("importer: attachment path outside allowed root")
    ErrInvalidAttachmentRef  = errors.New("importer: attachment ref not in ImportArtifacts mapping")
    ErrRoundTripDrift        = errors.New("importer: Export(Import(b)) != b (caller bug or corruption)")
    ErrImporterClosed        = errors.New("importer: closed")
)
```

## Test plan

- **Unit:**
  - Frontmatter parsing: well-formed YAML produces the expected `name / description / trigger / task_type / tags / required_tools / required_ns / required_tags / scope` fields.
  - Frontmatter missing -> `ErrMissingFrontmatter`.
  - Body section parsing: `## Steps` -> ordered `Steps` slice (one per dash-space line); `## Preconditions` -> ordered `Preconditions` slice; `## Failure modes` -> ordered `FailureModes` slice; case + plural / singular variants normalized.
  - Body description: the prose between frontmatter end and the first two-hash heading becomes `Description`.
  - Name fallback: frontmatter without `name` derives from `PathHint`'s basename slugified (lowercase, alphanumeric, `-` between words; matches the `pack_loader._slugify` shape).
  - Validator delegation: empty `Trigger` / empty `Steps` after parsing surface as wrapped `ErrInvalidSkill` (the importer calls `skills.Skill.Validate`).
  - **Golden round-trip:** for each fixture under `testdata/golden/*.md`, run `Import -> Export -> bytes.Equal(src, exported)`. Failure prints the byte-level diff.
  - **`.want.json` schema match:** decode `testdata/golden/<name>.want.json` into a `skills.Skill` (lifecycle fields excluded) and assert deep-equal against `Import(src).Skill`.
  - Exporter on synthetic `Skill` (not produced by Import): emits canonical headings (`## Steps`, `## Preconditions`, `## Failure modes`) with deterministic ordering; attachment refs unknown to `ImportArtifacts` return `ErrInvalidAttachmentRef`.

- **Integration:**
  - **Path safety:** for each entry in a representative table — `../escape`, `/abs/path`, `./allowed/file`, `allowed/../escape`, `` (empty), `../../etc/passwd` — assert acceptance vs. rejection. Symlink escape: create a temp tree where `attachments/link` -> `../../outside.txt`; assert rejection.
  - **Attachment round-trip with real `inmem` ArtifactStore:** import `with-attachments.md`, assert each `![alt](path)` uploaded to the store and returned an `ArtifactRef`; assert `Export` materializes the original path verbatim; assert `bytes.Equal(src, exported)`.
  - **Identity propagation at the persistence boundary:** after `Import`, hand the `Skill` to a real `SkillStore.Upsert` with `Origin=OriginPack` + an `identity.Quadruple` populated. Missing identity returns wrapped `ErrIdentityRequired`. Real `events.EventBus` wired through the seam; the `skill.identity_rejected` event lands on the bus.

- **Conformance:**
  - N/A — there is one Importer implementation. (Phase 41 generator reuses the same canonicalisation rules but ships its own pipeline.)

- **Concurrency / leak:**
  - **D-025 N=128 stress:** N=128 goroutines, each calling `Import` then `Export` on a distinct in-memory Skills.md payload against one shared `*Importer`. Mix of payload shapes (minimal / full / with-attachments). Per-goroutine identity assertions on the produced `Skill`'s `Name` field (encoded with the goroutine index). Pre-cancelled ctxes on `i%5==0` return `ctx.Err()` (no cross-cancellation). Goroutine baseline restored within 500ms of `WaitGroup.Wait`.

- **Negative (binding):**
  - Empty file -> `ErrMissingFrontmatter` (no `---` fence at all).
  - Frontmatter without closing `---` -> `ErrMalformedYAML` (truncated fence).
  - Frontmatter missing `trigger` -> wrapped `skills.ErrInvalidSkill` (importer applies the Phase 37 validator).
  - Body without `## Steps` -> wrapped `skills.ErrInvalidSkill` (steps empty).
  - Body with `## Steps` but no list items -> wrapped `skills.ErrInvalidSkill`.
  - Body with an unknown `## SomethingElse` heading -> `ErrUnknownSection`. The lenient flag is **NOT** introduced at V1 (CLAUDE.md §13 — silent degradation forbidden, fail-closed default).
  - Malformed YAML (`name: : :`) -> `ErrMalformedYAML`.
  - Attachment path outside `AllowedRoot` -> `ErrAttachmentOutsideRoot`.
  - Duplicate attachment path in one source -> `ErrInvalidAttachmentRef` (duplicate-rejection at Import preserves the Export injectivity invariant).

## Smoke script additions

`scripts/smoke/phase-40.sh` asserts two things at preflight:

1. `go test -race -count=1 -timeout 120s ./internal/skills/importer/...` exits 0 — the importer's unit + integration + concurrent-reuse test surface is green under the race detector.
2. The golden corpus directory `internal/skills/importer/testdata/golden/` exists and is non-empty — the byte-stable round-trip gate has fixtures to assert against.

Both checks are Go-level (the importer surfaces no Protocol/HTTP API at Phase 40; Phase 60+ exposes import-by-upload over the Protocol). The script registers an `OK` per check.

## Coverage target

- `internal/skills/importer`: **90%** (highest-stakes Stage D phase per master plan; the byte-stable round-trip is the gate).

## Dependencies

- **Phase 37** — `skills.Skill`, `skills.SkillStore`, `skills.ErrInvalidSkill`, `skills.ErrIdentityRequired`, `Origin=OriginPack`, the canonical content hash.
- **Phase 17** — `artifacts.ArtifactStore` + `artifacts.ArtifactRef` + `artifacts.ArtifactScope` (the attachment store).
- **Phase 19** — confirms the `ArtifactRef` shape is V1-stable across drivers (the importer is driver-agnostic; it uses only the interface).
- Phase 01 / Phase 07 / Phase 03 — events bus + identity quadruple + audit redactor (consumed transitively via the `SkillStore.Upsert` call site).

## Risks / open questions

- **Round-trip stability under heading-variation normalization.** Skills.md authors write `## steps`, `## Step`, `## Steps:` interchangeably. The importer accepts these variations on the parse side but emits the canonical `## Steps` on Export. This means a source with `## steps` would NOT round-trip byte-stable. **Mitigation:** the golden corpus uses the canonical heading on every fixture (the round-trip invariant gates canonical sources only); non-canonical sources are deliberately accepted with documented normalization — captured in D-053.
- **YAML key ordering.** Phase 40 preserves the raw frontmatter bytes verbatim (captured between the `---` fences) on the Export side. The parser walks the parsed YAML tree for value extraction but never re-emits the YAML — Export concatenates the captured raw bytes back. This is the only design that survives the round-trip when authors hand-order keys. D-053 pins this.
- **CommonMark divergence.** The line-based parser is stricter than CommonMark — see Findings I'm departing from. Source files using setext headings (`Heading\n=======`), tab indentation, or lazy-continuation list items are rejected. The corpus uses ATX-only / spaces-only / one-line-one-item shapes throughout. CLAUDE.md §13 forbids "two parallel implementations of the same conceptual feature" — adding a CommonMark-compat fallback would be exactly that. We pick line-based + fail-loudly.
- **Attachment scope.** The importer uploads attachments under the caller-supplied `ArtifactScope` on `ImportSource.Scope`. The convention is that pack imports stamp `ArtifactScope.TaskID = "import:" + sha256(src)[:12]` so all attachments of one Skills.md file cluster under a stable task-shaped key. The importer does not synthesise the scope itself — callers (Phase 60+ upload handlers) thread the identity quadruple plus the import-task ID through. Documented in D-053.

## Glossary additions

- **Skills.md** — the open Markdown + YAML-frontmatter file format Harbor's importer (Phase 40) consumes natively. Frontmatter carries `name / description / trigger / task_type / tags / required_*`; body carries `## Steps`, `## Preconditions`, `## Failure modes` sections. Byte-stable round-trip is a tested invariant.
- **Importer** — Phase 40 `internal/skills/importer` package. Two operations (`Import` / `Export`) round-trip Skills.md bytes through `skills.Skill`. Identity-mandatory at the persistence boundary; attachments resolve to `ArtifactRef` per RFC §6.7 option (b).
- **ArtifactRef (skills context)** — `artifacts.ArtifactRef` returned by `Importer.Import` for each inline attachment. Body-side references replace the relative path with `ArtifactRef.ID`; `Export` materializes back to the original path via `ImportArtifacts.PathToRef`.
- **Round-trip invariant** — `Export(Import(b)) == b` for any spec-compliant `b`. Asserted via `bytes.Equal` on the golden corpus. The load-bearing test that distinguishes a working importer from a working-by-coincidence importer.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §6.7`, `brief 04`) resolve
- [ ] Coverage on `internal/skills/importer` >= 90%
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A (the importer is stateless; identity flows only at the persistence boundary which Phase 37 already covers).
- [ ] **Concurrent-reuse test passes — N=128 concurrent invocations against a single shared `*Importer` under `-race`**: `internal/skills/importer/concurrent_test.go`.
- [ ] **Integration test exists** — `attachments_test.go` wires a real `inmem` ArtifactStore through the seam, asserts attachment round-trip, covers >= 1 failure mode (attachment-outside-root), runs under `-race`.
- [ ] Glossary updated (`Skills.md`, `Importer`, `ArtifactRef (skills)`, round-trip invariant)
- [ ] D-053 filed in `docs/decisions.md`
- [ ] README.md Phase 40 row flipped to Shipped
- [ ] docs/plans/README.md Phase 40 row flipped to Shipped
