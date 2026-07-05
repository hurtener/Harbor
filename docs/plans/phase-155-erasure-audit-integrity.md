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

- [ ] The cascade's ordering guarantees the invariant: at no point can (data irrevocably gone) ∧ (no durable audit record) ∧ (success returned) hold. The implementor picks the mechanism — persist the compliance record (via the StateStore, outside the erased scope) before the irreversible clear, or bounded-retry the emit and fail the call on exhaustion — and documents the chosen ordering in the package godoc; the "record says erased but data present + success returned" inverse is equally asserted impossible.
- [ ] Bus publish failure on the final emit → the call fails loud with a typed error; a subsequent re-invoke converges (emits the record, completes) — proven by a fault-injection test with a failing-then-healthy bus.
- [ ] Redactor refusal follows the same loud path (no `Error`-log-and-continue).
- [ ] Per-store deletion counts accumulate across attempts: a cascade interrupted mid-way and re-invoked reports the cumulative total in both the response and the event — proven by a fault-injection test (fail after artifacts step, re-invoke, assert totals equal first+second attempt sums).
- [ ] `SessionsDeleteResponse` count-field docs + `docs/site` reference updated to state cumulative semantics.
- [ ] Existing erasure tests (running-task refusal, own-session scope, idempotent convergence) stay green; `scripts/smoke/phase-155.sh` OK ≥ 1, FAIL = 0.

## Files added or changed

- `internal/sessions/erasure.go` — ordering + cumulative counts.
- `internal/sessions/erasure_test.go` + fault-injection tests.
- `internal/protocol/types/` — field doc comments only (no shape change; no D-223 manifest impact expected — verified by `make protocol-ts-gen-check`).
- `scripts/smoke/phase-155.sh`.

## Public API surface

- None. One new typed sentinel (shape: `ErrErasureRecordFailed`) surfaced by `sessions.delete`'s error mapping.

## Test plan

- **Unit:** ordering invariant under injected redactor failure / bus failure / store failure at each cascade step; cumulative count math.
- **Integration:** real state + artifact + memory drivers, real bus: happy path; bus-fails-once-then-heals → first call fails loud, second converges with cumulative counts and exactly one durable record; identity triple asserted on the event.
- **Conformance:** N/A — no driver seam change (the cascade uses existing StateStore primitives).
- **Concurrency / leak:** two concurrent `sessions.delete` for the same session race safely (one wins, one gets the idempotent/not-found path, never a double event); `-race`; goroutine baseline.

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

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] Concurrent-reuse: N/A — no new reusable artifact; the concurrent-delete race test above runs under `-race`.
- [ ] Integration test wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, runs under `-race`
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
