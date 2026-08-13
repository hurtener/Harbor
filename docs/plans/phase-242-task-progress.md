# Phase 242 — Task progress (HA-60)

## Summary

Expose durable task progress as a typed, identity-safe Protocol projection. D-421 is the phase authority: progress reports completed and active step tranches, forwarded artifact handles, virtual-child labels, and resumability without leaking raw outputs.

## RFC anchor

- RFC §6.10
- RFC §6.11
- RFC §7.3
- RFC §5.2

## Briefs informing this phase

- brief 14
- brief 03

## Brief findings incorporated

- brief 03 §8: StateStore is the persistence seam and identity is propagated through every provider call.
- brief 06 §4: bounded projections and server-side identity filtering are mandatory.
- brief 14 §3: `not_found` must be an honest, typed state rather than silent degradation.

## Findings I'm departing from (if any)

- None.

## Goals

- Add D-421's durable bounded task-progress projection and additive optional `TaskRow` fields.
- Align task source projection, StateStore lifecycle/erasure fences, and Protocol mapping.

## Non-goals

- No raw output streaming, unrelated artifact retention, new identity axis, second progress source, or MCP App tool-context retention policy.

## Acceptance criteria

- [ ] `TaskRow` carries additive optional `progress_snapshot`, `virtual_key`, and `virtual_label` fields.
- [ ] Progress is derived from the task source of truth and remains bounded and durable.
- [ ] Identity triple scoping and session-lifecycle/erasure fences prevent cross-session exposure and stale records.
- [ ] Unknown/cross-identity ids return typed not-found without silent degradation.
- [ ] Real durable StateStore integration proves identity propagation, failure mode, and cross-session isolation; erasure removes projections.

## Files added or changed

- `internal/tasks/*`, `internal/state` lifecycle adapters, and Protocol task-row sources
- `test/integration/*` for durable projection, erasure, and identity isolation
- `docs/glossary.md`, `RFC-001-Harbor.md`, `CHANGELOG.md`, `scripts/smoke/phase-242.sh`

## Public API surface

- `TaskRow` gains additive optional `progress_snapshot`, `virtual_key`, and `virtual_label` fields; `ProtocolVersion` remains unchanged. This is not an MCP App tool-context retention surface.

## Test plan

- **Unit:** bounded snapshot mapping, optional fields, unknown/cross-identity miss, and erasure fences.
- **Integration:** real durable StateStore and task projection with identity and forced read failure.
- **Conformance:** in-memory, SQLite, and Postgres lifecycle/erasure behavior where drivers are enabled.
- **Concurrency / leak:** N≥100 shared projection reads/writes under `-race`, with no cross-talk or stale lifecycle records.

## Smoke script additions

- Static assertions for D-421, additive optional TaskRow fields, identity/lifecycle fences, bounded projection, and typed not-found. No script execution in this planning change.

## Coverage target

- `internal/tasks`: 90%; StateStore touched adapters: 90%; integration: 85%.

## Dependencies

- Depends on Phases 239 and 241; independent of Phases 236–238.

## Risks / open questions

- Progress snapshots must remain bounded and derived; do not let them become a second task source of truth.
- Postgres integration remains a web-CI gate where the local environment lacks the required service.

## Validation gate ledger

- **Local skip:** local validation intentionally skipped for this documentation reconciliation.
- **Web CI:** all three StateStore conformance paths, race integration, and Protocol wire lockstep are authoritative.

## Glossary additions

- **Task progress** — a bounded, identity-addressed projection of a task's tranche state and resumability.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] Cross-references resolve
- [ ] Coverage target met
- [ ] Cross-session isolation passes
- [ ] Concurrent-reuse N≥100 under `-race`
- [ ] Real durable-driver integration with identity and failure mode under `-race`
- [ ] Glossary updated
