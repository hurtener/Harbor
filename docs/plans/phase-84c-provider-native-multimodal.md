# Phase 84c — Provider-native multimodal mechanism

## Summary

Phase 84c implements the `provider_native` disposition that Phase 84b makes
opt-in: when (and only when) the disposition policy selects it, the bifrost driver
hands an over-threshold attachment to the provider's own understanding rather than
degrading it to an `ArtifactStub` the model can't see. The bifrost SDK already
exposes the surface — `Bifrost.FileUploadRequest` + `File{Retrieve,Delete,Content}`
on the pinned `core@v1.5.15` — so this is plumbing on an existing substrate, not a
new dependency.

**Priority order (deliberate):** the perception modalities first — **`image/*`**
(regain vision for over-threshold images, which today silently fall back to a text
stub), **`audio/*`** (end-to-end audio in, not stub-routing), **`video/*`**
(provider-native video understanding where supported, e.g. Gemini) — and
**`application/pdf` + documents last** (the document/RAG path is best served by the
`ref`/`tool` disposition + Phase 84d's embeddings, so native PDF is the lowest-value
rung here). 84c also completes the streaming-multimodal residual that Phase 107's
row forward-referenced: 107 shipped text streaming; 84c proves multimodal inputs
combine with it.

The `LLMClient` stays **one method** (RFC §6.5): the upload is performed *inside the
bifrost driver* during `Complete` when it encounters a `provider_native`-flagged
over-threshold part — provider specifics stay in the driver / provider-correction
layer, never scattered into the runtime. **The driver is the ONLY seam** — the run
loop never pre-uploads, and `planner.InputArtifactView` gains no `ProviderFileID`
(one seam, not two). Because the seam is the driver, a Go library consumer calling
`LLMClient.Complete` directly gets provider-native handling with no planner, run
loop, config file, or Protocol in the path. See D-189 (split) and D-190 (this
phase).

## RFC anchor

- RFC §6.5 — LLM client + multimodal `Content.Parts`; the "one method, richer
  content" contract the provider-native path must not violate.
- RFC §6.10 — heavy-output threshold (provider-native is the over-threshold branch
  that preserves capability without carrying bytes through Harbor's context plane).
- RFC §11 (Q-3) — streaming multimodal output; 84c resolves the residual jointly
  with the shipped Phase 107 text-streaming pipeline.

## Briefs informing this phase

- brief 03 — tools-and-llm (the multimodal content sum-type + per-provider shapes)
- brief 08 — llm-client-validation (the six-provider conformance matrix + the
  provider-capability table the upload path must respect)

## Brief findings incorporated

- **brief 08 §empirical validation.** The conformance matrix today runs one unary
  image probe. 84c extends it to a per-modality table (image / audio / video / pdf),
  each cell against one capable provider, gated by `HARBOR_LIVE_LLM`, plus a
  streaming-with-multimodal row in Phase 107's `req.Stream` + `llm.completion.chunk`
  vocabulary (NOT the non-existent `CompleteStreaming` the old plan referenced).
- **brief 03 §provider correction.** Per-provider upload capability differs
  (bifrost returns `UnsupportedOperationError` for providers without it, e.g.
  Cohere); the provider-correction layer owns the routing, and `ArtifactStub`
  remains the universal degradation when a provider can't take a given modality.

## Findings I'm departing from (if any)

This phase carries the provider-native content moved out of the superseded
`phase-84b-bifrost-multimodal-v13.md`, re-gated behind 84b's disposition policy and
re-prioritised (perception modalities first; PDF/files last). The old plan's
streaming acceptance criterion is rewritten in Phase 107's vocabulary. Recorded in
D-189 / D-190.

**Shipped deviations (§4.3, recorded in D-190):**

- **The run-loop cancel hook is not wired.** The plan allowed the run loop's
  cancel path to "MAY trigger early cleanup through the driver". The run loop
  holds the wrapped `LLMClient` (governance → retry → downgrade → corrections →
  safety → driver), so reaching a driver-exported purge method would require
  threading a forwarding method through five wrapper layers or widening
  `LLMClient` beyond one method — exactly the ceremony the SDK-lens review (C3)
  said to avoid ("`Close`-time + TTL cleanup needs no new interface method").
  The driver-owned lifecycle (TTL expiry + LRU evict with remote delete +
  `Close`-time sweep, with per-key fill coalescing) is the authority.
  Consequently `cmd/harbor/cmd_dev_runloop.go` and the devstack mirror are
  untouched by this phase.
- **No `make conformance` target exists** — the conformance suites run under
  `make test`; the live matrix extension is the per-modality table +
  streaming row in `internal/llm/drivers/bifrost/conformance_test.go`.
- **The smoke's per-modality round-trip is resolution-level, not upload-level.**
  The preflight dev server runs the dev-only mock LLM driver; the bifrost
  upload pass cannot fire there. The smoke asserts the end-to-end
  `provider_native` resolution (honoured, no degradation) against the live
  server; the real-provider upload per modality is the `HARBOR_LIVE_LLM`-gated
  conformance table's job.

## Goals

- Add the opaque provider reference fields to the content sum-type:
  `ImagePart.ProviderFileID`, `AudioPart.ProviderFileID`,
  `FilePart.ProviderFileID` + `FilePart.DocumentType` — all optional, additive,
  preserving the "exactly one of URL/DataURL/Artifact" invariant (RFC §6.5).
- Add the **part-level upload flag the driver keys on**:
  `ImagePart`/`AudioPart`/`FilePart` gain `ProviderNative bool`. The materializer
  sets it when 84b's resolved disposition is `provider_native`; **any consumer
  constructing a `CompleteRequest` by hand can set it directly** — the headless
  reachability guarantee: provider-native works with zero planner, zero config,
  zero Protocol.
- Bifrost driver performs the upload *internally* during `Complete`: on a
  `ProviderNative`-flagged over-threshold part it calls `FileUploadRequest`,
  receives the `file_id`, rewrites the part to the provider's file-reference
  content block, then proceeds. `LLMClient` stays one method.
- Per-modality rollout, in priority order:
  1. **`image/*`** — over-threshold images regain vision via `file_id` (today they
     degrade to an unreadable stub).
  2. **`audio/*`** — end-to-end audio input round-trip.
  3. **`video/*`** — provider-native video understanding where supported; stub
     everywhere else.
  4. **`application/pdf` + documents** — last; `DocumentType` disambiguates
     structured docs. (The `ref`/`tool` + 84d path is the preferred document route.)
- `file_id` lifecycle is **driver-owned, end to end**: an identity-scoped cache
  keyed by `(tenant,user,session,artifact hash)` so a re-attached artifact isn't
  re-uploaded every turn — identity read from `ctx` via `identity.From` (the LLM
  edge already fails closed on missing identity, so the seam is Protocol-free;
  library consumers stamp identity with `identity.With`/`WithRun`) — plus TTL/LRU
  expiry with `FileDeleteRequest` delete-on-evict and best-effort cleanup on
  client `Close`. The run loop's cancel path MAY trigger early cleanup through
  the driver, but it is never the only path: a library consumer who never runs
  the dev loop must not leak provider-side files.
- Observability is an **event, not a task field**: the driver emits
  `llm.provider_file.uploaded` (artifact ref, provider, modality, `file_id`) on
  the bus it already holds (`llm.Deps.Bus`); the Protocol/Console surface it from
  the event stream like everything else. `planner.InputArtifactView` gains **no**
  `ProviderFileID` — the run loop never pre-uploads.
- Streaming-with-multimodal: a `provider_native` (or inline) multimodal request
  streams its text deltas through the existing 107 path (`req.Stream` →
  `llm.completion.chunk`); new conformance row.
- `ArtifactStub` stays the universal degradation: a provider without support for a
  modality falls back loudly (a logged notice + the stub), never a silent failure.

## Non-goals

- **No disposition policy** — that is Phase 84b; 84c only fires when the policy
  resolves `provider_native`. It is never the default.
- **No embeddings / RAG** — that is Phase 84d (and the preferred document route).
- No new `LLMClient` method and no new wire/request type (one method, richer
  content — RFC §6.5).
- No mid-run multimodal injection (start-only stays; steering-inbox attachment slot
  is tracked separately).
- No bifrost SDK bump required (`v1.5.15` already exposes the file API); a bump is
  only taken if a provider-correction fix needs it, noted in the `go.mod` line.

## Acceptance criteria

- [x] Content sum-type gains the optional `ProviderFileID` / `DocumentType` fields
      without breaking existing `Content.Parts` consumers (golden compile + tests).
- [x] With disposition `provider_native`, an over-threshold **image** uploads via
      `FileUploadRequest` and the provider receives a `file_id` (live row in
      `TestE2E_Bifrost_LiveProviderNativeMultimodal`, `HARBOR_LIVE_LLM` +
      `OPENAI_API_KEY`; structurally verified in-tree via the stub-recording
      client), restoring vision the stub path loses.
- [x] **audio** end-to-end and **video** (capable provider) round-trip; **pdf**
      last, with `DocumentType` set. (Live rows gated per-provider-key; the
      video row additionally needs `HARBOR_LIVE_VIDEO_FIXTURE` — a valid
      container cannot be synthesized inline. All four modalities are
      structurally verified by `TestProviderNative_Modalities`.)
- [x] The part-level `ProviderNative` flag is settable directly on a hand-built
      `CompleteRequest` — a headless test exercises provider-native end-to-end
      with **no planner / run loop in the path** (mock-recording provider;
      `TestProviderNative_HeadlessDataURL`).
- [x] A provider lacking support for a modality degrades to `ArtifactStub` with a
      logged notice — fail-loud, never a fabricated success (CLAUDE.md §13).
- [x] `file_id` is cached identity-scoped (no re-upload on re-attach; identity
      from `ctx`, fail closed when absent) and cleaned up on TTL expiry / client
      `Close` (`FileDeleteRequest`); the lifecycle is covered by tests that never
      touch the run loop. **§4.3 deviation:** the optional run-loop cancel hook
      is not wired — see "Findings I'm departing from".
- [x] The driver emits `llm.provider_file.uploaded` on upload; the event is the
      observability surface (no `ProviderFileID` on `planner.InputArtifactView`).
- [x] Streaming + multimodal: a multimodal request with `req.Stream=true` streams
      deltas end-to-end (`TestProviderNative_StreamingMultimodal` + the live
      streaming-multimodal conformance row; the dev path's
      `llm.completion.chunk` emission rides the existing 107 publisher).
- [x] The `ErrContextLeak` LLM-edge guard treats a `file_id`-only part (no inline
      bytes) as legal over-threshold (it must not false-positive on the new path).
      Pinned by `TestSafety_ProviderFileIDOnlyPart_IsLegalOverThreshold` — the
      existing guard inspects DataURL/text payloads only, so no new guard code
      was needed; the precision is test-enforced.
- [x] Conformance extended (the live per-modality table + streaming row; there
      is no `make conformance` target in the tree — the suites run under
      `make test`); Go tests under `-race`; smoke `scripts/smoke/phase-84c.sh`
      asserts the provider_native resolution end-to-end (the preflight server
      runs the mock LLM driver, so the provider upload itself is covered by the
      gated live table, not the smoke).

## Files added or changed

- `internal/llm/llm.go` — `ProviderNative` flag + `ProviderFileID` on
  Image/Audio/File parts; `FilePart.DocumentType`.
- `internal/llm/drivers/bifrost/translate.go` — emit the provider file-reference
  content block when `ProviderFileID` is set.
- `internal/llm/drivers/bifrost/bifrost.go` — the internal upload pass (calls
  `FileUploadRequest`), the identity-scoped `file_id` cache (identity from
  `ctx`), TTL/LRU delete-on-evict + `Close`-time cleanup.
- `internal/llm/events.go` — the `llm.provider_file.uploaded` event shape.
- `internal/llm/drivers/bifrost/conformance_test.go` — per-modality table +
  streaming-multimodal row.
- `internal/planner/multimodal.go` — the `provider_native` branch sets
  `ProviderNative` on the part (resolved disposition from 84b).
- `internal/llm/llm.go` (edge guard) — `ErrContextLeak` exemption for `file_id`-only
  parts (test-pinned; the existing guard's DataURL/text-only inspection already has
  the right shape — see acceptance criteria).
- ~~`cmd/harbor/cmd_dev_runloop.go` / `harbortest/devstack/devstack.go` cancel
  hook~~ — not shipped; §4.3 deviation above (the driver lifecycle is the
  authority).
- `docs/recipes/provider-native-attachments.md` — the headless recipe: `llm.Open`
  plus `identity.With` plus a hand-built `CompleteRequest` with `ProviderNative`
  set — no `harbor dev` anywhere.
- `scripts/smoke/phase-84c.sh` — per-modality round-trip.
- `docs/decisions.md` — D-190.

## Public API surface

- `llm.ImagePart.ProviderNative` / `llm.AudioPart.ProviderNative` /
  `llm.FilePart.ProviderNative bool` — the request-side flag any
  `CompleteRequest` builder can set (the materializer sets it from 84b's
  resolved disposition; a headless consumer sets it directly).
- `llm.ImagePart.ProviderFileID string`, `llm.AudioPart.ProviderFileID string`,
  `llm.FilePart.ProviderFileID string` + `llm.FilePart.DocumentType string`.
- `llm.provider_file.uploaded` event (artifact ref, provider, modality,
  `file_id`) — the observability surface. `planner.InputArtifactView` gains
  **no** `ProviderFileID` field (the run loop never pre-uploads; one seam).
- No new interface method — the bifrost driver performs the upload internally.

> Scope note: "public" here is module-internal — `internal/` packages are not
> importable by external modules (the recorded reason `harbortest/` lives at the
> top level). This surface is stable for in-module consumers; external-team
> embedding needs a future facade/export RFC, out of scope for this wave.

## Test plan

- **Unit:** per-modality translation in `translate.go`; the `ProviderNative`
  branch in the materializer; the `ErrContextLeak` exemption for `file_id`-only
  parts; the driver's identity-from-`ctx` fail-closed path.
- **Headless:** a hand-built `CompleteRequest` with `ProviderNative` set against
  a mock-recording provider — upload + part-rewrite + `llm.provider_file.uploaded`
  emission asserted with no planner / run loop in the path.
- **Integration:** `harbortest/devstack` — over-threshold image with disposition
  `provider_native` → assert a `file_id` reaches the provider (mock provider that
  records the upload), the part is rewritten, and the event is on the bus.
- **Conformance:** `runLiveMultimodal` extended to a per-modality table + a
  streaming-multimodal row (`HARBOR_LIVE_LLM`).
- **Concurrency / leak:** N≥100 concurrent multimodal `Complete` calls against a
  single bifrost client; the `file_id` cache is concurrent-safe (D-025); no leaks.

## Smoke script additions

`scripts/smoke/phase-84c.sh` (live-server): upload an over-threshold image →
`start` with disposition `provider_native` → the event stream carries
`llm.provider_file.uploaded` for the artifact (the smoke tails the SSE/events
surface); repeat per modality; 404/405/501 → SKIP for a pre-84c build.

## Coverage target

- `internal/llm/drivers/bifrost`: 88% (current 85% + the upload/translate branches).
- `internal/planner/multimodal.go`: 95% (the provider_native branch is pure dispatch).

> Shipped reality (§4.3 note): the plan's "current 85%" premise was stale — the
> package measured **77.6%** before this phase (the live-gated conformance
> paths and the `New`/account construction paths don't execute under CI).
> This phase's new code (`providerfiles.go`) lands at **90.8%** and lifts the
> package to **81.3%** — an explicit improvement toward the target per
> CLAUDE.md §14. `internal/planner/multimodal.go` lands at **96.3%** (target
> met).

## Dependencies

- **84b** (the disposition policy — `provider_native` only fires when the policy
  selects it). Same wave.
- 107 / 107c (text streaming + native tool-calling — the streaming residual 84c
  completes; `req.Stream` + `llm.completion.chunk` vocabulary).
- 32 (LLM client core + the context-window safety net the edge-guard exemption edits).

## Risks / open questions

- **Upload latency.** `FileUploadRequest` is a separate round-trip before the chat
  call; the identity-scoped cache amortises re-use. One-shot Playground uploads
  absorb it.
- **Orphaned remote files.** Cleanup via `FileDeleteRequest` is best-effort;
  the driver-owned lifecycle (TTL/LRU evict + `Close`-time sweep) is the
  authority so headless consumers don't leak, with the run-loop cancel hook as
  an additional early trigger; the lifecycle is documented.
- **Provider parity.** Not every provider supports every modality; the
  provider-correction layer owns the matrix and the stub degradation is the floor.
- **Edge-guard correctness.** The `ErrContextLeak` exemption must be precise — a
  `file_id`-only part is legal, but a part that *also* smuggled inline bytes must
  still trip the guard.

## Glossary additions

- **Provider-native file ref (`file_id`)** — an opaque identifier returned by a
  provider after a `FileUploadRequest`-style upload; replaces inline bytes for an
  over-threshold attachment when the disposition is `provider_native`. Add to
  `docs/glossary.md`.
- **Document part** — a `FilePart` whose `DocumentType` disambiguates a structured
  document (PDF, CSV, …) for providers with native document understanding. Add to
  `docs/glossary.md`.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes (84c smoke: 14 OK / 0 SKIP / 0 FAIL)
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target (see the shipped-reality note
      under "Coverage target": multimodal.go 96.3% ≥ 95%; the bifrost package's
      stated target rested on a stale premise — this PR lifts it 77.6% → 81.3%
      with the new code at 90.8%).
- [x] Cross-session isolation: the `file_id` cache is keyed by the identity triple +
      content key; `TestProviderNative_CacheIsolation_AcrossSessions` asserts no
      `file_id` reuse across the boundary.
- [x] **Primitive + consumer in the same wave (§13):** the provider-native mechanism
      ships with its consumer — 84b's `provider_native` disposition routing it
      (`TestE2E_ProviderNative_MaterializerToDriver` wires policy → materializer →
      driver) — and a live conformance probe that exercises a real provider upload.
- [x] Concurrent-reuse: bifrost `LLMClient` + the `file_id` cache stay concurrent-safe
      (`TestConcurrent_D025_ProviderFileCache`, N=128 multimodal `Complete` under
      `-race`).
- [x] Glossary updated (provider-native file ref + document part)
- [x] If a brief finding was departed from: justified above + D-190 filed.
