# Portable session context implementation tracker

RFC 002 / PR #779. Implementation milestones and release acceptance are separate.
A checked implementation item does not mean the full PR is ready for an RC.

## Scope

Long-term memory remains external. Compaction uses the governed Bifrost-backed
client; no native compaction, new provider SDK, public transcript, or automatic
replay of interrupted external actions is introduced. Default retention is zero.

## Implementation

- [x] Runtime-owned coverage, recent/fresh exchange preservation and repeat compaction.
- [x] Assembled-request budgeting, output headroom and governed maintenance calls.
- [x] Bounded chronological summaries and exact numeric restoration.
- [x] Explicit served-root and embedded retention; no legacy-memory duplication.
- [x] Required dispatch intent and settlement journaling; ambiguous effects stay unknown.
- [x] Historical native exchanges and current-catalog discovery revalidation.
- [x] Source-bound cross-turn checkpoint reuse (`aeda3c0`).
- [x] Explicit embedded settled-journal reconciliation (`f207d61`).
- [x] Authenticated own-session served reconciliation and typed Protocol client.
- [x] Authorized dispatcher-offload recovery using existing artifact reads (`cb5ce55`).
- [x] Applied steering continuity, journal recovery and nonduplicated completion hooks.
- [x] Input attachment continuity and source-lifetime checks after compaction.
- [x] Compaction failure diagnostics use fixed content-free messages.
- [x] Pinned Bifrost request-prefix checks cover live appends, repeated builds and explicit compaction/model/authority boundaries.
- [x] Usage/cost report availability and Harbor-estimate flags; sparse streaming accounting does not erase earlier reports.
- [x] Bounded per-request capacity/section/installed-checkpoint diagnostics, with maintenance isolation and content-free fixed-shape events.

The published steering increment includes actual later and next-turn request
regressions, plus 128-session isolation.
Attachment IDs now share that context-frame path and commit atomically with
the admitted query; original binary contents are never copied to the journal.
The published journal format remains version 1 with explicit context-frame tags.
The existing retained-window format remains version 3.

## Release gates

- [x] Retained-context Postgres tests cover independent pools, exact recovery, pending refusal, checkpoint expiry and 128 dispatch/reconciliation races; existing CI runs them.
- [ ] Final-tree full Go lint, vet, race, coverage and build gates pass.
- [ ] Final-tree full drift/preflight and previous phase smoke gates pass.
- [ ] Final-tree Protocol generation/lockstep and applicable frontend gates pass.
- [ ] Final adversarial review has no unresolved critical/high-severity findings.
- [x] Temporary publication/source-recovery workflows and payloads are removed.
- [x] Sample-agent setup, migration and RC end-to-end test instructions are complete; publication of an RC remains a separate gate.
- [ ] RC is published after validation; no merge or release is implied by this tracker.

Real-model evaluation requires separate approval and provider credentials. The
scripted tests establish runtime/request behavior, not model proficiency, physical
external effects, paid cost savings or a comparative cache-performance ranking.

## PostgreSQL conformance checkpoint

The existing `state/postgres` CI job runs the new `TestPostgres_RetainedContext_`
scenarios against its PostgreSQL 16 service. No new workflow or production
backend is introduced. Local validation used a disposable PostgreSQL 16.15
cluster with two independent StateStore pools and a fresh schema per scenario.
The tests preserve exact numeric receipts, attachment identities and steering
updates; pending operations remain unknown, expired summaries are invalidated,
and another pool's erasure fences terminal publication. The concurrency scenario
runs 128 sessions with competing dispatch/reconciliation through separate pools.

The phase 269 smoke runs the same scenarios when `HARBOR_PG_DSN` is provided and
reports an explicit skip otherwise. These tests do not replace the full final-tree
regression, coverage and preflight gates above.

## Request diagnostics publication

The bounded diagnostics implementation is published at `1f2a184`. Phase 268
smoke passed 10 checks with no skips or failures, and the affected core race
suites, scoped Go lint, full Markdown and canonical generators passed.
Expanded Protocol tests exposed missing independent compatibility-list entries
for `llm.context.prepared` and `sessions.reconcile_context`. Both are now explicit;
the exact event-name and single-source method checks remain strict. The complete
Protocol documentation-generator and single-source checker race suites pass.
This correction does not replace final-tree preflight or the other release gates.

## Candidate test procedure

The [RC acceptance procedure](portable-context-rc.md) links the public-SDK
editing sample and separates deterministic proof from live model evaluation.
The sample checks actual persisted edits across separate invocations and exposes
inspection without inference; live calls are never performed by its test suite.

## Final-tree integration corrections

The Console's hand-maintained reconciliation wire types are now explicit, with
no untyped allowlist exception. The shared Protocol conformance inventory now
includes the named recovery method and both HTTP 409 refusal codes; its exact
counts and exhaustive membership checks remain independent. Phase 269 smoke
also runs the public fork conformance consumer that caught the stale inventory.
Full frontend and repository release gates remain separate acceptance above.

## Adversarial retained-state decoding

- [x] Corrupted duplicate/case-aliased host fields are rejected before recovery
      mutates retained state; canonical nested metadata remains strict.
- [x] Opaque tool-result keys and exact numeric values remain data, not host fields.
- [x] In-memory/SQLite regressions and independent-pool PostgreSQL checks pass.

This review correction is not completion of the whole adversarial or release
gate. Current validation runs as an unprivileged user; whole-repository checks
remain separate and no permission test or coverage floor is relaxed.

## Concurrency validation corrections

- [x] TUI synthetic event delivery waits for the actual stream registration
      (`13ef778`), not just the flushed HTTP headers; 100 full race runs pass.
- [x] Typed retained host metadata is checked with one streaming decoder, avoiding
      repeated nested payload scans without weakening duplicate/alias checks.
- [x] Full runctx/assembly/SDK assembly race suites and three repeated N=128
      embedded/served tests pass with unchanged production persistence deadlines.
- [ ] Hosted full-suite rerun confirms the remaining deadline failures resolved.
- [x] Hosted MinIO pull and real S3 conformance pass on `74d6171`.

Decoder benchmark measurements and their limitations are recorded in
[the release review](portable-context-release-review.md). These scoped results
are not a substitute for any unchecked final-tree gate above.

## CI fixture cleanup

The two temporary development workflows are removed, with no tracked payload
manifest left behind. The S3 job uses the same MinIO release on its documented
Quay registry and a loopback-only test port. Hosted run `35670204554` pulled the image and passed the real S3 suite.
No test command, timeout, or approval requirement is relaxed.

## Final preflight corrections and remaining macOS gate

The final Linux vet/test/build job is green at `74d6171`, along with hosted
PostgreSQL, S3, frontend checks, lint, examples, isolation, chaos, leak and
performance gates. macOS still fails `TestRetainedServer_ConcurrentReuse` and
`TestRunLLMSettingsReachProviderWithoutConsumingPendingSlot`. The matrix failure
skips hosted preflight and Playwright; their acceptance remains open.

Local full lint/vet, static build and Phase 268 smoke pass. Drift completed
1,590 checks, with no failures and one release-tag lookup warning in the
reconstructed offline repository. The first full preflight was stopped after
its static batch exposed three stale guards and a generated-agent module-fetch
failure. It did not complete its unit/live batches; no full pass is claimed.

- [x] Phase 111e checks the real guarded compactor call chain, not its old location.
- [x] Phase 83e anchors field checks while accepting gofmt alignment whitespace.
- [x] In-progress implementation is explicitly strict, not a planning/all-SKIP waiver.
- [x] Regression fixtures cover in-progress/candidate status and unknown `In review`.
- [ ] The complete final preflight and both platform test suites pass together.

The classifier regression failed before its correction. Updated phases 111e,
83e, 223 and 229, the full wave-v1.25 prompt-composition test, scoped integration
lint, shell syntax and whitespace checks pass. Phase 184 still fails locally
because its generated agent cannot resolve dependencies with the unavailable
module proxy; that failure was not relabeled as success or bypassed. All
production deadlines, N=128 concurrency workloads and release gates remain.

## Active concurrency follow-up

- [x] The served-driver fixture preserves the complete 128-task accepted burst
      with the production default queue capacity (`8f7231b`); zero-drop and
      identity/order regressions reproduce the old fixture's lost events.
- [x] Retained validation avoids discarded opaque-value copies and redundant
      opaque-root scans while preserving strict typed metadata and final decoding.
- [ ] Hosted full-suite validation confirms the task-delivery fixture correction.
- [ ] Hosted retained-context persistence/cleanup deadlines pass under the unchanged
      five-second production limit and 128-session workloads.
- [ ] Preserve typed retirement refusal when it becomes visible during override
      resolution; deterministic regression and production correction are under review.

CI `35672893508` on `324343b` passed Linux and S3 but still failed macOS. That
run also exposed embedded retention/steering persistence deadlines and a typed
retirement error reported as `runloop_error`. The task burst fixture correction
is not a claimed fix for those independent failures. Local validation and bounded
allocation measurements are recorded in
[the concurrency review](portable-context-concurrency-review.md). Final-tree
release gates above remain unchecked until their actual completed results exist.
