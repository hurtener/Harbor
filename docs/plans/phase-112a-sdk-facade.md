# Phase 112a — The public SDK facade (`sdk/`)

## Summary

Implements RFC §3.6 / D-204: a new top-level `sdk/` package tree of alias-based
re-exports making the runtime importable by external modules. `internal/` stays
the implementation home; each `sdk/<area>` package re-exports the curated public
surface via type aliases, re-exported constants/sentinels, and thin forwards.
The facade is the external API-stability contract: re-exported = supported.

## RFC anchor

- RFC §3.6 — the settled facade design (the alias posture, the inventory, the
  curation contract).
- RFC §1 — the "ships as a Go module" claim this phase makes true externally.

## Briefs informing this phase

- brief 06 — events-observability-devx (the embedding/devx posture; the Phase 71
  `harbortest` top-level precedent this phase generalizes)

## Brief findings incorporated

- **brief 06 §devx.** The test kit went top-level precisely because external
  consumers cannot import `internal/`; the facade applies the same reasoning to
  the runtime surface itself, with curation instead of wholesale exposure.

## Findings I'm departing from (if any)

None. The SDK friction audit §5 (`docs/notes/sdk-friction-audit.md`) is the
findings source; D-204 records the alias-over-move choice.

## Goals

- The `sdk/` tree per RFC §3.6's inventory: `identity`, `events`, `config`,
  `tools` (+ `tools/inproc`, `tools/builtin`), `llm`, `memory`, `state`,
  `artifacts`, `skills`, `planner` (+ `planner/react`, `planner/deterministic`
  registration import paths), `tasks`, `steering`, `dispatch`, `runctx`,
  `assemble`, `drivers/prod`.
- Each package: type aliases for the exported types, re-exported sentinels/
  constants, thin forwards for constructors/factories (`var Open = llm.Open`
  or wrapper funcs where signatures need no adaptation), package godoc naming
  the internal home and the curation contract.
- A facade-integrity test: every re-export resolves and a representative
  assembly compiles against `sdk/` imports ONLY (the headless recipe's path
  expressed through the facade).
- CLAUDE.md/AGENTS.md §3 layout amendment adding `sdk/` (mirror-gated).

## Non-goals

- No package moves out of `internal/` (D-204's settled alias-over-move call).
- No external-consumer conversions (templates/harbortest/recipes) — Phase 112b.
- No new mechanism of any kind; a facade line that needs adaptation logic is a
  smell — fix the internal surface instead.

## Acceptance criteria

- [ ] Every RFC §3.6 inventory package exists under `sdk/` with godoc + the
      curated re-export set.
- [ ] Facade-integrity test compiles a representative headless assembly using
      ONLY `sdk/` imports (no direct `internal/` imports in the test file —
      grep-asserted in the smoke).
- [ ] `sdk/drivers/prod` blank-import seats everything `internal/drivers/prod`
      seats (parity-asserted).
- [ ] No `sdk` package adds behavior: forwards only (reviewed; smoke greps for
      func bodies beyond returns/forwards are advisory).
- [ ] AGENTS.md + CLAUDE.md §3 amended identically (`make check-mirror`).
- [ ] D-205 authored; master-plan row flipped.

## Files added or changed

- `sdk/<area>/*.go` — the facade packages (one file per area typical).
- `sdk/doc.go` — the tree-level godoc (the contract statement).
- `test/integration/phase112a_sdk_facade_test.go` — the integrity/assembly test.
- `AGENTS.md` + `CLAUDE.md` §3 — the `sdk/` layout entry.
- `scripts/smoke/phase-112a.sh` — real assertions.
- `docs/decisions.md` — D-205. `docs/glossary.md` — "SDK facade".

## Public API surface

The entire phase IS public API surface: the `sdk/` tree per RFC §3.6. This is
the first Harbor surface that is public in the Go-visibility sense — external
modules can import it. Stability contract per RFC §3.6 item 2.

## Test plan

- **Unit/integrity:** per-package re-export resolution (compile-time); alias
  identity spot-checks (a value produced via `internal/` flows through an
  `sdk/` alias and back).
- **Integration:** the facade-expressed headless assembly (Defaults →
  ValidateCore → prod import → Assemble → RunLoop → AnswerEnvelope → Close)
  with identity propagation and one failure mode (missing LLM config fails
  loud through the facade).
- **Concurrency:** N/A new mechanism; the assembly test reuses the existing
  D-025-gated paths.

## Smoke script additions

`scripts/smoke/phase-112a.sh` (unit-tests class): the integrity test slice
under `-race`; greps: every inventory package present; the integration test
imports no `internal/` packages; `sdk/drivers/prod` parity gate.

## Coverage target

- `sdk/` packages: coverage is trivially high (forwards); the binding target is
  the integrity test's completeness — every exported name exercised ≥ 95%.

## Dependencies

- 110a–110d (the re-homed surfaces the facade exposes), 111a–f (the consumers
  that prove them). D-204 (this wave's decision).

## Risks / open questions

- **Alias limits:** Go aliases cover types; methods on internal types are
  reachable via the alias automatically. Generic functions and identifiers
  needing signature adaptation are forwarded as wrappers — each is a review
  point (no behavior).
- **Curation misses:** an omitted-but-needed re-export is additive to fix;
  112b's external conversions are the immediate consumer that flushes these.

## Glossary additions

- **SDK facade (`sdk/`)** — the curated, alias-based public re-export tree
  making the runtime importable by external modules (RFC §3.6, D-204).

## Pre-merge checklist

- [ ] `make drift-audit` / `make preflight` / `make check-mirror` pass
- [ ] All cross-references resolve
- [ ] Coverage/integrity targets met
- [ ] Cross-session isolation: N/A (no behavior)
- [ ] **Primitive + consumer same wave (§13):** 112b (same wave) converts the
      external consumers; the in-phase integrity test is the first consumer.
- [ ] Glossary updated; D-205 filed at ship
