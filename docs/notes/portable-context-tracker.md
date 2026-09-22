# Portable session context implementation tracker

RFC 002 / PR #779. Checked implementation items are published behavior, not RC
approval. The implementation head covered by this evidence is
`cea93340a298e0d1ba4eeda1849c89c457ff0b3d`; this documentation reconciliation
adds no runtime change. The current release blockers are listed separately below.

## Scope

Stowage owns long-term memory. Harbor retains bounded execution context using
its existing StateStore and governed Bifrost-backed client. No provider-native
compaction requirement, additional provider SDK, second public transcript,
automatic external-action replay, or default retention change is introduced.

## Slice 1 / Phase 268 — portable compaction

- [x] Runtime-owned coverage, recent/fresh exchanges and repeated compaction.
- [x] Bounded chronological summaries with strict shape/completion validation.
- [x] Failed, empty, truncated and non-shrinking candidates retain prior context.
- [x] Exact numeric restoration and source-bound checkpoint validation.
- [x] Assembled-request budgeting includes output headroom and full request input.
- [x] Governed maintenance identity, admission and accounting; strict grant checks.
- [x] Pinned OpenAI/Anthropic Bifrost HTTP integration without native compaction.
- [ ] Complete final-tree regression and non-preflight release acceptance.

## Slice 2 / Phase 269 — durable session continuity

- [x] Explicit served-root and embedded retention; override/disable stays explicit.
- [x] Legacy pair-only memory and trusted completion hooks are not duplicated.
- [x] Required admitted query, pre-dispatch intent and post-dispatch settlement.
- [x] Atomic generation-fenced terminal sealing and exact-generation cleanup.
- [x] Known receipts survive approval-bridge failure; pending effects stay unknown.
- [x] Native historical exchanges, exact evidence and current catalog/scope checks.
- [x] Source-bound cross-turn checkpoint reuse, with expiry/eviction invalidation.
- [x] Explicit embedded reconciliation and fencing of the original admission.
- [x] Authenticated own-session served reconciliation and typed Protocol clients.
- [x] Authorized result-reference recovery through existing artifact reads.
- [x] Applied steering continuity across requests, turns and SQLite recovery.
- [x] Attachment references commit atomically with the query; source lifetime is checked.
- [x] Independent-pool PostgreSQL conformance alongside in-memory and SQLite.
- [x] Strict custom-redactor host metadata validation before external dispatch.
- [x] Settlement rejects mismatched intent expiry and oversized stored intent before parsing.
- [x] Required custom redaction preserves action identity before dispatch and at terminal sealing.
- [ ] Final hosted and canonical-process acceptance under unchanged production deadlines.

## Slice 3 — diagnostics, integration and review

- [x] Fixed content-free failure messages and bounded prepared-request diagnostics.
- [x] Maintenance isolation and installed-checkpoint-only replay coordinates.
- [x] Request-prefix checks for append/rebuild, compaction, revocation and model changes.
- [x] Usage/cost availability, estimate flags and sparse streaming accounting.
- [x] Console wire-type parity and independent Protocol inventory.
- [x] Ambiguous typed metadata rejected; opaque tool fields remain data.
- [x] TUI fixture waits for actual SSE registration before synthetic delivery.
- [x] Served-driver fixture preserves the complete accepted 128-task burst.
- [x] Retained validation avoids redundant scans/copies without weakening checks.
- [x] Process-wide leak-count fixture excludes sibling tests, retaining its own concurrency.
- [x] Typed retirement refusal survives run-start configuration-read failures.
- [x] CI schedules independent stress packages sequentially, retaining every test and race check.
- [x] Temporary publication/source-recovery workflows and payloads removed.
- [x] MinIO registry corrected and actual hosted S3 conformance observed.
- [x] Stale source guards repaired; in-progress phase smoke remains strictly enforced.
- [x] Public-SDK editing sample, inspection, migration and RC procedure provided.
- [ ] Final adversarial review completed with no unresolved critical/high-severity findings.
      Two independent final-tree reviews found no P0; their two P1 findings are
      fixed at the current head, with narrow diff-only re-review still pending.

## Final release gates

- [ ] Canonical final-head Linux and macOS vet/test/build jobs pass.
- [x] Canonical PostgreSQL-backed served coverage reaches 85.1%, above its 85% target.
- [ ] Reconfirm every touched-package coverage result on the exact final documentation head.
- [ ] Final-tree full Go lint, race and build acceptance passes.
- [x] Final-tree drift audit passes: 1,592 OK / zero warnings / zero failures.
- [ ] Final-tree prior-phase smoke acceptance passes.
- [ ] Local and hosted preflight are owner-waived for this RC effort. They are
      intentionally skipped and are not recorded as green release evidence.
- [ ] Final-tree Protocol generation/lockstep and applicable frontend/E2E gates pass.
- [ ] Sample-agent RC procedure is executed against an approved release candidate.
- [ ] RC is published only after implementation and release gates pass.

## September 22 recovery and publication

The checksummed `d42423a` checkpoint was based on published `ac48de6`. Its runtime,
CI and test files were recovered rather than redesigned. The original local
commit IDs are not the new publication IDs; no remote history was rewritten.

| Published commit | Recovered change |
| --- | --- |
| `4f7d302` | Intent expiry/size guards and in-memory/SQLite settlement regressions |
| `b6ceb97` | Existing matrix test step uses `GOFLAGS=-p=1` |
| `c50ba57` | Typed retirement refusal and deterministic configuration-read tests |
| `9a97fa2` | Served-scope failure diagnostics and consolidated tracker |
| `30bcf7b` | Reject custom-redactor action-identity changes before external dispatch |
| `2d0f41c` | Served authority and failure-boundary coverage, raising the package to 81.9% |
| `2cfc99f` | PostgreSQL-backed served coverage and failure tests, reaching 85.1% |
| `ce684fc4` | Refresh the scaffold fallback module version and restore a clean drift audit |
| `cea93340` | Reject terminal custom-redactor action-identity changes before persistence |

The accompanying tracker checkpoint publishes the saved served-concurrency
failure diagnostics. They report synthetic scope/status/error details without
changing assertions, workload or deadlines. The tracker is consolidated here;
older detailed validation history remains in Git and the linked review notes.

## Evidence and limitations

Preserved combined-tree validation on Go 1.26.4 recorded complete runctx,
assembly and SDK assembly race passes, phase 269 with PostgreSQL at 14 OK / zero
skips / zero failures, full lint/vet/static build, 598 Markdown files without
errors, and 1,590 drift checks with one offline release-reference warning.
These are recovered results, not newly executed final-head checks.

At the recovery checkpoint, all 280 named served test roots passed in 19 fixed
process groups after the canonical local process exceeded the 4 GiB container
limit. Their aggregate statement coverage was **74.3%, below 85%**. That is
preserved historical evidence, not the current coverage result.

Commit `2cfc99f` added behavioral authority, rollback, rendering, provider and
run-loop failure tests through production seams. A canonical PostgreSQL-backed
race/coverage process then measured `internal/runtime/serve` at **85.1%**, above
its binding 85% target. An unchanged-tree service comparison held the preceding
81.9% result at 81.9%, so the gain comes from the new tests rather than merely
enabling PostgreSQL. Runctx, runtime assembly and SDK assembly retain their
separately measured 86.9%, 83.7% and 100% results. No production timeout,
N=128 workload, race instrumentation, file exclusion or coverage denominator
changed.

`30bcf7b` and `cea93340` close the two custom-redactor action-identity gaps found
during hardening. The former fails before external dispatch when a redactor
changes a tool target, call identity or control target; the latter applies the
same boundary before a completed historical action is sealed. Focused in-memory,
SQLite and real `RunOnce` race regressions pass, including positive redaction of
arguments, results, queries and answers. `ce684fc4` also restores the canonical
drift result to 1,592 OK / zero warnings / zero failures.

A controlled two-CPU experiment failed cleanup with overlapping package stress
suites and passed the identical workloads with package concurrency one. The
production five-second persistence bounds and each test's 128 sessions remain
unchanged. This supports the CI scheduling change, not a claim of macOS success.

Exact-head CI run `35697496723` on `cea93340` is still in progress. At the latest
check, Markdown, S3, skills/PostgreSQL and leak jobs had passed while the platform,
frontend and remaining conformance jobs were still running. No running job is
counted as green. Older Linux/S3/PostgreSQL/frontend successes do not establish
acceptance of this head. Local and hosted preflight are explicitly owner-waived
for this RC effort; that waiver is recorded as a skip, not a pass.

See [recovery evidence](portable-context-recovery-publication.md),
[concurrency review](portable-context-concurrency-review.md),
[release review](portable-context-release-review.md), and
[RC procedure](portable-context-rc.md).

Deterministic tests establish runtime/request behavior, not real-model editing
proficiency, prompt-cache savings or physical external effects. The live RC
sample-agent deployment and head-to-head model evaluation remain pending. No
paid call, merge, tag, deployment or RC publication is claimed by this tracker.
