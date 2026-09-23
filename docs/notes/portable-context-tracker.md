# Portable session context implementation tracker

## RC9 live compaction checkpoint — 2026-09-23

Published implementation `ae4ca9a9565b99773a545787c4a12675ed37804a` is tagged
`v1.32.0-rc.9` (annotated object
`b57b9938082b3707ddef2a52eca8e350ee452729`). Public module provenance resolves
that exact commit, with zip checksum
`h1:UgxQLfHTREd6ugDnWBIxaFC9xdU/ySLGMcrJhuVNmXI=`. Release workflow
`35924027479` succeeded; native Darwin ARM64 checksum, GitHub provenance
attestation and version stamp verify. Full implementation CI `35923980566`
still had both Go platforms running at the latest observation; the other
thirteen completed jobs passed. This is not final main-release acceptance.

The isolated consumer reports RC9 and the exact implementation through
Protocol. The original long session resumed with its separate governed
maintenance route and unchanged YAML 64000-token working-input budget.
Task `01M3847DRXJMW4MV2KV0XC3QQ6` finished complete at 21:56:13 UTC, advancing
the same saved UI project from revision 14 to 15. It used Read/Edit/Show and
one unwanted, rejected Create; no exact-action replay was authorized.

Provider telemetry independently confirms the maintenance call succeeded:
`gen-1790200506-11Phiz4XxtX9V7r9vMVC`, resolved model
`inception/mercury-2.5-20260908`, 93304 native input tokens, 1921 native output
tokens (1540 reasoning), `finish_reason: stop`, 8826ms generation time,
cost 0.00402031 USD. The driving model remained Terra High. Provider-native
input tokenization differs from the estimator; this measurement is not the
size of the subsequent compacted planner input or a cache-savings benchmark.

Transcript projection remains a separate concern: the failed RC8 turn was
absent on reload at 21:30 UTC but present at 21:52 UTC, before RC9 deployment.
The completed RC9 turn was again temporarily absent on reload, despite the
confirmed terminal task and saved edit. Eventual projection is not permanent
memory loss, but exact catch-up latency/cause and clean reopen acceptance are
not established. Do not replay a saved edit to repair its presentation.

The next, read-only recall turn disproves repeatable acceptance. Task
`01M384F9CV5BJVR80FJW24KXE7` failed at 21:59:45 UTC with
`summarizer: incomplete trajectory summary`, followed by a retained terminal
capacity refusal. Generation `gen-1790200772-O9TgSqkFs4q4RFaDEzUD` reports
97111 native input tokens, 2036 output tokens and `finish_reason: length`.
No tools were dispatched; the requested recall answer was not produced.

Source inspection identifies a fixed 2048-token maintenance completion default
in `internal/llm/summarizer/trajectory.go`. Its existing constructor option is
not exposed by `MemorySummarizerConfig` or wired by assembly, so deployment YAML
cannot adjust that allowance. This is distinct from the configurable 64000
working-input target and the removed 512 KiB evidence ceiling. A truncated
summary must remain rejected. The detailed-window rollover deliberately refuses
to discard an oldest turn without complete summary coverage, which reproduces
the subsequent capacity-failure class while preserving the prior record/journal.
The precise live refusal branch is not instrumented and is not claimed proven.

Next narrow repair: expose the existing maintenance-output option through
validated YAML/environment configuration and the production assembly seam;
preserve the omitted default and test actual routed requests and truncation
refusal. Select the test deployment's larger allowance in configuration, not a
new framework constant. Separately verify failure recovery/checkpoint reuse and
transcript catch-up. No reasoning-policy, prompt, authority, baseline or storage
change is authorized merely by this diagnosis.

## Live independent-model schema repair — 2026-09-23

RC8 (`be1a794b554b4f716d4e2f739744a3a0b3109edd`) is published and the isolated
sample reports that exact version/commit through Protocol. Release workflow
`35920363015` succeeded with six platform builds and thirteen assets. The Darwin
ARM64 checksum, provenance attestation and binary version stamp were verified.
The working-input editor's override/save/reload/reset-to-YAML cycle also passed
against this runtime. This is not final release acceptance.

The first continued conversation failed before tools with `runloop_error`:
the first trajectory-compaction completion returned the content-free external
provider failure. Upstream telemetry recorded HTTP 200, but that status did not
mean inference succeeded. A small independent probe reproduced an error body
with code 502: the provider rejected the bare schema because its named
`response_format.json_schema` envelope was missing. The same probe with the
named envelope and the existing 2048-token maintenance allowance returned valid
JSON and `finish_reason: stop`. No user action was replayed by these probes.

The repair is generic Bifrost wire translation: wrap bare schemas with a name
and `schema`, preserve already-enveloped legacy input and its strictness setting,
and preserve large numeric schema constants. No provider-specific branch,
credential fallback, policy bypass, prompt change or output-limit increase is
introduced. Regression checks inspect actual run-loop and independently routed
maintenance requests, not merely the presence of `response_format`.

Go 1.27.1 verification on this repair tree passes: canonical
`GOFLAGS=-p=1 go test -race ./internal/llm/... -count=1`, the focused assembled
independent-route HTTP regression, scoped `go vet`, and golangci-lint 2.13.2
(zero issues). Canonical Bifrost race/coverage execution measures **82.3%**,
above the historical Phase 33 **80%** floor but below the later binding
Phase 233c **90%** target. This remains a main-release coverage gap.
Changed Markdown and diff checks pass.
The new schema-envelope regression fails on RC8 before the repair.

A new deployed continuation is still pending. RC8's Linux/macOS full CI jobs
remain running at this observation; its other completed jobs have passed.
Existing memory/served coverage and full-release blockers remain. The owner
authorizes a quick exploratory RC cycle, not a stable release or merge.

## Compaction route deployment override — 2026-09-23

Integration of published RC7 (`bbcfa275`) exposed that the existing environment
loader did not descend the optional `memory.summarizer.provider_route` section.
This narrow correction applies the six documented leaf overrides to that section
only, without broadening other optional configuration surfaces. Absent selectors
remain absent; partial/empty selectors and invalid uint64 generations fail loud.
The working-input target remains independent YAML data. No model, prompt, route
credential or runtime identity is hardcoded in the framework.

On this increment, Go 1.27.1 `GOFLAGS=-p=1 go test -race ./internal/config
-count=1`, `go vet ./internal/config` and golangci-lint 2.13.2 pass. Tests cover
environment-only selection, exact large generations, invalid/partial overrides,
YAML precedence and preserving unselected routes. These are configuration gates,
not a deployed compaction-model acceptance claim. RC7 publication workflow
`35919056022` completed successfully with all six platform builds and 13 release
assets; its immutable tag is not moved by this correction.

## Independent compaction route — 2026-09-23

On base `b8353ec3458e5cf89fd01952424be8ffc257d7a2`, the source now adds the
optional YAML `memory.summarizer.provider_route` selector (D-482). The existing
external resolver separately authorizes the compaction model under the same
admitted runtime/agent/task and verified tenant/user/session/run. The existing
governed Bifrost client owns dispatch and accounting; no new provider client,
resolver endpoint or credential store is introduced. An explicit route requires
a current model profile. Missing admission, stale/revoked selectors and signed
grants fail closed, without a fallback credential. The static model guard remains.

Compaction chunks use their own model capacity and summary output reservation,
not the driving model's large output limit or reasoning effort. The operator's
working-input target remains YAML data, with the published versioned admin
budget override. The independent route is restart-required and YAML-only.

Focused Go 1.27.1 race checks cover exact YAML generation values, invalid and
ambiguous configuration, independent-model chunk packing, N=128 concurrent
identity isolation, grant/refusal boundaries, and revocation before dispatch.
The assembled-runtime regression goes through the real composed Bifrost client
to a local HTTP provider fixture: it observes the separate model, a 2048-token
summary reservation, the resolved route credential, and no fallback call after
revocation. This is deterministic harness evidence, not real-model proficiency.

Canonical local package execution also passes on this source increment:
`GOFLAGS=-p=1 go test -race ./internal/config ./internal/llm/...
./internal/runtime/assemble -count=1` (assembly: 185.476s). PostgreSQL was not
configured for this invocation; its env-gated branches remain skipped, not
service-backed acceptance. Coverage targets and final hosted validation remain open.

Scoped `go vet`, golangci-lint 2.13.2 (zero issues), and changed-document
markdownlint-cli2 0.22.1 pass. No live route or deployment was changed for this
increment. Deploying the new budget editor, selecting the approved live
compaction route, and verifying that route during an actual multi-turn session
remain pending, alongside the existing full-release gates and consumer UI issues.

## Versioned working-input budget — 2026-09-23

The owner requires the deployment's 64000-token target to remain YAML data.
An optional admin agent-config `memory.budget_tokens` now projects the same
budget at the next run: absent inherits YAML, zero selects automatic sizing,
positive values override the working-input target. No output or storage ceiling
is introduced. The `agent_config_memory_v1` capability is advertised only with
both the configuration service and a served compactor. Invalid/unwired edits
fail loudly. Section-scoped edits preserve it; revisions, diff, CAS and rollback
remain the existing owner. This source increment is not yet deployed.

Focused tests cover round-trip/reset, malformed/unwired refusal, conflict,
diff/rollback, YAML precedence, failure injection, actual planner budget on
consecutive runs, and concurrent N=128 tenant isolation. Full release acceptance,
the independent compaction-model route and live editor verification remain open.

Local Go 1.27.1 evidence for this implementation increment:
`GOFLAGS=-p=1 go test -race ./internal/agentcfg/...
./internal/runtime/agentcfg/... ./internal/protocol/... -count=1` passes
(canonical package execution, not process-group coverage acceptance).
The focused served projection/next-run tests and `go vet` also pass.
No real-model calls or live configuration edits were made for this increment.

## Evidence-size correction — 2026-09-23

Owner direction: remove the legacy 512 KiB retained-evidence capacity, not replace
it with a larger hidden cap. Working-input compaction remains an operator YAML
choice through `memory.budget_tokens`; no deployment-specific token constant is
introduced in Harbor. D-480 records the corrected storage/input distinction.

On base `c41ce48c`, the new real-store regression failed on turn 8 in both
in-memory and SQLite after **seven successful compactions**. Command:
`GOFLAGS=-p=1 go test -race ./internal/memory/session -run
'^TestRetainedCumulative_LargeReceiptsStayBoundedAfterCompaction$' -count=1 -v`
(the test is renamed to `LargeReceiptsExceedLegacyByteLimitAfterCompaction`
after the owner clarified that exact stored bytes are not token-bounded).
This reproduces the storage failure class, not the exact private live payload.

The correction removes that ceiling from terminal records, intent/settlement
journals, recovery, historical-envelope decoding, administrative notes and
reference scanning. Keep all existing identity/redactor/expiry/erasure and
generation checks, unknown-outcome refusal, count/depth/schema bounds and
five-second production persistence contexts. Recovery still checks exact byte
accounting against its journal head; no evidence is silently clipped or replayed.

Locally verified with Go 1.27.1 on the correction tree:

- Real in-memory, SQLite and PostgreSQL 17.11 regressions preserve twelve exact
  64 KiB receipts through repeated compaction, including large version strings
  and `more:false`, past the old aggregate ceiling.
- Query, intent preamble, settled result and steering entries each larger than
  512 KiB survive explicit recovery; SQLite is closed and reopened. Pending
  external outcomes still refuse recovery. Large administrative notes round-trip.
- Canonical `GOMAXPROCS=2 GOFLAGS=-p=1 go test -race
  ./internal/memory/session -count=1 -coverprofile=...` with `HARBOR_PG_DSN`
  set passes: **84.7%**, still below the **92%** release target. The earlier
  no-service run passed but is not service-backed acceptance.
- Full `planner`, `llm` and `llm/summarizer` race suites pass; targeted
  `go vet` passes. Pinned golangci-lint 2.13.2 (built with Go 1.27.1) reports
  zero issues for the touched memory/planner trees. Changed Markdown, mirror
  and diff checks pass.

Published implementation: `8c6fc405cce7765615ca74a4f60093b1d842509a`.
The final focused embedded/served race regression also passes on this tree:
`HARBOR_PG_DSN=... GOMAXPROCS=2 GOFLAGS=-p=1 go test -race
./internal/runtime/assemble ./internal/runtime/serve -run
'TestRunOnce_(Retained|CumulativeMemory)|TestRetainedServer_' -count=1`
(166.720s / 31.721s). This includes actual-request multi-window checks, not
only persisted-state assertions.

Exploratory `v1.32.0-rc.6` points to that exact implementation (annotated tag
`efab61217c5381ba1d6fb7f4c80f3459c2b177bf`). Release workflow `35910506218`
passes all six platform builds and publishes thirteen prerelease assets.
The public module checksum is
`h1:X8Odny6hSJSBFt7z/VifxNDN7IHo5fJQo4FVaji1Et0=`. The owner explicitly
authorized quick RC cycles before final acceptance; this does not authorize
a stable release or merge. Exact implementation docs run `35909435652`
passes; main CI `35909435862` remains in progress at this publication.

The isolated owner sample has been deployed to RC6, with runtime-reported
version and commit independently verified through Protocol. Its existing
conversation resumed for a short post-restart edit. Task
`01M37WT20J4GHQKNR4C0GAEMA6` completed at 19:46:58 UTC after saving revision
14 of the same project, without the earlier terminal-capacity error. This
proves live persistence for this resumed turn, not a matched baseline benchmark.
Consumer activity after redeploy needed a browser reload, and replay showed
duplicate tool rows plus an empty app viewer. These remain separate consumer
acceptance issues; the successful runtime terminal state does not mark them green.
The owner-selected sample budget is 64,000 tokens in its deployment YAML.
Memory fields are currently restart-required and absent from the admin
agent-config Protocol. A separate static summarizer model is supported without
an external route/grant; a bound route still forbids changing its admitted model.
The requested administrative editor and independently authorized maintenance
route remain pending. Do not bypass route authority or silently fall back to a
different credential while adding them.

## Owner functional acceptance — 2026-09-23

The owner accepts the demonstrated cumulative-memory behavior: continuity over
successive UI edits, reuse of the same project and earlier requirements, and
focused reads rather than repeated reconstruction. The owner also reports
approximately **80% OpenRouter cache hits in normal interactions**, compared
with approximately zero before this change. This is an owner-observed live
measurement, not a controlled benchmark or an equivalent cost-saving claim.

The live candidate remains `v1.32.0-rc.5` at `4a6f187e`; the verified PR head
before this documentation increment is `570faedb34f11e929f0b31f5a15f0df4092f201e`.
CI run `35895153111` on that exact head has all 16 unwaived jobs passing,
including Linux/macOS race suites and builds, Console Playwright, performance,
lint and service-backed conformance. The owner-waived preflight job is still
running at this observation; it is not a pass. This supersedes the older CI
snapshot below, without claiming a root-cause repair for the previous flake.

The candidate reached revision 13 of the same sample project. A queued turn
started after confirmed completion without Resume/resubmission; a subsequent
queued turn also ran without resubmission and restored the first-turn-only
`Request cover` wording. The owner acceptance does not relabel the incomplete
stable-agent trial as a matched multi-window benchmark.

Release hardening is still pending separately. At 18:55:00 UTC, sample task
`01M37SKJ34GY814KXV6KCDRYTJ` failed its terminal write with
`retained execution context capacity exceeded`, after its external edit and
Show had succeeded. The next task completed at 18:57:14 UTC. Investigate which
storage/checkpoint capacity branch fired and how compaction handles the
retained evidence; this error is not proof of provider context exhaustion and
must not authorize replay of the already executed tool calls. The
`memory/session` 92% coverage floor remains open. No stable release or merge
is authorized by this functional acceptance.

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

### Consolidated memory owner — release acceptance pending

#### Service-backed coverage and adapter lifecycle acceptance

At `4d2267c4`, Go 1.27.1 with PostgreSQL 17.11 available through
`HARBOR_PG_DSN`, `GOMAXPROCS=2 GOFLAGS=-p=1 go test -race
./internal/runtime/serve -count=1 -coverprofile=served.out` passes the complete
served package in **61.548s**, with **86.3%** statement coverage against its
85% floor. This is one canonical package process, not merged grouped coverage.
It does not resolve the earlier hosted macOS cleanup timeout.

The following test-only increment covers memory adapter construction and pool
ownership through existing public constructors and real database drivers:

- PostgreSQL apply/verify initialization, failure before a usable adapter is
  returned, missing dependencies/removed strategies, closed-pool rejection,
  borrowed-pool usability after failed initialization and repeated close,
  reopening over that same pool, and owned-pool cleanup on migration failure.
- SQLite supported file/memory DSNs, explicit transaction mode, missing
  dependencies/removed strategies, invalid URI/unavailable parent rejection,
  and read-only initialization refusing migration without modifying the schema.

Complete `GOFLAGS=-p=1 go test -race` package runs with the same Go/PostgreSQL
versions and `GOMAXPROCS=2` pass: memory PostgreSQL **96.9% / 3.353s** and
memory SQLite **95.1% / 9.203s**, both above their 85% floors. These measurements
include all production files; no target, denominator, runtime deadline,
concurrency assertion, prompt or deployed configuration changes. The initial
test drafts needed fixture corrections (pgx parses a malformed DSN at Ping;
an opaque SQLite URI is not a malformed path), not production repairs.

The relocated `memory/session` 92% floor and final-head hosted/live acceptance
remain pending. Previous 100-turn evidence does not replace the matched
real-model test across multiple detailed-history windows and session returns.

#### Exact-head CI and tool-dispatch boundary check

Hosted CI `35888583397` on `6157c938` is terminal: Linux Go, performance,
frontend, lint, service conformance, isolation, chaos and leak jobs pass.
macOS fails `TestRetainedServer_ConcurrentReuse`: 18 scopes report
`cleanup dispatch head: context deadline exceeded` during first-turn terminal
persistence. Both platform vet steps pass; macOS build and downstream
Playwright are not passing evidence. The five-second persistence deadline,
128 concurrent scopes and race detector remain unchanged. A two-CPU local
profile passes the unchanged test in **7.350s** but records substantial shared
task/state mutex wait and JSON/redaction work. That is diagnostic evidence,
not a reproduction of the hosted failure or release approval.

The real-model UI trial still has a Show/Create mismatch: narration promises
to display an existing artifact but recorded tool actions create artifacts.
Provider metadata confirms streamed Terra completions with `tool_calls`;
provider I/O logging was disabled, so the raw returned function names cannot
be established from those historical logs. No content logging was enabled.

A new local HTTP/SSE boundary test exercises the real ReAct declarations,
pinned OpenRouter decoder and planner response projection together. Six
successive Create/Show/Edit/Show/Create/Show responses retain distinct long
source-prefixed names, matching schemas, call IDs and fragmented arguments
(including integer `9007199254740993`). Identical stream index zero on later
requests does not reuse an earlier name. Deliberately contradictory narration
does not override the structured tool choice. This is deterministic boundary
evidence, not a reproduction or repair of the live model failure.
Go 1.27.1, `GOFLAGS=-p=1 go test -race` over complete Bifrost, ReAct and tools
packages passes (**5.596s / 1.782s / 2.734s**); pinned golangci-lint 2.13.2
reports **0 issues** for Bifrost. No production code, guidance or live
configuration changes accompany this test.

#### Request-rebuild allocation follow-on

The test-only head `52c7c3b3` still fails hosted performance: run
`35887269383` reports declared-tool **+37.28%** and tool-free **+43.37%**.
The earlier docs-only head's declared-tool result was **+19.10%** with the
same runtime source; timing varies, so repeated green runs are not a repair.

Inspection found that ReAct always allocated its captured RunContext and
message-rebuild closure, even without a usable runtime compactor. The narrow
follow-on uses the request-preparation wrapper's same active/disabled predicate
before allocating the rebuilder. An automatic zero target remains enabled;
there is no new configuration, benchmark change or weakened budget/gate.
Actual-request regressions cover absent, nil, negative, automatic and explicit
preparation, identical rebuilt messages and no duplicate planner events.
The first three cases fail on `52c7c3b3` before the production correction.

Go 1.27.1, `GOFLAGS=-p=1`: complete ReAct, LLM and steering race suites pass
(**1.940s / 24.449s / 1.675s**). The nine real-store 100-turn combinations
(in-memory/SQLite/PostgreSQL 17.11 times automatic/1/100000 budgets) pass with
no skips (**124.086s**), retaining the first-turn constraint through five or
more generations and persistent-store reopen every 25 turns. This confirms
that eliminating unused closures does not disable cumulative compaction.
Pinned golangci-lint 2.13.2 across LLM/ReAct packages reports **0 issues**;
the same package-tree vet, tracker Markdown and whitespace checks pass.

Unchanged canonical-style local benchmarks (six 100ms samples) reduce each
path by exactly **752 B/op and two allocations**: declared-tool 14776/37 to
14024/35, tool-free 12312/24 to 11560/22. Those match the hosted stable-base
allocation counts. Before timing samples were noisy; no wall-clock speedup or
hosted gate pass is inferred from the local timing. Exact-head CI remains
required; the published RC5 runtime is not yet changed by this follow-on.

RC5 hosted CI `35881558751` finished with failures in both platform Go jobs
and the benchmark gate. Both Go jobs name the same two devstack provenance
fixtures: they construct a standard-memory driver without its required
StateStore, redactor and positive retention TTL. The real devstack assembly
already supplies those dependencies. This follow-on wires the fixture's real
in-memory store and redactor with the standard memory config and a one-hour
TTL; no disabled-memory shortcut, timeout increase or assertion reduction.
On Go 1.27.1 / darwin-arm64, `GOFLAGS=-p=1 go test -race
./harbortest/devstack -count=1` passes (**3.040s**), including the real devstack
cumulative-memory wiring test. Package vet, golangci-lint 2.13.2 (**0 issues**),
tracker Markdown and whitespace checks pass. Hosted verification of this
correction remains pending. The **32.66%** benchmark regression and coverage/live acceptance
requirements below are still open. This test-only follow-on does not change
the published RC5 runtime or its tag.

Exploratory **v1.32.0-rc.5** is published at
`4a6f187e6b9fa9f26ce0e03f870cc6e69b230025`, annotated tag object
`c1a8e7c582f6f3a4452b84bd228513f582af43ab`. The public Go proxy independently
resolves that tag/commit with module sum
`h1:ze9HGZiqfVbFs6KUPoNfwg1w2MoT+qZJyFB7Zk9EcU0=`. Release workflow
`35881621360` passed all six binary builds and published 13 prerelease assets.
No old RC tag moved, no stable release or merge occurred, and artifact-level
checksum/attestation verification remains distinct from successful publication.

The complete corrected Phase 269 smoke passes **15 OK / 0 SKIP / 0 FAIL**
with real PostgreSQL 17.11, Go 1.27.1 and `GOFLAGS=-p=1`. This includes actual
HTTP source reads, all cumulative store/budget combinations, exact result and
attachment continuity, steering, settlement/recovery, independent-pool
PostgreSQL fences and the public SDK sample. Full Console/static build and
docs also passed as recorded below. The RC5 docs workflow `35881558677`
passes. CI `35881558751` is not green: its declared-tool ReAct benchmark was
**32.66%** slower than the same-host baseline, above the **30%** gate; the
regression still needs investigation. No threshold is changed. Main coverage
deficits and matched real-model acceptance remain open. Preflight alone is
owner-waived for this exploratory cycle.

Published consolidation: `d0a091c888e5fed9775ef588f62c950e12043473`.
Phase 268 passes **10 OK / 0 SKIP / 0 FAIL** on that head. The first Phase 269
run found a repeat-run PostgreSQL fixture collision: the expiry test reused
the fixed `t/u/expired` scope, which already contained a previous invocation's
backdated note (the database contained two sources). The follow-on gives every
invocation its own session, including the expiry fixture, with unchanged
expiry/restart assertions. Three consecutive real-store race repetitions pass
on in-memory, SQLite and PostgreSQL (**1.974s**, Go 1.27.1). This is a test
isolation correction, not a retention change or blanket retry.

Hosted docs run `35880617274` failed on one relative tracker link in the
configuration page as included by VitePress. The follow-on points to the exact
repository document; no dead-link exclusion or weakened gate is added.
The corrected `make docs` passes (VitePress 1.6.4, 8.83s), and full
`make build` passes, including a fresh Svelte Console bundle and the static
CLI binary. The first Phase 269 run completed **14 OK / 0 SKIP / 1 FAIL**;
its sole failure was the shared expiry fixture above. The complete service-backed
rerun is in progress. The upcoming exploratory RC does not waive final coverage,
main-release or matched real-model acceptance.

Retirement started on `444fc01c145d814c540f7430c58a5903f9646426`. The local
increment removes the pair-summary/truncation executors, background recovery
loop, pair summarizer, SDK snapshot/restore/context-patch vocabulary and both
runtime pair read/write branches. `Inspect`/`Put`/`Delete` share the cumulative
owner; only explicit `none` disables memory. Removed strategy/backlog settings
fail validation rather than selecting a compatibility path. Session erasure
now marks memory purged after the authoritative StateStore scope deletion,
not after flushing an unrelated pair store. This implementation checkpoint
does not claim complete final-tree validation or RC/main acceptance.

Latest local follow-on based on published parent `18a11c7e`:

- Heavy `memory.get` now returns a scope/content/key/expiry-bound reference.
  The existing bounded `artifacts.get` resolves current memory on every read;
  no second artifact copy, presigned URL, endpoint or store is introduced.
  Reserved references never fall back to blob lookup. Runtime BuildMux wires
  the same memory owner into the reader.
- Remove the obsolete note input's timestamp, trajectory-digest and artifact
  fields, with SDK and caller migration. Notes carry authored text only;
  settled execution receipts remain runtime-owned. A regression checks that
  receipt-shaped note text stays text rather than becoming tool authority.
- Go 1.27.1, `GOFLAGS=-p=1 go test -race ./internal/memory/...
  ./internal/protocol ./internal/protocol/transports/stream`: all packages pass.
  Memory Protocol **1.831s**, source owner **5.222s**, SQLite **8.814s**,
  Protocol **3.696s**, HTTP stream **20.944s**. PostgreSQL service was not set;
  that driver package result is not service-backed evidence.
- Subsequent `GOFLAGS=-p=1 go test -race ./internal/memory ./test/integration
  -run 'TestSourceReference_|TestMemorySourceReference_' -count=1 -v`
  tests passed (**1.583s / 2.183s**). The invocation also included legacy name
  patterns that matched no tests; only the four new named roots and their
  in-memory/SQLite subtests are claimed here. They prove exact-source binding,
  SQLite close/reopen, controlled-clock expiry, cancellation, required-read
  failure, actual HTTP bounded bytes, deletion, non-presignability, no stored
  copy and N=128 HTTP identity isolation.
- Broader `GOFLAGS=-p=1 go test -race ./test/integration
  ./internal/runtime/assemble ./internal/runtime/serve ./internal/sessions
  -count=1`: integration passes **276.698s**. Assembly compilation found three
  stale `GetDeps.Artifacts` arguments and the old expected heavy-read refusal;
  repaired to assert exact source resolution, no stored copy and invalidation
  after deletion. Served passes **52.016s**, erasure passes **8.484s**;
  the repaired full assembly rerun passes **91.091s**.
- The earlier whole-tree compile-only process completed successfully before
  these latest reference/API edits. It is not a final-tree full-test result.

Release acceptance, remaining obsolete health/recovery vocabulary, canonical
generated documentation, coverage floors and live comparison remain pending.
The Docker daemon still reports an image-content I/O error. A fresh native
PostgreSQL **17.11** test cluster now runs in a private temporary directory over
a Unix socket only (no TCP listener), with its own disposable database. This
does not alter any existing container or application database. Service-backed
gates use that cluster, not the damaged Docker store.

Fresh service-backed acceptance on the same unpublished tree, Go 1.27.1 /
PostgreSQL 17.11: `HARBOR_PG_DSN=<isolated local socket DSN> GOFLAGS=-p=1
go test -race ./internal/memory ./internal/runtime/assemble
./internal/state/drivers/postgres -run
'TestSourceReference_|TestRunOnce_CumulativeMemory_|TestPostgres_RetainedContext_'
-count=1 -v` passes with **no skips** (**1.816s / 126.826s / 2.273s**).
All nine 100-turn combinations (in-memory, SQLite, PostgreSQL × automatic,
one-token and 100000-token targets) preserve the first-turn-only constraint in
actual requests, reach at least five generations and reopen persistent stores
every 25 turns. The independent-pool PostgreSQL dispatch-reconciliation race,
settled/pending recovery, checkpoint/source expiry and ambiguous-host-encoding
refusals also pass. This is deterministic functional evidence, not a paid-model
or cache-efficiency claim and not the whole service-backed release gate.

Canonical Protocol docs, Console wire manifest and external-client TypeScript
generation completed successfully; they produced no additional generated diff.
The agent scaffold now explicitly selects standard cumulative memory, matching
configuration defaults; its golden was regenerated by
`TestScaffold_Golden_MatchesAcmeAgent -update` (**1.980s**, race enabled), not by
editing the expected YAML. Full `make lint` (golangci-lint **2.13.2**,
`GOFLAGS=-p=1`) reports **0 issues** after removing four obsolete test helpers
and one constant-only helper parameter. `make markdownlint` passes **599 files /
0 errors**. The five initial lint findings were confined to test helpers left
behind by the retired pair engine. `GOFLAGS=-p=1 make vet` passes. Final focused
HTTP source-reference and CLI scaffold/init/template/golden checks pass
(**2.261s / 2.563s**), including the strengthened reserved-ID fallback refusal.
No skipped preflight is being counted as a pass.

Full fresh driver/helper race tests with the real PostgreSQL service and a
coverage profile pass: summarizer **1.453s / 86.6%**, PostgreSQL memory driver
**2.953s / 74.6%**, SQLite memory driver **9.603s / 67.6%**. Both SQL memory
drivers remain below their **85%** binding floors. Their production paths were
removed because the separate pair engine was retired, not to adjust coverage;
the remaining failure/lifecycle paths need behavioral coverage before main
acceptance. These percentages are not replaced by grouped runs or omitted
production files.

Canonical `protocol-docs-gen-check`, `protocol-ts-gen-check`,
`protocol-ts-types-gen-check`, `check-mirror` and `drift-audit` pass; drift reports
**1592 OK / 0 WARN / 0 FAIL**. Final inspection found a concrete constructor
leak: PostgreSQL `New` allocated a SQL opener before rejecting `truncation`.
The N=128 `TestPostgres_RejectedStrategyDoesNotLeakPool` fails before the fix
with leaked `database/sql.(*DB).connectionOpener` goroutines. Strategy validation
now precedes allocation. This is a three-line validation fix, not a timeout,
retry or workload change. Post-fix full driver race runs pass: PostgreSQL
**3.172s / 75.4%**, SQLite **9.322s / 67.6%**. Coverage floors remain unmet;
the new rejection regression closes the observed leak rather than changing
what the percentage counts. Final full lint passes with **0 issues**.
Post-fix full `make vet` and `make markdownlint` also pass (599 Markdown files,
0 errors). This consolidation is the next publication increment after
`18a11c7e`; its exact published revision will be recorded in the PR description.
The parent CI has passed both platform Go jobs, frontend/Playwright and its
service-backed jobs; its still-running, owner-waived preflight is not accepted
as green, and no parent result validates this consolidation head.

Go 1.27.1 local evidence: memory **1.507s**, cumulative owner **4.957s**,
runctx **1.507s**, config **2.324s**, in-memory driver **1.590s**, shared
conformance **1.583s**, trajectory summarizer **1.412s**, SQLite driver
**8.713s** pass under `GOFLAGS=-p=1 go test -race ... -count=1`.
The SQLite cohort uses a real SQLite StateStore; shared conformance preserves
N=128, eight mutation/read/deletion cycles and the leak assertion. PostgreSQL
was unset-service and is not accepted as a service-backed pass.

The broader assembly race run completed in **85.699s** with one obsolete
expected error string (it now fails at the single trajectory compactor instead
of the removed pair summarizer). The expectation is repaired; full final-tree
rerun remains required. Protocol tests still expose expiring heavy-memory
reference retrieval and the legacy note-metadata expectation. Remaining
SDK/integration/benchmark fixtures and docs still need migration
off removed APIs. No skipped tests, compatibility stubs or disabled assertions
are used to conceal those failures. Do not publish or deploy this working tree
until its affected consumers and gates are repaired.

Follow-on local evidence on this unpublished tree, Go 1.27.1:
`GOFLAGS=-p=1 go test -race ./internal/sessions -count=1` passes **8.530s**;
`GOFLAGS=-p=1 go test -race ./internal/runtime/serve -count=1` passes
**50.308s**. Erasure faults now target the authoritative StateStore deletion,
not a removed pair-store flush. A new regression first failed when deletion
succeeded but its ledger checkpoint failed: convergence reported memory as
unpurged. Convergence now repeats the idempotent clear and checkpoints it before
terminal side effects; failed convergence, pending erasure and completed erasure
all reject late memory writes. The default-strategy administrative projection
also initially reported the obsolete memory driver; it now reports the actual
StateStore. Served read/commit failure tests use the cumulative owner, and the
default-strategy execution test verifies the same committed administrative view.
The 14,660-byte receipt, large numeric version, no-replay and N=128 served tests
remain in the passing package. PostgreSQL-dependent cases remain unset-service
skips; these package passes do not replace service-backed acceptance or the
remaining whole-tree gates.

The whole-repository compile-only check (`GOFLAGS=-p=1 go test ./... -run '^$'`)
failed on remaining integration/benchmark strategy imports and old fixture APIs.
The HTTP fixtures now compile against the cumulative owner, and follow-on
compile-only checks of `harbortest/devstack` and `cmd/harbor-gen-protocol-docs`
pass (**0.700s / 0.695s**). The actual HTTP memory tests, run under race detection
with `-run '^TestMemoryHandler' -count=1`, still fail
`TestMemoryHandler_GetHeavyValueRoutesToArtifact`: expiring heavy values are
rejected rather than copied into independently retained artifacts. This confirms
the outstanding source-bound retrieval gap at the HTTP surface; it is not an
accepted failure or a reason to remove the test.

Follow-on consumer migration now compiles the integration package. The
file-scoped cumulative budget/notes/Wave 7a/runtime-hardening race run passes
**57.153s**, including 12 concurrent identities with 40 real assembled turns
each. The package-selected caller-memory, Phase 23/83d/83f/84d/110c and Wave 8
race run passes **3.584s**. It retains N=128 SQLite identity isolation and checks
actual outgoing requests for separated external caller data and historical
evidence. Malformed durable memory fails served admission before inference.
The replacement cumulative-run/inspection benchmark smoke passes **3.102s**;
its new workload is not comparable to the retired pair-append benchmark.

The canonical integration-package race run completed in **272.656s** and
**failed**. Besides the known heavy-memory retrieval gap, it found obsolete
backlog settings in the Console/scaffold YAML, two agent-selection/reattachment
fixtures lacking an explicit memory mode, and the control-test mismatches below.
The YAML and fixture corrections are local. Their focused race rerun passes
**11.851s** (`TestE2E_(AgentSelection|HarborConsole|Phase66_|
WaveV124_(Named|Byte|Reconcile|Concurrent))`), including real Console boot,
draft-save/preview, selection isolation and connection reattachment. The
scaffold golden was regenerated by `TestScaffold_Golden_MatchesAcmeAgent -update`
and passes **2.222s**. A new full package pass remains pending. No service-backed
or release acceptance is implied.

Legacy smoke consumers now target the cumulative owner rather than the deleted
pair executor. PostgreSQL smoke without a DSN reports SKIP, not OK. Shell syntax
and whitespace checks pass. Focused smoke 119 passes **11 OK / 0 SKIP / 0 FAIL**;
smoke 111e passes **28 OK / 0 SKIP / 0 FAIL**, including its real compression
race tests. This is not a full preflight pass. The final whole-repository
compile-only race command (`GOFLAGS=-p=1 go test -race ./... -run '^$'`) is in
progress; do not classify it as a test-suite pass even if compilation succeeds.

### Published-head CI regression repair

CI **35864195954** on `444fc01c` completed with both Linux and macOS Go jobs
failing: the authority minting registry still named deleted `semantic.go`, the
scope matrix supplied an empty USER_MESSAGE, and two hard-cancel tests expected
the old nil-error/step-boundary behavior. Docs CI **35864196049** passed. Other
completed jobs passed, but Playwright and preflight were skipped, not green.

The narrow repair removes only that stale registration, supplies a valid steer
message, and requires `context.Canceled` plus cancelled termination. The batch
cascade fixture now dispatches its children before Stop; cancellation in its
old position correctly prevents dispatch, so no descendants would exist. The
test still requires both descendants to be cancelled and now checks that a late
successful Finish loses. No production behavior, workload, deadline or retry
policy is changed by this repair.

Go 1.27.1, `GOFLAGS=-p=1 go test -race ./internal/protocol/bodyscope
./test/integration -run 'TestGate_|TestE2E_Phase52_|TestE2E_Phase53_|
TestBatchExecutor_HardCancelThroughRunLoop_CascadesToDescendants' -count=1`
passes **3.494s / 2.297s** on the working tree, which also contains the unpublished
memory migration. Hosted validation of the isolated published repair is pending;
these focused results are not exact-published-tree or whole-suite acceptance.
Harbor preflight remains owner-waived. The PR remains draft; no RC or deployment
is part of this repair.

### Native semantic session-memory retirement — partial consolidation

On parent `05e415027162108ef9724da800749f8884f62404`, remove the native
session-memory vector index, `SearchTurns` and SDK aliases, retrieval settings
and runtime recall plumbing. Removed YAML fields and environment overrides fail
explicitly, including empty/zero values. Semantic skill retrieval, embeddings
and external caller-memory composition remain intact. No vector migration,
compatibility reader, dependency or provider-specific behavior is added.
The default agent template and operator docs no longer advertise removed keys.
The old pair-store projection/summarizer remains pending removal: this increment
does **not** finish the one-owner consolidation or authorize an RC verdict.

Reviewed source tree `7bf4c1403f0b53341009e4aa91cd3c11d4550bbe`, Go 1.27.1:

- `GOFLAGS=-p=1 go test -race ./internal/memory/... ./internal/config
  ./internal/runtime/runctx -coverprofile=<local receipt> -count=1` passes.
  Coverage: memory **90.2%**, in-memory **96.1%**, SQLite **74.5%**, memory
  Protocol **87.7%**, cumulative owner **84.6%**, remaining strategy **86.5%**,
  config **83.1%**, runctx **92.4%**. SQLite (85%), cumulative owner (92%)
  and config (85%) remain below their binding floors. PostgreSQL **5.6%** is
  an unset-service run, not a conformance pass. Coverage changes caused by
  removal of the retired feature are not evidence of improved test depth.
- `GOFLAGS=-p=1 go test -race ./test/integration -run
  'TestE2E_(CallerMemory|Phase84d)' -count=1` passes (**2.550s**), including
  retained semantic-skill behavior and N=128 isolated session projections.
- `GOFLAGS=-p=1 go test -race ./internal/runtime/assemble
  ./internal/runtime/serve -count=1` passes (**87.491s / 50.245s**).
- `GOFLAGS=-p=1 go test -race ./cmd/harbor/... -run
  '(Init|Generate|Scaffold|Template|Golden)' -count=1` passes.
- Affected-package golangci-lint 2.13.2: **0 issues**; broad production Go build
  passes, not a full Console/release build. Markdown **599 files / 0 errors**,
  mirror, shell syntax and whitespace checks pass. Canonical Protocol docs,
  Console manifest and external-client TypeScript generation checks pass.

Initial verification caught a stale parity-test reference, an accidentally
removed semantic-skill fixture embedder (restored before the passing integration
run), and the existing Bifrost 1.9 provider-list mismatch. The validator now
includes `databricks` and `github-copilot`, matching the pinned SDK's enum and
existing exact-set tests; no new provider integration is introduced.

The local PostgreSQL container's `psql` executable returned an I/O error during
a read-only readiness probe; no database or Docker service was changed. Required
service-backed evidence, full final-tree gates and matched live RC acceptance
remain pending. Harbor local/web preflight is waived by the owner, not passed.
No RC tag, deployment, stable publication or merge belongs to this increment.

### OAuth precheck cannot dispatch a superseded action

Parent `47bd512625842b6e783554357768363885641cb5` fails three deterministic
race regressions (**0.651s**): invalidation before token acquisition, steering
during acquisition, and cancellation during acquisition all entered the tool.
The existing OAuth wrapper now checks the existing invocation fence before and
after credential acquisition. Identity validation retains precedence, provider
errors remain intact, and unfenced/current calls retain their normal behavior.
No OAuth lifecycle, provider, tool contract or new cancellation mechanism.

Source tree `feeef760e1acf4166ea0b2bec5f3576bb3d6028d`, Go 1.27.1,
`GOFLAGS=-p=1 go test -race ./internal/tools/catalog -cover -count=1` passes
with **88.5%** coverage; targeted vet and pinned golangci-lint 2.13.2 pass.
The first lint run rejected an unnecessary test-only conversion; it was removed
and the complete package gates rerun. No deadline, concurrency or assertion was
weakened. This is a focused boundary repair, not full final-tree acceptance.

Parent `47bd5126` docs CI `35858355360` passes; main CI `35858355364`
still has Linux/macOS tests running at inspection, with other jobs passed.
No active run cancelled/restarted, tag, deployment or live-model call here.
Consumer controls, remaining memory consolidation and matched live multi-window
acceptance are still required; earlier receipts below remain revision-specific.

### Text-only steering rejects unsupported inputs before interruption

Parent `5e5b18e3ba04b103ad85f8b0ef1ca50d5f37e76c` fails the new admission
regression (**0.432s**): unsupported attachment/extra fields were accepted and
interrupted planning, while missing/empty/non-string messages were silently
queued. Admission now accepts only a nonempty `message` string, preserving its
exact bytes. Invalid input returns existing `422 payload_invalid` before queue,
planning-cancellation or invocation-fence mutation. No new control or wire type.

Tested source tree `f19fd6170c769a05684161ce986a78981b49590b`, Go 1.27.1
darwin-arm64, `GOFLAGS=-p=1`, `-race -count=1`: complete steering, Protocol and
control-transport packages pass **1.831s / 3.678s / 1.464s**, coverage
**87.6% / 76.6% / 75.9%**. These are measurements, not full coverage acceptance.
Authenticated HTTP/JWT/JWKS control tests pass **2.167s**, including invalid
payload rejection followed by valid in-flight correction. The first HTTP test
incorrectly expected 400; corrected to the existing 422 contract, with no
production transport change. Targeted vet passes; pinned lint reports zero issues.

Published-parent CI `35856245211` was still running at inspection. No tag,
deployment, real-model call or service-backed acceptance in this increment.
Consumer Stop/Steer/Queue, legacy memory retirement, coverage, final-tree gates
and matched live multi-window acceptance remain open. Preflight is owner-waived.

Publication follow-up: exact-head docs run `35857236721` on `01a552f6` failed
`protocol-docs-gen-check`: the error-table description was edited in generated
output without its canonical generator. Correct the generator and regenerate;
the generated page remains byte-identical to the intended published description.
`GOFLAGS=-p=1 make protocol-docs-gen-check` and the full generator race suite
pass locally. This repairs that actual hosted failure; new-head hosted
verification is still pending. No runtime behavior changes in this follow-up.

### Steering fences queued invocations and approval waits

Parent `a6d9433d9213d45e07ce0fd9679cc1fd335cc254` fails two new race
regressions (**3.654s**): a queued parallel invocation still executes after
correction, and a pending approval never yields to steering. The existing
instruction generation now supplies a context-carried invalidation signal to
dispatch, the tool-policy shell and approval wrappers. No new execution engine,
queue, store or provider. Already-started actions retain their outcomes; future
invocations and retries are refused. A prior uncertain attempt retains its error,
partial receipt and attempt count, never a false whole-action not-executed claim.

The approval gate withdraws an obsolete request through the existing Coordinator
as a rejection with a five-second independent cleanup context. Recheck immediately
after approval returns. Failed required withdrawal terminates after settlement;
parallel/batch result projection keeps successful sibling receipts alongside the
failure. First-success/N joins now join cancelled siblings, so early success
cannot silently discard later cleanup failure. Their normal selected-result
contract is unchanged; a required-cleanup error returns settled evidence too.

Source tree `ba555ccbb63dc09c0dee8f5d03a957720c9de23d`, Go 1.27.1
darwin-arm64, `GOFLAGS=-p=1`, `-race -count=1`. Complete tools, approval,
catalog, parallel, dispatch and steering suites pass. The final approval-only
test addition is validated separately; timings are **4.131s / 1.694s / 1.508s /
1.418s / 1.591s / 1.718s**; coverage **82.9% / 91.6% / 88.1% / 91.3% /
79.9% / 87.5%**. Tools and dispatch remain below their 85% floors. Approval
meets the binding Phase 111f 90% floor, including deterministic withdrawal,
already-resolved and early-refusal regressions. No targets or denominators changed.

Real authenticated HTTP/JWT/JWKS control tests pass **2.124s**, now including
pending-approval withdrawal without invocation. N=128 independent invocation
fences and the existing N=128 controls/retention regressions remain intact.
Targeted lint and vet pass; the final approval-test-only addition also passes
its targeted lint. The initial lint findings (checked type assertion and slice
assignment clarity) were fixed without exclusions. No live-model/service opt-in,
tag or deployment in this increment.

Full final embedded/served consumer race suites pass **90.297s / 50.040s** on
the same source tree. Markdown (599 files), mirror and whitespace checks pass.
Parent exact-head CI `35854017827` passes lint, frontend, PostgreSQL, S3 and
auxiliary jobs; Linux/macOS suites were still running at inspection. No active
job was cancelled/restarted. Final new-head hosted verification remains pending.

At this historical checkpoint still open: unsupported steering attachments, consumer Stop/Steer/Queue UX,
legacy memory retirement, coverage deficits, exact-head hosted/final-tree gates
and matched live multi-window acceptance. Preflight is owner-waived, not green.

### In-flight steering — bounded implementation increment

On parent `0f2431c3c16085916a39214b5569e2880f02e374`, the new blocked-model
and intent-admission regressions fail: steering cannot interrupt either late
Finish or late tool decisions, and the obsolete intent executes. The focused
race command fails in **4.574s**. D-479 now gives each planning attempt the
existing run inbox's instruction generation and cancellation handle. New text
interrupts planning, invalidates serial pending calls, and fences obsolete
decisions and chunks. Already-started tools settle normally; an intent refused
before dispatch settles explicitly as not executed. Required failures joined
with cancellation remain failures, not retry instructions. No new store,
provider, prompt, wire shape or production timeout.

Source tree `e7492b7f7ca7bee6a7e8423795ea4b743f2ba51f`, Go 1.27.1
darwin-arm64, `GOFLAGS=-p=1`, race tests with `-count=1`: full steering,
ReAct, dispatch and parallel packages pass **1.812s / 1.749s / 1.599s /
1.398s**, coverage **87.7% / 88.1% / 79.8% / 92.4%**. Dispatch remains
below its 85% target; this is not full coverage acceptance. The authenticated
HTTP/JWT/JWKS control test passes **2.134s**, proving exact correction text
reaches the replacement ReAct request and foreign-user steering is refused.
N=128 in-flight identity isolation, late-output rejection, final-response
arbitration, independent intent settlement, attachment carry and required-write
failure tests pass.

The first full consumer check exposed two stale fixture expectations: they
expected a superseded read to execute. Preserve the original one-execution and
no-replay assertions by issuing one fresh read after correction; explicitly
assert no obsolete read occurred. All 128 shared-stack sessions remain. These
focused retained-steering tests pass **4.500s**. The core suite also caught an
overbroad callback guard; normal completion callbacks now retain the existing
terminal-seal behavior while superseded generations are rejected.

After those fixture corrections, both complete consumer race packages pass:
embedded assembly **87.088s**, served runtime **50.818s**. No service/provider
opt-ins were configured; these are not service-backed or coverage acceptance.
Final targeted lint (steering, assembly and integration) reports zero issues;
targeted vet, Markdown (599 files), mirrors and whitespace checks pass.
The final HTTP-control rerun passes **2.229s**; the drift audit reports
**1,592 OK / 0 WARN / 0 FAIL**. Publication adds this evidence only.

Still pending: per-invocation fencing for queued parallel/approval calls,
explicit unsupported steering-attachment refusal, consumer lifecycle/queue UX,
legacy memory removal and matched live multi-window acceptance. No tag, deploy
or real-model call in this increment. Parent CI `35851315472` now passes lint,
frontend, PostgreSQL, S3 and auxiliary jobs; both platform suites are still
running at this checkpoint, not green. Harbor preflight remains explicitly
waived by the owner, not executed or passed.

### Go 1.27 analyzer compatibility — release-gate repair

Parent `957880c9` CI `35848630795`, job `107140739102`, fails inside
staticcheck 0.7.0 while analysing Linux's standard-library `poll` package:
`unexpected expr: *ast.KeyValueExpr`. This is a failed gate, not a lint pass.
Rebuilding golangci-lint 2.12.2 with Go 1.27.1 only addresses build-version
admission, not analyzer support. The official
[2.13 changelog](https://github.com/golangci/golangci-lint/blob/v2.13.2/CHANGELOG.md)
adds Go 1.27 support; pin patch release **2.13.2**, with staticcheck 0.8.1,
in CI and the Makefile installation instruction. Runtime dependencies and lint
configuration/targets are unchanged.

The new analyzer exposed eight findings. Remove unused assignment results
without removing their side-effecting calls; remove an unused bootstrap logger
assignment; preserve the TUI's existing unconditional styled-span output
(its background getter is never nil). Replace deprecated ECDSA key-field
construction with the standard validated SEC 1 parser, preserving leading-zero
coordinate acceptance. Tests cover all three allowed curves, exact key identity,
invalid points and oversized coordinates. Two line-local documented deprecation
exceptions retain tests that intentionally exercise forbidden legacy TLS hooks;
no production warning or enabled linter is suppressed.

Source tree `057d7bdab619fa0d33406bdbc3d80f69cfe46627`, Go 1.27.1
darwin-arm64, `GOFLAGS=-p=1 go test -race` with `-count=1`: complete durable
events, provider-route HTTP transport, Protocol auth, agent-config Protocol,
embedded assembly and TUI app packages pass **30.141s / 1.643s / 2.047s /
10.979s / 90.099s / 22.962s**. No live-provider or service opt-in configured;
this is not service-backed or final-tree coverage acceptance. Full `make lint`
with 2.13.2 built using Go 1.27.1 reports **zero issues** both natively and with
`GOOS=linux GOARCH=amd64 CGO_ENABLED=0 GOFLAGS=-p=1`. The latter checks Linux
source/analyzers from macOS; it is not a native Linux test run or hosted CI
pass. Full `GOFLAGS=-p=1 make vet`, Markdown (599 files), mirror and whitespace
checks pass. Publication
adds this receipt and clarifies the CI installation comment. Exact-head hosted
verification, the original journal deadline issue and release acceptance remain
pending. No tag, deployment, prompt change or model call.

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
