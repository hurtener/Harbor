# Phase 239 — Same-run step tranche resume (HA-57)

## Summary

Resume a paused or interrupted run at the next same-run step tranche while the original run loop remains available. Persist the tranche boundary and continuation state so in-process resume does not replay completed steps or lose identity. D-418 is the phase authority; D-417 remains the bounded restart-unavailable subdecision.

## RFC anchor

- RFC §6.4
- RFC §6.13
- RFC §6.15
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

- Add a durable, typed tranche checkpoint and same-run resume continuation.
- Preserve identity, run/task keys, completed-step results by reference, and cancellation semantics.
- Same-run resume is idempotent at each step tranche.

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

- No second pause/resume mechanism, replay of completed steps, raw output duplication, or caller-selected run identity.

## Acceptance criteria

- [ ] Resume continues from the last committed tranche without replaying completed steps.
- [ ] Cancellation between tranches continues from the last committed boundary deterministically while the original run loop remains live; a fresh process returns typed `ErrRestartUnavailable` because no trusted frozen-run relaunch boundary exists.
- [ ] Repeated resume is idempotent; a stale checkpoint fails loudly.
- [ ] Identity `(tenant,user,session,run)` and authorization are checked at resume.
- [ ] Protocol progress is bounded and carries no raw arguments/results.

## Files added or changed

- `internal/tools/policy.go`, dispatch/event emitters, `internal/events/*`
- `internal/protocol/types/{events,tools}.go` and lockstep sources if needed
- `test/integration/tool_failure_events_test.go`
- `docs/skills/use-the-harbor-protocol/SKILL.md`, `docs/glossary.md`, `RFC-001-Harbor.md`, `CHANGELOG.md`
- `scripts/smoke/phase-239.sh`

## Public API surface

- Additive tranche checkpoint/progress fields on the task control surface; Protocol version remains 0.1.0.

## Test plan

- **Unit:** event payload encoding, absent/unclassified, attempt/budget propagation, redaction.
- **Integration:** real MCP driver → policy → event bus → Protocol stream, including deterministic failure.
- **Conformance:** registered drivers emit equivalent metadata for equivalent classifications.
- **Concurrency / leak:** N=128 mixed classifications on one shared shell, identity isolation and joined subscriptions.

## Smoke script additions

- Static assertions for same-run wording, tranche checkpoints, idempotence, typed resume errors, and Protocol lockstep.

## Coverage target

- `internal/tools`: 90%; `internal/events`: 85%; Protocol event adapters: 90%; integration: 80%.

## Dependencies

- Depends only on Phase 236; independent of Phases 237, 238, 240, 241, and 242.

## Risks / open questions

- Event consumers may assume old JSON shape; optional fields and absent semantics must remain backward-compatible.
- Generated Protocol reference pages are explicitly out of scope for this planning track; the shipping implementation must regenerate them.

## Validation gate ledger

- **Local skip:** none for unit, event, redaction, or in-process integration coverage; only an explicitly live external provider probe may skip.
- **Web CI:** Protocol lockstep, generated-reference regeneration, race integration, and event redaction gates are required.

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
