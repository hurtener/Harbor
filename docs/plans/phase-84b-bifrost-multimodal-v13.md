# Phase 84b — Bifrost extended multimodal (V1.3)

## Summary

Round-7 F11 / D-166 closed the V1.1 multimodal "happy path": image bytes inline via `ImagePart.DataURL`, PDFs / audio routed as `ArtifactStub` refs, the catch-all stub-text path for everything else. Phase 84b extends Harbor's bifrost-driver coverage to the rest of the bifrost Go-SDK multimodal surface — provider-native file uploads, structured documents, full audio round-trip, **streaming-with-multimodal** — based on the bifrost docs the user referenced post-V1.1:

- [bifrost SDK — multimodal](https://docs.getbifrost.ai/quickstart/go-sdk/multimodal) (full multimodal surface)
- [bifrost SDK — streaming](https://docs.getbifrost.ai/quickstart/go-sdk/streaming) (multimodal streaming)

V1.3 cadence note: V1.0 = current shipped surface; V1.1 = round-6/7 fixes (Playground composer + multimodal start); V1.2 = MCP wave (phase 85a–85j); V1.3 = this phase + adjacent observability work.

## RFC anchor

- RFC §6.5 — LLM client + multimodal sum-type contract (`Content.Parts` with `ImagePart` / `AudioPart` / `FilePart`)
- RFC §6.5 — Context-window safety net (D-026: heavy outputs route through artifact refs; D-166 carve-out: operator-uploaded inputs inline below threshold)
- RFC §11 Q-3 — streaming multimodal output (token stream + image stream interleaving)

## Briefs informing this phase

- brief 08 — bifrost driver coverage + the six-provider conformance matrix
- brief 13 — Playground composer + multimodal operator UX (also referenced by round-6 F10 and round-7 F11)

## Brief findings incorporated

- **brief 08 §"Empirical validation".** The current six-provider matrix exercises text-chat / JSON / stream / cancel / cost-passthrough / one multimodal probe. The bifrost SDK multimodal surface is wider — phase 84b extends the matrix to cover the SDK's full input shape per the [bifrost multimodal docs](https://docs.getbifrost.ai/quickstart/go-sdk/multimodal).
- **brief 13 §"Playground operator UX".** The Playground composer's attach control is already mounted (uploads via `artifacts.put`, sends as `input_artifact_ids` per D-166). Phase 84b grows what the Playground can actually *route* to providers — currently `image/*` inlines, everything else stays as ref. Adding native file-id refs (provider-uploaded large attachments) and end-to-end audio round-trip is the next-rung surface.
- **D-166 carve-out edge.** The current image-inline path bypasses D-026 only below the heavy-output threshold (32KB). Larger images fall back to `ArtifactStub` text — which the LLM can't actually see. Phase 84b adds the provider-native upload path (e.g. OpenAI's `files.create` + a content `file_id` ref) so the operator can attach an arbitrarily-large image without losing vision capability.

## Findings I'm departing from (if any)

None.

## Goals

- Bifrost driver covers the SDK's full multimodal input surface (per the linked docs):
  - **`image/*`** — current `ImagePart.DataURL` (V1.1) PLUS provider-native upload path for over-threshold images.
  - **`application/pdf`** — provider-native PDF understanding (Anthropic Claude native PDF; OpenAI via assistants-style file refs).
  - **`audio/*`** — end-to-end audio input (whisper / native audio models), not just stub-routing.
  - **`video/*`** — provider-native video understanding where supported (Gemini today; route as stub everywhere else).
  - **Document parts** — structured-document attachments distinct from raw bytes.
- **Streaming-with-multimodal**: token-stream the response while a multimodal request is in flight; surface streaming via the existing `client.complete_streaming` accessor (per the [bifrost streaming docs](https://docs.getbifrost.ai/quickstart/go-sdk/streaming)).
- The six-provider conformance matrix (`internal/llm/drivers/bifrost/conformance_test.go`) gains per-MIME multimodal probes — each MIME exercised against one capable provider, gated by the same `HARBOR_LIVE_LLM` flag.
- Console Playground composer accepts the new MIMEs via the existing chat-attach control; the F11 per-MIME dispatcher (`internal/planner/multimodal.go::MaterializeInputContent`) grows the corresponding branches.

## Non-goals

- A new wire surface for streaming multimodal output (already covered by `events.subscribe` and the planner's per-step events).
- Replacing the V1.1 DataURL-inline path for sub-threshold images — that stays the fast path; provider-native file refs are added as the over-threshold branch.
- Mid-run `user_message` multimodal injection — V1.1 is start-only; mid-run remains text-only until the steering inbox grows an attachment slot. Tracked separately as V1.3+ "steering multimodal."

## Acceptance criteria

- [ ] `internal/llm/llm.go` content sum-type extends without breaking existing `Content.Parts` consumers. New `ImagePart` / `FilePart` / `AudioPart` fields for `ProviderFileID` (the SDK's `file_id` slot) where applicable.
- [ ] `internal/llm/drivers/bifrost/translate.go` translates each new field shape per the SDK's wire requirements; ArtifactStub fallback remains the universal degradation for providers without native support.
- [ ] `internal/planner/multimodal.go` per-MIME dispatcher learns to route to native file refs when the artifact size exceeds `heavy_output_threshold_bytes` (per-MIME branch decides DataURL inline vs provider-native upload).
- [ ] The Playground composer's `uploadArtifact` adapter handles the round-trip — if the artifact MIME maps to a provider-native upload, the runtime triggers that upload after `artifacts.put` and stores the resulting `file_id` on the artifact ref's `Source` map.
- [ ] `internal/llm/drivers/bifrost/conformance_test.go::runLiveMultimodal` extends to a per-MIME table; each cell runs against one vision-capable provider and asserts a non-empty response with `HARBOR_LIVE_LLM=1`.
- [ ] **Streaming multimodal** — `bifrost.LLMClient.CompleteStreaming` shipped (Phase 12 / 32 existing surface) handles multimodal inputs end-to-end: the request carries `Content.Parts`, the response stream interleaves text deltas as usual. New conformance row.
- [ ] Console Playground UX: an over-threshold image upload still produces a chat-attached chip; the operator sees a small "(uploaded to provider)" indicator when the provider-native path fires.
- [ ] Smoke script `scripts/smoke/phase-84b.sh` round-trips an artifact ID through `start` → `tasks.list` → response inspection per MIME family.

## Files added or changed

- `internal/llm/llm.go` — `ImagePart`/`FilePart`/`AudioPart` gain `ProviderFileID string` and (for FilePart) `DocumentType string`.
- `internal/llm/drivers/bifrost/translate.go` — per-MIME translation extensions; native file-id passthrough where the SDK supports it.
- `internal/llm/drivers/bifrost/conformance_test.go` — per-MIME multimodal matrix; streaming multimodal subtest.
- `internal/planner/multimodal.go` — over-threshold image branch (provider-native upload signal); video/document MIME branches.
- `internal/planner/multimodal_test.go` — new branch tests.
- `internal/runtime/steering/runloop.go` — over-threshold image: trigger the provider-upload side-effect before the planner first turn (synchronous; the run loop already owns the pre-fetch pass).
- `cmd/harbor/cmd_dev_runloop.go::resolveInputArtifacts` — produces the augmented `InputArtifactView` (now carries `ProviderFileID` when applicable).
- `harbortest/devstack/devstack.go` — D-094 mirror.
- `web/console/src/routes/(console)/playground/[session_id]/+page.svelte` — UX indicator when an attachment was provider-uploaded.
- `docs/research/14-bifrost-extended-multimodal.md` — brief consolidating findings from the linked bifrost docs (see "Research links" below).

## Public API surface

- `llm.ImagePart.ProviderFileID string` — opaque provider reference; bifrost driver passes verbatim.
- `llm.FilePart.ProviderFileID string` + `DocumentType string` — pdf/csv/etc. document classification beyond MIME.
- `llm.AudioPart.ProviderFileID string`.
- `planner.InputArtifactView.ProviderFileID string` — populated by the run loop when the provider-upload path fires.

## Test plan

- **Unit:** per-MIME branch coverage in `internal/planner/multimodal_test.go` (image over-threshold → native, image under-threshold → DataURL, audio with `ProviderFileID` → AudioPart with ref, video → AudioPart-shaped File, document → FilePart with DocumentType).
- **Integration:** `harbortest/devstack` boot with the artifact store + bifrost; spawn a task with a 100KB PNG (over-threshold); assert the planner first turn carries a `ProviderFileID`, not a DataURL.
- **Conformance:** extend `runLiveMultimodal` to a per-MIME table (image / pdf / audio / video); each cell against one capable provider.
- **Concurrency / leak:** N≥100 concurrent multimodal `Complete` calls against a single bifrost client; no goroutine leaks, no per-call state on the client struct.

## Smoke script additions

`scripts/smoke/phase-84b.sh`:

- Upload an image under heavy-output threshold → spawn `start` with input artifact → confirm task completes and the LLM response makes sense for the image.
- Upload an image over threshold → spawn `start` → confirm a provider-native file_id appears on the task's input-artifact-view (via `tasks.get`).
- Upload a PDF → spawn `start` → confirm `FilePart` translation reaches the provider.
- Upload an audio file → spawn `start` → confirm `AudioPart` translation.

## Coverage target

- `internal/llm/drivers/bifrost`: 88% (current 85% + new branches).
- `internal/planner/multimodal.go`: 95% (the new per-MIME branches are pure-function dispatch — easy to cover).

## Dependencies

- 84a (runtime-capability gate + session aggregates — round-8 closeout) — non-blocking, but the streaming-multimodal capability advertisement uses the same `runtime.info.capabilities` extensibility F1 closes.
- 28 (MCP wave — phase 85a–85j) — V1.2 ships before V1.3.
- Existing F11 / D-166 surface (already shipped in PR #230).

## Risks / open questions

- **Provider parity.** Not every provider supports every MIME. The bifrost SDK abstracts SOME of this; per the docs, OpenAI uses `files.create` + file_id, Anthropic uses native base64 inline for documents up to 32MB. Phase 84b documents which provider supports which MIME and routes via the bifrost driver's provider-correction layer.
- **File upload latency.** Provider-native upload (e.g. OpenAI `files.create`) is a separate HTTP round-trip before the chat call. Acceptable for one-shot uploads (Playground); for repeated reuse (the same image attached to multiple turns) the runtime should cache the `file_id` on the artifact ref. V1.3 caches; V1.4 evolves.
- **Streaming-with-multimodal cancellation.** When a streaming response is cancelled mid-flight, the per-MIME materializer's pre-uploaded `file_id` is already in the provider — it stays there until the operator deletes it. Acceptable; document the lifecycle.

## Research links (consolidated for the implementer)

The user surfaced these post-V1.1; phase 84b's implementor reads them as the first pass:

- **Full multimodal surface:** [bifrost multimodal docs](https://docs.getbifrost.ai/quickstart/go-sdk/multimodal) — image / pdf / audio / video input shapes; file_id refs; cross-provider compatibility table.
- **Streaming:** [bifrost streaming docs](https://docs.getbifrost.ai/quickstart/go-sdk/streaming) — `CompleteStreaming` accessor; chunk shapes; how multimodal inputs combine with streaming output.
- **Bifrost release notes** — capability shifts between SDK versions affect provider-correction; pin the SDK version in this phase's `go.mod` bump and note breaking changes.

The implementer drafts `docs/research/14-bifrost-extended-multimodal.md` consolidating the surface as their first commit on the phase 84b branch; the brief then anchors the design decisions for the rest of the phase.

## Glossary additions

- **Provider-native file ref** (`file_id`) — an opaque identifier the bifrost driver receives from the provider after a `files.create`-style upload. Replaces inline bytes for over-threshold attachments; round-trips on the next chat call as a content-block reference. Add to `docs/glossary.md`.
- **Document part** — a FilePart whose `DocumentType` field disambiguates structured documents (PDF, CSV, etc.) for providers with native document understanding distinct from raw file attachments. Add to `docs/glossary.md`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve — including the new brief 14 if added.
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-session isolation: N/A — multimodal inputs are session-scoped via the existing identity quadruple; the new MIME branches don't widen the boundary.
- [ ] Concurrent-reuse: bifrost LLMClient is already concurrent-safe; new tests assert that multimodal `Complete` calls stay so (N≥100 concurrent invocations).
- [ ] Integration test exists (`harbortest/devstack` over-threshold image; provider-native path verified end-to-end).
- [ ] Glossary updated (provider-native file ref + document part)
- [ ] If a brief finding was departed from: N/A (or filed)
