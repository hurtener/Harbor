# Portable session context implementation tracker

RFC 002 / PR #779. Checked implementation items are published behavior, not
stable-release approval. The last fully covered implementation head is
`95bd401257a02c991317b66c99ae0907c208f5c6`; the current candidate adds the
provider-route attempt-boundary repair recorded below. The published
`v1.32.0-rc.2` tag peels to `ad6a724639ddf282631ff7cb1de25dfb1ecb1727`
and therefore does not contain that repair. RC evidence and current-candidate
evidence are kept separate below.

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
- [x] Two independent adversarial reviews of `742a76e..95bd401` completed with
      P0: 0 and P1: 0 after narrow diff-only re-review.

## Final release gates

- [x] Exact-head Linux and macOS vet/test/build jobs pass in hosted CI run
      `35742931975`.
- [x] Canonical PostgreSQL-backed served coverage reaches 85.1%, above its 85% target.
- [x] Reconfirmed on the release-candidate documentation tree: runctx 87.0%,
      runtime assembly 83.7%, SDK assembly 100%, and served runtime 85.1%.
- [x] Exact-head local `GOFLAGS=-p=1 make test` passes on Go 1.26.4; hosted
      lint and both platform test/build jobs also pass.
- [x] Final-tree drift audit passes: 1,592 OK / zero warnings / zero failures.
- [ ] Final-tree prior-phase smoke acceptance passes.
- [ ] Local and hosted preflight are owner-waived for this RC effort. They are
      intentionally skipped and are not recorded as green release evidence.
- [x] Exact-head frontend check/lint/unit/build and Console Playwright pass in
      run `35742931975`.
- [ ] The RC sample-agent evaluation completes after the downstream consumer fixes
      are published, deployed and live-retested.
- [x] `v1.32.0-rc.2` is published as a prerelease at the reviewed `ad6a724`
      target. It is not the current PR head or a stable release.

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
| `742a76e` | Add terminal custom-redactor refusal regressions; `v1.32.0-rc.1` target |
| `95bd401` | Persist canonical failed finish reasons, including `no_path`, across restart and Protocol projection |

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

The current implementation head adds the durable failure projection found by the
live RC exercise. `95bd401` maps only canonical non-goal planner failure codes
from `task.failed` into the closed turn finish-reason set. Its regressions pin a
failed `no_path` turn through SQLite restart and byte-identical
`sessions.turns.get` projection while retaining an independent error class. This
repairs an outcome-reporting defect; it does not change retained context or turn a
failed run into success.

On Go 1.26.4, exact-head local `GOFLAGS=-p=1 make test` passed. Hosted CI run
[`35742931975`](https://github.com/hurtener/Harbor/actions/runs/35742931975)
targets `95bd401`. At the latest evidence check, Linux and macOS vet/test/build,
lint, Markdown, frontend check/lint/unit/build, examples, PostgreSQL, S3,
performance, isolation, chaos, leak, mirror and Console Playwright jobs had
succeeded. Hosted preflight was still running and is not counted as green.
The docs build in run `35742932499` passed; its Pages deployment was skipped and
is not called green. Local and hosted preflight remain explicitly owner-waived
for this RC effort; the fact that the hosted job started does not convert the
waiver or an in-progress result into a pass.

See [recovery evidence](portable-context-recovery-publication.md),
[concurrency review](portable-context-concurrency-review.md),
[release review](portable-context-release-review.md), and
[RC procedure](portable-context-rc.md).

## RC publication and live acceptance

The annotated `v1.32.0-rc.2` tag object
`818595d34ae494148cc1b265e1419faa64cd1cfd` peels to `ad6a724`, including the
`95bd401` durable outcome repair but not the provider-route attempt-boundary
repair below. The GitHub prerelease was published on September 22, 2026. No
stable release or merge is claimed.

The disposable baseline failed its complex editing sequence. Against the RC,
Terra preserved the exact project state through complex revisions, a refused
operation, switching away from and back to the session, work in an unrelated
session, a runtime restart, a subsequent edit and an audit of the stored result.
That is strong live evidence that retained Harbor context materially improved
the target workflow and is a credible main-version candidate after the remaining
consumer acceptance gates.

The MiMo run did not establish context corruption. It exposed a long-running UI
that remained in a processing state without useful reasoning progress, an expired
coordinator-minted runtime token when Stop was attempted, and the durable `no_path`
projection gap fixed by `95bd401`. Those are terminal-outcome and consumer token/
UX defects and must not be reported as failed context restoration.

The first page reload roughly 25 seconds after the runtime restart also returned
transient 404s from `mcp.servers.read_resource` and `tools.describe`, so five
persisted Workbench panels failed to mount. A later full reload after capability
reattachment recovered all five, proving that the static resource URI and
persisted project/revision references were valid. The smallest consumer repair is
bounded read-only re-resolution/retry plus a visible Retry action; it must never
replay a historical tool call or presentation credential. The consumer repair also
adds one-time remint/retry for idempotent Stop and configurable per-runtime token
lifetime. It is committed locally but remains unpublished, undeployed and not
live-accepted. Workbench show-only PR
[#26](https://github.com/pengui-ai/prototype_workbench/pull/26) merged as
`a4c9a37e8c7687ed1ed60959eabbbd58c04f7d42`.

Deterministic tests establish runtime/request behavior, not real-model editing
proficiency, prompt-cache savings or physical external effects. Remaining
release acceptance is downstream publication/deployment, a live replay of the
post-restart panel and Stop paths, and a completed MiMo comparison with explicit
terminal UX. The PR stays draft.

## Provider-route attempt-boundary repair candidate

The first post-Workbench-deploy live retry failed on RC1 task/run
`01M35375KPP6JSAJSQ2J848FVE` at planner step 3 with
`llm: external provider route is invalid`. The run lasted beyond the bounded
credential-free selection lifetime. Pengui's resolver remained fail-closed and
its `/v1/provider-route` calls returned current exact-bound responses; increasing
that resolver lifetime would only defer the same boundary failure.

The Bifrost leaf had treated the already-admitted credential-free selection's
expiry as attempt authority. That is incorrect after an upstream governance,
compaction or correction wrapper legitimately outlives the selection: the leaf
already performs a fresh resolver call for every actual provider attempt. The
candidate therefore removes only the redundant leaf rejection of the old
selection expiry. It still requires the trusted runtime/Agent/task/run purpose,
validates the fresh resolved credential against Harbor's unchanged five-minute
ceiling, and exact-matches provider, model, key label, endpoint, route and all
generations, model selector and model profile to the admitted selection before
one provider call.

Independent review of `44d09759` found one P1 at the preceding receipt
boundary: the outer `providerRouteClient` sampled its clock before the networked
selection call, so a selection could expire during resolver latency and still
reach validation and the inner policy chain. The follow-up samples the clock
again immediately after the resolver returns and rejects an expired-on-arrival
selection before either consumer. The original pre-call validation continues to
enforce the unchanged five-minute maximum lifetime; the Bifrost leaf continues
to refresh exact-bound credentials at the actual attempt boundary.

`TestDriver_ExpiredSelectionStillRequiresFreshExactResolution` deterministically
accepts the outer selection at its original clock instant, advances beyond its
expiry without sleeping, and proves the leaf performs exactly one fresh
resolution and one provider call. Restoring the old leaf check makes this test
fail before resolution with the live sentinel. Focused Bifrost and core LLM race
tests plus affected vet pass; broader exact-head gates remain required. Because
published `v1.32.0-rc.2` does not contain this runtime fix, a new RC tag is
required after review and release gates, before deployment or live retest. No
tag, release, merge or deployment is performed by this change.

`TestProviderRouteClient_RejectsSelectionExpiredDuringResolverCall` advances a
controllable clock inside the selection seam, proves the selection was valid at
request time but expired on arrival, and verifies that neither the selection
validator nor the inner chain runs.
