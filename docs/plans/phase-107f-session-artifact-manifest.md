# Phase 107f — session-artifact-manifest

## Summary

Keep the planner aware of a session's artifacts across turns. Today the model
sees an artifact only on the turn it is created (a user upload, or a tool result
materialised above the heavy-output threshold); on the next turn it has no idea
the artifact exists, so it cannot `artifact_fetch` it to iterate. This phase has
the run loop list the session's artifacts each turn and inject a read-only
`<session_artifacts>` manifest (ref id · filename · mime · size · provenance) into
the planner prompt, so the model can fetch any of them on demand. It also
canonicalises artifact provenance so tool- and flow-generated artifacts carry the
`source` discriminator (fixing the Console Artifacts page showing blank source).

## RFC anchor

- RFC §6.2
- RFC §6.4
- RFC §6.5

## Briefs informing this phase

- brief 13
- brief 07

## Brief findings incorporated

- brief 13 §"prompt sections / read-only injected context": the ReAct prompt is
  composed of explicit XML-tagged sections, with read-only injected blocks
  (`<read_only_*_memory>`, `<skills_context>`) spliced as separate system messages
  carrying an anti-injection framing. The session-artifact manifest is another such
  read-only block — UNTRUSTED metadata for awareness only, never an instruction.
- brief 07 §"the artifact-fetch escape hatch": the planner reaches heavy / out-of-
  context content through the `artifact_fetch` meta-tool by ref. The tool already
  resolves session-scoped, so an artifact from an earlier turn is fetchable now —
  the only missing piece is re-surfacing the ref, which this manifest supplies.

## Findings I'm departing from (if any)

None. The reserved `memory.ConversationTurn.ArtifactsShown` / `ArtifactsHiddenRefs`
fields (research brief 04) are a memory-strategy pruning concern, dead in V1; this
phase does NOT wire them — the manifest is built fresh from the ArtifactStore each
turn (run-loop pre-resolved, like `MemoryBlocks` / `SkillsContext`). A future "what's
new since last turn" optimisation can adopt those fields then.

## Goals

- The run loop lists the session's artifacts each turn
  (`ArtifactStore.List(ScopeOf(tenant,user,session))` — already in scope) and builds
  a metadata-only manifest: `{ref, filename, mime, size_bytes, provenance}`.
- A new pre-resolved `RunContext.SessionArtifacts []ArtifactManifestEntry` carries it
  to the planner — no I/O inside the planner (the D-166 pattern; the planner reads
  only from `rc`).
- The ReAct planner renders a read-only `<session_artifacts>` system block listing
  the entries and instructing the model it may `artifact_fetch` any ref to read /
  iterate. Empty session → no block (no fabricated rows).
- **Provenance canonicalisation:** the dev tool-executor and the flow catalog stamp
  the canonical `Source["source"]` key (`"tool"` / `"flow"`) in addition to the tool
  name, so `artifacts.list` AND the manifest show real provenance instead of blank.
  The manifest provenance string resolves `Source["source"]` else the tool name
  (`Source["tool"]` / `Source["producer"]`) else "unknown".
- Bounded: metadata-only; cap at N entries (newest-first) with an explicit
  "+K more" line — never a silent truncation (§17.6).
- `harbortest/devstack` mirrors the run-loop manifest build (§17.6 parity).

## Non-goals

- Wiring the dead `ArtifactsShown` / `ArtifactsHiddenRefs` memory fields (future
  "new-since-last-turn" delta optimisation).
- Fetching artifact CONTENT into the manifest — refs + metadata only (the model
  fetches on demand via `artifact_fetch`, D-026 heavy-content discipline).
- A new Protocol method — `ArtifactStore.List` + `artifacts.list` already exist.
- Changing `artifact_fetch` — it already resolves session-scoped.

## Acceptance criteria

- [ ] **AC-1** Each planner turn, the run loop builds a `SessionArtifacts` manifest
  from `ArtifactStore.List` scoped to `(tenant, user, session)`; it is set on the
  `RunContext` the planner receives (planner does no I/O — reads `rc` only).
- [ ] **AC-2** A turn that follows the creation of an artifact (user upload OR a
  tool-materialised heavy result on a PRIOR turn) sees that artifact in the
  `<session_artifacts>` block, with its ref, filename, mime, size, provenance.
- [ ] **AC-3** The `<session_artifacts>` block is read-only framed (UNTRUSTED
  metadata, not an instruction) and tells the model it may `artifact_fetch <ref>` to
  read any listed artifact.
- [ ] **AC-4** An empty session injects NO `<session_artifacts>` block (no fabricated
  rows, no empty-list noise).
- [ ] **AC-5** Provenance: a tool-generated artifact projects a non-blank `source`
  on `artifacts.list` (canonical `"tool"`) — was blank — and the manifest shows the
  originating tool. A user upload shows `user_upload`. (Regression: the Console
  Artifacts page no longer shows blank source for tool artifacts.)
- [ ] **AC-6** The manifest caps at `N` (newest-first); overflow appends an explicit
  "+K more (use artifact_fetch by ref)" line — no silent drop.
- [ ] **AC-7** Identity isolation: the List is scoped to the run's triple; a
  cross-session test asserts session A's artifacts never appear in session B's
  manifest.
- [ ] **AC-8** `harbortest/devstack` builds the same manifest (parity test); the dev
  run loop and the harness do not diverge (§17.6).
- [ ] **AC-9** Concurrent-reuse: the planner renders the manifest from `rc` only (no
  planner-struct state); the existing planner concurrent-reuse test covers it.
- [ ] **AC-10** `scripts/smoke/phase-107f.sh` asserts the manifest render + the
  provenance canonicalisation via `go test`.

## Files added or changed

- `internal/planner/planner.go` — `RunContext.SessionArtifacts []ArtifactManifestEntry`
  field + the `ArtifactManifestEntry` type (ref, filename, mime, sizeBytes, provenance).
- `internal/planner/react/memory_wrappers.go` — render the `<session_artifacts>` block
  (read-only framing) alongside the existing wrappers.
- `internal/planner/react/prompt.go` — splice it in `buildRequest`.
- `cmd/harbor/cmd_dev_runloop.go` — list artifacts + build the manifest + set
  `RunContext.SessionArtifacts` where `memBlocks` is built.
- `harbortest/devstack/devstack.go` — mirror the manifest build.
- `cmd/harbor/cmd_dev_executor.go` + `internal/runtime/flow/protocol/catalog.go` —
  stamp the canonical `Source["source"]` key.
- `internal/protocol/artifacts.go` — `projectRow` resolves the source discriminator
  from `"source"` else `"tool"`/`"producer"` so existing artifacts project correctly.
- `scripts/smoke/phase-107f.sh` (**NEW**).
- `docs/decisions.md` — D-176. `docs/plans/README.md` — row + status.

## Public API surface

- `planner.RunContext.SessionArtifacts []planner.ArtifactManifestEntry` (additive;
  nil = no manifest, existing callers compile unchanged).
- `planner.ArtifactManifestEntry{Ref, Filename, MIME string; SizeBytes int64; Provenance string}`.
- No new Protocol method; no ArtifactStore interface change.

## Test plan

- **Unit:** the `<session_artifacts>` render (entries → block text; read-only framing;
  empty → no block; cap + "+K more"); the provenance resolver (`source` else tool name
  else unknown).
- **Integration:** run-loop build over a real ArtifactStore + a session with a
  user-upload artifact and a tool-materialised artifact from a prior turn — assert
  both appear in the manifest with correct provenance; ≥1 failure mode (List error →
  the turn still proceeds with no manifest, logged, never a hard fail — but never a
  fabricated manifest either). Under `-race`. Identity propagation asserted (AC-7).
- **Conformance:** N/A — no new driver surface (`List` is already conformance-tested).
- **Concurrency / leak:** the planner is a reusable artifact; the manifest is `rc`-
  scoped — the existing concurrent-reuse test covers no-bleed across runs (AC-9).

## Smoke script additions

- `scripts/smoke/phase-107f.sh` (classification `unit-tests`): `go test` the manifest
  render + provenance resolver in `internal/planner/react` and the provenance
  projection in `internal/protocol`.

## Coverage target

- `internal/planner/react`: 85%.
- `cmd/harbor` (run-loop manifest build): exercised by the integration test.
- `internal/protocol` (provenance projection): maintain ≥ current.

## Dependencies

- 107c (the `artifact_fetch` meta-tool + the heavy-result ArtifactStub path).
- 17–19 (Artifacts + `ArtifactStore.List`). 33 (multimodal upload). D-166 (run-loop
  pre-resolution of `RunContext` inputs — the pattern the manifest follows).

## Risks / open questions

- **Token budget.** The manifest is metadata-only and capped (AC-6), so its prompt
  cost is bounded and small; the cap is operator-tunable later if needed. Flag the
  default N.
- **Provenance back-fill.** Artifacts created BEFORE this phase (already in a store)
  have no canonical `"source"`; `projectRow`'s else-chain (read `"tool"`/`"producer"`)
  covers them, so no migration is needed — call this out.
- **§17.6 parity.** The dev run loop and `devstack` both build the manifest; the
  parity test guards against the F1 divergence pattern.
- **No fabrication.** A `List` error yields NO manifest (logged), never a guessed or
  partial one — the model simply isn't told about artifacts that turn (CLAUDE.md §5).

## Glossary additions

- **Session artifact manifest** — the read-only `<session_artifacts>` planner-prompt
  block listing a session's artifacts (ref · filename · mime · size · provenance) so
  the model stays aware of uploads + tool-generated artifacts across turns and can
  `artifact_fetch` them. Run-loop pre-resolved; metadata-only; capped. D-176.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] **If multi-isolation paths changed: cross-session isolation test passes** — the
  manifest List is identity-scoped; AC-7 asserts no cross-session bleed.
- [ ] **Concurrent-reuse test passes** — the planner renders the manifest from `rc`
  only; the existing N≥100 concurrent-reuse test covers no cross-run bleed.
- [ ] **Integration test exists** — run-loop manifest over a real ArtifactStore with a
  prior-turn artifact + a user upload, ≥1 failure mode, under `-race`.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
