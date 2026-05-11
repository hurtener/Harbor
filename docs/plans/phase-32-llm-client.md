# Phase 32 — LLM client core + StreamSink contract

## Summary

Ship Harbor's `LLMClient` interface — **one method**, `Complete(ctx, req) (resp, error)` — alongside the multimodal sum-type message shape (D-021), the auto-materialize boundary that rewrites oversize inline `DataURL`s as `ArtifactRef`s (D-022), and the context-window safety net that fails loudly with `ErrContextLeak` / `ErrContextWindowExceeded` at the LLM-client edge (D-026). The package ships with a §4.4 driver registry (`mock` self-registers; bifrost lands at Phase 33), a typed-payload event taxonomy (`llm.image.materialized`, `llm.context_leak`, `llm.context_window_exceeded`, `llm.cost.recorded` placeholder for Phase 36a, `llm.mode_downgraded` placeholder for Phase 35), and the config plumbing (`LLMConfig.Driver`, `LLMConfig.ContextWindowReserve`, `LLMConfig.ModelProfiles`).

## RFC anchor

- RFC §6.5

## Briefs informing this phase

- brief 03
- brief 07
- brief 08

## Brief findings incorporated

- **brief 07 §6 + §7:** The `LLMClient` is the smallest possible surface — one method, JSON in / JSON out, runtime owns tool dispatch. Phase 32 ships exactly that shape; the static guard in `scripts/smoke/phase-32.sh` enforces "no provider-native tool-calling symbols (`ToolChoice` / `FunctionCall` / `ToolUse` / `ToolCallSpec`) in `internal/llm/`."
- **brief 03 §5:** "Two parallel LLM modes (the toggle smell)" — Harbor picks one architecture and bakes the correction layer in. Phase 32 ships the single-mode skeleton; Phase 34 will compile in `SchemaSanitizer` between the runtime and the client.
- **brief 08 §"How bifrost maps onto Harbor's LLMClient":** The driver translates `CompleteRequest` ↔ bifrost's `BifrostChatRequest` and ignores bifrost's `Tools` / `ToolChoice` parameters. Phase 32 designs the `Driver` interface so the bifrost adapter at Phase 33 is a thin shim — no surface tax on the bifrost driver.
- **brief 03 §6 (Cost reporting):** `Cost` + `Usage` types live on `CompleteResponse`. Phase 32 ships those shapes today even though Phase 36a is the first consumer (governance accumulators); the event type `llm.cost.recorded` registers in this PR so Phase 36a's subscription lands clean.
- **brief 08 §"Per-model seam":** Model-specific knobs (`context_window_tokens`, `reasoning_effort`, `json_schema_mode`, `cost_overrides`) live in `LLMConfig.ModelProfiles[modelName]`. Phase 32 ships the `ModelProfile` shape; Phase 33 reads it; Phase 35 reads `json_schema_mode`; Phase 36a/36b read `cost_overrides` + `default_max_tokens`.

## Findings I'm departing from (if any)

- **brief 03 §2 sketches a two-method `LLMClient` (`Complete` + `Stream`).** Harbor settles on the RFC §6.5 single-method shape: `Complete(ctx, req)` carries `Stream`/`OnContent`/`OnReasoning` on the request, NOT a separate `Stream` method. Justification: brief 07's "the smallest possible surface" wins; bifrost validation (brief 08) shows the streaming/non-streaming split fits cleanly inside one method via `req.Stream` + callbacks. Recording this departure here so future readers find the rationale in one place.
- **brief 03 §2 sketches `ChatMessage.Parts []ContentPart` with `ToolCallPart` / `ToolResultPart` parts.** Harbor's `ContentPart` carries only `text` / `image` / `audio` / `file` payloads. Tool-call rendering happens at the `ObservationRenderer` (runtime side, RFC §6.4 + brief 07 §5) as user-role text messages — the LLM client never sees a "tool-call part." Justification: RFC §6.4 boundary; brief 07 §6 explicit. Symbols ruled out by the smoke script's static guard.

## Goals

- Define the load-bearing `LLMClient` interface that five downstream phases (33 bifrost, 34 corrections, 35 downgrade chain, 36 retry, 36a/36b governance) hang off.
- Land the multimodal sum-type message shape (D-021) and the auto-materialize boundary (D-022) at the LLM-client edge so multimodal inputs work end-to-end on day one — no retrofit at Phase 33.
- Land the context-window safety net (D-026) as a runtime-wide invariant: every `Complete` call routes through one catch-all pass that fails loudly on raw heavy content or token-budget breach.
- Ship the §4.4 driver registry + factory + a `mock` driver, so downstream phases plug in via blank import.
- Ship the typed event taxonomy (`llm.image.materialized`, `llm.context_leak`, `llm.context_window_exceeded`, `llm.cost.recorded`, `llm.mode_downgraded`) so Phase 34/35/36a's emit-sites land clean.

## Non-goals

- Phase 33 wires bifrost (one-driver real implementation). This phase ships interface + mock only.
- Phase 34 ships the SchemaSanitizer / message-shape normalizer that sits between runtime and client. This phase does NOT ship any provider correction.
- Phase 35 ships `OutputMode = Native | Tools | Prompted` + downgrade chain. The `llm.mode_downgraded` event type registers here as a forward-compat seam, but no downgrade logic ships.
- Phase 36 ships retry-with-feedback. This phase ships error sentinels (`ErrContextLeak`, `ErrContextWindowExceeded`); no repair loop.
- Phase 36a/36b ship governance. The `llm.cost.recorded` event type registers here as a forward-compat seam, but no governance subscriber ships.
- Tool dispatch — runtime-side (RFC §6.4). No tool-calling type leaks into `internal/llm/...`. Smoke enforces.
- HTTP / Protocol surface — phase 60+.

## Acceptance criteria

- [ ] `internal/llm/llm.go` defines `LLMClient`, `CompleteRequest`, `CompleteResponse`, `ChatMessage`, `Role`, `Content`, `ContentPart`, `PartType`, `ImagePart`, `AudioPart`, `FilePart`, `ResponseFormat`, `ResponseFormatKind`, `Usage`, `Cost`, `ReasoningEffort`, `ArtifactStub`, `StubFetch`, `ModelProfile`. Per RFC §6.5 + D-021 + D-026 shapes.
- [ ] `internal/llm/errors.go` exports sentinels `ErrContextLeak`, `ErrContextWindowExceeded`, `ErrUnknownDriver`, `ErrClientClosed`, `ErrIdentityMissing`, `ErrInvalidContent`. Compare via `errors.Is`.
- [ ] `internal/llm/events.go` registers `llm.image.materialized`, `llm.context_leak`, `llm.context_window_exceeded`, `llm.cost.recorded`, `llm.mode_downgraded`. Typed `SafePayload` structs (no secret-shaped data).
- [ ] `internal/llm/registry.go` ships the §4.4 driver factory + registry, `Open(ctx, cfg, deps) (LLMClient, error)`, `Register(name, Factory)`, `RegisteredDrivers()`. `Deps` carries `Artifacts artifacts.ArtifactStore`, `Bus events.EventBus`, optional `Redactor audit.Redactor`. Factory error names registered drivers.
- [ ] `internal/llm/safety.go` ships the context-window safety net `enforceContextSafety(...)`: (a) auto-materializes inline `DataURL` content ≥ heavy-output threshold to `ArtifactRef` + emits `llm.image.materialized`; (b) asserts no raw heavy content survived (else `ErrContextLeak` + emit); (c) estimates total tokens against `ModelProfile.ContextWindowTokens` and fails with `ErrContextWindowExceeded` (with the corresponding emit) when the estimate is within `ContextWindowReserve` of the cap. V1 fails loudly only; no auto-truncate.
- [ ] The safety net is **mandatory by construction**: `Open(...)` returns a `*safetyClient` that wraps any `Driver` and runs the safety pass before delegating. Drivers cannot bypass it.
- [ ] `internal/llm/mock/` registers under `"mock"` from `init()`. Supports text-only AND multimodal (text + image part) round-trips, streaming callbacks (`OnContent`, `OnReasoning`), and ctx cancellation.
- [ ] **Identity-mandatory test**: `Complete` rejects requests with no identity in ctx → `ErrIdentityMissing`.
- [ ] **Cancellation test**: a streaming `Complete` cancels cleanly under ctx-cancel (no goroutine leaks; callbacks stop within the cancel window).
- [ ] **No Tool* symbol leak**: smoke script's static guard finds zero `ToolChoice` / `FunctionCall` / `ToolUse` / `ToolCallSpec` strings in `internal/llm/`.
- [ ] **Auto-materialize threshold test**: a `DataURL` exceeding the heavy-output threshold (32 KB default) is rewritten as `ImagePart{Artifact: …}` and emits `llm.image.materialized`.
- [ ] **Planted-leak test**: a deliberately-buggy producer that emits ≥-threshold raw bytes triggers `ErrContextLeak` + `llm.context_leak`.
- [ ] **Token-budget test**: a synthetic huge prompt assembled within `ContextWindowReserve` of a fake model's cap triggers `ErrContextWindowExceeded` cleanly.
- [ ] **ArtifactStub round-trip test**: an `ArtifactStub` renders to the model-agnostic JSON shape and parses back byte-stable.
- [ ] **Concurrent-reuse test (D-025)**: N≥100 concurrent `Complete` calls against ONE shared `LLMClient` instance with the mock driver under `-race`. No data races; per-call identity assertion (no context bleed); per-call ctx cancellation does not cross-cancel; goroutine baseline restored after teardown.
- [ ] `internal/config/config.go` extends `LLMConfig` with `Driver string`, `ContextWindowReserve float64`, `ModelProfiles map[string]ModelProfileConfig`. Validation in `validate.go` (allowlist for `Driver`; reserve in `[0, 1)`; per-profile `ContextWindowTokens > 0`).
- [ ] `examples/harbor.yaml` shows the `mock` driver + a commented `bifrost` stub + `context_window_reserve: 0.05` + two `model_profiles` entries (text + multimodal).
- [ ] `docs/glossary.md` adds `OnContent`, `OnReasoning`, `ContextWindowReserve`, `ModelProfile`, `ReasoningEffort`, `ResponseFormat`, `ErrContextLeak`, `ErrContextWindowExceeded`.
- [ ] `README.md` Status table flips Phase 32 row to Shipped.
- [ ] `docs/plans/README.md` master table flips Phase 32 row to Shipped.
- [ ] Coverage on `internal/llm`: ≥ 85%.
- [ ] `scripts/smoke/phase-32.sh` passes.

## Files added or changed

- `internal/llm/llm.go` (interface + sum-types + Driver) — NEW
- `internal/llm/registry.go` (factory + Open + Deps + ConfigSnapshot + safetyClient) — NEW
- `internal/llm/errors.go` (sentinels) — NEW
- `internal/llm/events.go` (event taxonomy + payloads) — NEW
- `internal/llm/safety.go` (context-window safety net pass) — NEW
- `internal/llm/materialize.go` (DataURL → ArtifactRef materialization) — NEW
- `internal/llm/tokens.go` (default chars/4 token estimator) — NEW
- `internal/llm/llm_test.go`, `safety_test.go`, `materialize_test.go`, `events_test.go`, `concurrent_test.go` — NEW
- `internal/llm/mock/mock.go` + `mock_test.go` — NEW
- `internal/config/config.go` (extend `LLMConfig`, add `ModelProfileConfig`) — MODIFIED
- `internal/config/validate.go` (`validateLLM` widens; allow-list driver; per-profile checks) — MODIFIED
- `internal/config/config_test.go` (parse new fields, validate new constraints) — MODIFIED
- `examples/harbor.yaml` (new `llm:` block shape with `model_profiles`) — MODIFIED
- `docs/plans/phase-32-llm-client.md` — NEW (this file)
- `docs/glossary.md` — MODIFIED (new entries)
- `docs/decisions.md` — MODIFIED (one D-NNN entry — see "Glossary additions" / "Risks / open questions")
- `README.md` — MODIFIED (Status row)
- `docs/plans/README.md` — MODIFIED (Status row)
- `scripts/smoke/phase-32.sh` — NEW

## Public API surface

```go
package llm

type LLMClient interface {
    Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error)
}

type Driver interface {
    Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error)
    Close(ctx context.Context) error
}

type Factory func(cfg ConfigSnapshot, deps Deps) (Driver, error)

type Deps struct {
    Artifacts artifacts.ArtifactStore // mandatory; auto-materialize target
    Bus       events.EventBus         // mandatory; safety-net + cost emit
}

type ConfigSnapshot struct {
    Driver                string
    ContextWindowReserve  float64                  // default 0.05
    HeavyOutputThreshold  int                      // default 32 KiB
    ModelProfiles         map[string]ModelProfile  // keyed by canonical model
    Provider, Model, APIKey, BaseURL string        // bifrost knobs (Phase 33)
    Timeout              time.Duration
}

type ModelProfile struct {
    ContextWindowTokens int
    TokenEstimator      string   // "chars_div_4" default
    JSONSchemaMode      string   // "native" | "tools" | "prompted" (Phase 35 reads)
    DefaultMaxTokens    *int     // Phase 36b reads
    ReasoningEffort     string   // "off" | "low" | "medium" | "high" | ""
    CostOverrides       *CostTable
}

type CompleteRequest struct {
    Model           string
    Messages        []ChatMessage
    ResponseFormat  *ResponseFormat
    Stream          bool
    OnContent       func(delta string, done bool)
    OnReasoning     func(delta string, done bool)
    Temperature     *float32
    MaxTokens       *int
    Stops           []string
    ReasoningEffort ReasoningEffort
    Extra           map[string]any
}

type CompleteResponse struct {
    Content string
    Cost    Cost
    Usage   Usage
}

type ChatMessage struct {
    Role    Role
    Content Content
    Name    *string
}

type Content struct {
    // Exactly one of Text or Parts is set. Text is the common case.
    Text  *string
    Parts []ContentPart
}

type ContentPart struct {
    Type  PartType
    Text  string      // when Type == PartText
    Image *ImagePart  // when Type == PartImage
    Audio *AudioPart  // when Type == PartAudio
    File  *FilePart   // when Type == PartFile
}

// ImagePart / AudioPart / FilePart: exactly one of URL / DataURL / Artifact set.
// Above the heavy-output threshold, the runtime auto-materializes DataURL → Artifact.

// ResponseFormat: nil | json_object | json_schema(schema). No tool-calling kinds.
type ResponseFormatKind string
const (
    FormatText       ResponseFormatKind = "text"
    FormatJSONObject ResponseFormatKind = "json_object"
    FormatJSONSchema ResponseFormatKind = "json_schema"
)
type ResponseFormat struct {
    Kind       ResponseFormatKind
    JSONSchema json.RawMessage
}

// Sentinels: ErrContextLeak, ErrContextWindowExceeded, ErrUnknownDriver,
// ErrClientClosed, ErrIdentityMissing, ErrInvalidContent.

// Factory entry points:
func Register(name string, f Factory)
func Open(ctx context.Context, cfg ConfigSnapshot, deps Deps) (LLMClient, error)
func RegisteredDrivers() []string
```

## Test plan

- **Unit:**
  - `llm_test.go` — `Open` rejects unknown driver; factory error names registered drivers; `Open` rejects missing `Deps.Artifacts` / `Deps.Bus`; `Complete` rejects missing identity → `ErrIdentityMissing`.
  - `safety_test.go` — planted-leak test; token-budget test; safety-net runs BEFORE driver call (driver's Complete is never invoked when the safety pass fails); idempotency on already-materialized requests.
  - `materialize_test.go` — DataURL ≥ threshold rewritten as `Artifact`; sub-threshold DataURL passes through; URL passes through; existing `Artifact` is a no-op; `llm.image.materialized` event payload carries the new ref.
  - `events_test.go` — every Phase-32 event type registered; payloads are SafePayload; payload field shapes match the godoc contracts.
  - `tokens_test.go` — chars/4 estimator across text-only, multimodal, ArtifactStub-rendered shapes; estimator is deterministic.
  - `mock/mock_test.go` — text round-trip; multimodal (text + image part) round-trip; streaming callbacks fire; ctx cancellation during stream returns `ctx.Err()`; closing the mock returns `ErrClientClosed` on subsequent Complete.

- **Integration:** `internal/llm/llm_test.go` (in-package adapter test) wires `mock` driver + real `events.EventBus` + real `audit.Redactor` + real `artifacts.ArtifactStore` (inmem) end-to-end. A `Complete` with a multimodal payload (DataURL above threshold) demonstrates: (a) the DataURL is rewritten as `Artifact`, (b) `llm.image.materialized` lands on the bus, (c) the driver sees the materialized form. Identity propagation pinned. ≥1 failure mode: a planted oversize raw-bytes string in `ChatMessage.Content.Text` triggers `ErrContextLeak` and emits `llm.context_leak`.

- **Conformance:** N/A — no driver-conformance suite at Phase 32 (mock is the only driver). Phase 33 + the wave-end E2E exercise cross-driver invariants via a single live OpenRouter smoke.

- **Concurrency / leak:** `concurrent_test.go` — N=128 concurrent goroutines × one shared `LLMClient` (mock driver) running `Complete` with per-goroutine identity in ctx. Assertions: no data races (`-race`); per-call identity assertion (the mock writes the seen identity into the response's Cost.ModelHint or similar test-only side-channel); ctx cancellation on goroutine A does not affect goroutine B's Complete; `runtime.NumGoroutine()` returns to baseline within a 2s deadline after the wave completes (using `Gosched`, no `time.Sleep` for sync).

## Smoke script additions

`scripts/smoke/phase-32.sh`:

- Run `go test -race -count=1 -timeout 180s ./internal/llm/...` → OK on pass / FAIL otherwise.
- Static guard: grep for provider-native tool-calling symbols (`ToolChoice` / `FunctionCall` / `ToolUse` / `ToolCallSpec`) in `internal/llm/` → FAIL on hit, OK on clean.
- Skip the HTTP / Protocol surface stub (Phase 60+).

## Coverage target

- `internal/llm`: 85%
- `internal/llm/mock`: 85%

## Dependencies

- 05 (event bus)
- 09 (envelopes — identity quadruple shape used by request validation)
- 17 (artifacts — `ArtifactStore` interface + `ArtifactRef` shape consumed by auto-materialize)
- 03 (audit redactor — bus respects SafePayload bypass)

## Risks / open questions

- **Token estimator accuracy.** Chars/4 is the default per brief 04; it's calibrated for English. Phase 32 ships the constant-shape estimator + a seam (`ModelProfile.TokenEstimator`) so Phase 33+ can register a tiktoken-equivalent without changing the API. RFC §11 Q-4-adjacent.
- **Safety-net ordering vs auto-materialize.** The safety pass runs BEFORE the driver call but materialization happens INSIDE the safety pass (materialize first → then assert no leak → then estimate tokens). This composition is non-obvious; landing a small D-NNN entry documenting the order is the conservative call. Marked in "Glossary additions" / "Files added or changed" below — will add `docs/decisions.md` D-039 in the same PR.
- **Mandatory-by-construction safety net.** `Open` returns a `*safetyClient`, not a raw `Driver`. The `Driver` interface is a private seam. Operators who genuinely need to bypass the safety net (e.g. a non-streaming evaluation harness that hand-constructs `CompleteRequest` already through the safety pass) can construct the wrapper directly — but the registry path is the canonical entry and the safety pass is mandatory there.

## Glossary additions

- `OnContent` — content-delta streaming callback on `CompleteRequest`. Optional; nil when `Stream=false`.
- `OnReasoning` — thinking-channel-delta streaming callback on `CompleteRequest`. Optional; provider-specific (e.g. `o1`/`o3`/`deepseek-reasoner`).
- `ContextWindowReserve` — fraction of a model's context-window cap held back as a safety margin (default 0.05 / 5%). When the safety net's token estimate falls within the reserve, `Complete` fails with `ErrContextWindowExceeded`. RFC §6.5, D-026.
- `ModelProfile` — per-model knobs (`ContextWindowTokens`, `TokenEstimator`, `JSONSchemaMode`, `DefaultMaxTokens`, `ReasoningEffort`, `CostOverrides`). Keyed by canonical model name in `LLMConfig.ModelProfiles`. Phase 32 ships the shape; Phase 33+ consume.
- `ReasoningEffort` — request-level hint (`off` / `low` / `medium` / `high` / `""`). Provider-specific routing (thinking-class models); silently ignored by providers that don't expose the channel.
- `ResponseFormat` — optional structured-output hint on `CompleteRequest`. Kinds: `text` (default; no constraint), `json_object` (provider's "JSON mode"), `json_schema` (caller-supplied JSON Schema). Phase 35 owns the downgrade chain across these kinds.
- `ErrContextLeak` — safety-net sentinel. Raised when a `CompleteRequest` reaches the LLM-client edge carrying raw bytes / strings / `DataURL` ≥ heavy-output threshold that aren't already an `ArtifactStub`. RFC §6.5, D-026.
- `ErrContextWindowExceeded` — safety-net sentinel. Raised when the estimated token count of the assembled `CompleteRequest` falls within `ContextWindowReserve` of the configured model's context-window cap. RFC §6.5, D-026.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **Reusable artifact (`LLMClient`) D-025 test passes** — N≥100 concurrent Complete under `-race`; no races, no context bleed, no cross-cancellation, no goroutine leaks
- [ ] **Integration test exists** — `internal/llm/llm_test.go` wires events bus + audit redactor + artifacts store + mock driver end-to-end; identity propagation pinned; ≥1 failure mode (planted leak) covered
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
