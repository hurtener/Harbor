# Phase 269 — Retained session execution context

## Summary

Implement RFC 002's second slice using the existing StateStore, artifact
machinery, and run-context projection. The first increment wires bounded terminal
retention into embedded `RunOnce`; this phase remains in progress, not RC-ready.

## RFC anchor

- RFC §6.2
- RFC §6.9
- RFC §6.11

## Briefs informing this phase

- brief 02
- brief 05
- brief 08

## Brief findings incorporated

- brief 02 §1: runtime mechanisms stay separate from planner reasoning policy.
- brief 05 §1: identity-scoped StateStore and ArtifactStore own persistence;
  a new service or competing backend is unnecessary.
- brief 08: model inference continues through the existing Bifrost-backed client.

## Findings I'm departing from (if any)

None in the target contract. This incremental implementation is deliberately not
reported as the completed durability contract: terminal persistence alone does
not close the side-effect-before-receipt crash window. D-464 records this boundary.

## Goals

- Preserve permitted execution evidence across retained session turns.
- Restore committed context without replaying past tool actions or duplicating
  legacy pair-only memory or trusted completion-hook ingestion.
- Bound storage work, retained data, concurrency, and recovery failures.
- Honor erasure and preserve exact source strings and numeric identifiers.

## Non-goals

- No long-term memory, native compaction, new provider client or public transcript.
- No automatic cold-run relaunch, guessed successful outcomes, or exactly-once
  side-effect guarantee.
- No domain-specific resource schema or generic history-search service.

## Acceptance criteria

- [x] Embedded runs explicitly opt in through `WithRetainedContext(1..32)`;
      omitted/zero leaves existing memory and retention behavior unchanged.
- [x] One bounded session slot uses StateStore conditional writes and existing
      pending/tombstone erasure fences; stale terminal callbacks cannot resurrect
      erased admissions.
- [x] Prior terminal evidence reaches the next actual ReAct request, including
      the complete synthetic 14,660-byte receipt and its exact large version.
- [x] In-memory and SQLite restoration retain observed evidence; imported history
      is not recursively persisted or sent to completion ingestion again.
- [x] Required admission/terminal writes and redaction fail explicitly; expired
      or count-evicted turns produce a partial-history notice.
- [x] N>=128 runs share a Stack and stores without session bleed or repeated
      historical dispatch; simultaneous siblings do not import in-flight steps.
- [ ] Serve consumes the same projection under explicit runtime configuration,
      resolved agent authority, and the session erasure lifecycle.
- [ ] Per-action intent/settlement persistence closes the required crash-boundary
      acceptance; interrupted-prefix reuse has an explicit execution fence.
- [ ] Cross-turn checkpoint reuse and typed historical native exchange projection
      preserve provider formatting without becoming executable Decisions.
- [ ] Authorized large-result reference recovery, freshness/expiry, attachment
      context, steering corrections, and discovered-tool revalidation are covered.
- [ ] Postgres conformance, full coverage, preflight, and release gates pass.

## Files added or changed

- `internal/runtime/runctx/retained_context.go` and its driver/identity tests.
- `internal/runtime/assemble/runonce.go` and request-level integration tests.
- `sdk/assemble/assemble.go`, RFCs, decisions, glossary, and embedding recipe.
- `scripts/smoke/phase-269.sh` and this phase's master-index entry.

## Public API surface

```go
func WithRetainedContext(turns int) RunOption
```

The SDK aliases the existing assembly implementation. No Protocol method, wire
schema, backend, production dependency, or default retention change is added.
This increment applies to `RunOnce`, not `harbor serve` configuration.

## Test plan

- **Unit:** malformed/versioned records, bounded count/TTL, redaction refusal,
  required writes, duplicate finalization and whole-turn eviction.
- **Integration:** real RunOnce/RunLoop/ReAct/catalog dispatch and actual outgoing
  request construction; completion-hook isolation and failed store seams.
- **Conformance:** shared in-memory/SQLite scenarios, close/reopen SQLite,
  conditional conflicts and erasure; Postgres remains required before completion.
- **Concurrency / leak:** 128 simultaneous scoped invocations against one Stack
  and shared stores under `-race`; no new worker or cleanup goroutine is introduced.

## Smoke script additions

Run the retained-context and embedded request tests under `go test -race` with
real production stores. Assert the plan/decision and SDK consumer exist. No test
or placeholder claims the pending serve or per-action durability surfaces shipped.

## Coverage target

Preserve touched-package floors. Record `-cover` measurements for runctx and
assembly and exercise every new admission, retention, erasure and failure branch
before declaring the complete phase finished. Incremental tests are not a claim
that full branch coverage or repository preflight is already green.

## Dependencies

- 268 — portable compaction and request budgeting.
- 15, 16, 17 — persistence floor and driver contracts.
- 246 — preserve the consumer-turn projection's distinct authority.

## Risks / open questions

The terminal-only increment stores a maximum of 32 recent turns, 256 own steps
per turn and 512 KiB per session slot; up to 32 active admissions are tracked.
A run's TTL uses the configured session idle TTL (24 hours when unspecified).
Exceeding an indivisible turn's bound fails explicitly rather than clipping it.
An abandoned admission remains visible as unsettled and consumes the bounded
active allowance; it is not auto-released as proof that its actions failed.
No automatic admission recovery is claimed before the execution-fence acceptance.

History in this increment is inert lower-trust evidence in the existing
trajectory, not reconstructed executable Decisions. It is selected once on
admission; siblings' in-flight results are excluded. Only the current run's own
steps are written at termination. Rehydrating historical native tool calls,
checkpoint reuse, and preserving additional attachment/steering context remain
explicit pending acceptance, not assumptions about generic JSON restoration.

Required terminal persistence can fail after external effects succeeded. The
caller receives an error and must reconcile, not repeat the whole run blindly.
The existing completion hook is unchanged and still observes the runtime's
terminal boundary; it is not a durable-retention success receipt.

## Glossary additions

Retained execution window: a bounded private session projection, distinct from
long-term memory, consumer turns, and authorization to repeat external actions.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [x] `make check-mirror` passes
- [ ] All cross-references resolve through the full drift gate
- [ ] Coverage meets touched-package targets
- [x] Cross-session and N>=128 shared-Stack race tests pass
- [x] First production consumer and real-driver failure integration land together
- [x] Vocabulary and incremental boundary are recorded in D-464
