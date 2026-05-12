# Phase 33 — bifrost integration

## Summary

Wire `github.com/maximhq/bifrost/core` (the pure-Go LLM gateway library settled by RFC §11 Q-3 / brief 08) behind Harbor's `llm.Driver` interface from Phase 32. The bifrost driver is a thin translation adapter: `llm.CompleteRequest` ↔ `schemas.BifrostChatRequest`, `schemas.BifrostChatResponse` ↔ `llm.CompleteResponse`, multimodal `ContentPart`s mapped to bifrost's `ChatContentBlock` shapes (D-021), stream chunks fanned out to Harbor's `OnContent` / `OnReasoning` callbacks, ctx cancellation abandoning the chunk reader (brief 08 §"Cancellation caveat"), token usage + cost parsed through, and `llm.cost.recorded` emitted on success so Phase 36a's governance accumulator subscribes against a live emit site. Self-registers under `"bifrost"`; blank-imported in `cmd/harbor`. Bifrost's `Tools` / `ToolChoice` / `FunctionCall` / `ToolUse` types are NEVER referenced — Harbor's runtime owns tool dispatch (RFC §6.4 / brief 07).

## RFC anchor

- RFC §6.5
- RFC §11 Q-3 (RESOLVED 2026-05-08 per brief 08)

## Briefs informing this phase

- brief 03
- brief 07
- brief 08

## Brief findings incorporated

- **brief 08 §"How `bifrost` maps onto Harbor's `LLMClient`":** the driver is a `Complete` adapter that uses `BifrostContext`, calls `ChatCompletionRequest` / `ChatCompletionStreamRequest`, and ignores bifrost's `Tools` / `ToolChoice` parameters. Phase 33 ships exactly this shape; the smoke static guard (added in Phase 32) extends to `internal/llm/drivers/bifrost/` so any future drift fails CI.
- **brief 08 §"Empirical validation":** six OpenRouter-routed models pass 23/24 gating items. Harbor inherits that result wholesale — Phase 33 ships the live conformance test gated behind `HARBOR_LIVE_LLM=1` (the wave-end E2E re-runs ONE provider against the operator's real key).
- **brief 08 §"Cancellation caveat":** three of six providers had delayed channel-close on long-stream cancel. The mitigation Harbor settled on at brief time is "abandon the channel reader on `ctx.Done()`" — Phase 33's stream loop does this in a `select`: when the parent ctx fires, the driver returns `ctx.Err()` and the inflight bifrost worker drains its upstream on its own goroutine, exits, and closes the channel. The runtime never blocks on a stale chunk.
- **brief 08 §"Per-model seam":** model identifiers carry the provider prefix (`openai/gpt-5.3-chat`, `google/gemini-3.1-flash-lite`); the `Provider` in `LLMConfig` is the bifrost-side routing key (e.g. `openrouter`); each `ModelProfile` keys by canonical model name. The bifrost driver passes `cfg.Provider` (typed as `schemas.ModelProvider`) and `req.Model` straight through.
- **brief 08 §"What `bifrost` provides":** the `Account` interface is ~30 lines of API-key resolution. Phase 33's `account.go` implements `GetConfiguredProviders` + `GetKeysForProvider` + `GetConfigForProvider`; reads keys from env vars; fails closed at `New` time when the configured provider's env var is unset; never logs the key value.
- **brief 03 §6 (cost reporting):** `BifrostCost.TotalCost` flows verbatim into `llm.Cost.TotalCost`; per-axis fields (`InputTokensCost`, `OutputTokensCost`, `ReasoningTokensCost`) flow into their counterparts. Phase 36a subscribes to `llm.cost.recorded` against this emit site.
- **brief 07 §6:** the LLM client never sees a tool-shaped type. Harbor builds prompts and parses responses as text/JSON; bifrost's `Tools` / `ToolChoice` parameters are intentionally untouched. Smoke static guard enforces.

## Findings I'm departing from (if any)

- **None.** Brief 08 is recent (2026-05-08) and prescriptive; the bifrost API at v1.5.8 still matches the report (verified by direct source inspection of `~/go/pkg/mod/github.com/maximhq/bifrost/core@v1.5.8/`).

## Goals

- Land a thin `Driver` adapter that lets Harbor's runtime call any of bifrost's 23 first-class providers without the runtime importing a single provider-specific type.
- Multimodal translation works end-to-end on day one: `ImagePart` / `AudioPart` / `FilePart` (in their three supply forms) flow into bifrost's `ChatContentBlock` shapes.
- Cancellation hygiene: a streaming `Complete` cancelled mid-flight does NOT leak goroutines from the driver, even when the upstream provider takes a few seconds to close its end of the channel.
- Cost passthrough wires `llm.cost.recorded` to a real emit site so Phase 36a's governance accumulator has a producer to subscribe to.
- The live six-provider conformance test exists, runs only behind `HARBOR_LIVE_LLM=1`, and exercises the surface brief 08 validated.

## Non-goals

- Provider correction layer (Phase 34's SchemaSanitizer + message-shape normalizer). Phase 33's driver translates faithfully and does NOT silently adjust per-provider quirks — those land at a separate layer between the runtime and the driver.
- Structured-output downgrade chain (Phase 35). `ResponseFormat` flows through verbatim; if a provider rejects it, the failure surfaces as a `BifrostError` and the runtime catches it later.
- Retry with planner feedback (Phase 36).
- Governance enforcement (Phase 36a/36b). Phase 33 only EMITS `llm.cost.recorded`; no accumulator, no ceiling, no rate limiting.
- Tool dispatch — runtime-side (RFC §6.4). No tool-calling type leaks into `internal/llm/drivers/bifrost/`.
- HTTP / Protocol surface — Phase 60+.
- Multi-provider attachments — Phase 33 ships one configured provider per Harbor instance (`LLMConfig.Provider`). Multi-provider routing is a post-V1 consideration if real-world usage demands it.

## Acceptance criteria

- [ ] `internal/llm/drivers/bifrost/bifrost.go` defines `Driver`, `New(cfg llm.ConfigSnapshot, deps llm.Deps) (llm.Driver, error)`, registers `"bifrost"` from `init()`. The Phase 32 `Driver` interface is the surface; no surface tax on the bifrost-side adapter.
- [ ] `internal/llm/drivers/bifrost/account.go` defines `Account`, implements `schemas.Account` (`GetConfiguredProviders`, `GetKeysForProvider`, `GetConfigForProvider`). Reads keys via env vars; missing keys → fail closed at `New` with `ErrMissingAPIKey`; never logs values.
- [ ] `internal/llm/drivers/bifrost/translate.go` ships the translation helpers: `translateRequest(llm.CompleteRequest) (*schemas.BifrostChatRequest, error)`, `translateMessages([]llm.ChatMessage) []schemas.ChatMessage`, `translateContent(llm.Content) *schemas.ChatMessageContent`, `translateResponse(*schemas.BifrostChatResponse) llm.CompleteResponse`. Pure functions; no driver state.
- [ ] **Zero references** to `Tools` / `ToolChoice` / `FunctionCall` / `ToolUse` / `ToolCallSpec` anywhere in `internal/llm/drivers/bifrost/`. The Phase 32 smoke script's static guard already covers `internal/llm/`; Phase 33's smoke script extends it to the bifrost driver path.
- [ ] **Multimodal translation tests** cover `ImagePart` / `AudioPart` / `FilePart` in their three supply forms (URL / DataURL / Artifact). Auto-materialize has ALREADY run upstream (D-039); the driver sees post-materialization values.
- [ ] **ResponseFormat passthrough** — `llm.FormatJSONObject` → bifrost's `response_format: {"type":"json_object"}`; `llm.FormatJSONSchema` → bifrost's `response_format: {"type":"json_schema", ...}`; nil/Text → unset.
- [ ] **ReasoningEffort passthrough** — Harbor's `ReasoningHigh` / `Medium` / `Low` / `Off` map to bifrost's `ChatReasoning.Effort` strings; empty effort leaves bifrost's default untouched.
- [ ] **Identity rejection** — `Complete` with no identity in ctx → `ErrIdentityMissing` BEFORE any bifrost call. (The Phase 32 safety client also rejects; the driver-edge check is a belt-and-suspenders guard for direct-driver tests.)
- [ ] **Streaming** — when `req.Stream` is true, the driver consumes bifrost's chunk channel, invokes `OnContent` per content delta, invokes `OnReasoning` per reasoning delta, and finishes with the assembled `Content` in the returned `CompleteResponse`.
- [ ] **Cancellation hygiene** — a streaming `Complete` cancelled mid-flight returns `ctx.Err()` (raw `context.Canceled` or `context.DeadlineExceeded`); subsequent goroutine count returns to baseline. The driver's chunk reader is non-blocking with a `select` on `ctx.Done()`.
- [ ] **Concurrent-reuse test (D-025)** — N≥100 concurrent `Complete` calls against ONE shared driver instance under `-race`, using a stubbed bifrost client (NOT live network). Asserts no races, no goroutine leaks (baseline restored), no context bleed.
- [ ] **Cost passthrough + emit** — `BifrostCost.TotalCost` / per-axis fields flow into `llm.Cost`; the driver emits `llm.cost.recorded` after a successful `Complete` with the full identity quadruple, `model`, `cost`, `usage`. Test asserts the bus observer sees the emit.
- [ ] **Live six-provider conformance test** exists at `internal/llm/drivers/bifrost/conformance_test.go`, guarded by `t.Skip` unless `HARBOR_LIVE_LLM=1`. Surface: basic chat + `json_object` response_format + streaming + ctx cancel + token usage + cost + one multimodal text+image round-trip. **CI default skips.** Tests do NOT run in this PR.
- [ ] `internal/config/config.go` — no new fields needed; Phase 32 already ships `LLMConfig.Provider/Model/APIKey/BaseURL/Timeout` + `ModelProfiles`. Validator's `bifrost` allowlist entry is already present (Phase 32).
- [ ] `cmd/harbor/main.go` blank-imports `_ "github.com/hurtener/Harbor/internal/llm/drivers/bifrost"` so the driver self-registers in the binary.
- [ ] `scripts/smoke/phase-33.sh` runs the bifrost-driver unit tests under `-race`, extends the Phase 32 no-tools-symbol static guard to cover `internal/llm/drivers/bifrost/`, and skips the live-conformance + HTTP-surface portions cleanly.
- [ ] `examples/harbor.yaml` adds a commented bifrost block that operators uncomment when they're ready to ship real traffic (the YAML already mentions bifrost; Phase 33 just confirms the shape works).
- [ ] `docs/glossary.md` adds `BifrostDriver`, `BifrostContext` (bifrost's wrapped ctx), `ProviderRouting`.
- [ ] `docs/decisions.md` — IF a non-obvious design call surfaces (e.g. streaming-cancel pattern, env-var lookup strategy, cost-emit responsibility), append a D-NNN. Default expectation is a single new D-NNN documenting the bifrost-driver design choices that are easier to capture in one place than to spread across godoc.
- [ ] `README.md` Status table flips Phase 33 row to Shipped.
- [ ] `docs/plans/README.md` master table flips Phase 33 row to Shipped.
- [ ] Coverage on `internal/llm/drivers/bifrost`: ≥ 80%.

## Files added or changed

- `internal/llm/drivers/bifrost/bifrost.go` — driver struct, New, init() registration, Complete, Close — NEW
- `internal/llm/drivers/bifrost/account.go` — Account implementation (env-var key resolution) — NEW
- `internal/llm/drivers/bifrost/translate.go` — request/response/stream translation helpers — NEW
- `internal/llm/drivers/bifrost/cost.go` — `llm.cost.recorded` emit helper — NEW
- `internal/llm/drivers/bifrost/bifrost_test.go` — Complete, identity rejection, ResponseFormat / ReasoningEffort passthrough, stream callback wiring — NEW
- `internal/llm/drivers/bifrost/account_test.go` — env-var resolution, missing-key fail-closed — NEW
- `internal/llm/drivers/bifrost/translate_test.go` — multimodal mapping per supply form, cost/usage translation — NEW
- `internal/llm/drivers/bifrost/concurrent_test.go` — D-025 N≥100 + cancellation isolation + goroutine baseline — NEW
- `internal/llm/drivers/bifrost/conformance_test.go` — gated live six-provider tests — NEW
- `internal/llm/drivers/bifrost/export_test.go` — package-private constructor for tests injecting a stub bifrost client — NEW
- `cmd/harbor/main.go` — blank import — MODIFIED
- `examples/harbor.yaml` — annotated bifrost block — MODIFIED (light)
- `scripts/smoke/phase-33.sh` — smoke — NEW
- `docs/plans/phase-33-bifrost.md` — this file — NEW
- `docs/glossary.md` — `BifrostDriver`, `BifrostContext`, `ProviderRouting` — MODIFIED
- `docs/decisions.md` — one D-NNN entry — MODIFIED
- `README.md` — Status row — MODIFIED
- `docs/plans/README.md` — Status row — MODIFIED

## Public API surface

```go
package bifrost

// New constructs a bifrost-backed llm.Driver. The Phase 32 safety
// pass wraps it via the registry path (llm.Open); operators do NOT
// construct this directly in production.
//
// cfg.Provider selects the bifrost provider; cfg.APIKey is either a
// literal key or an `env.NAME` reference; cfg.Timeout flows into
// bifrost's NetworkConfig.DefaultRequestTimeoutInSeconds; cfg.BaseURL
// (when set) overrides the provider's default endpoint.
//
// Fails closed with ErrMissingAPIKey when an env.* reference cannot
// be resolved at construction time.
func New(cfg llm.ConfigSnapshot, deps llm.Deps) (llm.Driver, error)
```

The package self-registers under `"bifrost"` from `init()`; production callers route through `llm.Open(ctx, cfg, deps)` with `cfg.Driver = "bifrost"`.

## Test plan

- **Unit:** `bifrost_test.go` — happy-path text Complete, identity-rejection, ResponseFormat (Text / JSONObject / JSONSchema) translation, ReasoningEffort translation, Temperature / MaxTokens / Stops translation, BifrostError → wrapped Go error mapping, BackfillParams handling, Close idempotency.
- **Unit:** `translate_test.go` — multimodal per supply form (URL / DataURL / Artifact for each of Image / Audio / File), cost/usage parsing including missing-cost shape (BifrostLLMUsage.Cost == nil), streaming-chunk delta accumulation.
- **Unit:** `account_test.go` — `env.NAME` resolution, literal key passthrough, missing env var → ErrMissingAPIKey, redaction-safe error messages (never includes the key value).
- **Concurrency / leak:** `concurrent_test.go` — N=128 concurrent Complete under `-race` against a stub bifrost client. No data races; per-call identity bleed asserted via a `seen` channel from the stub; goroutine baseline restored within 2s of teardown.
- **Cancellation:** included in `concurrent_test.go` — mid-stream ctx cancel returns `ctx.Err()`; chunk reader stops within the cancel window; bifrost's residual upstream drain is tolerated (we don't wait for it).
- **Conformance (gated):** `conformance_test.go` — six-provider live matrix against OpenRouter; runs only with `HARBOR_LIVE_LLM=1`. CI default skips. The wave-end E2E exercises ONE provider against the real key.
- **Integration:** the safety pass + bifrost driver wiring is covered indirectly by the existing `internal/llm` tests once Phase 33's blank-import lands. The wave-end E2E (separate PR) ships the cross-subsystem test.

## Smoke script additions

- Run `internal/llm/drivers/bifrost` tests under `-race` (excludes the gated live-conformance test by default).
- Extend the Phase 32 no-tools-symbol grep guard to cover `internal/llm/drivers/bifrost/`.
- Skip-on-404 the HTTP/Protocol surface (lands Phase 60+).
- Skip-on-no-key the live-conformance assertion (the env-var-gated test self-skips; the smoke calls out the gating mechanism so operators know to set `HARBOR_LIVE_LLM=1` when they want to exercise real providers).

## Coverage target

- `internal/llm/drivers/bifrost`: 80% (per master plan).

## Dependencies

- 32 (LLM client core + safety net + Driver interface; merged 2026-05-11 at `d88766a`).
- 09 (Envelopes / identity quadruple — for ctx-carried identity; long-merged).
- 11 (Event bus skeleton — Phase 32 already registered the LLM event types; Phase 33 uses them).

## Risks / open questions

- **Stream-channel close timing on long streams** (brief 08 §"Cancellation caveat"). Mitigation: driver abandons the chunk reader on `ctx.Done()`; goroutine-leak test asserts baseline. Acceptable behaviour — Harbor's runtime never blocks on a stale chunk. A deeper probe (a 30-second cancel budget; whether `len(chan)` keeps growing) is appropriate when an operator hits the case in production.
- **Bifrost API drift since brief 08.** Bifrost v1.5.8 (the indirect dep added by the parent prep commit) matches brief 08's surface as of 2026-05-08; the dispatcher confirmed by reading the source. If a future bump materially changes `ChatCompletionRequest` / `ChatCompletionStreamRequest` signatures or `BifrostChatRequest` field shape, a follow-up phase plan handles the bump in isolation.
- **Provider-keyed API-key resolution** — the operator's `.env` carries `OPENROUTER_API_KEY` (one provider). The driver looks up `cfg.APIKey` as the literal key OR an `env.NAME` reference. Multi-provider attachments are out of scope (single-provider per Harbor instance at V1).
- **Cost-emit responsibility** — the emit lives in the bifrost driver, not the safety client. Rationale: the safety client is provider-blind; the driver knows the request's model and the bifrost-reported cost shape. Phase 36a's accumulator subscribes to the type, not to the producer. If a future phase wants to fold the emit into the safety client (so other drivers — none today — get cost emission for free), the §17.6 wave-end audit can flag it.

## Glossary additions

- `BifrostDriver` — Harbor's adapter that wires `github.com/maximhq/bifrost/core` behind `llm.Driver`. Self-registers under `"bifrost"`.
- `BifrostContext` — bifrost's `*schemas.BifrostContext`, a custom `context.Context` implementation that tracks user-set values and propagates cancellation. Harbor constructs one per Complete via `schemas.NewBifrostContext(ctx, schemas.NoDeadline)`.
- `ProviderRouting` — the per-Harbor-instance bifrost provider selection (`LLMConfig.Provider`). V1 supports one configured provider per binary; multi-provider routing is post-V1.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (N/A — bifrost driver passes identity through; no shared per-session state on the driver)
- [ ] **Concurrent-reuse test passes** — see `concurrent_test.go`
- [ ] **Integration test exists** — covered by the existing `internal/llm` test suite once Phase 33 self-registers + Phase 33's smoke verifies the registry resolves it; the wave-end E2E exercises one provider live
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (N/A — no departures)
