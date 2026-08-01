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

- [ ] `make drift-audit`, mirror, CI preflight, coverage, integration, and concurrent-reuse gates pass
