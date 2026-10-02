# Durable runtime contracts: candidate qualification

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
