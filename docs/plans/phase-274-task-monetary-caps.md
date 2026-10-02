# Phase 274 — Trusted inclusive task monetary caps

## Summary

Extend cumulative task allocations with an immutable operator-installed pricing
catalog and atomic integer monetary reservations. Every supported physical
attempt is funded before transport; uncertain work retains its complete monetary
envelope. Charged capacity is a conservative ceiling, never actual provider spend.

## RFC anchor

- RFC §6.5: bounded provider transport
- RFC §6.11: durable conditional accounting and immutable catalog pins
- RFC §6.15: task inference governance

## Briefs informing this phase

- brief 03: tools and provider correction boundaries
- brief 05: identity-scoped durable task and state contracts
- brief 07: runtime dispatch owns execution policy
- brief 08: provider capabilities, per-model seams and uncertain cancellation

## Brief findings incorporated

- brief 08, Per-model seam: model selectors and billing differ; technical token
  bounds do not supply monetary tariffs
- brief 08, Cancellation caveat: a caller stopping does not prove the provider
  performed no work; unknown liability cannot be refunded
- brief 05, Persistence: immutable intent and accounting must survive manager
  reconstruction through the shared StateStore contract
- brief 07, Runtime ownership: the runtime authorizes dispatch; model-generated
  prices or a task's declared estimate cannot authorize its own expenditure

## Findings I'm departing from (if any)

None. This implements Phase 272's trusted pricing extension. Floating-point cost
observability and existing per-identity governance remain independent mechanisms.

## Goals

- Validate and detach an operator/coordinator installed, versioned USD catalog
- Bind accepted task intent to exact manifest ID, revision and full SHA-256
- Refuse changed catalog revisions at restart and exact reference mismatches
- Match exact immutable provider/model/version and configured endpoint on each call
- Atomically reserve both token and micro-USD envelopes across all managers
- Preserve root funding through helpers, retries, resumes and descendants
- Expose conservative charged/held capacity without fabricating actual spend

## Non-goals

- Installing production prices, credentials, grants or deployment settings
- Paid provider validation or inferring current market prices
- General OpenRouter, external route, custom-provider or multimodal cost guarantees
- Automatically reconciling invoices or refunding unknown/expired holds
- Claiming a stable release, completed independent review or deployed documentation

## Acceptance criteria

- [x] Incomplete, duplicate, negative, unpinned and non-USD tariffs fail startup
- [x] Catalog inputs are detached and all exact reference/target mismatches refuse
- [x] Every rate category rounds upward with checked multiplication/addition
- [x] A shared 100-way race obeys money and token caps on all three StateStores
- [x] SQLite close/reopen preserves unknown money and rejects revision drift
- [x] Accepted money/hash survive durable task recovery and unchanged inheritance
- [x] A real Bifrost localhost fixture reserves every bounded hidden retry
- [x] Unsupported requests make no monetary-authorized provider dispatch
- [x] Real runtime assembly, run-loop consumption and task detail projection agree
- [ ] Protocol/SDK/client joins and published documentation match the final code

## Files added or changed

- internal/llm/pricing: manifest validation, exact quote and arithmetic tests
- internal/llm/allocation: atomic monetary accounting and durable catalog pins
- internal/llm/drivers/bifrost/allocation.go: bounded text transport admission
- internal/config, runtime/assemble, tasks/engine: boot and acceptance consumers
- internal/protocol and tasks/protocol: accepted reference and content-free counters
- sdk/llm and Protocol clients: public pricing construction and wire joins
- docs/CONFIG.md, operator skills and phase smoke

## Completion evidence

Focused race and real PostgreSQL qualification is recorded in
`docs/notes/phase-274-local-validation.md`. Generated joins, combined preflight,
final independent review and documentation deployment remain open integration
and release gates.

## Public API surface

`llm.pricing_manifests` is restart-required operator configuration, outside all
Protocol/model inputs. SDK coordinators use `NewPricingCatalog` and inject the
same immutable catalog into LLM and task dependencies. Runtime assembly also
pins catalog revisions in reserved StateStore coordination records; SDK hosts
must call `BindPricingCatalog` against their shared store at boot.

Start's allocation may include `max_cost_micro_usd` (nonnegative integer),
`pricing_manifest_id`, positive `pricing_manifest_revision`, and lowercase full
`pricing_manifest_sha256`. The complete reference is fingerprinted with task
intent and inherited unchanged. A reference alone confers no tariff authority.

Every tariff explicitly declares exact provider, model, identical immutable
model-version selector, endpoint binding, USD currency, input/output/cache-read/
cache-write/reasoning rates per million tokens, fixed per-request and ancillary
ceilings, and an all-charges assertion. Each numeric field must be present,
including verified zero. The operator attests that the version selector is
immutable and these ceilings include every applicable charge.

`charged_cost_micro_usd` is permanently consumed conservative capacity, not an
invoice or measured expense. `reserved_cost_micro_usd` includes
`unknown_cost_micro_usd`. Receipts distinguish `charged_ceiling`, `unknown`, and
`released_before_dispatch`. Monetary snapshots identify their exact tariff and
use `guarantee: tokens_and_cost_micro_usd` and
`pricing_status: trusted_inclusive_ceiling`. They do not imply that every model
or request shape can run. Token-only snapshots preserve their old labels.

## Test plan

- **Unit:** incomplete authority, immutable copy, all rate categories, overflow
  against arbitrary-precision arithmetic, exact changed-reference rejection
- **Integration:** real Bifrost HTTP retries, startup wiring, served task funding,
  scoped task projection and failure before dispatch
- **Conformance:** in-memory, SQLite and real PostgreSQL CAS accounting; SQLite
  close/reopen and durable task reference reconstruction
- **Concurrency / leak:** 100 catalog readers and 100 competing manager instances,
  foreign owner isolation, bounded tests and normal dependency cleanup

## Smoke script additions

Phase 274 executes focused monetary catalog, store, provider, task, Protocol and
runtime tests. No network price lookup or paid inference is part of the smoke.

## Coverage target

Pricing and allocation accounting packages: at least 80%. Every new refusal and
settlement mode has direct assertions. Existing package gates remain applicable.

## Dependencies

Phase 272; StateStore conditional batches; task idempotency and inheritance;
Bifrost's pinned finite retry behavior. No new third-party dependency or schema.

## Risks / open questions

The guarantee depends on truthful operator ceilings and provider compliance with
attested version and physical request bounds. It cannot prove an external bill.
Unsupported or unknown selectors remain fail-closed. A provider bound violation
latches the allocation closed; it cannot undo already performed external work.

The initial Bifrost monetary consumer admits only static OpenAI/Anthropic text
requests, with a hashed exact configured endpoint (or explicit provider default).
Externally resolved routes, custom-provider IDs, OpenRouter, multimodal/native
file operations and arbitrary passthrough require additional independent bounds
and are refused. This does not reduce ordinary token-only task compatibility.

The full configured input window and explicit maximum output are reserved, plus
Anthropic's separate configured thinking allowance. Input, cache-read and
cache-write categories are added, as are output and reasoning categories; this
can over-reserve but cannot choose an unsafe discount. Fixed and ancillary
ceilings are charged for every possible physical retry. Successful final usage
cannot prove that unreported intermediate attempts did not run: that entire
money envelope remains unknown. A proven complete single-attempt response
consumes its full monetary envelope as charged capacity. Only cancellation
proven before driver entry releases money.

Accepted catalog references are durable task intent. Catalog pins prevent
changing a previously installed ID/revision even when no task is currently
running. Removing a version does not refund old holds or authorize repricing.
Full restart guarantees still require durable tasks plus SQLite/PostgreSQL state.
A capability describing allocation accounting does not install a tariff or
upgrade volatile storage. Stable release and hosted documentation remain gated
by final combined qualification and independent adversarial review.

## Glossary additions

- Trusted pricing manifest
- Charged monetary capacity

## Pre-merge checklist

- [x] `make drift-audit` passes
- [ ] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] Cross-session isolation tests pass
- [x] N≥100 concurrent reuse passes under race detection
- [x] Real-driver cross-subsystem integration passes under race detection
- [x] Glossary and D-490 integrated
- [ ] Independent adversarial review findings fixed
- [ ] Documentation site current, built, deployed and verified at final release
