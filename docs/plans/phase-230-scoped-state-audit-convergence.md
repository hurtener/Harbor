# Phase 230 — Scoped state and audit convergence

## Summary

Closes #396, #612, and #462: StateStore enumeration is identity-scoped in every driver, granted search widening is audited on all indexes, and a stale erasure ledger converges its own lifecycle before removal.

## RFC anchor

- RFC §6.9.
- RFC §6.11.
- RFC §6.13.
- RFC §9.

## Briefs informing this phase

- brief 05
- brief 06

## Brief findings incorporated

- brief 05 §4: the persistence triad shares one mandatory interface and conformance suite.
- brief 06 §4: cross-scope observation is explicit, server-enforced, and audited.

## Findings I'm departing from (if any)

- None.

## Goals

- Bound agent-config history reads at storage, unify widening audit facts, and make stale-ledger cleanup retry-safe.

## Non-goals

- No `agent_id` isolation filter and no optional StateStore capability.

## Acceptance criteria

- [x] In-memory, SQLite, and Postgres enumerate by mandatory identity and kind prefix inside the driver.
- [x] Agentcfg revision listing requires no maintenance-wide scan or Go-side identity filtering.
- [x] Sessions/tasks/artifacts/events emit exactly one redacted audit fact for each granted widening and none for ordinary/denied reads.
- [x] A mismatched erasure ledger publishes the old lifecycle record before deleting the checkpoint; a publish or cleanup failure retains retryable state and leaves the current lifecycle untouched. Retries are best-effort deduplicated from retained history; when that oracle is unavailable, failed, or outside its bounded window, duplicate compliance records are allowed but a record is never knowingly discarded.
- [x] Cancellation, escaping, restart, identity isolation, and N≥100 concurrent calls pass.

## Files added or changed

- `internal/state/` and all StateStore drivers/conformance
- `internal/agentcfg/`, `internal/search/`, `internal/sessions/`
- `scripts/smoke/phase-230.sh`

## Public API surface

- Mandatory StateStore identity-and-kind-prefix enumeration method.

## Test plan

- **Unit:** widening classification and stale-ledger state machine.
- **Integration:** real StateStore drivers and event/audit pipeline.
- **Conformance:** all persistence drivers filter before returning rows.
- **Concurrency / leak:** N≥100 identity scopes plus cancellation and restart.

## Smoke script additions

- Run scoped enumeration, four-index audit, and stale-ledger convergence tests.

## Coverage target

- Touched state/search/sessions packages do not fall below v1.25 floors.

## Dependencies

- 130, 205, 218, 221.

## Risks / open questions

- Postgres conformance remains environment-gated but its SQL predicate and harness must execute in CI.

## Glossary additions

- None.

## Pre-merge checklist

- [x] Drift, mirror, CI preflight, three-driver conformance, identity, integration, and concurrency gates pass
