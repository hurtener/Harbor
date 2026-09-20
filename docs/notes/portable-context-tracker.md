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
- [ ] Complete bounded per-request context diagnostics.

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
- [ ] Sample-agent setup, migration and RC end-to-end test instructions are complete.
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
