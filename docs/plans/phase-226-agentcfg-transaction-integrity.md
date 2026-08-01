# Phase 226 — Agent-config transaction integrity

## Summary

Supersedes the affected Phase 221 write ordering: conditional skill mutations are side-effect free on refusal, and ambiguous revision-record saves are cleaned only when an exact point-read proves the record orphaned.

## RFC anchor

- RFC §6.7.
- RFC §6.11.
- RFC §6.16.
- RFC §9.

## Briefs informing this phase

- brief 04
- brief 05

## Brief findings incorporated

- brief 04: skill state remains scoped and versioned rather than global mutable state.
- brief 05 §4: all persistence drivers implement one mandatory contract and share conformance tests.

## Findings I'm departing from (if any)

- Phase 221's acceptance tests treated revision refusal as sufficient; this phase supersedes that claim by covering SkillStore effects before the revision write.

## Goals

- Make each conditional skill mutation coordinated and compensating across SkillStore and agent-config persistence.
- Handle commit-then-error without deleting on an unknown answer.

## Non-goals

- No claim of cross-process ACID or StateStore CAS.

## Acceptance criteria

- [x] All four admin/user skill doors check the expected hash under the owner lock before body mutation.
- [x] Later revision failure restores the exact prior body or deletes only the body created by that call.
- [x] Conflict paths change no body, revision, pointer, or success event.
- [x] Ambiguous initial record saves use an exact scoped point-read and delete only a byte-identical unreferenced candidate.
- [x] Unknown/mismatched reads retain loudly; compensation failures preserve the original cause and residual.
- [x] In-memory/SQLite fault arms and N≥100 same-owner races pass under `-race`.

## Files added or changed

- `internal/runtime/agentcfg/protocol/`
- `internal/agentcfg/drivers/statestore/`
- `scripts/smoke/phase-226.sh`

## Public API surface

- No wire change. Internal coordinated-write helpers and fault conformance expand.

## Test plan

- **Unit:** four stale doors and exact restore/delete matrices.
- **Integration:** real SkillStore + registry path with identity propagation and injected failure.
- **Conformance:** in-memory and SQLite commit-then-error variants.
- **Concurrency / leak:** N≥100 same-owner writers; separate identities never cross.

## Smoke script additions

- Run named stale-write and ambiguous-save tests with PASS non-vacuity checks.

## Coverage target

- `internal/runtime/agentcfg/protocol` and `internal/agentcfg/drivers/statestore`: no reduction from v1.25 floors.

## Dependencies

- Phase 221.

## Risks / open questions

- Compensation is explicit coordination, not an atomic transaction across two stores.

## Glossary additions

- None.

## Pre-merge checklist

- [x] `make drift-audit` and `make check-mirror` pass
- [ ] CI preflight, coverage, isolation, concurrency, conformance, and integration gates pass
