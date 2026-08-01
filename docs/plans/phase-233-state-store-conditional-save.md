# Phase 233 — StateStore conditional save

## Summary

Add one mandatory multi-slot conditional-save primitive to the StateStore interface and implement it atomically on the in-memory, SQLite, and Postgres drivers. This closes the process-local bound of expected agent-config writes and supplies the durable linearization primitive consumed by Phase 234 retirement.

## RFC anchor

- RFC §6.11.
- RFC §6.16.
- RFC §9.

## Briefs informing this phase

- brief 05
- brief 07

## Brief findings incorporated

- brief 05 §4: the persistence triad implements one mandatory interface with one conformance suite.
- brief 05 §10: durability, idempotency, cancellation, and identity isolation are semantic contracts, not driver accidents.
- brief 07 §2: one runtime primitive must serve every consumer rather than a Postgres-only transaction path.

## Findings I'm departing from (if any)

- None.

## Goals

- Atomically compare exact current event IDs for one or more identity-scoped slots and write one next record.
- Give absence a precise expectation and return one comparable condition-failed sentinel.
- Upgrade agent-config conditional agent/user writes to use the durable primitive.

## Non-goals

- No general SQL transaction callback, distributed lock service, or optional driver capability.
- No mutation of applied `0001` migrations.

## Acceptance criteria

- [ ] `StateStore.SaveIf` requires a non-empty unique expectation set, requires the next slot among those expectations, treats empty expected event ID as expected absence, and returns `ErrConditionFailed` without a partial write.
- [ ] In-memory checks and writes under one mutex; SQLite uses one write transaction; Postgres prevents absent-row phantoms and permits exactly one winner across independent clients.
- [ ] Ordinary `Save` idempotency by next event ID remains unchanged and cannot bypass a failed condition.
- [ ] Every StateStore wrapper/fake forwards or faults `SaveIf` explicitly; the driver registry and conformance census hold the triad closed.
- [ ] Agent-tier SetRevision/Rollback condition the active pointer; user-tier writes condition both the user pointer and agent lifecycle slot so retirement can win terminally.
- [ ] Shared SQLite and environment-gated real Postgres races prove one winner across two registry instances; N≥100 reuse, cancellation, close, and leak checks pass under `-race`.

## Files added or changed

- `internal/state/state.go`
- `internal/state/drivers/inmem/`
- `internal/state/drivers/sqlite/`
- `internal/state/drivers/postgres/`
- `internal/state/conformancetest/`
- StateStore wrappers/fakes across `internal/`
- `internal/agentcfg/drivers/statestore/`
- `scripts/smoke/phase-233.sh`

## Public API surface

- `StateStore.SaveIf(ctx context.Context, expectations []SlotExpectation, next StateRecord) error`
- `SlotExpectation` and `ErrConditionFailed`.

## Test plan

- **Unit:** validation, matching/stale/absent expectations, idempotency order, cancellation, and closed-store behavior.
- **Integration:** two independent agent-config registries over shared SQLite and Postgres exercise cross-process-equivalent conditional writes.
- **Conformance:** every registered StateStore driver passes the same matching, stale, multi-slot, identity, and race rows.
- **Concurrency / leak:** N≥100 calls on one driver plus two-client winner tests under `-race`; goroutine baseline restored.

## Smoke script additions

- Run the conditional-save conformance subset for all available drivers and the shared-SQLite agent-config race.
- Statically reject edits to existing migration hashes.

## Coverage target

- `internal/state`: 90%; each touched StateStore driver and conformance package: 85%; `internal/agentcfg/drivers/statestore`: 90%.

## Dependencies

- 130, 221, 230.

## Risks / open questions

- Postgres conformance remains environment-gated locally; CI and Phase 235 must execute the real two-client case before release.
- SQL absent-row locking must be proven by the adversarial race, not inferred from transaction syntax.

## Glossary additions

- Conditional save.
- Slot expectation.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] StateStore concurrent-reuse N≥100 test passes with no race, bleed, cancellation cross-talk, or leak
- [ ] Real-driver integration test covers identity and condition-failed behavior
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
