# Phase 84b — Multimodal attachment disposition policy

## Summary

Today the runtime decides — alone, in a hardcoded `switch` — what happens to an
uploaded attachment: `internal/planner/multimodal.go::materializeOne` maps MIME →
disposition (image inline, PDF/audio/everything-else as `ArtifactStub`). There is
no per-MIME / per-attachment policy anywhere in config, and a Protocol client or a
third-party app has no way to express intent. That **forces behavior**: a
developer who wants a PDF parsed by a tool, chunked for retrieval, or handled by a
domain tool — rather than shipped to a provider's native document understanding —
cannot say so.

Phase 84b turns that mechanism into **declared policy**. It introduces an
`AttachmentDisposition` enum and resolves it from three layers (per-attachment
Protocol hint → agent config → runtime default), with the materializer consulting
the policy instead of a fixed switch. **The default stays exactly what ships
today** — `ref` (an `ArtifactStub` carrying the existing `Fetch.Tool` hint), the
developer-controllable path the planner already drives via native tool-calling
(107c). Phase 84b ships **no** new provider mechanism and **no** embeddings — it is
purely the policy seam that lets 84c's `provider_native` upload be an **opt-in
disposition** rather than an automatic override, and that keeps the "process it
myself" path first-class for Playground, Protocol, and third-party clients alike —
**and for Go library consumers embedding the runtime headless** (no Protocol, no
config file): the policy core (enum, policy type, precedence resolver) lives in
`internal/planner` as pure, exported surface; the Protocol hint and the
`harbor.yaml` block are thin carriers over it, never its home.

See D-189 for the 84b/84c/84d split rationale.

## RFC anchor

- RFC §6.4 — code-level tool dispatch + the `ArtifactStub.Fetch.Tool` hint (the
  existing "use this tool to read the bytes" seam the `ref` disposition rides on).
- RFC §6.5 — LLM client + multimodal `Content.Parts` (the parts the disposition
  decides how to populate).
- RFC §6.10 — heavy-output threshold (the size boundary disposition interacts with).

## Briefs informing this phase

- brief 03 — tools-and-llm (multimodal content shapes + the materializer dispatch)
- brief 11 — console-feature-surface (the Playground composer attach UX)

## Brief findings incorporated

- **brief 03 §multimodal dispatch.** The per-MIME materializer is the right place
  for disposition, but the *choice* of disposition must not be hardcoded there —
  the materializer becomes a policy *consumer*, not the policy *author*.
- **brief 11 §Playground composer.** The composer already attaches via
  `artifacts.put` + `input_artifact_ids`; 84b lets each attachment carry an
  explicit disposition so the operator/developer — not the runtime — decides how a
  given upload is handled.

## Findings I'm departing from (if any)

The superseded `phase-84b-bifrost-multimodal-v13.md` plan folded disposition into
the provider-native upload mechanism and auto-routed by MIME — i.e. it forced
behavior. That content moves to **Phase 84c** and is re-gated behind this policy.
Recorded in D-189.

## Goals

- Define `AttachmentDisposition`:
  - `ref` *(default)* — emit `ArtifactStub` + `Fetch.Tool` hint; the planner /
    developer processes the bytes via a tool. Unchanged from today's default.
  - `inline` — DataURL inline (small images; the sub-threshold fast path).
  - `provider_native` — hand the artifact to the provider's own understanding
    (implemented in 84c; in 84b it resolves but degrades to `ref` with a logged
    notice until 84c lands — never a silent no-op).
  - `tool:<name>` — force a specific catalog tool (`pdf.extract`,
    `document.index`, …) via the existing `Fetch.Tool` mechanism.
- Resolve disposition with explicit precedence: **per-attachment caller hint >
  per-agent policy map > runtime default (`ref`)**. The layers are semantic; the
  carriers are adapters: the Protocol input-artifact `disposition` field is one
  carrier of the per-attachment hint (direct `InputArtifactView.Disposition`
  construction by a library consumer is the other), and `harbor.yaml` is one
  carrier of the per-agent map (programmatic `planner.DispositionPolicy`
  construction is the other).
- The resolver is an **exported pure function in `internal/planner`**
  (`disposition.go`): `ResolveDisposition(hint, policy, mime)` returns the
  resolved `AttachmentDisposition` **and the winning `DispositionLayer`**. The
  run loop (`cmd/harbor` + the devstack mirror) is a thin caller — the
  precedence logic is never homed in `cmd/harbor`. A headless library consumer
  calls the same function, or skips it entirely by setting `Disposition`
  directly on the views it constructs (the consumer *is* the top-precedence
  layer).
- The `DispositionPolicy` type (per-MIME map + default) is **defined in
  `internal/planner`**; `internal/config` merely decodes the `harbor.yaml` block
  into it. Both construction paths — Go and config — produce the same value.
- Surface the policy in four places, one enum:
  - **Programmatic (headless SDK)** — construct `planner.DispositionPolicy`
    and/or set `InputArtifactView.Disposition` directly; no Protocol, no config
    file. This is the seam the other three adapt onto.
  - **Agent config** (`harbor.yaml`) — a per-MIME default-disposition map.
  - **Protocol** — a `disposition` hint on the input-artifact shape so the
    Playground *and* third-party clients can override per upload.
  - **Planner** — because the default is a stub + fetch hint, the planner already
    elects the tool path turn-by-turn under native tool-calling (107c); no new
    mechanism needed for that path.
- `materializeOne` consults the resolved disposition; the hardcoded MIME switch
  becomes the *fallback default map*, not the authority.
- Audit/telemetry: the resolver and materializer are pure — they **return** the
  winning layer and any degradation fact (unknown `tool:<name>`, pre-84c
  `provider_native`) as typed values; the **caller** (run loop, devstack, or a
  consumer's own loop) logs and emits them. On the dev path every
  materialization records which disposition fired and why (which layer won), so
  operators can see the runtime never silently overrode them.

## Non-goals

- **No provider-native upload mechanism** — that is Phase 84c (the `provider_native`
  branch resolves but degrades to `ref` until 84c ships).
- **No embeddings / Embedder** — that is Phase 84d.
- No new MIME support and no change to the sub-threshold image inline fast path's
  default behaviour.
- No removal of the `ArtifactStub` universal-degradation guarantee (it stays the
  fallback for every disposition).

## Acceptance criteria

- [ ] `AttachmentDisposition` enum defined; `planner.InputArtifactView` carries a
      resolved `Disposition`.
- [ ] `planner.ResolveDisposition` is an exported **pure** function returning the
      resolved disposition + the winning `DispositionLayer`;
      `planner.DispositionPolicy` is the planner-package policy type;
      `internal/config` decodes into it. **No precedence logic in `cmd/harbor`**
      — the run loop and devstack are thin callers (asserted by the unit tests
      living in `internal/planner`, not in `cmd/harbor`).
- [ ] Agent-config per-MIME disposition map parses + validates (`internal/config`)
      and decodes into `planner.DispositionPolicy`.
- [ ] Protocol input-artifact shape carries an optional per-attachment
      `disposition` hint; the run loop threads it into the view.
- [ ] Precedence resolves correctly (per-attachment > per-agent map > default) —
      unit-tested across all four enum values.
- [ ] Default is unchanged: with no policy set, every MIME resolves exactly as it
      does today (image inline, everything else `ref`/stub) — a golden test pins
      byte-for-byte parity.
- [ ] `tool:<name>` routes to the named catalog tool via `Fetch.Tool`; an unknown
      tool name degrades to `ref` — the resolver **returns a typed degradation
      fact** and the caller logs the warning (fail-loud, never a silent drop —
      CLAUDE.md §13; the dev run loop's emission is asserted in the integration
      test).
- [ ] `provider_native` resolves but degrades to `ref` with a returned "84c not
      yet shipped" degradation fact the caller logs (the §13 honest-degradation
      seam for the same-wave 84c).
- [ ] `docs/recipes/control-attachment-disposition.md` ships the headless path:
      set `InputArtifactView.Disposition` directly and/or build a
      `DispositionPolicy` + call `ResolveDisposition` in Go — no Protocol, no
      `harbor.yaml`.
- [ ] Smoke `scripts/smoke/phase-84b.sh` asserts a per-attachment disposition hint
      round-trips through `start` and is reflected on `tasks.get`.

## Files added or changed

- `internal/planner/multimodal.go` — `materializeOne` consults the resolved
  disposition; the MIME switch becomes the default map.
- `internal/planner/disposition.go` — `AttachmentDisposition` enum,
  `DispositionPolicy`, `DispositionLayer`, and the exported pure
  `ResolveDisposition` (the policy core; everything else adapts onto it).
- `internal/planner/planner.go` — `InputArtifactView.Disposition`.
- `internal/planner/multimodal_test.go` + `disposition_test.go` — precedence +
  default-parity tests (the precedence tests live here, not in `cmd/harbor`).
- `internal/config/config.go` + `loader.go` — decode the per-MIME disposition
  block on the agent/multimodal config into `planner.DispositionPolicy` +
  validation (the type lives in `internal/planner`, not here).
- `internal/protocol/types/*.go` — optional `disposition` field on the
  input-artifact shape (snake_case wire tag).
- `cmd/harbor/cmd_dev_runloop.go::resolveInputArtifacts` — thread the hint into
  the view; **thin call** to `planner.ResolveDisposition`; log/emit the
  winning-layer + degradation facts it returns.
- `harbortest/devstack/devstack.go` — D-094 mirror (same thin-caller shape).
- `docs/recipes/control-attachment-disposition.md` — the headless library-consumer
  recipe (direct `Disposition` construction + programmatic `DispositionPolicy`).
- `web/console/src/routes/(console)/playground/[session_id]/+page.svelte` — an
  optional per-attachment disposition selector on the composer (defaults to `ref`).
- `scripts/smoke/phase-84b.sh` — disposition round-trip assertion.
- `docs/decisions.md` — D-189.

## Public API surface

- `planner.AttachmentDisposition` enum (`ref` / `inline` / `provider_native` /
  `tool:<name>`).
- `planner.InputArtifactView.Disposition AttachmentDisposition` — settable
  directly by a headless consumer constructing its own views.
- `planner.DispositionPolicy` (per-MIME map + default) +
  `planner.ResolveDisposition(hint, policy, mime) (AttachmentDisposition,
  DispositionLayer)` — the programmatic seam; config and Protocol are carriers
  over it.
- Agent-config: `multimodal.disposition` per-MIME map (documented default = `ref`).
- Protocol: optional `disposition` string on the input-artifact request shape.

> Scope note: "public" here is module-internal — `internal/` packages are not
> importable by external modules (the recorded reason `harbortest/` lives at the
> top level). This surface is stable for in-module consumers (cmd, harbortest,
> examples); external-team embedding needs a future facade/export RFC, out of
> scope for this wave.

## Test plan

- **Unit:** precedence resolution across all enum values + all three layers
  (pure-function tests in `internal/planner`); default-parity golden test (no
  policy ⇒ today's behaviour byte-for-byte); unknown-tool and pre-84c
  `provider_native` degradation paths return the typed degradation fact (the
  dev run loop's log/event emission is asserted in the integration test).
- **Integration:** `harbortest/devstack` — spawn a task with a per-attachment
  `disposition: tool:pdf.extract`; assert the materializer emitted a stub with the
  named `Fetch.Tool`, not an inline/native part.
- **Concurrency / leak:** disposition resolution is pure; covered by the existing
  planner concurrent-reuse test (no per-run state added to a compiled artifact).

## Smoke script additions

`scripts/smoke/phase-84b.sh` (live-server): `start` with an input artifact carrying
a `disposition` hint → `tasks.get` reflects the resolved disposition on the
input-artifact view; the 404/405/501 → SKIP convention guards a pre-84b build.

## Coverage target

- `internal/planner` (disposition resolution): 95% — pure-function dispatch.
- `internal/config` (the new map + validation): meets the package's existing target.

## Dependencies

- F11 / D-166 (the materializer + `ArtifactStub.Fetch.Tool` hint this re-gates).
- 107c / D-167 (native tool-calling — how the planner drives the `ref`/`tool` path).
- Same wave: **84c** (the `provider_native` mechanism this policy gates).

## Risks / open questions

- **Default drift.** The single biggest risk is silently changing today's behaviour;
  the byte-for-byte default-parity golden test is the guard.
- **Protocol-hint scope.** The per-attachment `disposition` field is an additive
  optional Protocol field (backward-compatible); third-party clients that omit it
  get the agent/runtime default.
- **`tool:<name>` validation timing.** A named tool absent from the catalog at
  resolution time degrades to `ref` + warning rather than failing the run — chosen
  for resilience; revisit if operators prefer a hard error.

## Glossary additions

- **Attachment disposition** — the declared policy (per-attachment / per-agent /
  runtime-default) deciding how an uploaded artifact is handed to the model: `ref`
  (stub + fetch-tool hint), `inline` (DataURL), `provider_native` (84c), or
  `tool:<name>`. Add to `docs/glossary.md`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-session isolation: N/A — disposition is session-scoped via the existing
      identity quadruple; it does not widen the boundary.
- [ ] **Primitive + consumer in the same wave (§13):** the policy primitive ships
      with its consumer — the materializer — AND the same-wave 84c consumes
      `provider_native`. The default `ref` path is exercised end-to-end.
- [ ] Glossary updated (attachment disposition)
- [ ] If a brief finding was departed from: justified above + D-189 filed — yes.
