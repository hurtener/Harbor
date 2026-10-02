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
- Monetary caps are explicitly unavailable. The existing pricing seams lack a
  trusted versioned inclusive pre-dispatch tariff. Token accounting is not proof
  of a monetary budget. See the allocation plan's pricing extension contract.

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
tests pass together, as do all preceding tests together; this is not yet a passing
whole-package result. Lowering the Go heap soft limit did not resolve it.

Full aggregate race tests, release preflight and hosted CI must be separately
established. These focused results do not authorize a stable-release claim.

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
source compatibility is not universal. Restart guarantees require durable tasks
and SQLite/PostgreSQL state. Volatile storage does not become durable merely by
advertising an allocation mechanism.
