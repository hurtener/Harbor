# Phase 269 — Retained session execution context

## Summary

Implement RFC 002's second slice using the existing StateStore, artifact
machinery, and run-context projection. The current increment wires bounded terminal
retention into serving and embedded `RunOnce`; the phase remains in progress,
not RC-ready.

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

- [x] Positive `sessions.retained_context_turns` or `WithRetainedContext(1..32)`
      explicitly opts in; configuration defaults to zero and the per-call option
      can explicitly disable configured retention.
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
- [x] Serve consumes the same projection under explicit runtime configuration
      after the existing agent/route/catalog resolution. Root and child context
      stay separate; required persistence precedes task completion.
- [x] Required query, dispatch intent and returned settlement are committed at
      their runtime boundaries; failed writes block dependent work, and bounded
      per-action frames avoid rewriting the whole trajectory on each call.
- [ ] Interrupted-prefix reuse has an explicit reconciliation/execution fence
      and end-to-end authorized recovery before phase completion.
- [x] Typed historical exchange envelopes reuse native call/result rendering
      without becoming dispatchable actions, including aggregate/failure paths.
- [ ] Cross-turn checkpoint reuse preserves prior narrative and bounded work
      across user turns without re-summarizing the complete retained window.
- [x] Retained native discovery and canonical invoked tool names are bounded,
      resolved against current schemas/scopes/exclusions, and never auto-executed.
- [ ] Authorized large-result reference recovery, freshness/expiry, attachment
      context and steering corrections are covered.
- [ ] Postgres conformance, full coverage, preflight, and release gates pass.

## Files added or changed

- `internal/runtime/runctx/retained_context.go` and its driver/identity tests.
- `internal/runtime/assemble/runonce.go` and request-level integration tests.
- `internal/runtime/serve/`, `internal/config/`, configuration docs and example.
- `sdk/assemble/assemble.go`, RFCs, decisions, glossary, and embedding recipe.
- `scripts/smoke/phase-269.sh` and this phase's master-index entry.

## Public API surface

```go
func WithRetainedContext(turns int) RunOption
```

The SDK aliases the existing assembly implementation. No Protocol method, wire
schema, backend, production dependency, or default retention change is added.
The explicit `sessions.retained_context_turns` setting applies to serving and
embedding; the per-call option overrides it. Zero is disabled by default. Child
tasks never publish private transcripts into the root conversation window.

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
or placeholder claims the pending per-action durability surfaces shipped.

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

## Serving-consumer checkpoint

Real Boot and RunLoopDriver tests cover opt-in/default configuration, exact
14,660-byte source receipts, numeric version preservation, required admission
and terminal failures, and root/child separation. An N=128 shared driver test
checks two-turn continuation across tenant/user/session tuples without replaying
historical tool actions. Embedded tests pin configuration inheritance and an
explicit zero override. Existing low-trust caller context and current catalog
resolution remain upstream of the shared retained-window helper.

This checkpoint remains terminal-only. It does not establish crash-safe tool
settlement, restored native tool formatting, or completion of phase 269.

## Dispatch-checkpoint increment

D-466 extends retained mode to required dispatch-boundary persistence for both
production consumers. The runtime records one intent and settled frame per
whole exchange using the existing atomic conditional StateStore contract.
Terminal publication seals the journal, then exact-generation cleanup removes
the transient records. Scope deletion also covers the run-scoped frames.

A persistence error is a fatal runtime error, never tool feedback to retry a
write. A known returned result receives a bounded post-cancellation persistence
attempt. Failed/unsettled journals cannot be sealed as successful terminal turns.
The old terminal-only status above describes the preceding checkpoints, not a
claim that current action-boundary writes are absent. Safe interrupted-prefix
reuse and the remaining phase acceptance criteria remain unclaimed.

## Historical native projection increment

D-467 adds non-executable historical envelopes and reuses the live ReAct renderer
for native tool, parallel, batch, progress and task-control exchanges. Exact
source strings/numeric lexemes/completeness values survive JSON restoration;
envelope formatting may canonicalize. Stable origin-based wire IDs avoid
cross-turn provider-ID reuse and remain unchanged across supported adapters.

Retained windows advance to version 2, preserving legacy version-1 rows as inert
evidence and fencing older readers from misinterpreting the new representation.
The dispatch journal stays version 1. The production consumer, old/new format
tests, cross-provider scripted wire tests and 128-way isolation land together.
Checkpoint reuse and the other unchecked criteria remain unfinished.

## Retained tool-discovery increment

D-468 reuses existing native exchange projection and current catalog resolution.
Single, parallel and batch discovery survive restoration, as do canonical names
of actually invoked catalog tools. The historical contribution is capped at 128
recent unique names; current-run discovery remains independent. A current schema
update is reflected in the next declaration, and revoked scopes or removed/hidden
tools cannot be re-enabled by historical names. Unresolved aliases are not guessed.
No second catalog, schema cache, custom tool contract, or provider client is added.

The embedded request regression observes a prior search, a changed descriptor,
and then scope revocation over three retained turns. The search executes once,
no historical edit executes, and actual outgoing declarations track the catalog.
Shared-catalog and historical-reader tests exercise 128 concurrent invocations.
