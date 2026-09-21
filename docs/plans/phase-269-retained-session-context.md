# Phase 269 — Retained session execution context

## Summary

Implement RFC 002's second slice using the existing StateStore, artifact
machinery, and run-context projection. Served and embedded runs now retain
bounded execution journals and terminal context, with explicit recovery, native
historical projection, checkpoint reuse and source-lifetime checks. Implementation
acceptance is tracked below; final release gates remain in progress.

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

None in the target contract. D-464 records the original terminal-only boundary.
Later increments below add intent/settlement journaling and explicit reconciliation;
an intent without a returned receipt remains unknown, never automatically replayed.

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
- [x] Interrupted-prefix reuse has an explicit reconciliation/execution fence
      and end-to-end authorized recovery before phase completion.
- [x] Typed historical exchange envelopes reuse native call/result rendering
      without becoming dispatchable actions, including aggregate/failure paths.
- [x] Cross-turn checkpoint reuse preserves prior narrative and bounded work
      across user turns without re-summarizing the complete retained window.
- [x] Retained native discovery and canonical invoked tool names are bounded,
      resolved against current schemas/scopes/exclusions, and never auto-executed.
- [x] Authorized dispatcher-offloaded result recovery uses existing bounded
      artifact reads, scoped lookup and source expiry/deletion guards.
- [x] Applied user-message, redirect and injected-context corrections survive
      retained continuation and settled-journal reconciliation without replaying
      control actions or changing the next run's goal/authority.
- [x] Supplied attachment references survive retained continuation and summary
      coverage; missing-input admission and scoped deletion guards apply.
- [x] PostgreSQL conformance exercises the production driver with independent pools.
- [ ] Full final-tree coverage, preflight, and release gates pass.

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

The SDK aliases the existing assembly implementation. The explicit own-session
`sessions.reconcile_context` Protocol operation and typed Go client use the same
settled-journal primitive. No backend, production dependency, or default retention
change is added.
The explicit `sessions.retained_context_turns` setting applies to serving and
embedding; the per-call option overrides it. Zero is disabled by default. Child
tasks never publish private transcripts into the root conversation window.

## Test plan

- **Unit:** malformed/versioned records, bounded count/TTL, redaction refusal,
  required writes, duplicate finalization and whole-turn eviction.
- **Integration:** real RunOnce/RunLoop/ReAct/catalog dispatch and actual outgoing
  request construction; completion-hook isolation and failed store seams.
- **Conformance:** shared in-memory/SQLite scenarios, close/reopen SQLite,
  conditional conflicts and erasure; independent PostgreSQL pools exercise the
  same new behavior. Final-tree reruns remain required before completion.
- **Concurrency / leak:** 128 simultaneous scoped invocations against one Stack
  and shared stores under `-race`; no new worker or cleanup goroutine is introduced.

## Smoke script additions

Run the retained-context and embedded request tests under `go test -race` with
real production stores. Assert the plan/decision and SDK consumer exist. The scripted tests do not claim paid model proficiency, provider cache hits or
exactly-once external actions.

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

The bounded session window retains up to 32 recent turns, 256 own steps per turn
and 512 KiB per session slot; up to 32 active admissions are tracked. Per-action
frames use the same existing StateStore, with explicit size and generation bounds.
A run's TTL uses the configured session idle TTL (24 hours when unspecified).
An indivisible entry exceeding its bound fails rather than being clipped.

Historical native envelopes remain context, not executable Decisions. Source
selection is frozen on admission; siblings' in-flight results are excluded.
Only the current run's own steps enter its terminal record. Source-bound
checkpoints can be reused only while their exact evidence remains valid;
expiry, eviction, mutation or changed redaction invalidates derived summaries.
Attachment and offload references are revalidated without duplicating binaries.

An abandoned admission remains unsettled until explicit reconciliation succeeds.
A fully settled journal can be sealed while fencing the old admission; a pending
external operation remains unknown and is refused. No automatic clearance,
cold-run relaunch, provider-call cancellation or external exactly-once promise
is made. Later sections describe the individual historical delivery increments.

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

## Retained checkpoint reuse increment

D-469 stores one source-bound narrative in the existing session window. It is
restored without inference only while its exact source prefix remains retained.
Current queries, uncovered exchanges and transient status notices remain visible.
The ordinary compactor advances coverage using the previous narrative rather
than summarizing covered raw history again. Source redaction, expiry, eviction,
concurrent membership and corrupt metadata are checked explicitly.

The window advances to version 3; prior v1/v2 evidence-only windows migrate on
write, and old readers reject the new format. The journal format is unchanged.
Whole-source expiry or eviction invalidates the checkpoint instead of extending
retention through a summary. Recompaction of the remaining retained window may
therefore be necessary at a retention boundary.

Regressions cover three-turn rolling coverage, a changed query, SQLite close and
reopen, corruption, older-version injection, redaction, count/TTL invalidation,
concurrent completion ordering and 128 scoped checkpoint restorations. The real
RunOnce/RunLoop/ReAct/composed-client test asserts that the next request receives
the saved narrative and exact latest source without an extra summary call or
historical tool execution. These are scripted-runtime results, not model-quality
measurements or an RC completion claim.

## Explicit settled-journal reconciliation increment

D-470 adds the first recovery consumer, `Stack.ReconcileRetainedContext`, with
SDK error aliases and an embedding recipe. It requires configured retained
context and the full identity of one specific source run. The operation checks
every bounded committed frame and atomically seals that exact journal generation
as interrupted evidence. The source admission is fenced before any future
dispatch can commit. It does not execute a model/tool/hook or resume a lost run.

Pending intents return a distinct unknown-outcome error and remain unchanged.
Expired, erased, malformed, missing, or immediately evicted sources are not
acknowledged as recovered. Original expiry is preserved. Required redaction and
seal failures are explicit; cleanup failure preserves the committed outcome and
fence and supports exact-generation cleanup on a later call. An already running
provider request is not cancelled by this operation, but its next dispatch must
pass the existing admission checks.

Tests exercise in-memory/SQLite reopen, exact large numeric IDs, pending refusal,
identity separation, source TTL, erasure, corrupt/missing frames, write/redaction
failures, interrupted cleanup, and 128-way dispatch/reconciliation races. A real
embedded RunOnce request receives the recovered receipt without invoking its
historical action. Native rendering of untagged journal frames is not guessed.

The embedding consumer does not complete the served reconciliation requirement.
Served Protocol wiring, authorized result recovery, attachment/steering context,
Postgres conformance, and full release gates remain pending. This is not an RC
readiness claim or an exactly-once external-side-effect guarantee.

## Served reconciliation increment

D-471 wires `POST /v1/sessions/reconcile_context` through the production mux,
identity/body-scope gate and session service. The sole target is `source_run_id`;
admin/fleet does not bypass the caller's own-session boundary. Required retained
context configuration remains unchanged. The response includes only session/run
IDs and `reconciled: true`, never private execution evidence.

Tests cover the actual next served ReAct request, exact 14,660-byte evidence,
idempotence, admission fencing, pending refusal, all identity dimensions,
cancellation, unknown/trailing payloads, disabled retention and 128 concurrent
callers. Full runtime persistence and final release gates remain separately
tracked; a green focused test does not mark the entire phase shipped.

## Existing-artifact result recovery increment

D-472 keeps bounded references to dispatcher-offloaded results in the actual
request even when the corresponding exchange is covered by a retained summary.
The runtime validates each reference under the run's identity before inference
and before dependent dispatch; it does not expose an ArtifactStore to the planner.
The ordinary `artifact_fetch` tool remains the only byte-retrieval operation.

Request-level regressions first failed on the prior implementation: a compacted
reference disappeared, and deletion of its blob still allowed inference against
a retained summary. Tests now exercise the real dispatcher/offload, checkpoint,
RunOnce/ReAct request and bounded artifact read, checking exact trailing source,
version and completeness metadata. Served continuations use the same guard.
Negative cases cover source deletion before/during inference, foreign metadata,
lookup failure, cancellation, malformed envelopes, reference/metadata/scan bounds
and 128 shared read-only projections. No historical action is rerun.

This completes the generic offloaded-result recovery path, not attachment or
steering continuity, remaining persistence conformance or final RC acceptance.

## Applied steering continuity increment

D-473 records accepted USER_MESSAGE, REDIRECT and INJECT_CONTEXT content in the
existing ordered journal before another model decision. The same non-executable
observation is appended under the inspection lock after persistence succeeds.
The write itself runs outside that lock. Control execution still uses the
existing inbox, scope checks and per-step signals; no control is replayed from
history and no additional inference or planner-tranche count is introduced.

Context-only journal frames are explicitly tagged and fully settled. Recovery
refuses a frame whose tag disagrees with the presence of an action. Redaction
and the shared historical private-field validator apply before persistence.
Failed required writes stop dependent work rather than silently dropping the
correction. Existing action-only journals remain readable.

The pre-fix embedded regression lost all three correction types on the next
turn. Recording-provider request tests now preserve exact correction content and
numeric data without promoting history to system policy. Tests also cover SQLite
reopen/reconciliation, malformed or pending context, cancellation, immutable
payload capture, failure before dependent inference, and N=128 sessions sharing
one Stack without leaked inboxes or repeated historical tool calls.

## Attachment continuity increment

D-474 records supplied input IDs as one context frame, atomically with the
admitted query. It preserves association with the original user turn without
copying bytes. Both retained consumers reject inputs omitted by materialization;
non-retained callers keep their existing behavior. Existing result projection
validates attachment scope/lifetime independently of summary coverage and uses
ordinary authorized artifact reads. No image-understanding claim is synthesized.

Three request-level regressions failed before implementation. Tests now cover
compacted input recovery and exact fetched content, deletion during inference,
missing input admission, actual served continuation, SQLite close/reopen and
explicit reconciliation, atomic start failure, malformed/nested input markers,
no uploaded-byte persistence, and 128 concurrent identity-scoped projections.
Postgres conformance and complete release acceptance remain pending.

## Public SDK acceptance sample

The [portable-context sample](../../examples/portable-context/README.md) exercises
read/edit/inspect across independent stack lifecycles, using the real Bifrost
adapter and persistent SQLite. Its tests use synthetic local provider responses;
live calls require an explicit provider, model, capacity and test credential.
The phase smoke runs those tests. The SDK aliases the existing ordinary-slot
`SlotExpectation` and `ErrConditionFailed` so application tools can use
StateStore generation preconditions without internal imports. No new state
behavior or backend is added. Release and migration checks are in
[the RC procedure](../notes/portable-context-rc.md); the phase remains subject to
its full release gates, not merely sample success.
