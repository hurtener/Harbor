# Phase 227 — Declared tool-name resolution

## Summary

Closes #654 by giving every model-authored tool name one shared declaration projection and reverse resolver. Catalog keys remain internal; collision losers cannot be reached through their raw names.

## RFC anchor

- RFC §6.2.
- RFC §6.4.

## Briefs informing this phase

- brief 03
- brief 07

## Brief findings incorporated

- brief 03 §1: planners see a transport-neutral tool view while dispatch retains internal descriptors.
- brief 07 §8: tool resolution and dispatch are runtime mechanisms shared by planner concretes.

## Findings I'm departing from (if any)

- None.

## Goals

- Make declared name generation and reverse resolution one immutable tools-layer mechanism, frozen from one catalog snapshot for each provider turn.

## Non-goals

- No catalog-key rename and no provider-native tool-name policy.

## Acceptance criteria

- [x] ReAct and builtins consume the same declaration table.
- [x] Declared names resolve only to their winning catalog key; unknown names fail loudly.
- [x] The exact projection sent in `req.Tools` survives catalog mutation during `Complete`; response resolution never rebuilds from the mutable catalog.
- [x] The `clock.now`/`clock_now` collision dispatches the declared winner and announces the loser.
- [x] Length/shortening guarantees and N≥100 concurrent reuse remain deterministic.

## Files added or changed

- `internal/tools/`
- `internal/planner/react/`, `internal/tools/builtin/`
- `scripts/smoke/phase-227.sh`

## Public API surface

- Internal immutable per-turn declaration projection: catalog-key to declared-name and declared-name to winner.

## Test plan

- **Unit:** sanitization, shortening, collisions, unknown names.
- **Integration:** planner declaration through builtin dispatch to the real catalog.
  Real builtin `tool_search` results are harvested through the trajectory into
  the next ReAct declaration, including shortened aliases and loading overrides.
  Discovery resolves through current scope/exclusion filters and the shared name
  projection; retained canonical names remain compatible without retargeting a
  collision owner. Discovery alone invokes no domain handler.
- **Conformance:** all model-authored builtin resolution paths share the projection.
- **Concurrency / leak:** N≥100 projections/resolutions on one shared catalog.
- **Performance:** `BenchmarkReActPlanner_NextStep_DeclaredTool` measures the
  strict real-catalog projection, while `BenchmarkReActPlanner_NextStep_ToolFree`
  keeps the no-catalog terminal path separate. The renamed identities reset the
  baseline because the former benchmark accepted a raw undeclared tool name and
  therefore measured a different, now-forbidden workload.

## Smoke script additions

- Run the collision integration and raw-key mutation guard.

## Coverage target

- Touched tools/planner packages do not fall below their v1.25 floors.

## Dependencies

- Phase 220.

## Risks / open questions

- Any second name renderer or resolver is a release-blocking drift regression.

## Glossary additions

- Declared tool name.

## Pre-merge checklist

- [x] Original phase delivery: `make drift-audit`, mirror, CI preflight,
      coverage, integration, and concurrent-reuse gates passed. The correction's
      qualification is recorded separately below.

## Discovery correction qualification (2026-10-17)

The final discovery correction is
`4a6ae0fd843d991d1d409619d9cfa02d7a94587d`. Focused race tests, scoped
lint/vet/build, Phase 227 smoke (6 OK, zero SKIP, zero FAIL), and the external
current-source discovery reproduction passed. Its independent narrow review
confirmed legacy canonical discovery through wrappers, current-source shortened
alias declarations, production collision refusal, and disabled/paused denial,
with no P0, P1, or P2 findings. Discovery alone executes no domain handler.

The earlier broad gate associated with
`bf90083ab8329da159e2341a4c60d9c091768cd8` was incomplete; it is not a full
preflight pass on either discovery checkpoint. The subsequent combined runtime
`9a324f0544591d432c6f786b95c7c752a5dd2bda` has separately recorded
[full-gate and narrow-retry evidence](phase-83l-real-bifrost-tests.md#scoped-release-qualification-2026-10-17),
including its failed initial full gate and remaining qualification limits.
