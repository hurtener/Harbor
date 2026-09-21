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
- [ ] Temporary publication/source-recovery workflows and payloads are removed.
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
