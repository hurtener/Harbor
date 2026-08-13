# Phase 239 — Same-run step tranche resume (HA-57)

## Summary

Persist finite same-run step-tranche receipts and resume the original live run at its last committed boundary. Resume preserves the identity quadruple, does not replay completed work, and never creates a replacement run. D-418 is the phase authority; D-417 remains the bounded restart-unavailable boundary when the original run is not live.

## RFC anchor

- RFC §3.3
- RFC §6.8
- RFC §6.11
- RFC §7
- RFC §5.2

## Briefs informing this phase

- brief 03
- brief 06
- brief 07

## Brief findings incorporated

- brief 06 §1: the event bus is the canonical projection, not a parallel telemetry channel.
- brief 06 §4: server-side identity filtering and bounded payloads are mandatory.
- brief 07 §5: outcome identity and dispatch accounting stay in runtime orchestration.

## Findings I'm departing from (if any)

- None.

## Goals

- Add finite, durable, typed step-tranche receipts and same-run resume.
- Preserve `(tenant,user,session,run)`, run/task keys, and cancellation semantics.
- Resume the original live run idempotently from each committed boundary.

### Bounded restart contract

The current architecture cannot safely relaunch a frozen run in this phase.
 The durable receipt contains the trajectory and selector, but no trusted
completion boundary or frozen planner/executor/run-context factory. `RunLoop.Run`
receives those dependencies through its live `RunSpec`; rebuilding them from
mutable current profile or catalog state could alter the frozen run. A fresh
process therefore retains the receipt for inspection and returns typed
`ErrRestartUnavailable` rather than creating a new run or falling back to
current configuration.

## Non-goals

- No second pause/resume mechanism, replay of completed steps, raw output duplication, caller-selected run identity, tool-failure events, or tool-failure classifier surface.

## Acceptance criteria

- [ ] Resume continues from the last committed tranche without replaying completed steps.
- [ ] Cancellation between tranches continues from the last committed boundary deterministically while the original run loop remains live; a fresh process returns typed `ErrRestartUnavailable` because no trusted frozen-run relaunch boundary exists.
- [ ] Repeated resume is idempotent; a stale checkpoint fails loudly.
- [ ] Identity `(tenant,user,session,run)` and authorization are checked at resume.
- [ ] Protocol progress is bounded and carries no raw arguments/results.

## Files added or changed

- `internal/runtime/*`, `internal/tasks/*`, and Protocol task-control sources
- `test/integration/*` for live-run receipt/resume and identity boundaries
- `docs/glossary.md`, `RFC-001-Harbor.md`, `CHANGELOG.md`
- `scripts/smoke/phase-239.sh`

## Public API surface

- Additive tranche checkpoint/progress fields on the task control surface; Protocol version remains 0.1.0.

## Test plan

- **Unit:** receipt encoding, tranche boundaries, idempotence, stale receipt, and restart-unavailable behavior.
- **Integration:** original live run continuation with identity authorization and a fresh-process failure boundary.
- **Concurrency / leak:** N=128 same-run receipts with identity isolation and joined run lifecycles.

## Smoke script additions

- Static assertions for finite receipts, original-live-run wording, identity quadruple, idempotence, typed restart errors, and Protocol lockstep.

## Coverage target

- `internal/runtime`: 90%; `internal/tasks`: 90%; Protocol task-control adapters: 90%; integration: 80%.

## Dependencies

- Depends on Phases 176, 193, and 233; independent of Phases 236, 237, 238, 240, 241, and 242.

## Risks / open questions

- Event consumers may assume old JSON shape; optional fields and absent semantics must remain backward-compatible.
- Generated Protocol reference pages are explicitly out of scope for this planning track; the shipping implementation must regenerate them.

## Validation gate ledger

- **Local skip:** local validation intentionally skipped for this documentation reconciliation.
- **Web CI:** Protocol lockstep, generated-reference regeneration, race integration, and live-run receipt/resume gates remain authoritative.

## Glossary additions

- **Step tranche** — the ordered, atomically checkpointed slice of a run between two resumable boundaries.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] Cross-references resolve
- [ ] Coverage target met
- [ ] Cross-session isolation passes
- [ ] Concurrent-reuse N≥100 under `-race`
- [ ] Real-driver integration with identity and failure mode under `-race`
- [ ] Glossary and operator skill updated
