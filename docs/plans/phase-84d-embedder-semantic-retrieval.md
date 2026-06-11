# Phase 84d — Embedding client + semantic retrieval (memory & skills)

## Summary

Harbor has **no embeddings capability today** — `LLMClient` is chat-only and there
is no `Embedder`, no vector surface anywhere in the runtime. That is the missing
primitive behind the "process it myself" path Phase 84b's `ref`/`tool` disposition
points at: a developer who keeps a document as a ref (rather than 84c's
provider-native upload) and wants to *retrieve over it* needs embeddings.

Phase 84d adds an `Embedder` interface as a §4.4 seam, wired to bifrost's existing
`EmbeddingRequest` (`core@v1.5.15`, `bifrost.go`), with the driver/factory/registry
pattern. Per the §13 primitive-with-consumer rule, it ships with its first real
consumers — **semantic retrieval in the memory subsystem and in the skill
catalog** (the direction the project owner set: embeddings go toward semantic
memory / skill retrieval, *not* a one-off RAG tool). Both consumers are **opt-in**
modes — they do not replace `rolling_summary` memory or the token-savvy skill
retrieval; they are additional, configured-on retrieval strategies.

The `Embedder` is a **standalone, factory-constructible primitive**: memory and
skills are its first consumers (§13), not its gatekeepers. A Go library consumer
embedding the runtime headless constructs it via the factory (`embeddings.Open`
with a `ConfigSnapshot` + `Deps`, mirroring `llm.Open`) and calls `Embed` directly
for their own retrieval — no memory subsystem, no config file, no Protocol.

See D-189 (the 84b/c/d split) and D-191 (this phase). Requires the RFC §6.5
addendum (the `Embedder` seam) introduced in the same PR.

## RFC anchor

- RFC §6.5 — LLM client layer; the `Embedder` is a sibling seam to `LLMClient`
  (a separate capability, NOT a new method on the one-method chat client). The §6.5
  addendum in this PR sanctions it.
- RFC §6.6 — Memory subsystem (semantic-retrieval consumer).
- RFC §6.7 — Skills subsystem (semantic skill-retrieval consumer).

## Briefs informing this phase

- brief 04 — memory-and-skills (the memory + skill retrieval surfaces the embedder
  feeds)
- brief 08 — llm-client-validation (the bifrost driver + conformance pattern the
  `Embedder` driver mirrors)

## Brief findings incorporated

- **brief 04 §retrieval.** Memory and skills both retrieve today by non-semantic
  means (rolling summary; token-savvy catalog). Semantic retrieval is the natural
  next rung, and it is the *consumer* that justifies the embedder primitive (§13) —
  an `Embedder` with no consumer would bit-rot.
- **brief 08 §driver seam.** The `Embedder` follows the same interface + driver +
  factory + registry shape as `LLMClient`, with a bifrost driver and a
  `HARBOR_LIVE_LLM`-gated conformance probe; no provider logic leaks outside the
  driver.

## Findings I'm departing from (if any)

The earlier discussion floated a standalone `document.search` RAG tool as the
embedder's consumer. The project owner redirected the embeddings path toward
**semantic memory / skill retrieval**; this plan adopts that as the in-wave
consumer. Recorded in D-189 / D-191.

### §4.3 deviations recorded at ship time (D-191)

- **Driver registration home.** The Goals' original "blank-import at
  `cmd/harbor`" wording predates Phase 110c (D-196): the bifrost embedder
  driver registers via the production driver aggregator
  `internal/drivers/prod`, which `cmd/harbor`, devstack, and embedders
  import. Library consumers blank-import the aggregator (or the driver) in
  their own main — documented in the recipe.
- **Interface carries `Close`.** `Embedder` is `Embed` + a lifecycle
  `Close`, mirroring `LLMClient`: the production driver owns gateway worker
  pools that must join on teardown (goroutine-baseline gate).
- **Skills injection seam.** "Injected at the skills directory /
  `skill_search` constructor" resolved to the STORE seam:
  `skills.Deps.Embedder` + the localdb driver's semantic `Search` path
  (result path `semantic`). The directory is a recency-ordered browse window
  where similarity ranking doesn't apply; ranking at the store keeps one
  implementation under `skill_search`, direct `Search`, and future callers.
- **RFC delta shape.** The §6.5 Embedder-seam addendum pre-landed with the
  D-189 plans PR; this PR's RFC edit is the D-191 contract sentence in §6.5
  plus the §6.6 / §6.7 consumer-side settled text.

## Goals

- `Embedder` interface (`Embed(ctx, []string) ([][]float32, error)`) in
  **`internal/embeddings`** — its own package, NOT under the chat client's
  `internal/llm`, so an embeddings-only consumer doesn't inherit the chat
  client's `Deps` surface (artifacts + bus). The §4.4 seam: driver in
  `internal/embeddings/drivers/bifrost`, factory + registry
  (`embeddings.Open(ctx, cfg ConfigSnapshot, deps Deps)` mirroring `llm.Open`),
  blank-import at `cmd/harbor` (a library consumer blank-imports the driver in
  their own main — the standard driver pattern; the `cmd/harbor` import only
  wires the binary).
- bifrost `Embedder` driver wired to `Bifrost.EmbeddingRequest`; embedding model +
  provider configured in `harbor.yaml` (separate from the chat model) — the
  config block decodes into the same `ConfigSnapshot` a programmatic consumer
  constructs directly; both paths are first-class.
- Identity at the `Embed` edge mirrors the LLM edge: identity is **mandatory in
  `ctx`** (fail closed on absence, as `llm.HasIdentity` enforces for chat) —
  embedding calls are billable provider traffic governance will meter, and the
  seam stays Protocol-free (`identity.With`/`WithRun` is the library consumer's
  path).
- **Consumer 1 — semantic memory retrieval (opt-in).** A memory retrieval mode
  that embeds turns (identity-scoped) and retrieves by cosine similarity, composing
  with — not replacing — `rolling_summary`. Vectors persist alongside memory
  records in the existing state/sqlite/postgres stores (brute-force similarity at
  V1 scale; an ANN index is a later phase). Injection is via **`memory.Deps.Embedder`**
  with the registry's fail-loud guard mirroring the existing `Deps.Summarizer`
  rule (`internal/memory/registry.go` — required when the mode is enabled, no
  stub fallback), so the mode is fully constructible in Go with no config file.
- **Consumer 2 — semantic skill retrieval (opt-in).** Embed skill descriptions;
  `skill_search` gains a semantic mode over the token-savvy catalog. The embedder
  is injected at the skills directory / `skill_search` constructor — same
  injection pattern, same fail-loud guard.
- **À la carte:** the `Embedder` is usable directly —
  `docs/recipes/embed-and-retrieve.md` ships the headless path
  (`embeddings.Open` + `Embed` + cosine ranking over a consumer's own corpus).
  A future `document.search`-style tool is a consumer of this same primitive,
  never a parallel implementation (§13).
- Identity scoping: all embedding-derived state is keyed by `(tenant,user,session)`;
  retrieval never crosses the boundary.

## Non-goals

- **Not forced.** Both consumers are opt-in modes; the V1 defaults (rolling_summary
  memory, token-savvy skill retrieval) are unchanged.
- **No vector-DB / ANN subsystem.** V1 stores vectors in the existing stores with
  brute-force similarity; a dedicated vector index is a later phase if scale demands.
- **No new method on `LLMClient`.** `Embedder` is a separate interface (§6.5
  addendum); the chat client stays one method.
- **No standalone RAG tool** as the primary consumer (redirected to memory/skills).
- Not coupled to 84c — embeddings serve the `ref`/`tool` document path, independent
  of provider-native upload.

## Acceptance criteria

- [x] `Embedder` interface + bifrost driver + factory + registry
      (`embeddings.Open(ctx, cfg, deps)` in `internal/embeddings`);
      misconfiguration error lists registered drivers (§4.4).
- [x] Embedding model/provider configured separately in `harbor.yaml`; missing
      config fails loudly at boot (names the key) when an embedding-consuming mode
      is enabled (CLAUDE.md §13 — no silent stub default).
- [x] `memory.Deps.Embedder` + the fail-loud registry guard (mirroring the
      `Deps.Summarizer` rule): semantic mode enabled without an embedder →
      construction error naming the dependency; the same guard on the skills
      constructor seam. Both consumers are constructible in Go with no config
      file.
- [x] `Embed` fails closed on missing identity in `ctx` (mirrors the LLM edge);
      covered by a unit test.
- [x] The memory conformance suite (`internal/memory/conformancetest`) gains the
      semantic-retrieval cases and **all three drivers** (in-mem / SQLite /
      Postgres) pass them — vector persistence is never a single-driver feature
      (§9).
- [x] `docs/recipes/embed-and-retrieve.md` ships the à-la-carte headless path
      (factory + `Embed` + ranking; no memory subsystem, no Protocol).
- [x] `HARBOR_LIVE_LLM` conformance probe: `Embed` returns non-empty vectors of the
      expected dimension against one capable provider.
- [x] **Semantic memory retrieval mode** consumes the embedder end-to-end: a turn
      is embedded + retrieved by similarity, identity-scoped, composing with
      rolling_summary — covered by a test.
- [x] **Semantic skill retrieval mode** consumes the embedder: `skill_search`
      returns semantically-ranked skills — covered by a test.
- [x] Cross-session isolation test: embeddings/vectors never retrieved across the
      identity boundary.
- [x] Concurrent-reuse: the `Embedder` is concurrent-safe (N≥100 concurrent `Embed`
      under `-race`, no per-call state on the driver).
- [x] Smoke `scripts/smoke/phase-84d.sh` exercises the embed surface + one retrieval
      round-trip.

## Files added or changed

- `RFC-001-Harbor.md` §6.5 — addendum sanctioning the `Embedder` seam (this PR).
- `internal/embeddings/` — `Embedder` interface, `ConfigSnapshot`, `Deps`,
  factory (`Open`), registry.
- `internal/embeddings/drivers/bifrost/` — bifrost `Embedder` driver +
  `embed_conformance_test.go`.
- `internal/memory/registry.go` + `internal/memory/...` — `Deps.Embedder` + the
  fail-loud guard; the semantic-retrieval mode + vector persistence +
  identity-scoped retrieval; `internal/memory/conformancetest` — the
  semantic-retrieval cases (all three drivers).
- `internal/skills/...` — the semantic skill-retrieval mode (embedder injected
  at the directory / `skill_search` constructor; same fail-loud guard).
- `internal/config/config.go` + `loader.go` — embedding model/provider config +
  the opt-in retrieval-mode flags + validation (decoding into
  `embeddings.ConfigSnapshot`).
- `internal/drivers/prod/prod.go` — blank-import the bifrost embedder driver
  (D-196 aggregator; library consumers blank-import the aggregator in their
  own main).
- `docs/recipes/embed-and-retrieve.md` — the à-la-carte headless recipe.
- `scripts/smoke/phase-84d.sh` — embed + retrieval round-trip.
- `docs/decisions.md` — D-191. `docs/glossary.md` — embedding client + semantic
  retrieval.

## Public API surface

- `embeddings.Embedder` interface (`Embed(ctx, texts []string) ([][]float32,
  error)`) + `embeddings.Open(ctx, cfg ConfigSnapshot, deps Deps)` — the
  standalone, à-la-carte-usable primitive.
- `memory.Deps.Embedder` + the skills constructor injection — the programmatic
  seams for the two in-wave consumers.
- Config: `embeddings.{driver,provider,model}` + the per-subsystem opt-in retrieval
  mode flags (`memory.retrieval=semantic`, `skills.retrieval=semantic`) — carriers
  over the same `ConfigSnapshot` / `Deps` a programmatic consumer builds directly.
- No change to `LLMClient`.

> Scope note: "public" here is module-internal — `internal/` packages are not
> importable by external modules (the recorded reason `harbortest/` lives at the
> top level). This surface is stable for in-module consumers; external-team
> embedding needs a future facade/export RFC, out of scope for this wave.

## Test plan

- **Unit:** the `Embedder` factory/registry dispatch; the identity-mandatory
  fail-closed path at the `Embed` edge; the `Deps.Embedder` fail-loud guards
  (memory + skills); cosine-similarity ranking; identity-scoped retrieval
  filters.
- **Integration:** `harbortest/devstack` — embed a memory turn + a skill, retrieve
  by similarity, assert identity scoping across two sessions.
- **Conformance:** `HARBOR_LIVE_LLM` embed probe (vector dimension + non-empty).
- **Concurrency / leak:** N≥100 concurrent `Embed` against a single driver under
  `-race`; baseline goroutine count restored.

## Smoke script additions

`scripts/smoke/phase-84d.sh` (unit-tests class; §4.3 deviation from the sketch
below): static seam/consumer/doc assertions + a built-binary `harbor validate`
fail-loud round-trip (semantic mode without an `embeddings` block exits 1
naming the key; with the block exits 0) + the embed→persist→retrieve
round-trip via the Phase 84d integration tests under `-race`. The original
sketch ("an embedding-enabled boot answers a semantic `skill_search`") is not
honest against the preflight dev boot, which deliberately carries NO embedding
provider (there is no stub embeddings driver — CLAUDE.md §13); the
live-provider leg is the `HARBOR_LIVE_LLM` conformance probe instead.

## Coverage target

- `internal/embeddings` + `internal/embeddings/drivers/bifrost` (embed
  driver): meets the package target.
- The memory/skills semantic-retrieval modes: ≥ the touched packages' targets.

## Dependencies

- 32 (LLM client core + the driver/factory/registry pattern the `Embedder` mirrors).
- 23–25 (MemoryStore) + the skills subsystem (the retrieval consumers).
- F11 / 84b (the `ref`/`tool` disposition path embeddings empower; non-blocking).

## Risks / open questions

- **Vector storage at scale.** Brute-force cosine over the existing stores is fine
  at V1 conversation/skill cardinality; a real ANN index is deferred to a later
  phase and flagged here, not silently assumed.
- **Embedding model config.** A second model/provider to configure; the boot-time
  fail-loud guard (when a semantic mode is enabled but no embedder is configured)
  prevents a silent degradation to non-semantic retrieval.
- **Dimension/model drift.** Re-embedding on a model change is required for
  consistency; document the migration (vectors are derived, not source-of-truth).

## Glossary additions

- **Embedding client (`Embedder`)** — Harbor's interface for turning text into
  vectors, a §4.4 seam wired to bifrost's embedding surface; a sibling to
  `LLMClient`, not a method on it. Add to `docs/glossary.md`.
- **Semantic retrieval** — an opt-in memory/skill retrieval mode that ranks by
  embedding similarity, composing with (not replacing) rolling_summary / token-savvy
  retrieval. Add to `docs/glossary.md`.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve — including the §6.5
      addendum this PR adds.
- [x] Coverage on touched packages ≥ stated target
- [x] Cross-session isolation: embeddings/vectors keyed + filtered by the identity
      triple; isolation test passes.
- [x] **Primitive + consumer in the same wave (§13):** the `Embedder` primitive
      ships with its consumers — semantic memory retrieval AND semantic skill
      retrieval — each exercised end-to-end with a test. No bare primitive.
- [x] **No test stub as a production default (§13):** the embedder fails loudly at
      boot when a semantic mode is enabled without a configured provider; no mock
      default.
- [x] Concurrent-reuse test (N≥100 `Embed` under `-race`)
- [x] Glossary updated (embedding client + semantic retrieval)
- [x] RFC §6.5 addendum landed + referenced
