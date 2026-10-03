# Phase 271 — Exact-task text-input receipts

## Summary

Extend the existing text-only `user_message` control with caller-keyed task
receipts, ordered input revisions, and sealed answer provenance. A lost HTTP
acknowledgement is recovered through `control.receipt` or an exact retry without
applying the input twice. The canonical task record remains the only receipt
store; the runtime inbox and planner boundary remain the only execution path.

## RFC anchor

- RFC §5.2
- RFC §5.5
- RFC §6.3
- RFC §6.8

## Briefs informing this phase

- brief 02
- brief 05

## Brief findings incorporated

- **brief 02 §3:** steering validation and delivery belong to the runtime;
  planners observe the existing `RunContext.Control` projection, never a new
  coordinator control API.
- **brief 02 §4:** required context checkpoints fail loudly. A failed checkpoint
  or consumed-receipt write cannot produce a successful applied receipt or
  execute the resulting tool decision.
- **brief 05 §4:** retries use idempotent handlers over stable caller event IDs;
  this phase makes payload conflicts explicit and retains old keys.
- **brief 05 §5:** one mandatory registry interface is shared by the reference
  and durable task drivers; no optional receipt capability interface is added.

## Findings I'm departing from (if any)

None. Restart preserves task records, not execution. RFC §6.3's explicit
restart-unavailable pause contract remains unchanged.

## Goals

- Bind a bounded text clarification to an exact identity quadruple and task.
- Distinguish accepted, actually consumed, declined, and unavailable terminal
  input outcomes without relying on best-effort events or an enqueue ACK.
- Preserve immutable payload identity, monotonically accepted input revisions,
  and exact sealed answer provenance across response loss and durable restart.
- Reuse task lifecycle persistence, runtime steering, and the unified pause
  primitive; create no second run, executor, queue, or pause mechanism.

## Non-goals

- Durable receipts for cancellation, approval, redirect, context injection, or
  attachment-bearing messages. Those controls retain their existing semantics.
- Automatic relaunch after runtime restart, multiple runtime instances sharing
  a task registry, or changing the default task/state drivers.
- A model guarantee that a clarification was understood or obeyed. Applied
  means the planning invocation consumed its runtime input projection.
- Persisting provider credentials or unredacted input in receipt projections.

## Acceptance criteria

- [ ] Existing `user_message` with nonempty `event_id` returns a content-free
  receipt; empty keys retain existing process-local control behavior.
- [ ] Optional `expected_input_revision` atomically rejects stale new intent;
  exact retries retain their original outcome after later revisions.
- [ ] An exact retry returns the same receipt/revision and never enqueues or
  interrupts twice. Reusing a key with different text returns HTTP 409 and
  `control_receipt_conflict`.
- [ ] Unsupported payload shapes fail before admission. Event IDs are bounded
  to 128 bytes; text retains the existing 4096-character control limit.
- [ ] At most 256 receipts are retained per task. New IDs beyond the bound fail
  closed; old IDs and conflict proofs are never evicted.
- [ ] `control.receipt` reads exact retained receipts after cancellation or
  completion, enforces verified scope, and never starts or resumes work.
- [ ] Accepted revisions increase monotonically. Applied is persisted only
  after a planner invocation consumes the projected input, before execution of
  its resulting decision. Context/checkpoint failures remain unconsumed.
- [ ] Terminal task transitions atomically resolve unconsumed accepted inputs.
  Restart-failed running tasks cannot reapply them; a recovered paused task
  retains its pause but pending inputs become terminal/restart-unavailable.
- [ ] A sealed task result and answer envelope carry
  `incorporated_input_revision` from the admitted terminal planning invocation.
  A late input cannot relabel an old output or advance its revision.
- [ ] Lost ACK, 100 concurrent exact retries, payload conflict, cross-scope
  refusal, cancellation, checkpoint failure, and state-save failure tests pass.
- [ ] Restart fixtures pass with in-memory, SQLite, and real Postgres state.
- [ ] Capability `durable_task_input_receipts_v1` is advertised only with
  `tasks.driver: durable` and `state.driver: sqlite` or `postgres`.

## Files added or changed

- `internal/tasks/input.go`, `engine/input.go`, lifecycle and recovery helpers.
- `internal/runtime/steering/input_receipts.go`, inbox and run-loop boundaries.
- `internal/protocol/types/control_receipt.go`, handler, transport decoder, and
  typed client; task result projection and SDK aliases.
- `internal/planner/decision.go`, `answer_envelope.go`, shared answer builder,
  and served task result construction.
- Focused runtime, registry, protocol, client, and restart conformance tests.
- `scripts/smoke/phase-271.sh`; shared Protocol inventory and generated docs
  are reconciled by the integration owner in the same change.

## Public API surface

- `user_message`: existing `event_id` opts into exact text-input receipts;
  `ControlResponse.receipt` is additive.
- `control.receipt`: `ControlReceiptRequest {identity, event_id}` returns
  `ControlReceiptResponse {receipt, protocol_version}`.
- Receipt fields: `event_id`, `task_id`, `input_revision`, `status`, optional
  bounded `reason`, and nanosecond acceptance/application/terminal timestamps.
- Status vocabulary: `accepted`, `applied`, `declined`, `terminal`.
- SDK `Client.ControlReceipt` is a read-only exact-event lookup.
- `ControlRequest.expected_input_revision` optionally fences keyed text admission;
  `TaskDetail.input_revision` reports the current accepted counter (absence = zero).
- `TaskDetail.incorporated_input_revision` and the same answer-envelope field
  expose sealed result provenance; zero/absence denotes no durable text input.

## Test plan

- **Unit:** payload validation, stable identity hash, bounded retention, revision
  order, store-write rollback, exact result revision validation, and wire shape.
- **Integration:** real task registry plus inbox and scripted planner; accept
  during a blocked invocation, lose ACK, retry 100 times, consume once, and
  refuse a late input during terminal sealing. Real HTTP transport covers
  stable refusal lookup and typed conflicts.
- **Conformance:** common registry scenarios run on both task drivers. Restart
  fixtures reopen the durable task registry over all three StateStore drivers.
- **Concurrency / leak:** existing task and steering suites remain applicable;
  new concurrent exact retries join all goroutines and preserve one revision,
  one delivery, and one context projection.

## Smoke script additions

- Run focused real-driver task, steering, Protocol, and SDK receipt regressions.
- Assert the served `control.receipt` endpoint rejects a missing exact task identity
  without executing work, following the shared smoke skip convention.

## Coverage target

No regression in existing touched-package gates. New receipt branches must have
positive, conflict, refusal, persistence-failure, cancellation, and restart cases.
The integration gate records measured coverage rather than claiming an unrun bar.

## Dependencies

- Phase 20 task registry and lifecycle.
- Phase 52 steering inbox and Phase 54 Protocol control surface.
- Phase 60 HTTP control transport.
- Phase 269 retained context/checkpoint behavior.

## Risks / open questions

- The in-process task driver deliberately forgets task records on process
  restart. It provides process-lifetime idempotency only. Durable guarantees
  require the existing durable task driver and persistent SQLite/Postgres state;
  capability advertisement must be conditional and truthful.
- Receipt retention is bounded by task lifetime. Erasing the canonical task
  also erases its receipts; it cannot recreate execution under the old task ID.
- Planner consumption is not successful completion. Consumers must compare the
  desired accepted revision against sealed output provenance, not assume that
  an accepted or applied receipt proves the final output includes later input.
- Existing `control.applied` events describe step projection. Only receipt
  lookup is the durable proof of planning consumption for keyed text input.

## Glossary additions

- **Task input receipt:** content-free evidence for one caller-keyed text input
  on one exact task, including its accepted revision and consumption outcome.
- **Incorporated input revision:** the task input revision of the planning
  invocation whose admitted result became the sealed answer.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] Concurrent-reuse test passes with N≥100 under `-race`, with joined
  goroutines and no context or cancellation cross-talk
- [ ] Real-driver integration covers the cross-subsystem seam and failure paths
- [ ] Glossary and shared Protocol/RFC/decision references updated in integration
- [ ] No brief departure requires a separate decision
