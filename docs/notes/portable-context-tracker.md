# Portable session context implementation tracker

## Current recovery direction — 2026-09-23

The owner-approved D-477 amendment makes cumulative session memory the required
outcome for PR #779. Use one `memory` configuration/owner/projector/compactor;
replace separate retained-context activation and the old pair-summary pipeline.
`recent_turns` limits detail, not the cumulative checkpoint's historical reach.
No backward-compatibility layer is required. Long-term memory remains external.

### Current cumulative-memory increments

- [x] Diagnose count eviction invalidating the checkpoint before replacement.
- [x] Reproduce loss in actual outgoing requests: with a 20-turn window, the
      turn-1-only constraint disappears on turn 22 in both in-memory and SQLite,
      despite 22 maintenance calls. Go 1.26.4, `GOFLAGS=-p=1 go test -race
      ./internal/runtime/assemble -run
      '^TestRunOnce_CumulativeMemory_FirstConstraintSurvivesWindowRollover$'
      -count=1`; both cases fail as expected on `fdb45861` plus local tests and
      in-progress hard-Stop changes. No cumulative implementation change was
      present. This is red evidence, not a passing gate.
- [x] Amend both RFCs, config docs, decisions and phases; add the real-request
      regression as an intentionally failing acceptance test. This first
      increment contains no cumulative-memory runtime fix or RC publication.
- [x] Implement cumulative generation/coverage and atomic rollover, with both
      served and embedded consumers in the same increment. Preserve failure,
      expiry/erasure, concurrent-sibling and unknown-outcome fences.
- [ ] Consolidate configuration, remove legacy paths, finish exact references,
      attachments, steering, recovery, SDK/examples and bounded diagnostics.
- [ ] Pass 100 turns / at least five generations across multiple full windows,
      all three stores, failure/restart/isolation cases and actual requests.
- [ ] Repeat matched real UI iteration across compaction boundaries.

### Cumulative rollover core — implementation increment, not release acceptance

The second increment replaces count eviction with conditional cumulative
publication in the existing StateStore slot. A checkpoint records one generation,
committed boundary and original source expiry; a bounded recent tail can roll
without invalidating that checkpoint. Both serving and embedding request the
existing compactor at history pressure, outside persistence deadlines. Missing
summaries or failed writes preserve the old state and report capacity/failure.
Admission order is now a session-local CAS-allocated sequence, not ULID sort
order: random entropy in one clock tick previously reordered sequential turns.

Local Go 1.26.4 / darwin-arm64, `GOFLAGS=-p=1`, `-race -count=1` evidence on the
implementation over `126e5a91` (with separate unfinished hard-Stop work present):

- 100-turn actual-request tests pass for both serving and embedding on in-memory,
  SQLite and PostgreSQL 17.11. Embedded tests cover token targets 1 and 100000;
  served tests cover storage-pressure compaction at target 100000. Every case
  checks at least five generations, the turn-1-only constraint, previous-summary
  inputs and storage/request bounds. SQLite/PostgreSQL embedded cases close and
  reopen the full stack after turns 25, 50 and 75. Served cases preserve a
  correction introduced on turn 31. These are deterministic transport tests,
  not evidence of real-model proficiency or an RC deployment.
- Real in-memory/SQLite seam tests pass for failed atomic publication, refusal
  to drop unsummarized history, non-ordered event IDs, expiry after rollover and
  restart, and a late publisher preserving a newer sibling's exact tail.
- An existing artifact receipt remains retrievable byte-for-byte after raw-turn
  rollover and SQLite restart: 14,660 bytes, version 9007199254740993, `more:false`.
  Deletion before/during inference still refuses the dependent decision.
- PostgreSQL's four retained-context roots pass, including independent-pool
  dispatch/reconciliation racing and strict host-envelope decoding. A dedicated
  native test instance was used; Docker's unrelated storage I/O failure was not
  treated as a service-backed pass or repaired by deleting its data.
- The earlier broader retained/compaction regression selection passes in
  runctx, assembly, served runtime and steering. Final full-package coverage,
  whole-tree release gates and new-head hosted validation remain separate.

The isolated staged tree `2fd903ed077b32d7fb9b5f9d476dc9debcb873d7`, excluding
all unfinished hard-Stop changes, subsequently passed repository-wide
`GOFLAGS=-p=1 golangci-lint run`; full `go test -race
./internal/runtime/runctx -count=1 -coverprofile=…` (**86.1%** statements);
and `go vet` on runctx, assembly, served runtime and steering. Its PostgreSQL-backed
`go test -race` selection `Retained|Cumulative|Compression|Compress|ContextPreparation`
also passed in assembly, served runtime, steering and state/postgres. This repeats
the 900-turn matrix without relying on unrelated working-tree changes. The only
subsequent change to this increment is this documentation receipt.

Reproduction: set `HARBOR_PG_DSN` to an isolated test database and run
`GOFLAGS=-p=1 go test -race ./internal/runtime/runctx
./internal/runtime/assemble ./internal/runtime/serve
./internal/state/drivers/postgres -run 'Cumulative|TestPostgres_Retained'
-count=1 -v`. Without that variable the PostgreSQL cumulative cases explicitly
skip; such a run is not three-store acceptance. Phase 269 now selects these
cumulative roots too.

**Still pending, not hidden by the passing no-tool conversation test:** the
single public `memory` configuration and removal of the pair-only pipeline;
compact reference representation for covered inline tool evidence (currently
preserved under the existing strict private byte bound, with capacity failure
rather than loss); archived-tool discovery, further attachment/recovery and
diagnostic integration; adversarial and real multi-window UI acceptance.
No old storage-format compatibility is provided. No RC tag or deployment has
been changed by this increment.

Hosted results for the previous published `126e5a91`: run `35823332227` failed
Linux/macOS only at the intentionally red turn-22 constraint test; the other
completed jobs passed and downstream Playwright/preflight skipped. Docs run
`35823332228` found an RFC 002 relative link broken by the VitePress include.
The source link is corrected to the published contract revision, without
disabling link validation; local `DOCS_BASE=/Harbor/ make docs` now passes.

Hard Stop, in-flight Steer and single-owner consumer Queue remain in scope.
Current unpublished hard-Stop tests pass through the authenticated control
endpoint for blocked model/tool work and reject late successful output; this is
not deployed or complete. A real pinned Bifrost 1.7.4 streaming HTTP cancellation
test closes the upstream connection but exposes a fasthttp close/read data race.
That transport boundary remains a blocker, not a waived test. No new dependency,
provider client or capability has been introduced to work around it.

### Working-input budget consolidation

The next bounded increment removes `PlannerConfig.TokenBudget` and its YAML/env
activation. Served runtime, embedded `RunOnce`, devstack and the SDK sample now
read `memory.budget_tokens`; removed YAML/environment settings fail loudly with
migration guidance. No output-token setting is changed, and existing narrower
virtual-agent input targets still apply.

Rolling-summary assembly constructs the existing governed compactor with a zero
explicit budget. Request preparation then derives its target from the actual
model's input capacity and output reservation. A regression found that restored
history was incorrectly treated as fresh when the explicit budget was zero,
preventing storage-pressure rollover at turn 21. Its settled/fresh boundary is
now independent of that knob; new results and attachments remain protected.

This is **not** the complete memory-owner migration: defaults, the separate
retention switch, pair-store interfaces/Protocol consumers and obsolete summary
loop still require consolidation. There is no new compactor, provider SDK,
storage service, deployment or RC tag in this increment.

Verification on isolated tree `dd916e2714d19d398e0f5684368ea5d3f326894c`, without
unfinished Stop changes, Go 1.26.4 / darwin-arm64 / `GOFLAGS=-p=1`:

- Full config, LLM and runctx race suites pass. Statement coverage: **82.9%**,
  **74.9%**, **86.1%** respectively. These measurements do not satisfy all
  package targets; config's 85% floor and LLM coverage remain release work.
  A fresh full-race baseline on published `4bba36f6` measures config **82.9%**
  and LLM **73.9%**: unchanged config coverage and a one-point LLM improvement,
  not a claim that the outstanding package floors are met.
- The same production tree passed **800 deterministic turns**: embedded
  in-memory/SQLite targets 0, 1 and 100000, plus both served 100-turn cases.
  Zero derives capacity from the actual model and compacts before the detailed
  20-turn window fills. First-only constraints, later corrections, previous
  summaries and restart checks remain asserted. PostgreSQL was **not configured
  for this increment**; its skipped cases are not a current three-store pass.
- Direct-compactor fixtures now explicitly mark the earlier observed prefix,
  leaving the newest result fresh. The complete runctx suite and focused
  embedded attachment/result-reference tests pass, including the assertion that
  unobserved execution evidence cannot be compacted. Production code is unchanged
  from the tree that ran the 800-turn matrix; later changes are fixture/smoke
  corrections only. Served/steering and Phase111e/WaveC integration selections
  pass. SDK/sample packages compile in that selection, but selected no tests.
- A subsequent broader embedded selection reproduced
  `TestRunOnce_RetainedContextConcurrentReuse` failing with `cleanup dispatch
  head: context deadline exceeded`. Preserve that failure. Single-run profiling
  controls on published `4bba36f6` and this candidate both passed at N=128
  (11.924s / 11.772s), with similar JSON/race-detector work. They do **not**
  resolve the intermittent deadline or replace the failed selection. No timeout,
  concurrency, assertion or retry policy was changed to obtain a pass.
- Repository lint reports **0 issues**; affected-package vet passes. The updated
  compaction smoke passes **28 OK / 0 SKIP / 0 FAIL**. Markdown (599 files),
  mirror and VitePress build pass; full local/web preflight remains owner-waived.

Hosted **previous-head** evidence: both platform Go jobs, frontend, lint,
service conformance and Console Playwright passed on `4bba36f6`. Its preflight
was still running at the last inspection. None of those results is final-head
approval of this next increment or the overall release.

### RC increment disposition

The RC4 signed per-tool retry-policy extension (`80165fd5`) and its dependent
restart compensation (`7434b645`) are withdrawn from this candidate. They were
not required for portable context or the requested interaction fixes. This is
a normal forward removal, not a rewrite of history or an existing RC tag.
The RC2 failure-reason fix, RC3 provider-route freshness/expiry fixes, and
post-RC4 routed schema classification remain intact. Historical receipts below
describe their original trees, not the newly narrowed candidate.

The removal restores the pre-extension signed descriptor and ordinary MCP/tool
behavior. Canonical Protocol generators regenerate documentation and TypeScript
artifacts. A strict descriptor decoder rejects unsupported authority-bearing
fields in both signed envelopes and persisted pairs: a downgraded reader must
not silently erase a newer restriction. Regressions first reproduced that
silent stripping, then passed with the rejection in place. No signed retry
configuration or enforcement feature remains.

Companion consumer removal must preserve already-applied migration history and
refuse any nonempty legacy policy rather than silently widen behavior. This
source increment does not change a deployed runtime. The known-working RC1
reference remains unchanged. The owner also requested hard cancellation,
effective in-flight steering, and single-owner queue lifecycle repairs; these
are subsequent implementation and real-control-endpoint acceptance work, not
completed by this removal. A restart-window preview race in a downstream
consumer is accepted follow-up debt for the current test milestone, not evidence
that retained context failed.

Validation on Go 1.26.4, `GOFLAGS=-p=1`, with `-race -count=1`: full agentcfg,
state-backed agentcfg driver, runtime agentcfg Protocol, tools, MCP driver,
Protocol types and single-source packages pass. Focused served signed-MCP and
attachment tests and the three Protocol generator packages pass. This is
focused removal validation, not final-tree main-release acceptance. Preflight
remains explicitly waived by the owner, not green.

## Historical publication evidence

RFC 002 / PR #779. Checked implementation items are published behavior, not
stable-release or consumer acceptance. The published PR head before the
retry-safety work was `e91791b8` (documentation); the reviewed PR head is
`7434b645`, including `80165fd5` and the restart-compensation follow-up.
Its RC3 implementation target was
`742ced8e3ec5d6197ed22edb448be89d45659753`. Annotated prerelease
`v1.32.0-rc.3` peels to that exact commit and includes the provider-route
attempt-boundary and expired-on-arrival repairs. Local, hosted and downstream
consumer evidence are kept separate below.

Annotated exploratory prerelease `v1.32.0-rc.4` (`d359e728`) peels to
`7434b645`; its six binaries/checksums were published as a prerelease. This
permits bounded consumer comparison, not main/stable release acceptance. The
coverage floors, hosted validation, and final consumer acceptance remain
independent blockers for a main release.

## Scope

Stowage owns long-term memory. Harbor retains bounded execution context using
its existing StateStore and governed Bifrost-backed client. No provider-native
compaction requirement, additional provider SDK, second public transcript,
automatic external-action replay is introduced. D-477 explicitly authorizes the
single cumulative-memory default/configuration migration; earlier no-default-change
statements below describe their historical revisions, not the current target.

## Slice 1 / Phase 268 — portable compaction

- [x] Runtime-owned coverage, recent/fresh exchanges and repeated compaction.
- [x] Bounded chronological summaries with strict shape/completion validation.
- [x] Failed, empty, truncated and non-shrinking candidates retain prior context.
- [x] Exact numeric restoration and source-bound checkpoint validation.
- [x] Assembled-request budgeting includes output headroom and full request input.
- [x] Governed maintenance identity, admission and accounting; strict grant checks.
- [x] Pinned OpenAI/Anthropic Bifrost HTTP integration without native compaction.
- [x] Complete final-tree local regression and non-preflight hosted release checks.

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
- [x] Final non-preflight hosted and canonical-process checks under unchanged production deadlines.
- [ ] Complete RC3 downstream deployment and live consumer acceptance.

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

## Signed MCP retry-safety correction: published for exploratory RC4

RC3 Workbench acceptance uncovered a separate capability-boundary P1: the
closed signed dynamic MCP descriptor had no per-tool retry policy, so
non-idempotent create/write/edit calls inherited four attempts. Local Harbor
work based on `e91791b` adds a signer-bound, bounded server-local
`tool_policies` retry ceiling through durable readback, replay fingerprints,
restart reconciliation, and MCP attach. A transport-level fixture observed
**four** outbound ambiguous mutations before correcting the policy shell's
explicit-empty retry-list zero check. Before PR publication,
`GOFLAGS=-p=1 go test ./internal/tools ./internal/agentcfg
./internal/protocol/types ./internal/runtime/agentcfg/protocol
./internal/runtime/serve ./internal/tools/drivers/mcp -count=1` passed and
the signed HTTPS MCP fixture observed **one** outbound mutation after a
gateway timeout at `max_attempts:1`. A separate static-policy test pins the
same one-attempt behavior; an unlisted read tool retains defaults. The
matching named-root `go test -race` across five touched packages passed; the
unknown-discovery-target regression checks deterministic candidate rejection
and corrected new-JTI registration. Independent review then found a restart
crash window: reconciliation had not routed that new typed target refusal
through the existing preparation-rejection compensation. The narrow published
follow-up exercises a physically active RevisionCommitted candidate,
restart discovery refusal, aborted fence, removed active authority, and
corrected new-JTI registration. It also defensively clones the signed policy
map in legacy and collection pair views. The focused normal and race tests for
these paths pass locally; pre-fix full-suite runs remain historical only.
Full revision-specific release gates and scored consumer comparison remain
pending. The implementation is published on PR #779 and included in exploratory
RC4, but is not yet hosted-verified, deployed, consumer-accepted, or main-ready;
no older-head green run covers it. On the published `7434b645` tree, service-backed
PostgreSQL 17.11 statement coverage measured served runtime at **85.1%** (its
85% floor), while Phase 233b/26b package floors remain unmet: agentcfg/protocol
79.0%, MCP driver 81.5%, tools/auth 79.1%, Protocol types 64.2%, Protocol
transport stream 68.9%, config 82.9%, and agentcfg StateStore driver 75.7%.
These are measured gaps, not waived targets. The first Phase 269 smoke under
concurrent host load was **13 OK / 0 SKIP / 1 FAIL** due to the known
five-second retained steering cleanup deadline. With the same N=128 workload,
production deadline, assertions and race detector, the isolated rerun against
a fresh PostgreSQL 17.11 database passed **14 OK / 0 SKIP / 0 FAIL**, including
two independent PostgreSQL pools. The concurrent failure is retained as
resource-contention evidence, not erased by the isolated pass.

On published `7434b645` with Go 1.26.4, `GOFLAGS=-p=1 make test`
(`go test -race ./...`),
`make vet`, `make lint`, `make build` (full Console), Protocol docs/TS/type
generation checks, `make markdownlint` (599 files, zero errors),
`make check-mirror`, and `make drift-audit` (1,592 OK / zero warnings / zero failures)
passed locally. Phase 268 smoke passed **10 OK / 0 SKIP / 0 FAIL**. The clean
dedicated-database Phase 233b two-runtime PostgreSQL reconciliation test also
passed under race. Local and web preflight were owner-waived, not green;
hosted CI and RC4 consumer comparison are distinct pending evidence. The
package coverage floors above keep the branch short of main/stable acceptance.

## Final release gates

- [x] Exact-head Linux and macOS vet/test/build jobs pass in hosted CI run
      `35742931975`.
- [x] Canonical PostgreSQL-backed served coverage reaches 85.1%, above its 85% target.
- [x] Reconfirmed on the release-candidate documentation tree: runctx 87.0%,
      runtime assembly 83.7%, SDK assembly 100%, and served runtime 85.1%.
- [x] Exact-head local `GOFLAGS=-p=1 make test` passes on Go 1.26.4; hosted
      lint and both platform test/build jobs also pass.
- [x] Final-tree drift audit passes: 1,592 OK / zero warnings / zero failures.
- [x] Phase 268 and 269 local smoke acceptance: 10/0/0 and 14/0/0 respectively;
      Phase 269 used PostgreSQL 17.11 with dedicated independent pools.
- [ ] Local and hosted preflight are owner-waived for this RC effort. The
      hosted job was still running at the last check and is not green evidence.
- [x] Exact-head frontend check/lint/unit/build and Console Playwright pass in
      run `35742931975`.
- [ ] The RC sample-agent evaluation completes after the downstream consumer fixes
      are published, deployed and live-retested.
- [x] `v1.32.0-rc.2` is published as a prerelease at the reviewed `ad6a724`
      target. It is not the current PR head or a stable release.
- [x] `v1.32.0-rc.3` is published as a prerelease at exact code head `742ced8`;
      release run `35773295028` succeeded. This is not merge, deployment or
      live consumer acceptance.

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
tests plus affected vet pass; later exact-head gates are recorded below. Because
published `v1.32.0-rc.2` does not contain this runtime fix, it could not
validate the repaired attempt boundary. RC3 publication is recorded below;
downstream deployment and live retest remain required.

`TestProviderRouteClient_RejectsSelectionExpiredDuringResolverCall` advances a
controllable clock inside the selection seam, proves the selection was valid at
request time but expired on arrival, and verifies that neither the selection
validator nor the inner chain runs.

## RC3 publication and exact-head evidence (September 22)

The preceding RC2 sections preserve their point-in-time history. The repair is
now published: annotated tag object `0ff41ccdb03bd9f0b0f48e0b2c8f137475c80539`
for [`v1.32.0-rc.3`](https://github.com/hurtener/Harbor/releases/tag/v1.32.0-rc.3)
peels to `742ced8e3ec5d6197ed22edb448be89d45659753`. GitHub published the
prerelease at 2026-09-22T19:24:31Z. [Release run 35773295028](https://github.com/hurtener/Harbor/actions/runs/35773295028)
succeeded with six platform binaries, checksum sidecars, aggregate checksums and
provenance. RC3 is a prerelease of the unmerged draft PR, not a stable release.

At exact `742ced8`, local Go 1.26.4 `GOFLAGS=-p=1 make test`, `make vet`,
`make lint`, `make build` and `make release-dryrun` passed. Protocol docs,
TypeScript and generation checks, Markdown, mirror and drift checks passed
(1,592 OK / zero warnings / zero failures). Phase 268 passed 10/0/0 and Phase
269 passed 14/0/0 using PostgreSQL 17.11 dedicated independent pools. The
canonical PostgreSQL-backed race coverage measured runctx 87.1%, runtime
assembly 83.7%, SDK assembly 100% and served 85.1%, meeting the served 85%
target. Local Console Playwright recorded 177 pass, 10 intentional skips and
zero failures; the page-coverage gate passed. The local PostgreSQL cluster was
stopped after testing. Two independent adversarial reviews and the narrow fix
re-review report P0: 0, P1: 0 for the runtime repair.

[Exact-head CI run 35768783311](https://github.com/hurtener/Harbor/actions/runs/35768783311)
passed all non-preflight jobs, including Linux/macOS Go race, vet and build,
Console Playwright, lint, PostgreSQL and S3 conformance. [Docs run 35768783408](https://github.com/hurtener/Harbor/actions/runs/35768783408)
succeeded. Preflight is owner-waived for this RC and was still running at the
last check; it is not a passing gate.

Downstream acceptance remains open. Fleet draft PR #107 has newer `fd6eaa1d`
with local PostgreSQL 17 five-store apply/verify evidence, but its hosted CI
failed before executing steps because of billing. The deployed Render fleet
still runs RC2 code `f7a2fc7`; the separate RC3 fleet candidate is not yet
deployed. Pengui draft PR #358 remains at `89a61c`, with zero-step hosted jobs
and no deployment. The Terra RC2/RC1 continuity observations above remain
historical live evidence; they are not an RC3 consumer test. RC3 still needs
deployment, authorized live editing and Stop/panel recovery checks, and an
explicit MiMo outcome. The sample 12,000-token planner input target remains
separate from provider completion limits. A future typed run-limit Continue
flow remains separate from runtime JWT lifetime and idempotent Stop retry.
