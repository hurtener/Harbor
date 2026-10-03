# Durable runtime contracts: candidate qualification

## Release-candidate checkpoint: 2026-10-03

The intended publication is `v1.33.0-rc.1`, an opt-in prerelease for consumer
testing, not stable-release qualification. Runtime source checkpoint
`8ff7a63bdc910b0a8472e3fc24a2e5992f16250f` passed all 17 hosted CI jobs,
including preflight, Console end-to-end tests and the performance gate, in
[run 37105844251](https://github.com/hurtener/Harbor/actions/runs/37105844251).
The [docs build](https://github.com/hurtener/Harbor/actions/runs/37105844330)
also passed. The release owner reported completed independent adversarial review
and its credential-admission timeout correction. This is owner-reported review
evidence; no separate GitHub review submission is recorded.

A frozen `v1.32.1` implementer-contract compile witness passes against that stable
source and fails against this candidate: `TaskRegistry` adds six required
methods, and Protocol `Client` adds nine. Custom implementations and mocks need
source changes. `AllocationStore` is newly introduced in this candidate, not a
previous stable interface. No claim of source-compatible minor stable release
follows from additive wire changes or the `-rc.1` suffix. The Protocol remains
`0.1.0` and the generated candidate manifest contains 160 methods.

The required registry additions are `AcceptInput`, `RefuseInput`,
`GetInputReceipt`, `MarkInputApplied`, `BeginOutputInvocation` and
`FinishOutputInvocation`. The client additions are `SessionsSetAdmission`,
`SessionTurnsList`, `SessionTurnsGet`, `ControlReceipt`, `ArtifactsExportAnswer`,
`ArtifactsPrepareImport`, `ArtifactsTransfer`, `ArtifactsTransferStatus` and
`ArtifactsRevokeTransfer`. Implementations must honor the documented contracts;
empty success stubs do not provide compatibility.

The newer scoped-admission plan records five complete two-runtime native
consumer reruns covering lost-acknowledgement replay, restart, receipt provenance
and downstream completion. This supersedes earlier pending statements for that
scenario, not every possible downstream consumer. Crash-uncertain admission
reservations still remain blocked without automatic expiry or refund; ordinary
lost-HTTP-acknowledgement recovery does not close that crash window.

The following sections preserve historical focused checkpoints. Their old
pending CI and review statements are superseded only by the evidence above.
Release-metadata changes require their own exact-head checks. Main deployment,
published candidate-document verification, RC artifacts and downstream RC
acceptance are not established by a PR build. Stable release remains held.

## Original qualification scope

This is an unmerged candidate for three generic runtime contracts: exact artifact
transfer, durable task input receipts, and cumulative inference allocations. All
provider fixtures in the checks below are synthetic; no paid inference is part
of this qualification.

## Contract boundaries

- Artifact transfer requires independent source ownership, signed exact-content
  authority and recipient admission. Endpoints are boot-pinned. Erasure fences
  prevent late writes; a receipt proves past delivery, not continuing access.
- Final-answer export selects an exact sealed turn and completed root task,
  returning an immutable artifact reference and incorporated input revision.
- Keyed text input is bound to the exact task and optional expected accepted
  input revision. Exact retries recover the original receipt; stale new intent
  and changed retries are rejected. Applied means the planning boundary consumed
  the projection, not that a model understood it.
- One runtime owns an active task registry. Sequential durable close/reopen is
  supported; concurrent active task registries sharing state are not.
- Inference reservations use cross-manager storage CAS. Unknown physical-attempt
  liability survives cancellation and restart. Oversized provider counters hold
  the entire envelope and latch a breach instead of wrapping arithmetic.
- Phase 272's frozen token-only checkpoint explicitly refused money. Phase 274
  extends the candidate with operator-installed immutable inclusive tariffs and
  conservative monetary reservations for bounded static OpenAI/Anthropic text.
  Unsupported routes and missing/mismatched tariffs still refuse; charged/held
  capacity is not actual spend. See the Phase 274 plan and its local evidence.

## Verified focused evidence

Using Go 1.27.1 and race detection:

- In-memory, SQLite and real PostgreSQL 17 storage conformance, including direct
  HTTP byte equality, exact replay and 100-owner isolation
- Recipient erasure races and source-native final-answer production wiring
- Same-runtime 100-way expected-input-revision admission, exact/changed replay,
  and SQLite/PostgreSQL close/reopen with stale-intent rejection
- Cross-manager 100-way allocation reservation, inheritance, bounded hidden
  attempts, missing/partial usage, cancellation and restart liability
- Deterministic allocated child parent binding and full owner-session state
  isolation, including pending and already-resolved sibling owners
- Transfer package coverage 80.7%; allocation accounting package coverage 87.2%

The full Console lint, type check, 1,034 unit tests and production build passed.
The documentation site build and 603-file Markdown lint passed. Canonical joins
include generated reference pages, the Console wire manifest and the external
TypeScript wire module.

The final serial checkpoint also passed full `golangci-lint` (zero findings),
`go vet ./...`, all three generated-source lockstep gates, and `make build` with
a freshly built Console and CGo-free binary. Live phase smokes 270, 271 and 272
passed against that binary with real PostgreSQL available: respectively 8, 2 and
2 positive assertions, zero failures and zero skips. Their focused race tests
also passed, including production final-answer wiring and receipt restart.

## Integrated follow-on checkpoint

The local follow-on integrates native task-output provenance, conservative
monetary allocations and scoped session admission. It also repairs input-receipt
identity when the durable task ID differs from the physical execution run ID.
The generated joins now contain 477 types, 160 methods, 46 errors and 150 events;
the three generators and Console TypeScript lockstep checks passed together.

Seven focused pricing-refusal test events passed under race. Pure pre-spawn
pricing validation now releases the session acceptance reservation through an
explicit registry proof. An arbitrary pricing error, including a joined error
after task acceptance, cannot manufacture that proof or refund unknown work.
Eighteen integration events covering the earlier event-surface, assembled
surface, concurrent-run and version-handshake failures also passed.

The composed native approval, OAuth, invocation boundary and MCP binary-output
gate passed 14 test events across four packages. It exercises the real runtime
wrapper chain: native pauses park acceptance only at the supported boundary,
resumption reacquires the original admission, and successful binary output keeps
its task provenance. A CGo-free local binary was then built from the integrated
source. These are focused results, not full integrated qualification.

Scoped admission requires an explicitly configured isolated audience, durable
state, and a coordinated drain of old writers. Durable task/input recovery also
requires the durable task driver and its separate advertised capability. A
process loss between a domain commit and acceptance finalization can leave a
durable pending reservation; there is no automatic expiry or administrative
refund. A canonical receipt-based reconciliation path remains open, so normal
lost-transport-ack recovery must not be described as proof of this crash case.

## Aggregate qualification remains open

An initial whole-repository race run was incomplete. It exposed corrected
integration joins in body-scope enforcement and a test client, plus an inherited
probabilistic telemetry sampler in the test environment. The telemetry test
process now explicitly selects deterministic sampling. Focused regression gates
for those fixes passed.

The aggregate also terminated the assemble and serve test processes under memory
pressure. A fresh full serve run reproduced termination in the retained-context
128-session test at approximately 5.7 GiB peak RSS. The same unchanged test passes
alone with race detection at under 1 GiB. The cumulative-history and concurrent
tests pass together, as do all preceding tests together. Lowering the Go heap
soft limit did not resolve that initial aggregate result.

The retained-finalization correction subsequently separates bounded preparation
from bounded publication/cleanup while preserving earlier caller deadlines.
Its full retained-context race selection passed in 128.364 seconds; the original
128-owner assemble and served tests each passed three repetitions. New deadline
regressions fail against the earlier implementation. This closes the targeted
local failure; full integrated package and hosted macOS results are still needed.

Full aggregate race tests, release preflight and hosted CI must be separately
established. The required independent integrated review is incomplete and
remains a release gate. These focused results do not authorize a stable-release
claim.

Canonical preflight passed its drift audit (1,604 checks, no warnings or failures)
before its static batch exposed two stale checks already present at the base
revision: the deleted pair-only memory projection and the former direct budget
assignment. Those guards now verify the retained owner and active budget helper,
with additional named runtime assertions. Preflight was intentionally stopped
for resource coordination and an explicitly authorized draft checkpoint; it is
not recorded as passed. The draft does not create an RC or waive release gates.

## Compatibility

Wire fields and methods are additive and Protocol remains `0.1.0`. Custom Go
Protocol Client and TaskRegistry implementations must implement the new methods;
source compatibility is not universal. Custom allocation stores must also
implement the atomic `ReserveMonetary` extension; standard stores do so. Restart guarantees require durable tasks
and SQLite/PostgreSQL state. Volatile storage does not become durable merely by
advertising an allocation mechanism.

## Allocation finality follow-on (D-493)

The next local source extends the allocation snapshot with negotiated irreversible
closure. Task terminality alone never releases an application reservation.
Built-in engines wait for the root and every accepted descendant, then fence new
provider reservations before the last terminal write. Existing and unknown
provider liability remains held; exact settlement is still permitted.

The accounting records use versioned envelopes in the protected coordination
namespace, partitioned by tenant and full-owner/root digests. Session erasure
cannot remove closed or outstanding funding. An older reader rejects the legacy
slot marker, and a writer that loaded the prior generation loses its CAS. This requires an old-writer drain
before rollout and no downgrade of an active funded store. No credentials,
pricing catalogs, grants or production configuration are changed by the patch.

The finality follow-on passed 38 full allocation-accounting race test events
with 84.7% package coverage and 424 full task-engine, in-process, durable-driver
and Protocol-projector race test events. The accounting run used real PostgreSQL
17 in addition to in-memory and SQLite storage, with no skipped tests. It covers
100-way close/reserve contention, late settlement, erased-session helpers,
legacy migration and its read interleaving, and old loaded-writer CAS loss.
The native served and assembled finality paths passed their focused regressions;
the canonical generators and TypeScript lockstep also passed. A local CGo-free
binary was built from that source. Hosted PostgreSQL CI now requires the exact
token, monetary and close/erasure test cases to pass without skips.

A real stock-Bifrost consumer reached irreversible closure while correctly
retaining unreported retry liability. Its successful final response reports only
the visible attempt's usage; the bounded transport can also make hidden retries.
Therefore `closed: true` alone is not proof of settled funding, and this route
does not currently prove that unused retry capacity can be released. A separate
single-attempt test-provider integration is still being qualified; it will not
be described as stock-Bifrost release evidence.

Full finality static analysis subsequently passed with zero findings. Its one
decoder-style finding was corrected without changing branch order, then the
38-event accounting race package passed again with all three stores and a new
CGo-free binary was built. The complete consumer acceptance, hosted integration
and independent integrated review remain open.

Subsequent pinned-SDK inspection found a separate provider-bound defect:
Bifrost's logical retry count omits the fasthttp stale-connection retry layer.
That transport can write a POST and retry after a response-header failure on a
reused socket. A loopback reproduction consumed four complete requests where
the earlier `MaxRetries+2` bound declared three. The corrected driver composes
the logical envelope with fasthttp's five-send outer cap, admits only audited
OpenAI/Anthropic text factories and exact OpenAI-compatible custom paths, and
refuses unproved families or opaque/media/file work before dispatch.

The full Bifrost driver race suite passed 241 test events, with only three
explicitly disabled paid probes skipped. Scoped lint passed with zero findings
and canonical Markdown passed 611 files. Native factory regressions consumed
four and six POSTs across nested retry paths, settled only the final 35 reported
tokens, and retained the remaining 16,465 tokens as unknown. The bound is not an
actual-attempt receipt and cannot certify work admitted under the old smaller
envelope. See [physical-attempt qualification](bifrost-physical-attempt-validation.md).
Combined binary/consumer, hosted and independent-review release gates remain
open; no production-enablement claim follows from these local tests.

## Bounded close progress correction

Hosted full qualification exposed starvation in the original 128-try allocation
close loop: 100 concurrent callers can each reserve and settle, producing more
than 128 valid accounting generations. Closing now yields after a lost CAS and
retries within a five-second operation bound, preserving an earlier caller
deadline. Cancellation and timeout leave liability intact; successful closure
still uses the same atomic total-record predicate.

A deterministic regression inserts 160 real reserve/settle pairs between close
reads and writes. It fails against the original implementation and passes after
the correction. Ten complete allocation race repetitions passed 420 test events
with no failures or skips, including the unchanged 100-way workload ten times
each on in-memory, SQLite and real PostgreSQL 17. Explicit cancellation and
deadline tests retain unknown liability. Scoped static analysis reports zero
findings. The concurrent test now joins every writer before reporting a close
error, so teardown cannot mask the original failure with a closed-store panic.

This is a focused correction. The preceding candidate's hosted aggregate also
exposed a stale monetary fixture, two capability-count fixtures, macOS retained
cleanup deadlines and a ReAct benchmark regression. Those remain separate
qualification blockers; neither this gate nor the selected local benchmark
comparison establishes an aggregate pass.

The reservation amplification and its usability limits are described in
[conservative inference reservations](inference-reservation-amplification.md).

## Finality and physical-envelope fixture joins

The version-handshake and combined-surface fixtures now assert all 23 canonical
capabilities, explicitly including allocation finality. The assembled pricing
fixture preserves the former 3,300-token/12-micro-USD allocation as a required
pre-dispatch refusal, and separately refuses an adequate token cap with the old
monetary cap. A newly funded 16,500-token/60-micro-USD synthetic allocation admits
one request and retains exactly 16,493 unknown tokens plus the 60-micro-USD
unknown monetary envelope after the final response reports seven tokens.
Second-call and post-closure refusal, immutable tariff authority and changed
manifest restart rejection remain asserted. No production policy is changed.

All ten test/subtest events passed with race detection, and scoped static
analysis passed with zero findings. These fixture joins do not resolve the
separate full-suite performance or hosted macOS cleanup failures.
