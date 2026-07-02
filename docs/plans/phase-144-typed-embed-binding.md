# Phase 144 — Typed embed binding: `RunTyped[T]` + the shared schema-derivation home

## Summary

Ships the typed sugar over Phase 143's mechanism: a generic `assemble.RunTyped[T any](ctx, stack, goal, id, opts...) (T, planner.AnswerEnvelope, error)` that derives the output JSON Schema from `T` (the same reflection machinery `inproc.RegisterFunc[I,O]` uses, promoted to a neutral shared package), calls `RunOnce` with `WithOutputSchema`, and unmarshals the validated `answer_payload` into `T`. The `sdk/` forward is the SECOND documented generic-func carve-out on the alias-only facade — Go cannot express a generic function as a `var` forward — amending D-205's one-carve-out language via D-273 (same rationale, same smoke-gated allow-list discipline). The binding is deliberately NOT named `Agent` (D-273): that noun is taken twice in Harbor's vocabulary (`harbortest.Agent`, the Agent Registry's registration entities).

## RFC anchor

- RFC §3.6 (the public SDK facade — the carve-out pattern this extends)
- RFC §6.2 ("schema mode" as a runtime-level run option — consumed via Phase 143)
- RFC §6.4 (tool catalog — the schema-derivation machinery's original home)

## Briefs informing this phase

- brief 03
- brief 07

## Brief findings incorporated

- brief 03 §"Phase impact" T-1: "in-process registration via generics + reflection; JSON-Schema derivation" — the derivation from Go types is a settled, shipped pattern (`tools.RegisterFunc(name, fn, opts...)` as "the minimum-expression API"). `RunTyped[T]` extends the same minimum-expression posture to the run's terminal answer: one generic call, schema derived, result typed.
- brief 07 §1/§6 (the elegance principle): the runtime — not the caller — owns the protocol details; the caller states intent in Go types. A typed binding that forces the embedder to hand-author a JSON Schema for a type Go already describes would violate the principle the tool path already honors.
- brief 03 §"Two parallel LLM modes (the toggle smell)": one implementation, deepened — `RunTyped` is a THIN wrapper over `WithOutputSchema` + `RunOnce`; it adds no second output path, no second validation loop, no second construction site (`runctx.NewRunContext` stays "the ONE factory").

## Findings I'm departing from (if any)

None.

## Goals

- The embed adopter path's typed "first ten minutes": `answer, env, err := assemble.RunTyped[Report](ctx, stack, goal, id)` — schema derived from `Report`, generation steered and validated by Phase 143's mechanism, result unmarshaled into `Report`. Parity with the `output_type` ergonomics adopters compare Harbor against, without forking any mechanism.
- **One schema-derivation implementation, §13-compliant home.** The derivation currently lives inside the inproc tool driver (`internal/tools/drivers/inproc`), and §13 forbids importing a concrete driver from anywhere but the prod aggregator / `cmd/harbor` / that driver's tests. This phase PROMOTES the derivation (+ its unsupported-type rejection: interfaces, channels, func fields, cycles) into a neutral internal package (`internal/tools/schema`), with the inproc driver re-based on it — one implementation, two consumers, zero duplication.
- **The facade stays honest.** `sdk/assemble.RunTyped` is a thin generic forward whose body adds no behavior (derive → option → `RunOnce` → unmarshal live internally); D-273 amends D-205 item 1 from "exactly ONE func" to an enumerated two-func allow-list, and the no-behavior smoke gate is updated to pin EXACTLY that list — the gate stays mechanical, the curation stays deliberate.
- Immutability preserved (D-025): `RunTyped` is a free function over the shared immutable `Stack`; identity stays a per-call argument; the derived schema is computed per call (or memoized behind an internally-synchronized cache keyed by `reflect.Type` — an implementation choice that must satisfy the concurrent-reuse test either way). No new stateful binding object is introduced in this phase.
- Fail-loud: an unsupported `T` (interface fields, channels, cycles) → the derivation's typed error at CALL time, before any LLM spend; a payload that validates against the schema but fails `json.Unmarshal` into `T` (a derivation/unmarshal mismatch — "impossible by construction" territory) → a loud typed error, never a zero-value `T` with nil error.

## Non-goals

- **A stateful `Agent`-style binding object** (bound instructions + tools + model settings as a struct). The bind-once surface already exists (`config` + `Assemble` → `Stack`); a second binding object is §13 parallel-implementation territory. If a future phase proposes one, it is a NEW decisions entry against this one — not an extension of this phase.
- The name `Agent` for anything shipped here — settled in D-273 (collides with `harbortest.Agent` and the Agent Registry entity vocabulary; `agent_id` is not an isolation principal, D-059).
- Partial-object streaming of the typed payload (named follow-up per D-272).
- Exporting the schema-derivation internals on the facade (`internal/tools/schema` stays internal; `sdk/tools/inproc`'s "Schema-derivation internals are deliberately private" posture is unchanged — `RunTyped` consumes it internally, exactly as `RegisterFunc` does).
- Any Protocol, wire-type, or config-schema change.

## Acceptance criteria

- [x] `internal/tools/schema` exists as the neutral derivation home; `internal/tools/drivers/inproc` is re-based on it with byte-identical derived schemas (a golden test pins a representative corpus before/after); no package outside the driver's sanctioned importers gains a concrete-driver import (§13 grep-asserted in the smoke). (The promotion surfaced a PRE-EXISTING §13 violation — `internal/runtime/flow` imported the concrete inproc driver directly for `DeriveSchema` — fixed in this same PR per §17.6.)
- [x] `assemble.RunTyped[T any](ctx, stack, goal, id, opts...) (T, planner.AnswerEnvelope, error)` derives the schema from `T`, appends `WithOutputSchema` to the caller's opts (a caller-supplied `WithOutputSchema` alongside `RunTyped` is a loud conflict error, not a silent override), executes `RunOnce`, and unmarshals `answer_payload` into `T`.
- [x] Unsupported `T` → the derivation's typed error before any run starts (asserted for interface/channel/cyclic fixtures). Validated-but-unmarshalable payload → loud typed error, never zero-value-with-nil.
- [x] `sdk/assemble.RunTyped` ships as a thin generic forward; the sdk no-behavior smoke's func allow-list is updated to exactly {`sdk/tools/inproc.RegisterFunc`, `sdk/assemble.RunTyped`} and FAILS on any third func; D-273 (amending D-205 item 1) flips to shipped wording in the same PR.
- [x] D-025 concurrent-reuse: N≥100 concurrent `RunTyped` invocations against ONE shared Stack under `-race`, mixing ≥3 distinct `T` types, asserting no schema/payload bleed across runs and goroutine baseline restored (this also gates the memoization choice, if taken). (Per-call derivation shipped — no reflect.Type-keyed cache; the plan's documented default absent a benchmark.)
- [x] §13 primitive-with-consumer: `examples/embed-runonce` gains a `RunTyped` variant compiled+run by the existing external-module compile gate (`scripts/smoke/phase-112b.sh` leg 6); a new `sdk/assemble` `Example_runTyped` renders on pkg.go.dev (offline, deterministic, mirroring the Phase 134 pattern). (The scaffold's `minimal-react` template has no RunOnce/typed-output surface at all — it is a tool-registration template, not an embed-runner template — so the checked-in example is the sanctioned consumer site; documented in the PR.)
- [x] §18 sweep: `docs/recipes/embed-harbor-headless.md` gains the typed step; no `docs/skills/` playbook demonstrates `RunOnce`/`assemble.*` (grepped at implementation time — exempt per §18).
- [x] `scripts/smoke/phase-144.sh` flips from skeleton to real assertions (unit-test leg + the two-func facade allow-list grep + the §13 concrete-driver-import grep).

## Files added or changed

- `internal/tools/schema/` — the promoted derivation package (+ golden corpus test)
- `internal/tools/drivers/inproc/` — re-based on the shared package
- `internal/runtime/assemble/runtyped.go` (+ `runtyped_test.go`, concurrent test)
- `sdk/assemble/` — the generic forward (second documented carve-out)
- `scripts/smoke/phase-112a.sh` or the facade no-behavior gate's home — allow-list update
- `examples/embed-runonce/` and/or `cmd/harbor/scaffold/templates/` — the §13 consumer
- `docs/recipes/embed-harbor-headless.md`, `docs/skills/` (per §18 sweep)
- `test/integration/phase144_runtyped_test.go`
- `scripts/smoke/phase-144.sh`
- `docs/glossary.md`, `docs/decisions.md` (D-273 flip to shipped), `docs/plans/README.md` (status flip)

## Public API surface

- `assemble.RunTyped[T any](ctx context.Context, s *Stack, goal string, id identity.Identity, opts ...RunOption) (T, planner.AnswerEnvelope, error)` (forwarded at `sdk/assemble`)
- (Internal but load-bearing for later phases: `internal/tools/schema.Derive` — the single derivation implementation both the inproc driver and `RunTyped` consume.)

## Test plan

- **Unit:** derivation-parity golden corpus (pre/post promotion, byte-identical); `RunTyped` happy path over a scripted LLM; unsupported-`T` call-time rejection table; the `WithOutputSchema`-conflict loud error; unmarshal-mismatch loud error.
- **Integration:** `test/integration/phase144_runtyped_test.go` — `RunTyped` through the real assembled stack (scripted-LLM devstack, real inmem drivers), identity propagated, one failure mode (schema-invalid after retries → `ErrOutputInvalid` surfaces through the typed call), `-race`; plus the external-module compile-gate leg on the scaffold/example consumer.
- **Conformance:** N/A — no new driver seam.
- **Concurrency / leak:** the mixed-`T` N≥100 shared-Stack test above.

## Smoke script additions

- `scripts/smoke/phase-144.sh` (`PREFLIGHT_REQUIRES: unit-tests`): `go test -race` for `internal/tools/schema` + `internal/runtime/assemble` (RunTyped tests) + the phase-144 integration test; static greps for (a) the exact two-func facade allow-list and (b) no new concrete-driver imports outside the §13 sanctioned set. Skeleton parks with `skip` until the surface lands.

## Coverage target

- `internal/tools/schema`: 85%
- `internal/runtime/assemble` (touched lines): no regression below the package's post-143 coverage
- `internal/tools/drivers/inproc` (touched lines): no regression below current package coverage

## Dependencies

- 143 (the `WithOutputSchema` mechanism + `answer_payload` — this phase is pure sugar over it)
- 26 (the inproc schema-derivation machinery being promoted, D-024)
- 112a (the facade + the one-carve-out precedent being amended, D-205)

## Risks / open questions

- **The D-205 amendment is the load-bearing review item.** If reviewers reject a second func under `sdk/`, the fallback that still ships v1.9 whole is documented here: 143 alone delivers typed output with two caller-side lines (`WithOutputSchema(schema)` + `json.Unmarshal(env.AnswerPayload, &out)`); this phase slips without blocking the wave. The amendment's case: identical rationale to the first carve-out (Go has no generic function values), an enumerated mechanical allow-list (the gate stays a grep), and the wrapper body living internally.
- **Memoization vs. per-call derivation.** Per-call reflection on every `RunTyped` is measurable on hot embed paths; a `reflect.Type`-keyed internally-synchronized cache is the likely shape but is package-level mutable state — permissible only under the §5 "write-once/internally synchronized + documented" carve-outs, and it must pass the mixed-`T` race test. Decide at implementation; default to per-call if the benchmark is a wash.
- **Naming gravity.** Adopter-facing docs will be tempted to describe `RunTyped` as "the Agent object." The recipe and skill wording should present it as what it is — a typed run call over the assembled stack — so the D-273 naming decision holds in prose, not just in code.

## Glossary additions

- **`RunTyped`** — the generic typed embed binding: derives a run output schema from a Go type, executes a schema-constrained `RunOnce`, and returns the validated payload unmarshaled into that type. The second documented generic-func carve-out on the alias-only SDK facade (D-273).

(Added to `docs/glossary.md` in the same PR as this plan.)

## Pre-merge checklist

- [x] `make drift-audit` passes
- [ ] `make preflight` passes (CI runs the full gate; skipped locally per PR note — no GHA access in the worktree)
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes (N/A — no identity-scoped storage change; the E2E's identity-propagation leg covers the seam this phase touches)
- [x] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. (The shared derivation package + the shared-Stack call are reusable surfaces — the mixed-`T` test is mandatory.)
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. (It consumes 143 + 26 + 112a — the integration test is mandatory.)
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry filed (N/A — no brief finding departed from; the pre-existing §13 flow-engine violation fixed in this PR is documented in D-273's implementation note, not a brief departure)
