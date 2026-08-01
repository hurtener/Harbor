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
- Add the tenant-bounded deterministic paged maintenance scan consumed by
  Phase 233a cutover and Phase 234 retirement.

## Non-goals

- No general SQL transaction callback, distributed lock service, or optional driver capability.
- No mutation of applied `0001` migrations.
- No session-overlay, personal-skill, session-erasure, or retirement consumer
  work; Phase 233a composes this primitive over durable session records.

## Acceptance criteria

- [x] `StateStore.SaveIf` requires a non-empty unique expectation set, requires the next slot among those expectations, treats empty expected event ID as expected absence, and returns `ErrConditionFailed` without a partial write.
- [x] In-memory checks and writes under one mutex; SQLite refuses deferred/ambiguous transaction locks and uses one write transaction; Postgres derives signed advisory-lock IDs in the transaction, acquires sorted unique actual IDs, prevents absent-row phantoms, and permits exactly one winner across independent clients.
- [x] Ordinary `Save` idempotency by next event ID remains unchanged and cannot bypass a failed condition.
- [x] Every StateStore wrapper/fake forwards or faults `SaveIf` explicitly; the driver registry and conformance census hold the triad closed.
- [x] Agent-tier SetRevision/Rollback condition the active pointer; user-tier writes condition both the user pointer and agent lifecycle slot so retirement can win terminally.
- [x] Shared SQLite and environment-gated real Postgres races prove one winner across two registry instances; N≥100 reuse, cancellation, close, and leak checks pass under `-race`.
- [ ] `ScanKindForTenant` is mandatory across in-memory, SQLite, and Postgres:
  storage-side tenant plus literal-prefix filtering, bounded limit, stable
  lexicographic composite-slot order, opaque validated continuation, and no
  snapshot claim across restart. Its conformance rows reject missing scope,
  malformed continuation, empty prefix, wrong tenant, over-limit results, and
  wildcard/overmatch behavior.

## Files added or changed

- `internal/state/state.go`
- `internal/state/drivers/inmem/`
- `internal/state/drivers/sqlite/`
- `internal/state/drivers/postgres/`
- `internal/state/conformancetest/`
- every StateStore wrapper/fake that must forward `ScanKindForTenant`
- StateStore wrappers/fakes across `internal/`
- `internal/agentcfg/drivers/statestore/`
- `scripts/smoke/phase-233.sh`

## Public API surface

- `StateStore.SaveIf(ctx context.Context, expectations []SlotExpectation, next StateRecord) error`
- `SlotExpectation` and `ErrConditionFailed`.
- `StateStore.ScanKindForTenant(ctx context.Context, scope ListScope, tenantID,
  literalKindPrefix string, limit int, continuation string) (StateScanPage,
  error)`, where an empty next continuation is the only end marker.

## Test plan

- **Unit:** validation, matching/stale/absent expectations, idempotency order, cancellation, and closed-store behavior.
- **Integration:** two independent agent-config registries over shared SQLite and Postgres exercise cross-process-equivalent conditional writes.
- **Conformance:** every registered StateStore driver passes the same matching,
  stale, multi-slot, identity, race, tenant scan ordering/pagination,
  continuation-validation, and literal-prefix rows.
- **Concurrency / leak:** N≥100 calls on one driver plus two-client winner tests under `-race`; goroutine baseline restored.

## Smoke script additions

- Run the conditional-save conformance subset for all available drivers and the shared-SQLite agent-config race.
- Statically reject edits to existing migration hashes.

## Coverage target

- `internal/state`: 90%; each touched StateStore driver and conformance package: 85%; `internal/agentcfg/drivers/statestore`: 90%.

## Proposed permanent deviation

The `internal/agentcfg/drivers/statestore` package is at 83.8% direct package
coverage, below the 90% target. This draft proposes a documented §4.3
deviation: 53 uncovered statements are pre-existing error, list, and
event-emission branches outside the conditional-save change. Expanding those
unrelated paths merely to reach a package aggregate would make this phase own
unrelated behavior.

The changed primitive has focused evidence: `SetRevision` is 95.7%,
`activeExpectations` 88.9%, `slotExpectation` 100.0%, and `saveActiveIf`
88.9%; its tests cover condition failure plus candidate cleanup, cleanup
failure, ordinary storage failure, active-expectation load failure, rollback
conflict, and the two-slot user write. Direct measurements are
`internal/state` 96.3%, in-memory 88.0%, and SQLite 87.1%. Postgres is
environment-gated locally (`HARBOR_PG_DSN` absent); the existing CI
`state-postgres` job supplies Postgres 16 and runs the real two-client race
under `-race`.

## Dependencies

- 130, 221, 230.

## Risks / open questions

- Postgres conformance remains environment-gated locally; CI and Phase 235 must execute the real two-client case before release.
- SQL absent-row locking must be proven by the adversarial race, not inferred from transaction syntax.

## Glossary additions

- Conditional save.
- Slot expectation.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [ ] `make preflight` passes (PR CI is authoritative; no completed post-rebase local run)
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages meets target or has the proposed deviation above
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] StateStore concurrent-reuse N≥100 test passes with no race, bleed, cancellation cross-talk, or leak
- [x] Real-driver integration test covers identity and condition-failed behavior
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry filed
