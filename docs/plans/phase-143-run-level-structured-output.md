# Phase 143 — Run-level structured output: the `WithOutputSchema` run option

## Summary

Ships the run-level typed-output mechanism: an opt-in `assemble.WithOutputSchema(schema)` `RunOption` on `Stack.RunOnce` that threads a caller-supplied JSON Schema for the FINAL answer through `runctx` → `planner.RunContext` into the React driver's terminal completion, riding the already-shipped-but-unconsumed LLM substrate (`CompleteRequest.ResponseFormat`, the `Validator` retry-with-feedback wrapper, the `OutputMode` three-strategy shaping + downgrade chain). The validated answer lands as a new ADDITIVE `answer_payload` key on `planner.AnswerEnvelope` (the `Answer` string and the pinned byte-shape are untouched). Posture (D-272, survey-validated): opt-in with zero default-path change; streaming preserved; when the option IS set, the terminal answer arrives via the validated typed payload rather than the token stream — the Claude Agent SDK / LangGraph `response_format` precedent, the standard pairing with a validate-and-retry loop.

## RFC anchor

- RFC §6.5 (structured output strategies + retry with feedback — the LLM-level substrate this consumes; the "Run-level structured output (planned — D-272)" paragraph added alongside this plan)
- RFC §6.2 ("schema mode" is already enumerated among "runtime-level run options, not planner state" — this phase implements that slot)
- RFC §3.6 (the facade re-export path for the new option and sentinels)

## Briefs informing this phase

- brief 03
- brief 07
- brief 08

## Brief findings incorporated

- brief 03 §"Argument validation" (line ~176): "Validation failures are *not* tool errors — they are routed back to the planner … with the error fed in via `LLMClient` retry feedback." The run-level output validator follows the same posture: a schema-invalid terminal answer feeds back through the existing `llm.retry_with_feedback` corrective loop, bounded by `ModelProfile.MaxRetries` — never a new retry mechanism.
- brief 03 §"Two parallel LLM modes (the toggle smell)": "Harbor picks one architecture and bakes the correction in." This phase adds NO new output-shaping strategy: the terminal completion rides the EXISTING `OutputMode = Native | Tools | Prompted` selection + `json_schema → json_object → text` downgrade chain per `ModelProfile`, so provider variance is already solved once, in one place.
- brief 07 §6: the client contract already carries "Optional `response_format` hint: `{"type": "json_object"}` or `{"type": "json_schema", …}`. The runtime is responsible for sanitizing/downgrading per provider; the client just forwards." The run-level option is a new PRODUCER for that existing hint, not a new client capability.
- brief 08 §"Empirical validation": `response_format` passthrough (`json_object`) was one of the six gating items empirically validated against six OpenRouter-routed models (23/24 pass) — the wire substrate under this feature is validated, not speculative.

## Findings I'm departing from (if any)

None.

## Goals

- An embedder can ask a run for a schema-conforming final answer in one line: `stack.RunOnce(ctx, goal, id, assemble.WithOutputSchema(schema))` — and receive a VALIDATED payload or a loud, typed error. This closes the largest remaining "first ten minutes" ergonomics gap on the embed adopter path (v1.8's RunOnce answers are string-only today: `AnswerEnvelope.Answer`).
- Zero default-path change. No schema → byte-identical behavior to v1.8: the option is a nil-default pointer-shaped run option; the `Validator`/`ResponseFormat` fields stay nil exactly as today; the envelope's existing three keys are byte-identical (golden test still pins them).
- Validation is runtime mechanism, planner-agnostic. The final `Finish` payload is validated against the schema at the RunOnce edge for EVERY planner (React, deterministic, external `planner.Register` concretes) — no `Supports*` capability ceremony (§4.4). The React driver ADDITIONALLY steers generation (constrained terminal completion + corrective retry); a planner that emits an already-structured payload (the deterministic planner's flow output) simply passes validation without generation steering.
- Fail-loud end-to-end: schema-invalid output after the retry budget → typed `planner.ErrOutputInvalid` wrapping the validation error (and the `llm.ErrRetryExhausted` chain when the React retry loop was engaged). NEVER a silent fallback to unvalidated text (§13).
- Streaming preserved, posture documented: `WithStream` composes with `WithOutputSchema`; `tool_dispatched` and `step` events stream exactly as today; assistant-content `token` chunks are suppressed for the schema-constrained run and the validated answer arrives once, in the envelope. This is a documented behavior choice (D-272), not a regression — stated in the option's godoc, the stream godoc, the embed recipe, and this plan's acceptance criteria, so it is a decision, not a surprise.
- The idle primitive gains its production consumer (§13): `CompleteRequest.Validator` + the retry-with-feedback wrapper currently have ONE production consumer (the trajectory summarizer's `ResponseFormat` use; `Validator` has zero). This phase is the end-to-end consumer that validates that machinery against a real call site.

## Non-goals

- **Partial-object streaming (progressively validated partial payloads) — the named, anticipated follow-up, NOT shipped here.** Roughly half the surveyed SOTA surfaces ship some form of it (progressively validated partials; unvalidated `DeepPartial` frames); it maps onto an additive `StreamEventKind` when demand arrives. Buffered-whole delivery is the correct v1 pairing with a validate-and-retry loop (no framework that retries also live-streams the constrained answer). Recorded in D-272 so the sequencing is a decision, not silence.
- The generic typed binding (`RunTyped[T]`, schema derivation from a Go type) — Phase 144, which consumes this phase's option.
- Retyping `AnswerEnvelope.Answer` or breaking the pinned envelope byte-shape. `Answer` continues to carry the string rendering; `answer_payload` is additive (`omitempty`).
- Structured output for INTERMEDIATE steps, tool calls, or spawned-task results — tool args/outputs already have per-tool schemas (`ArgsSchema`/`OutSchema`); this phase is the terminal answer only.
- New Protocol methods or wire types. The envelope crosses the Protocol as opaque JSON inside `tasks.get`'s `result_inline` exactly as today; the new key rides along additively. No TS-lockstep or protocol-docs impact.
- Any change to per-provider correction, `OutputMode` selection, or the downgrade chain — consumed as-is.

## Acceptance criteria

- [x] `assemble.WithOutputSchema(schema json.RawMessage) RunOption` exists; a nil/empty schema is a loud config error at call time, not a silent no-op. Without the option, `RunOnce` behavior and the envelope bytes are unchanged (golden test unchanged and green).
- [x] The schema threads `RunOnce` → `runctx.Sources`/`Option` → `planner.RunContext` (a read-only per-run field, per D-025: state on the run, never on the Stack) → the React driver's terminal completion, which sets `ResponseFormat{Kind: FormatJSONSchema, JSONSchema: schema}` shaped by the profile's EXISTING `OutputMode` strategy + downgrade chain, and a compiled-schema `Validator` engaging the retry-with-feedback wrapper bounded by `ModelProfile.MaxRetries`.
- [x] Runtime-edge final validation: the terminal `Finish` payload is validated against the schema at the RunOnce edge regardless of planner driver. Deterministic-planner runs with conforming payloads pass; non-conforming output → typed `planner.ErrOutputInvalid` (wrapping the schema error, and `llm.ErrRetryExhausted` when the React loop retried). No silent-text fallback path exists (asserted by test).
- [x] `planner.AnswerEnvelope` gains `AnswerPayload json.RawMessage` with tag `json:"answer_payload,omitempty"`; the golden encoding test is extended with a with-payload fixture AND re-pins the without-payload bytes unchanged. `Answer` carries the payload's string rendering for backward compatibility.
- [x] `WithStream` + `WithOutputSchema` compose: `tool_dispatched` + `step` events stream as today; assistant-content `token` chunks are suppressed for the schema-constrained run; every delivered `StreamEvent` still precedes the final envelope (the D-266 ordering guarantee re-asserted under the new option). The suppression posture is stated in the `WithOutputSchema` AND `WithStream` godoc.
- [x] Identity handling unchanged: the option carries no identity; `RunOnce`'s per-call `identity.Identity` argument and fail-loud validation are untouched (asserted by the existing tests staying green).
- [x] The D-025 concurrent-reuse test extends to mixed traffic: N≥100 concurrent `RunOnce` calls against ONE shared Stack, interleaving schema-constrained and plain runs with DISTINCT schemas, asserting no schema/payload bleed across runs, no cancellation cross-talk, goroutine baseline restored, under `-race`.
- [x] §13 primitive-with-consumer: an end-to-end test (scripted-LLM devstack harness) drives a schema-constrained run through the real RunLoop — happy path (validated payload in the envelope), corrective-retry path (first response schema-invalid → `llm.retry_with_feedback` observed → second response valid), and exhaustion path (`ErrOutputInvalid`).
- [x] `sdk/assemble` re-exports `WithOutputSchema`; `sdk/planner` re-exports `ErrOutputInvalid` (alias/forward only — no behavior in `sdk/`, D-204).
- [x] `examples/embed-runonce/` gains a typed-output variant (or a sibling example) exercising the option offline; §18 sweep: `docs/recipes/embed-harbor-headless.md` gains the option + the streaming-posture caveat in the same PR, plus any `docs/skills/` playbook that demonstrates `RunOnce` (grep `docs/skills/` for the surface at implementation time).
- [x] `scripts/smoke/phase-143.sh` flips from skeleton to real assertions (unit-test leg + a static grep pinning that no non-test code path returns unvalidated text when a schema is set).

## Files added or changed

- `internal/runtime/assemble/runonce.go` — the `WithOutputSchema` RunOption + runtime-edge final validation
- `internal/runtime/assemble/stream.go` — godoc posture note (token suppression under the option)
- `internal/runtime/runctx/newruncontext.go` — schema threading through `Sources`/`Option`
- `internal/planner/` — `RunContext` field, `AnswerEnvelope.AnswerPayload`, `ErrOutputInvalid`, golden-test extension
- `internal/planner/react/react.go` — terminal-completion `ResponseFormat` + `Validator` wiring
- `sdk/assemble/assemble.go`, `sdk/planner/planner.go` — alias/forward additions
- `examples/embed-runonce/` (typed-output variant), `docs/recipes/embed-harbor-headless.md`
- `test/integration/phase143_output_schema_test.go`
- `scripts/smoke/phase-143.sh`
- `docs/glossary.md`, `docs/decisions.md` (D-272 flip to shipped), `docs/plans/README.md` (status flip), `RFC-001-Harbor.md` (§6.5 paragraph wording flip)

## Public API surface

- `assemble.WithOutputSchema(schema json.RawMessage) RunOption` (re-exported at `sdk/assemble`)
- `planner.AnswerEnvelope.AnswerPayload json.RawMessage` (`json:"answer_payload,omitempty"`)
- `planner.ErrOutputInvalid` — typed sentinel for schema-invalid terminal output after the retry budget (re-exported at `sdk/planner`)

## Test plan

- **Unit:** option validation (nil/empty schema → loud error); envelope golden test (existing bytes unchanged + with-payload fixture); runtime-edge validation over table-driven payloads (valid / invalid / non-JSON); React terminal-completion request construction (ResponseFormat + Validator set only when the run carries a schema; intermediate tool-call turns unaffected); stream suppression (no `token` events under the option; `step`/`tool_dispatched` present; ordering before envelope).
- **Integration:** the §13 consumer E2E above (scripted-LLM devstack; happy / corrective-retry / exhaustion), run under `-race` with real inmem drivers + real bus; a deterministic-planner leg proving planner-agnostic validation (conforming flow payload passes; non-conforming fails loud).
- **Conformance:** N/A — no new driver seam (the option rides existing registries).
- **Concurrency / leak:** the mixed-traffic D-025 extension above (N≥100, distinct schemas, no bleed, goroutine baseline, `-race`).

## Smoke script additions

- `scripts/smoke/phase-143.sh` (`PREFLIGHT_REQUIRES: unit-tests`): `go test -race` for `internal/runtime/assemble` + `internal/planner` (golden envelope) + the phase-143 integration test; a static grep asserting `answer_payload` appears in exactly one wire-shape definition (the envelope). Skeleton parks with `skip` until the surface lands.

## Coverage target

- `internal/runtime/assemble`: 80%
- `internal/runtime/runctx` (touched lines): no regression below current package coverage
- `internal/planner` + `internal/planner/react` (touched lines): no regression below current package coverage

## Dependencies

- 35 (structured output strategies: `OutputMode` + downgrade chain, D-043)
- 36 (retry-with-feedback wrapper keyed off `CompleteRequest.Validator`, D-043)
- 110a (`planner.AnswerEnvelope` canonical export, D-194)
- 132 (`Stack.RunOnce` + `runctx.NewRunContext`, D-265)
- 132-stream (`WithStream` + the ordering guarantee, D-266)

## Risks / open questions

- **Terminal-turn detection strategy (implementation call, two named candidates).** In the React loop a completion's terminal-ness is only known when it arrives (tool calls vs. content). Candidate A: set `ResponseFormat` on every turn where the profile's `OutputMode` is `Native` (providers accept schema + tools together; tool-call turns carry no content, the content-bearing terminal turn is constrained). Candidate B: the output-tool shape — the schema rides the EXISTING `OutputMode.Tools` `respond_with` envelope, so "finishing" is emitting the output shape, which composes with native tool calling on every provider (the Pydantic-AI `ToolOutput` precedent). Recommendation: let the profile's already-selected `OutputMode` decide (A under `Native`, B under `Tools`/`Prompted`) — one strategy surface, no new toggle (brief 03's no-toggle rule). Resolved at implementation; documented in the shipped godoc either way.
- **Coordination with the pre-wave envelope fix.** The pre-wave `fix` PR redefines `ToolCallsSeen` semantics (step count → true tool-invocation count) across the five producer sites in the SAME files this phase touches (`runonce.go`, the envelope). Land the fix PR FIRST; this phase rebases on it.
- **`ExtractAssistantAnswer` string-collapse.** The extractor renders `Finish.Payload` to a string today; with a schema set, the payload's raw JSON must round-trip into `AnswerPayload` WITHOUT passing through the lossy string path (map key-order nondeterminism). The implementation must capture the validated raw bytes at the validation site.
- **AwaitTask observation growth.** The envelope is LLM-visible in the awaiting parent's observation (`taskOutcomeObservation` parses it generically); a large `answer_payload` inflates parent context. The D-026 heavy-output threshold already guards the task-result path — verify the envelope-with-payload rides the existing offload, add no second mechanism.
- **Provider variance on schema+tools.** Candidate A's provider support is uneven (the ADK lesson); the downgrade chain + candidate B are the mitigation. The live-provider check rides the existing `HARBOR_LIVE_*` env-gated pattern (§17.8), not CI.

## Glossary additions

- **Run output schema** — the opt-in, run-level JSON Schema a `RunOnce` caller supplies via `WithOutputSchema`; the terminal answer is validated against it (fail-loud) and delivered as the envelope's `answer_payload`.
- **Answer payload** — the additive `answer_payload` key on the answer envelope: the schema-validated raw-JSON terminal answer of a schema-constrained run; absent on plain runs.

(Both added to `docs/glossary.md` in the same PR as this plan.)

## Pre-merge checklist

- [x] `make drift-audit` passes
- [ ] `make preflight` passes (CI runs the full gate; skipped locally per PR note)
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. (This phase extends the shared-Stack surface — the mixed-traffic test is mandatory.)
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. (It consumes 35 + 36 + 132 — the integration test is mandatory.)
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry filed
