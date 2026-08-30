# Phase 266 — Indexed fan-out and ordered observability event path

## Summary

Keep Harbor's canonical event bus responsive at high subscriber counts and
when durable persistence is slow. This phase adds identity-indexed exact/admin
fan-out, atomic multi-event publication that preserves individual canonical
records, and one bounded per-bus FIFO used asynchronously by
`llm.cost.recorded` and the universal tool-lifecycle shell. A normal durable
publication is an ordering barrier, so successful `task.completed` cannot
overtake earlier accepted cost/tool telemetry.

This is publication mechanics, not a conversation-storage or Protocol phase.
`sessions.turns.*`, `live_resume_seq`, SSE event IDs, completion-chunk
`Sequence == 0`, and wire types remain unchanged. Accepted async telemetry can
still be lost on abrupt process loss before durable commit; Harbor adds no
outbox or second transcript to conceal that best-effort boundary.

## RFC anchor

- RFC §4
- RFC §5.2
- RFC §6.4
- RFC §6.5
- RFC §6.13
- RFC §6.14

## Briefs informing this phase

- brief 06
- brief 07
- brief 03

## Brief findings incorporated

- brief 06 §4 requires server-side identity filtering, bounded channels,
  explicit drop behavior, O(1)-shaped publication, and exact durable replay.
  Exact subscriptions are therefore identity-indexed, with admin fan-in kept
  in a distinct bucket.
- brief 06 §5 rejects a two-channel split. The FIFO is private mechanics
  behind the same EventBus, not a new stream, event shape, or replay source.
- brief 07 §8 puts tool dispatch in runtime-owned machinery. Phase 161's
  universal descriptor shell remains the one lifecycle producer; this phase
  changes only how it hands content-free events to the bus.
- brief 03 §5 requires identity propagation and safe observability for every
  tool transport. Indexed and queued records retain the full run quadruple and
  the existing audit-redaction/`SafePayload` boundary.

## Findings I'm departing from (if any)

None.

## Decision

- D-453 — exact subscriptions use an identity index; widened admin
  subscriptions use a separate bucket.
- D-454 — event batches commit atomically but replay as exact individual
  canonical records.
- D-455 — accepted cost/tool telemetry shares one bounded FIFO with
  synchronous publication barriers.

## Goals

- Select fan-out candidates without scanning unrelated exact subscribers while
  preserving filtering, isolation, audit, drop, reaper, cancel, and close.
- Add additive `events.BatchPublisher`; both shipped drivers implement it and
  legacy/custom EventBus implementations remain source-compatible.
- Validate/redact every batch member before mutation. One rejection leaves no
  sequence, history, or delivery side effect.
- Commit durable batches through one `StateStore.SaveBatchIf` transaction,
  including authority, individual bodies, and affected session heads.
- Preserve one event, consecutive sequence, and cursor per batch member.
- Route `llm.cost.recorded` and all five universal tool-lifecycle events into
  one bounded per-bus FIFO and return after acceptance.
- Make synchronous `Publish`/`PublishBatch` barriers. Successful
  `task.completed` waits for all earlier accepted cost/tool records.
- Drain and join on orderly `Close(ctx)`; state the abrupt-process-loss window.

## Non-goals

- No outbox, WAL, retry daemon, billing ledger, recovery synthesis, or
  exactly-once-across-process-loss claim.
- No second transcript or raw-event reconstruction of conversation turns.
- No durable completion chunks, synthetic terminal answers, `LivePublisher`
  change, or `Sequence == 0` / SSE / `live_resume_seq` change.
- No Protocol method/type/version, Console reducer, generated wire, or SDK
  change. Harbor Protocol remains `0.1.0`.
- No async conversion of fail-closed audit/security, governance-enforcement,
  or task/session authority events.
- No portable nanosecond, ratio, or percentage performance threshold.

## Contract

```go
type BatchPublisher interface {
    PublishBatch(context.Context, []Event) error
}

type AsyncPublisher interface {
    PublishAsync(context.Context, Event) error
}
```

An empty batch is rejected before queue admission. A batch must use one
identity/session so the durable driver updates one session head and the global
authority atomically; callers needing multiple identities submit separate
batches through the same ordered bus.
Validation, identity/type admission, timestamp normalization, caller-sequence
refusal, and redaction complete before mutation. Fan-out starts after commit
and preserves assigned sequence order.

`PublishAsync` validates/redacts before bounded admission. Once accepted,
persistence belongs to the bus lifetime, not the returning producer context.
Saturation/close fails admission explicitly; the queue retains no unredacted
payload. `Publish` and `PublishBatch` enter the same FIFO and wait, making
either a barrier for earlier accepts. A failed barrier remains loud and
invents no terminal state.

Because that FIFO handoff and completion barrier is a different operation from
the former direct-fan-out `Publish`, this phase renames the D-136 small-fan-out
benchmarks to `BenchmarkOrderedBusFanOut*` and establishes a reviewed semantic
baseline reset. The separate 1K/10K mostly-nonmatching benchmarks retain the
identity-index cardinality guard.

Exact non-admin subscriptions are keyed by full identity triple. Publication
visits that bucket and the distinct admin bucket, then applies unchanged
run/type predicates. `Admin: true` remains widened fan-in even when identity
fields are present. All subscription lifecycle operations update the same
indexed membership.

## Release evidence (2026-08-30)

Implementation PR #764 merged as
`f6d87b27d8381ed4438e74f75348343729294c8e`. Exact post-merge main CI run
`33297306154` completed successfully, including both Go platforms, the
performance gate, leak/isolation/chaos conformance, and full preflight. The
annotated `v1.31.0` tag object `dc009bb544ac1381ff6fa23b3e7aa867685adb27`
peels to that commit; release workflow `33299904384` succeeded with 13
published assets. Downstream runtime deployment and acceptance are not claimed.

## Acceptance criteria

- [x] Both buses use exact-identity plus admin candidate buckets; unrelated
      exact subscribers are neither scanned nor delivered to.
- [x] Run/type filters, drop notices, cancel, reaping, close, and admin audit
      behavior remain unchanged under `-race`.
- [x] Both buses implement `BatchPublisher`; an empty batch is rejected and any
      invalid member rejects the whole batch before mutation.
- [x] Durable batches use exactly one `SaveBatchIf`; injected failure exposes
      no prefix and consumes no sequence.
- [x] Restart replay returns individual input-ordered records with consecutive
      non-zero sequences and exact cursor tails.
- [x] Cost and all five universal tool lifecycle events share one bounded FIFO
      and return after acceptance while durable storage is blocked.
- [x] Synchronous publish cannot overtake an earlier accept; successful
      `task.completed` follows accepted cost/tool records.
- [x] Saturation/close admission is explicit; orderly Close drains/joins; crash
      loss is documented without synthesizing terminal state.
- [x] Cancelled late chunks create no durable/synthetic record; turns, resume,
      and wire semantics remain unchanged.
- [x] 1K/10K unrelated-subscriber and N>=128 identity gates run on the same
      host/toolchain with no fixed portable timing claim.

## Files added or changed

- Event interfaces/drivers, LLM cost producer, universal tool lifecycle, and
  successful task-terminal publication wiring.
- Phase 266 LLM/integration/benchmark tests committed at `f8d2e389`.
- RFC, decisions, master plan, changelog, site/operator docs, and static smoke.

## Public API surface

- Additive internal `events.BatchPublisher` and `events.AsyncPublisher`.
- No SDK, Protocol, REST/SSE, config, wire-type, or Console public surface.

## Test plan

- **Unit:** index membership/lifecycle; batch validation/rollback; bounded
  admission/drain; barriers; worker shutdown.
- **Integration:** real durable bus, in-memory StateStore, Catalog, LLM cost,
  and task terminal paths with restart and blocked/failing persistence.
- **Conformance:** both event drivers prove exact/admin, batch, async, and
  after-close behavior without capability skips.
- **Concurrency / leak:** N>=128 identities/producers against one shared bus,
  cancellation isolation, ordering, and goroutine restoration.

## Smoke script additions

`scripts/smoke/phase-266.sh` statically pins the committed gate files, three
decisions, plan/index, atomic individual-record and FIFO contracts, explicit
process-loss limitation, and unchanged turns/resume boundary.

## Coverage target

- Event drivers: no regression from existing 85% floors; every new durable
  transaction-failure branch covered.
- LLM/tools: every changed admission/error branch; no unrelated numeric claim.

## Dependencies

- 05, 06, 57, 147 — event taxonomy, replay, durable log, conformance.
- 161 / D-293 — driver-neutral cost and universal lifecycle producers.
- 246 / D-425 — authoritative conversation turns and live-resume handoff.
- D-452 — non-durable completion chunks and `Sequence == 0`.

## Risks / open questions

- Async success is bounded process-memory acceptance, not durability. Crash,
  SIGKILL, or machine loss before commit may lose the named telemetry. That is
  never acceptable for authority/security paths.
- Queue saturation fails admission explicitly and must not indefinitely block
  tool/LLM results.
- Admin work remains proportional to true admin consumers and exact matches.
- Benchmark both revisions on one host/toolchain:
