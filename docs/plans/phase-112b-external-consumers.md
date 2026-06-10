# Phase 112b — External consumers on the facade + the compile gate

## Summary

Converts everything that pretends to be external onto the real `sdk/` facade
(RFC §3.6 / D-204) and lands the standing gate that keeps it true: scaffold
templates emit `sdk/` imports (a tool-declaring scaffold compiles as an
external module — the SDK friction audit's headline external break),
harbortest's parameter vocabulary becomes externally satisfiable through the
aliases, consumer-facing recipes and the README's "runtime library" claim flip
to public paths, and a smoke gate compiles a scaffolded external module on
every preflight.

## RFC anchor

- RFC §3.6 — items 4 (mechanical gating) and 5 (harbortest vocabulary).
- RFC §8 — CLI layer (the scaffold templates this phase rewrites).

## Briefs informing this phase

- brief 06 — events-observability-devx (the first-five-minutes adoption posture
  the scaffold path carries)

## Brief findings incorporated

- **brief 06 §adoption.** The scaffold is the external team's first contact;
  emitting imports that cannot compile is the worst possible first five
  minutes. The compile gate makes the guarantee mechanical.

## Findings I'm departing from (if any)

None. Audit §5 findings (scaffold break verified by reproduction; harbortest
type-poisoning; README misclaim) are the source; recorded in D-204.

## Goals

- Scaffold templates (`cmd/harbor/scaffold/templates/**`) import `sdk/` paths;
  the tool-declaring shape compiles externally.
- `harbortest`: `Deps`, `AssertSequence`, `NewFaultInjector` parameters become
  externally satisfiable via `sdk/` aliases (signatures may stay — the alias
  makes the types nameable; add kit constructors where construction was the
  blocker, e.g. a bus/catalog via `sdk/events`/`sdk/tools`).
- Consumer-facing recipes (`embed-harbor-headless`, `define-a-tool`,
  `use-memory-and-skills-from-go`, `steer-and-resume-a-run`,
  `observe-an-embedded-runtime`) show `sdk/` imports; in-module-only caveats
  removed where no longer true. README's "runtime library" section imports
  `sdk/`, truthfully.
- **The compile gate:** `scripts/smoke/phase-112b.sh` scaffolds a
  tool-declaring agent into a temp dir as an external module (replace
  directive) and `go build`s it. FAIL on compile error. Runs in preflight's
  unit-tests class.

## Non-goals

- No facade additions beyond what conversions flush out (those are 112a
  follow-ups, additive).
- No template feature changes — import paths and nothing else where possible.

## Acceptance criteria

- [ ] `harbor scaffold --from-config` with ≥1 built-in and ≥1 custom tool
      produces an external module that `go build`s (the gate proves it).
- [ ] phase-67 smoke extended (or superseded by the 112b gate) to cover the
      tool-declaring shape — the audit's CI blind spot closed.
- [ ] harbortest external-usability: a compile probe as an external module
      constructs Deps (bus via sdk), calls AssertSequence with sdk-typed
      events, and builds a FaultInjector — asserted in the smoke.
- [ ] Recipes + README flipped; no consumer-facing doc instructs an
      `internal/` import (grep-asserted).
- [ ] D-206 authored; master-plan row flipped; §18 check: any SKILL.md naming
      scaffold output paths updated same-PR.

## Files added or changed

- `cmd/harbor/scaffold/templates/**` (+ golden test updates).
- `harbortest/*.go` (vocabulary/constructors), `harbortest/doc.go`.
- `docs/recipes/*.md` (the five consumer-facing recipes), `README.md`.
- `scripts/smoke/phase-112b.sh` — the external compile gate.
- `docs/decisions.md` — D-206.

## Public API surface

- The scaffold output contract (an external module that compiles).
- harbortest's externally-satisfiable vocabulary (via `sdk/` aliases).

## Test plan

- **Golden:** scaffold template goldens regenerate; the tool-declaring golden
  gains a compile assertion.
- **Integration:** the external-module probe (temp dir, replace directive,
  `go build`; one with tools, one harbortest-consumer shape).
- **Failure mode:** a deliberately-broken template fixture fails the gate
  loudly (gate self-test).

## Smoke script additions

`scripts/smoke/phase-112b.sh`: the external compile gate (scaffold → build),
the harbortest probe build, the no-internal-imports doc greps.

## Coverage target

- Touched packages meet existing targets; the gate's value is binary
  (compiles/doesn't) rather than coverage-shaped.

## Dependencies

- 112a (the facade the consumers convert onto). Same wave per §13.

## Risks / open questions

- **Gate runtime cost:** an external `go build` per preflight (~seconds with
  module cache; measured and bounded in the script).
- **Template goldens churn:** import-path-only diffs; the goldens pin that
  nothing else changed.

## Glossary additions

- **External compile gate** — the standing smoke that scaffolds a
  tool-declaring agent as an external module and compiles it against the
  `sdk/` facade (RFC §3.6 item 4).

## Pre-merge checklist

- [ ] `make drift-audit` / `make preflight` / `make check-mirror` pass
- [ ] All cross-references resolve
- [ ] Golden + gate green
- [ ] Cross-session isolation: N/A
- [ ] **Primitive + consumer same wave (§13):** this phase IS 112a's consumer.
- [ ] Glossary updated; D-206 filed at ship; §18 swept
