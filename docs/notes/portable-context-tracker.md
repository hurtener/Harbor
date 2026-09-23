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

### Steering text reaches actual requests — bounded repair

On published `957880c9c99ace6b24b6acef6ef338e1f4e43f88`, the ReAct
request builder never read `Control.UserMessages`. Without a dispatch
checkpoint, the runtime also discarded that correction after one boundary.
Two new real-request regressions reproduce both failures (ReAct **FAIL
0.614s**, steering **FAIL 0.396s**). They exercise the actual ReAct planner and
run loop, not merely control-history persistence.

The repair appends fresh steering verbatim as user messages, preserving FIFO,
whitespace, large numeric strings and the unchanged system prefix. It records
inert context in the existing trajectory for subsequent steps even with
cross-run memory disabled; it does not enable cross-run persistence in that
mode. Enabled cumulative memory still requires its existing context checkpoint
before inference. No new store, wire type, system guidance or tool policy.

Source tree `54837394de3039ac49280391e888f45295e120a1`, Go 1.27.1
darwin-arm64: `GOFLAGS=-p=1 go test -race ./internal/planner/react
./internal/runtime/steering -count=1 -coverprofile=<temporary-file>` passes
both complete packages: **1.742s / 1.639s**, **88.1% / 88.0%**. Focused
regressions pass with and without the required-checkpoint seam. Targeted vet
and the Go-1.27-built golangci-lint 2.12.2 pass (zero issues).

This is not in-flight Steer completion: interrupt/re-plan, pending-tool
invalidation, explicit unsupported-attachment rejection, real control-endpoint
acceptance and consumer pending/applied UX remain open. No tag, deploy or model
call. Parent CI `35848630795` still runs both platform tests. Its lint job
**failed** inside staticcheck's Go 1.27 standard-library analysis
(`poll`, `unexpected expr: *ast.KeyValueExpr`); rebuilding 2.12.2 is not enough
on Linux. Tool compatibility needs repair without disabling checks. Existing
macOS journal deadlines, coverage and release acceptance remain unresolved.

### LLM-free HTTP manifest fixture repair

On published `5a2f9d774cb02c7b79a1e1cc751c2553c0d495d5`, reproduce all five
Phase 149 failures with `GOFLAGS=-p=1 go test -race ./test/integration -run
'^TestE2E_Phase149_HTTPManifestBoot_' -count=1`: **FAIL 1.023s**. Its fixture
deliberately has no LLM and directly invokes HTTP tools, so explicitly set
`memory.strategy=none` there. No production code/default, workload, timeout,
assertion or retry changes; N=128 catalog concurrency is preserved.

On source tree `d5e23350a40cebd10ac1587676dcab9d5942dc3c`, Go 1.27.1 darwin-arm64,
the same race command now **PASS 3.648s**. Targeted assembly
tests for partial-stack failures, rolling memory without an LLM and automatic
compactor construction pass **2.225s**; full integration-package lint reports
zero issues. Production still fails loud for rolling memory without an LLM.
This closes that fixture regression only, not the macOS cleanup deadline or
final hosted/full-suite/service-backed acceptance. Parent `5a2f9d77` CI
`35848388762` and docs `35848388577` were running before this publication.

### Provider cancellation transport repair — published increment

Built on published `63f07c05cf008b32bb71d86c2ef946f058e5dc58`, same draft PR.
Adopt official Bifrost core **1.9.0**, the first inspected release containing
upstream [PR #7104](https://github.com/maximhq/bifrost/pull/7104)'s context-aware
RoundTripper. It closes the socket before response headers, not merely Harbor's
reader. No fork, proxy, provider SDK, new provider-route entry or plugin enabled.
The module requires Go 1.27; pin **Go 1.27.1** across module, CI, release and
scaffold. Rebuild the unchanged pinned golangci-lint **2.12.2** with that toolchain.

Review the intervening upstream changes at Harbor's consumer boundary: request
shaping/reasoning controls, provider switching, stream/usage normalization and
nested cost reports. Adapt the cost schema without relabeling request surcharges,
search or sidecar charges as token costs; retain the reported authoritative total.
Regression fixtures cover nested, legacy, total-only and explicit-zero reports.
The actual OpenRouter wire request preserves **128000** completion tokens and
**high** reasoning. This proves translation, not provider acceptance of that limit.

Local evidence, Go 1.27.1 darwin-arm64, `GOFLAGS=-p=1`, race tests `-count=1`;
tested source tree `2a263fa98e21748244505fa1afa85ae73d035655` (publication adds
this receipt):

- Full `go test -race ./internal/llm/...` passes, including governed receipts,
  broker credentials, corrections, retries, reasoning and native-provider wire
  fixtures. The final full Bifrost/steering/dispatch/parallel/scaffold race pass
  is **5.708s / 1.597s / 1.697s / 1.391s / 1.483s**. No live-model opt-in tests run.
- The previously failing pre-header socket probe now passes for both OpenAI and
  OpenRouter, streaming and unary. Established-stream cancellation also passes;
  deadlines and observation windows are unchanged. The full Bifrost coverage
  measurement before the final wire-only test addition is **81.9%**, still below
  Phase 233c's 90% target; not a coverage acceptance claim.
- Real HTTP/JWT/JWKS `TestE2E_NonAdminToken_SteeringContract` passes **2.098s**,
  including scoped hard Stop and N=128 identity isolation.
- Repository-wide `make lint` (zero issues), `make vet`, and **full `make build`**
  pass, including the freshly built Console and `CGO_ENABLED=0` binary. Final
  added Bifrost tests also pass targeted lint/vet. Scaffold golden regenerated
  through `TestScaffold_Golden_MatchesAcmeAgent -update`, passing **2.241s**.
- Module verification, Markdown (599 files), mirrors and drift audit pass:
  **1,592 OK / 0 WARN / 0 FAIL**. All three Protocol generation checks pass.
  Go 1.27 reflects `json.RawMessage` as its `jsontext.Value` alias; canonical
  generated Go-reference rows changed accordingly. No wire or TypeScript shape
  changed, and no generated file was hand-edited.

This resolves the local pre-header transport blocker below, **not release
acceptance**. The exact published parent `63f07c05` hosted performance gate now
passes; its Linux/macOS jobs were still running and docs passed. Older
`9524fef1` CI `35843299832` completed failed: both platforms exposed Phase 149's
LLM-free manifest fixture inheriting the new rolling default; macOS additionally
hit retained journal cleanup deadlines. Those fixes, coverage targets, final
service-backed/full-suite gates, in-flight Steer, consumer Stop/Queue lifecycle,
cumulative legacy retirement and matched live UI acceptance remain pending.
No RC tag, deployment, Workbench change, model call or service deletion here.
Harbor preflight remains explicitly owner-waived, not passed.

### Earlier hard Stop increment — historical evidence

Follow-up on published `6bc266ea`: hosted CI `35845869347` failed its performance
gate because `SteeringApply_EnqueueDrain` grew from 256 to 416 B/op (+62.5%).
Taking the address of the enqueue argument made every event escape to the heap,
even ordinary soft controls. Retain a branch-local copy only for accepted hard
Stop instead. No benchmark, threshold, workload or cancellation semantics change.
The unchanged benchmark, Go 1.26.4 darwin-arm64, six 100ms samples before/after
on this host, confirms **416 B/op / 6 allocations -> 256 B/op / 5 allocations**.
Full steering `-race -count=1` passes **1.650s**. This is a local repair of that
specific allocation regression, not a substitute for final-head hosted validation.
Source tree `98494be84ef22ca1a41b1e34740e85315082a27f`: targeted lint reports
zero issues; the existing performance-gate parser passes the six-sample pair
with its unchanged 30% threshold. Markdown passes; no performance baseline reset.

Built on published `9524fef16daa0c1a2cfcd970a5d5f18d2c1fa0f1`, same PR/branch.
The verified control inbox now interrupts its own execution context immediately
for `hard: true`, with identity isolation and atomic cancellation versus terminal
completion. Queued dispatch rejects cancellation; late planner success cannot
win after accepted Stop. Returned tool evidence and settlement are preserved.
Terminal bookkeeping retains identity in a separate five-second cleanup context.
Hard Stop suppresses new external completion-hook/naming dispatch (D-478).

The real local provider-stream probe reproduced a pooled-reader close race on
Bifrost 1.7.4 / fasthttp 1.71.0. Pinning fasthttp 1.74.0 and its required module
graph fixes that probe under `-race`; Bifrost remains 1.7.4 and Go remains 1.26.4.
Queued late stream chunks are rejected after cancellation is observed, and both
routed and ordinary provider failures preserve typed context cancellation rather
than misclassifying it as an outage. No provider-specific API or new service.

**Still blocking provider-termination acceptance:** cancellation before response
headers returns to Harbor but leaves Bifrost's underlying network call open.
The deterministic `TestDriver_CancellationBeforeHeadersClosesUpstream` fails on
this candidate after a one-second observation window; this is not a successful
Stop gate. The diagnostic probe and red logs are retained outside the working
tree for the next dependency repair. The pinned upstream
`providers/utils.makeRequestWithDoFunc` explicitly does not cancel the underlying
fasthttp call. Do not hide this with longer deadlines, sleeps or skipped gates.

Upstream now documents a context-aware RoundTripper repair in
[the Bifrost 2.2.0 transport release](https://github.com/maximhq/bifrost/releases/tag/transports%2Fv2.2.0),
issues #7034/#7104. The inspected core 1.8.0, 1.9.1 and 1.10.0 manifests require
Go 1.27.0; adopting a newer core entails a toolchain/dependency compatibility
review, not merely the fasthttp pin. No upstream fork, local module replacement,
proxy, or toolchain change was introduced for this increment.

Local evidence on source tree `228fbf78a980cb9777e0a74245da5ec1a5123eac`,
Go 1.26.4 darwin-arm64, `GOFLAGS=-p=1`, `-race -count=1`:

- Full steering, dispatch, parallel and Bifrost package suites pass:
  **1.801s / 1.574s / 1.472s / 7.216s**. Coverage is
  **86.3% / 78.3% / 91.5% / 80.8%**. Steering meets 85%; dispatch remains below
  85% and Bifrost below the later Phase 233c 90% target. These coverage gates
  are not satisfied. Live-provider opt-in tests were not run.
- Real HTTP/JWT/JWKS control endpoint regression passes **2.277s**, including
  foreign-user rejection, blocked model/tool interruption, late-finish rejection
  and N=128 isolated controls. Unit tests also cover N=128 run isolation,
  cancellation during intent persistence, cleanup failure and the inverse
  completion-before-Stop ordering. No concurrency reduction or test retry.
- Repository-wide `make lint` and `make vet` pass. Initial drift audit found
  decision IDs in three new godoc comments; those references were removed.
  The runtime is unchanged; lint/vet rerun on corrected tree
  `b6704ef4aaebecefb0856c5bef11e90ff8087dfe` also passes.
- Markdown (599 files), root/template mirrors and corrected drift audit pass:
  **1,592 OK / 0 WARN / 0 FAIL**. The final publication adds only evidence text
  to the checked implementation. No full release build, service-backed full
  suite, browser acceptance or complete provider-cancellation pass is claimed.

In-flight steering, consumer Stop/Queue lifecycle, cumulative-memory legacy
retirement and live acceptance remain pending. No RC tag, deployment, model call,
Workbench change or old-service deletion accompanies this increment.

Exact published-head CI check before this increment: `9524fef1` run
`35843299832` still runs Linux/macOS tests; all completed ancillary jobs pass.
The matching docs run `35843299831` passed.
Running jobs are not approval, and this does not resolve earlier cleanup failures.

### Default cumulative activation — implementation increment

Ordinary YAML and `config.Defaults()` now select `rolling_summary` with
`recent_turns: 20`. Explicit `none` remains stateless. The 100-turn/five-generation
served and embedded regressions now consume these defaults instead of manually
enabling memory; the scaffold and shared devstack also assert the default.
Configuration, sample, scaffold and operator documentation describe the changed
retention behavior and governed compaction calls. No live configuration, model
prompt, Workbench guidance, dependency, timeout or concurrency workload changed.
The legacy pair engine/interfaces remain to be removed; this is not complete
owner consolidation or RC acceptance.

Go 1.26.4, darwin-arm64, `GOFLAGS=-p=1`, `-race -count=1`, dedicated
PostgreSQL 17.11 on all applicable runs, excluding unfinished Stop edits:

- Tree `be7fee6a7c05485407205db07bd08e00241ea236`: 900 embedded and 300 served
  deterministic turns pass across in-memory, SQLite and PostgreSQL, preserving
  the turn-1 constraint in actual requests and previous-checkpoint input across
  multiple windows/restarts. Embedded selection **92.836s**, served **14.616s**.
  Explicit stateless opt-out regressions also pass.
- Tree `dd93bb1459c40e41cb092bdc74e11db38c43343f`: full config, embedded assembly,
  devstack, SDK assembly, scaffold and SDK-sample race suites pass. Coverage:
  **83.0%, 84.3%, 81.6%, 100%, 82.4%, 80.5%**, respectively. Full served coverage
  measured 85.1%, but that initial run **failed** the disabled-recovery test,
  which implicitly assumed omitted memory configuration still meant disabled.
- Tree `36dd637ff7eb66d4b155b1064f135374b9cc3321`: the disabled test explicitly
  selects `none` and additionally verifies that default-enabled recovery refuses
  absent evidence. Full canonical served race/coverage rerun passes **52.424s /
  85.1%**, meeting its documented 85% target. No grouping, skipped roots, weaker
  assertion or denominator change. Runtime code is unchanged from the earlier
  tested trees; only tests, smoke selection and documentation were added.
- Whole-repository lint on that isolated tree passes with **0 issues**, and
  `GOFLAGS=-p=1 go vet ./...` passes. Markdown
  (599 files, zero errors), root/template mirrors and smoke shell syntax pass.
  The unchanged published `14f516da` config package also measures 83.0% under
  the same race/coverage command; the default change did not reduce it.
  Config and assembly coverage remain below their 85% floors; all other
  outstanding coverage/release gates remain explicit blockers.
- The CGO-disabled portable-context sample builds and its `-h` succeeds on
  tree `36dd637f`. This is not a full Console/release build or live-model test.
- Working-tree drift audit passes **1,592 OK / 0 WARN / 0 FAIL**; unfinished
  Stop edits were present only for this coherence check, not the isolated Go
  gates. The disposable PostgreSQL instance was stopped after validation.

Cleanup diagnosis before this increment: the unchanged `14f516da` served suite
passes under `GOMAXPROCS=2` (**54.666s**). Its unchanged N=128 cleanup test also
passes with CPU/mutex/block profiling (**7.647s**). Moving StateStore byte copies
outside the shared map lock did not improve the comparison (**7.686s**; aggregate
driver lock delay increased from 228.69s to 268.92s). Aggregate goroutine delay is
not wall time. The candidate and its extra tests were discarded, not published.
This does **not** resolve the earlier hosted cleanup failure. Hosted run
`35840104883` on `14f516da` was still running both platform test jobs at this
checkpoint; completed ancillary jobs and docs `35840104837` passed. No run was
cancelled/restarted, and no new tag, deployment or real-model call was made.

### Cumulative inspection and mutation — implementation increment

Rolling-memory List/Get/Health/StrategyTrace now read the execution owner's
committed checkpoint, bounded evidence and recent tail. They do not read the
obsolete pair record or expose unsettled journals. Stable source keys survive
raw-turn rollover; viewing from another run does not change them. Source expiry
is reported without renewal. Serving reports the actual StateStore driver for
the cumulative projection. The existing Protocol shape and admin/identity gates
are preserved; the SDK exports the new mandatory inspection vocabulary.

Administrative Put redacts a conversation note before attaching host metadata,
returns its committed source key, and refuses capacity instead of evicting
unsummarized history. Delete conditionally removes the source, invalidates any
affected checkpoint and fences active admissions in the same session. CAS
conflicts reload current state so a newer sibling's tail is preserved. Required
persistence failures preserve committed state; no external action is replayed.

**Not complete:** expiring heavy `memory.get` values now explicitly refuse the
artifact-export path because it cannot yet bind the copy to source deletion and
expiry. This prevents a new unbounded private copy but is a release blocker,
not a successful detail read. Source-bound large-value retrieval, retirement of
the old pair-store/loop and remaining non-cumulative consumers, default rolling
activation, compact references, coverage and live acceptance remain pending.
No compatibility reader for old rolling records was added. Non-cumulative
strategies retain their existing behavior during this incomplete migration.

Focused tests exercise actual outgoing requests after note insertion/deletion,
checkpoint invalidation before/during inference, late-settlement refusal,
source-key stability, failure/capacity/expiry preservation, strict redactor
shapes, cancellation, 128 concurrent identities, independent PostgreSQL pools,
driver round trips and HTTP handler admin/cross-identity boundaries. Phase 269
includes these new inspection tests. The Protocol playbook describes the same
semantics and the outstanding heavy-value refusal. Unfinished hard-Stop changes
remain separate; no tag or deployment is part of this increment.

Local validation uses isolated source tree
`278730b0b64780e4d533bc2b0ebb437c168a67c9`, Go 1.26.4, darwin-arm64,
`GOFLAGS=-p=1`, `-race -count=1`, excluding all unfinished hard-Stop edits:

- Full `./internal/memory/...` passes with dedicated PostgreSQL 17.11 enabled.
  Statement coverage: parent memory **89.8%**, in-memory driver **96.3%**,
  PostgreSQL driver **82.1%**, SQLite driver **73.6%**, Protocol **87.6%**,
  session owner **84.4%**, legacy strategy **82.6%**. Session memory (92% floor),
  SQLite (85%) and strategy (85%) remain below target. The helper conformance
  package measures 66.8% and has no independent floor. No target or denominator
  was reduced; no fresh whole-served-package coverage is claimed.
- `HARBOR_PG_DSN=<dedicated test database> GOFLAGS=-p=1 bash
  scripts/smoke/phase-269.sh` passes **15 OK / 0 SKIP / 0 FAIL**. This includes
  1,200 deterministic turns, the same N=128 workloads, independent PostgreSQL
  pools, exact 14,660-byte receipts, large versions, `more:false`, source lifetime,
  restart, recovery, steering/attachment continuity and the new inspection tests.
  Embedded retained/cumulative selection: **96.724s**; served: **16.281s**.
- Full planner and Protocol stream race suites pass (**1.418s / 20.385s**).
  The production mux inspection test and Phase 83f/84e integration selections
  pass (**2.150s / 2.093s**); the SDK memory facade compiles. Affected-package
  vet and repository-wide lint pass, with a dedicated lint cache and **0 issues**.
  Earlier lint attempts found two unextended integration-test wrappers and two
  source/style issues; all were repaired, not suppressed.
- `CGO_ENABLED=0 GOFLAGS=-p=1 go build -o <temporary>/context-lab
  ./examples/portable-context` and the binary's `-h` pass. This is a sample
  build, **not** a full Console/release build or real-model acceptance.
- Markdown (599 files, zero errors), mirror and Phase 269 shell syntax pass.
  The dedicated PostgreSQL instance was stopped after testing. Publication adds
  only this tracker receipt and a godoc-hygiene wording correction to the tested
  source tree; runtime code is unchanged. The initial working-tree drift audit
  reported **1,591 OK / 0 WARN / 1 FAIL** for a decision number in that godoc
  comment; it was corrected without changing the checker. The corrected audit
  passes **1,592 OK / 0 WARN / 0 FAIL** (unfinished Stop edits were present only
  for this working-tree coherence check). Isolated lint also passes again on
  tree `bf94a00b5ed91f8760a7e7ead6da03a00f92ea25`, which adds only that comment
  correction to the tested tree.

The previous published head `39b7179e464b9e6d9668d61ee3a33dde77946238`
finished hosted run `35834720307` with failures: both platforms found the stale
`PlannerConfig.TokenBudget` reflection-exclusion entry; macOS also reproduced
`TestRetainedServer_ConcurrentReuse`'s five-second journal-cleanup deadline
failure (13 scopes). This increment removes only the obsolete exclusion entry;
the full planner suite now passes locally. **The cleanup failure remains open**;
local Phase 269 success does not establish its repair. Deadlines, workload,
assertions and race detection are unchanged. Both builds and downstream
Playwright/preflight skipped; other completed jobs and docs run `35834720387`
passed. These are previous-head results, not new-head acceptance. Owner-waived
preflight is not green. No run was cancelled or restarted.

### Previously published cumulative owner relocation

The execution-memory implementation and all 58 named retained-context tests now
live in `internal/memory/session`, below runtime composition. Serving, embedding
and explicit recovery call that same implementation directly. All seven moved
production files are byte-identical to the previous head after the package-name
change; no algorithm, storage format, public Protocol field, prompt, timeout or
workload changed. Phase 269's smoke selectors include the new package, so the
move does not silently omit those tests.

This is a dependency-direction step toward D-477's single owner, not completion
of the migration. The old pair-store/summary loop and its inspection, mutation
and retrieval consumers still need retirement. No wrapper, second engine, new
storage service, default flip, RC tag or deployment is introduced here. The
unfinished hard-Stop edits remain separate from this increment.

Coverage stays a release obligation: the relocated package inherits the
strictest touched runctx target (92%). Moving code does not lower that target
or exclude uncovered production branches. The remaining runctx package retains
its own target. Historical measurements below retain their original paths and
revisions; they are not new-package acceptance.

Local evidence on isolated tree `1912a8862f7a8168d90429fd6338c1678aed717c`
(Go 1.26.4, darwin-arm64, `GOFLAGS=-p=1`, `-race -count=1`; no unfinished
Stop edits):

- `Retained|Cumulative|SessionMemory` selections pass in session memory,
  embedded assembly (**97.895s**), serving (**16.904s**), state/PostgreSQL
  (**2.193s**) and devstack. This includes the same 1,200 deterministic turns
  across in-memory, SQLite and dedicated PostgreSQL 17.11, plus the independent
  pool conformance checks. Test workloads, five-second production persistence
  deadlines and assertions are unchanged; this pass does not resolve the
  earlier intermittent concurrent-cleanup failure.
- Full race suites pass for session memory (**84.9%** statements), runctx
  (**92.1%**), devstack (**81.6%**), SDK assembly (**100%**) and the public sample
  (**80.5%**). The relocated memory package is still below its target; no
  denominator or target was reduced. No new served-package coverage is claimed.
- Affected-package vet passes. Repository lint passes with **zero issues** using
  a fresh dedicated cache. The first lint invocation reused diagnostic paths
  from a retired temporary checkout and failed; it is not counted as a pass.
  Markdown (599 files, zero errors), mirror and Phase 269 shell syntax pass.
  The working-tree drift audit passes **1,592 OK / 0 WARN / 0 FAIL**; unfinished
  Stop edits were present for that documentation/coherence check only.
- The dedicated PostgreSQL instance was stopped after testing. No shared
  database or deployment changed. The temporary source tree excludes all
  unfinished hard-Stop changes.

The published source differs from this tested tree only by this tracker
receipt. Final-head hosted CI, complete release gates and real-model UI
acceptance remain pending; waived local/web preflight is not green.

### Previously published cumulative rollover core

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

### Single memory activation — implementation increment, not release acceptance

`memory.strategy: rolling_summary` now selects the cumulative execution-context
path for served, development and embedded runs. `memory.recent_turns` controls
the detailed window (0 selects 20; maximum 32), not checkpoint age.
`sessions.retained_context_turns`, its environment override, and the SDK
`WithRetainedContext` option are removed. YAML/environment use of the removed
setting fails with migration guidance, including explicit zero/empty values.
`memory.strategy: none` remains the stateless choice. Recovery reads the same
configuration; no per-call override can select a different history policy.

Wiring review also found that the devstack driver had never received the shared
history configuration and the execution compactor omitted existing operator
summarizer model/prompt settings. Both are wired here, with a real devstack
two-turn regression and actual outgoing maintenance-request assertions. Prompt
configuration appends operator text to the existing baseline; no baseline
guidance or capability-specific prompt changed. The model override still uses
the governed client. Existing isolation, receipt, failure and fresh-result
tests now use the sole memory config, with the same test bounds and workloads.

The omitted-strategy default is deliberately not called migrated yet. The old
pair-store interfaces/loop and their Protocol/semantic-retrieval consumers still
need replacement before release: they must observe and mutate the same
cumulative owner rather than the stale pair store. This partial configuration
increment is not safe evidence of complete memory management or RC acceptance.
No deployment or tag is included.

Local evidence: Go 1.26.4 / darwin-arm64 / `GOFLAGS=-p=1` / `-race -count=1`,
isolated source tree `246d97a4232f68040eb4cea892797368d13d8a49` (unfinished
Stop edits excluded). Later tree `0df4eadc2e0b53ef20f1636330771ced24871d1f`
adds only the phase-plan documentation and the PostgreSQL fixture correction
described below; runtime source is unchanged.

- Retained/cumulative selections pass in runctx, embedded assembly and serving,
  including N=128 isolation, actual receipt/attachment requests, recovery,
  source expiry and deletion fences. Embedded time: **79.823s**; served:
  **16.124s**. These successful executions do not close the previously recorded
  intermittent five-second concurrent-cleanup failure.
- **1,200 deterministic turns** pass across the three StateStore drivers:
  embedded targets 0/1/100000 on each backend, plus served 100-turn cases on each
  backend. The dedicated PostgreSQL 17.11 run exercises 400 of these turns
  (**33.080s** embedded / **6.092s** served), with restarts and at least five
  checkpoint generations. The temporary PostgreSQL instance was stopped after
  testing; no shared service or deployment was changed.
- Full config, LLM summarizer, devstack, SDK assembly and portable-context sample
  race suites pass (**2.405s / 1.436s / 2.988s / 1.987s / 4.053s**). The
  devstack test drives actual assembly and two completed tasks rather than
  hand-constructing a driver that could conceal missing wiring. Affected-package
  vet passes.
- Hosted state/PostgreSQL job **107083299621**, run **35830990450** on prior
  published `2055d373`, failed `TestPostgres_RetainedContext_CheckpointAndSourceExpiry`:
  `checkpoint not rebound to next request: 0 <nil>`. Reproduced locally on the
  isolated tree before repair. This direct-compactor fixture bypassed the run
  loop's observation acknowledgement. The correction first proves unobserved
  evidence cannot compact, then marks only the older prefix observed, leaving
  the newest result fresh. No production guard or assertion was weakened.
  All four retained PostgreSQL roots pass (**3.946s**); the full service-backed
  state/PostgreSQL race package then passes (**3.527s**).
- Repository lint passes with **0 issues**, using a fresh dedicated lint cache
  after the shared cache referenced a retired temporary checkout. Markdown
  (599 files), mirror, VitePress build and drift audit
  (**1,592 OK / 0 WARN / 0 FAIL**) pass.
  Full local/web preflight remains owner-waived, not green. Hosted gates
  on the forthcoming published head, release coverage/build, full owner
  consolidation and live acceptance remain pending.

### RC increment disposition (historical releases)

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
