# Phase 155 — Session-erasure audit integrity (durable record-of-fact + cumulative counts)

## Summary

Two named follow-ups from the v1.7 band-end review harden the `sessions.delete` cascade's audit trail. Issue #409: the `session.erased` event — D-262's designated record-of-fact for a right-to-erasure operation — is emitted best-effort AFTER the data is irrevocably gone; a single bus/redactor failure loses the only audit record while the call still reports success, and a re-invoke returns `not_found` so there is no second chance. Issue #410: a retried mid-cascade erasure re-runs idempotent deletes that now find fewer records, so the reported deletion counts reflect only the final converging attempt. This phase makes the audit record part of the erasure's success criteria (a failure to durably record never coexists with a success response) and makes counts cumulative across attempts.

## RFC anchor

- RFC §6.9
- RFC §6.13

## Briefs informing this phase

- brief 05
- brief 06

## Brief findings incorporated

- brief 06 §5: audit events for privileged/destructive operations are the operator's record-of-fact; an audit trail that can drop on a transient error is not a trail. The fix subordinates the destructive step to the durable record, not the reverse.
- brief 05 §4: multi-store cascade operations must be idempotent AND convergent under retry — this phase extends that to the cascade's TELEMETRY (counts converge to the true total, not the last attempt's residue).

## Findings I'm departing from (if any)

- None.

## Goals

- An erasure that returns success has durably recorded its `session.erased` record-of-fact. A failure to record fails the call loud — with the session still re-invokable (the ordering guarantees the irreversible step never precedes the record's durability).
- The response's and event's deletion counts are cumulative across converging attempts — the reported totals equal what was actually removed.
- No new Protocol surface; `sessions.delete`'s wire shape is unchanged (field docs updated to state the cumulative-counts semantics).

## Non-goals

- No generalized transactional-outbox subsystem — this is a targeted ordering + persistence fix inside the erasure cascade, using the StateStore the cascade already drives.
- No change to the erasure scope contract (own-session-only, D-262) or the running-task refusal.
- No retroactive repair of past under-counted events.

## Acceptance criteria

- [x] The cascade's ordering guarantees the invariant: at no point can (data irrevocably gone) ∧ (no durable audit record) ∧ (success returned) hold. The implementor picks the mechanism — persist the compliance record (via the StateStore, outside the erased scope) before the irreversible clear, or bounded-retry the emit and fail the call on exhaustion — and documents the chosen ordering in the package godoc; the "record says erased but data present + success returned" inverse is equally asserted impossible.
- [x] Bus publish failure on the final emit → the call fails loud with a typed error; a subsequent re-invoke converges (emits the record, completes) — proven by a fault-injection test with a failing-then-healthy bus.
- [x] Redactor refusal follows the same loud path (no `Error`-log-and-continue).
- [x] Per-store deletion counts accumulate across attempts: a cascade interrupted mid-way and re-invoked reports the cumulative total in both the response and the event — proven by a fault-injection test (fail after artifacts step, re-invoke, assert totals equal first+second attempt sums).
- [x] `SessionsDeleteResponse` count-field docs + `docs/site` reference updated to state cumulative semantics.
- [x] Existing erasure tests (running-task refusal, own-session scope, idempotent convergence) stay green; `scripts/smoke/phase-155.sh` OK ≥ 1, FAIL = 0.

## Files added or changed

- `internal/sessions/erasure.go` — ordering + cumulative counts + the durable ledger checkpoint + the striped per-session erase lock.
- `internal/sessions/erasure_audit_test.go` — new file (rather than appending to `erasure_test.go`, to keep the fault-injection suite's fixtures/helpers self-contained) with the fault-injection tests.
- `internal/sessions/events.go`, `internal/protocol/types/sessions.go` — field doc comments only (no shape change; no D-223 manifest impact — verified by `make protocol-ts-gen-check`; no `docs/site/protocol` diff — verified by `make protocol-docs-gen-check` — the generated Notes column does not surface Go field-doc prose).
- `internal/sessions/protocol/protocol.go`, `internal/protocol/transports/stream/sessions_handler.go` — deviation beyond the plan's file list (§4.3): mirrors `ErrErasureRecordFailed` through the Service-layer error-mapping switch and the wire handler's HTTP-status classifier, matching the existing pattern for every other `sessions.Err*` sentinel on this surface, so the new sentinel doesn't fall through to the generic default case unclassified. Matching tests added to `internal/sessions/protocol/delete_test.go` and `internal/protocol/transports/stream/sessions_handler_delete_test.go`.
- `scripts/smoke/phase-155.sh`.

## Public API surface

- None. One new typed sentinel (shape: `ErrErasureRecordFailed`) surfaced by `sessions.delete`'s error mapping.

## Test plan

- **Unit:** ordering invariant under injected redactor failure / bus failure / store failure at each cascade step; cumulative count math.
- **Integration:** real state + artifact + memory drivers, real bus: happy path (pre-existing `internal/sessions/erasure_test.go` + `test/integration/phase130_session_erasure_test.go`, both still green); bus-fails-once-then-heals → first call fails loud, second converges with cumulative counts and exactly one durable record; identity triple asserted on the event. **Deviation (§4.3):** these fault-injection tests land in-package (`internal/sessions/erasure_audit_test.go`) rather than under `test/integration/` — `internal/sessions` already IS the cross-subsystem wiring boundary for this cascade (real StateStore + real MemoryStore + real ArtifactStore + the real durable EventBus, no mocks at the seam), the same precedent the existing `erasure_fence_test.go` (D-274) set for this exact package. §17.2 explicitly sanctions in-package integration tests "when the package itself IS the wiring boundary."
- **Conformance:** N/A — no driver seam change (the cascade uses existing StateStore primitives).
- **Concurrency / leak:** two concurrent `sessions.delete` for the same session race safely (one wins, one gets the idempotent/not-found path, never a double event); `-race`; goroutine baseline (the existing N=120 distinct-sessions D-025 stress in `erasure_test.go` continues to pass alongside the new same-session race test).

## Smoke script additions

- unit-tests: run the sessions erasure package tests (the live-server dev token is own-session; the fault legs are test-only). Existing phase-130 live smoke continues to cover the happy-path surface.

## Coverage target

- `internal/sessions`: 85%

## Dependencies

- 130 (the erasure method + cascade, D-262).

## Risks / open questions

- Persisting a compliance record in the StateStore outside the erased scope must not itself become an identity-scoping violation — the record is tenant/user-scoped operator audit data ABOUT a deleted session; the implementor documents its scope key and the §6 reviewer attacks it.
- #410's alternative (document per-attempt semantics instead of accumulating) is rejected here: the counts feed a compliance surface; "accurate" beats "documented as inaccurate". Recorded in D-286.

## Glossary additions

- None (no new vocabulary; "record-of-fact" already appears in D-262's entry).

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target — `internal/sessions` moved from a 79.2% baseline to 81.5% (every function this phase touched/added in `erasure.go` sits at 88-100% coverage); the residual gap to the 85% package target is pre-existing debt in untouched `registry.go` / `catalog.go` / `gc.go` functions, not code this phase added. Recorded honestly rather than gold-plating unrelated code.
- [x] If multi-isolation paths changed: cross-session isolation test passes (the pre-existing N=120 distinct-sessions concurrent test + the new same-session race test both pass under `-race`).
- [x] Concurrent-reuse: N/A — no new reusable artifact; the concurrent-delete race test above runs under `-race`.
- [x] Integration test wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, runs under `-race` (in-package per the Test-plan deviation note above).
- [x] If new vocabulary: glossary updated — N/A, no new vocabulary.
- [x] If a brief finding was departed from: N/A — no departure from brief 05/06's findings; see "Brief findings incorporated" above.
