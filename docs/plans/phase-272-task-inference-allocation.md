# Phase 272 — Durable cumulative task inference allocation

## Summary

Accept an immutable task allocation and reserve a conservative token envelope before provider transport. The same durable allocation covers planner calls, naming and compaction helpers, retries, resumes, and same-identity spawned descendants. Unknown liability survives cancellation and restart.

## RFC anchor

- RFC §6.5: provider-independent LLM client and provider corrections
- RFC §6.11: identity-scoped durable StateStore
- RFC §6.15: inference governance and explicit policy refusal

## Briefs informing this phase

- brief 08: LLM client validation
- brief 03: tools, integrations and LLM client

## Brief findings incorporated

- brief 08, Cancellation caveat: cancellation need not prove provider work stopped; an uncertain call retains its reservation
- brief 08, Per-model seam: model capabilities and pricing differ; token capacity is not a monetary tariff
- brief 08, Findings summary: provider usage is reported through the transport; estimated backfill is not authoritative settlement

## Findings I'm departing from (if any)

None. Existing per-call and per-identity governance remains independently enforced. This task allocation is an additional cumulative constraint.

## Goals

- Immutable caller funding identity and positive revision, fingerprinted with accepted task intent
- Cross-process atomic reservation and settlement through StateStore.SaveBatchIf
- Preserve unknown liability across process failure, retry, resume and cancellation
- Expose content-free cumulative counters through tasks.get and the SDK
- Refuse unsupported hard monetary requests explicitly

## Non-goals

- Provisioning provider credentials, purchasing inference or deploying runtime configuration
- Deriving money from token counts, guessed prices, or provider-reported floating-point costs
- Replenishing a task on retry, deleting unresolved holds, or silently creating a new funded synthesis turn

## Acceptance criteria

- [x] One shared store admits no more than its cap under at least 100 concurrent reservations
- [x] All three StateStore drivers retain exact accounting through manager reconstruction
- [x] Changed allocation identity/revision/cap under an existing task or idempotency key is refused
- [x] Descendant tasks cannot replace or escape their parent's cumulative allocation
- [x] Provider transport is unreachable when the conservative envelope does not fit
- [x] Missing usage, cancellation after possible transport, and process loss retain liability
- [x] Bifrost internal retries and encrypted-reasoning repair fit a reserved envelope; final-only usage cannot refund unreported attempts
- [x] tasks.get returns scoped content-free counters and no monetary enforcement claim
- [x] Requested monetary cap returns inference_allocation_pricing_unavailable

## Files added or changed

- internal/llm/allocation.go
- internal/llm/allocation/: durable accounting and conformance tests
- internal/llm/safety.go, registry.go, drivers/bifrost/allocation.go and mock/mock.go
- internal/tasks/tasks.go, engine/engine.go and protocol/registry_projector.go
- internal/protocol/types/inference_allocation.go, control.go and tasks.go
- internal/runtime/assemble/assemble.go and runtime/serve/{mux,runloop}.go
- sdk/llm/llm.go and generated Protocol client aliases
- scripts/smoke/phase-272.sh

## Public API surface

Start.inference_allocation contains allocation_id, positive revision, positive max_total_tokens, and optional max_cost_micro_usd. The last field is deliberately rejected until a trusted monetary implementation exists. Nil preserves legacy tasks.

TaskDetail.inference_allocation contains the latest 32 content-free settlement receipts with an explicit truncation flag, plus allocation_id, revision, max_total_tokens, settled_tokens, reserved_tokens, unknown_tokens, attempt_count, bound_breached, guarantee=tokens, and pricing_status=unavailable. Reserved includes unknown liability. The counters are cumulative across the root and its same-identity spawned descendants; they are not a promise of unused financial funds. A provider violating its declared bound latches bound_breached and prevents further calls. Unrepresentable provider usage is recorded as a breached receipt with the complete envelope still unknown and reserved; it is never allowed to wrap arithmetic or refund capacity.

Drivers opt into the additive AllocationBoundedDriver contract by providing a finite upper bound on physical requests below Complete. Legacy third-party drivers remain usable for ordinary tasks; allocated tasks fail closed until their transport behavior can be bounded.

## Test plan

- **Unit:** immutable validation, pricing refusal, overflow/invalid envelope, known and unknown settlement, exact repeat settlement
- **Integration:** actual provider-edge safety composition, synthetic HTTP retry fixture, accepted task persistence and inheritance, task Protocol projection
- **Conformance:** same reservation/settlement/restart test across in-memory, SQLite and PostgreSQL StateStore
- **Concurrency / leak:** 100 competing reservations through distinct manager instances sharing one store; scoped identity isolation; no new background worker or goroutine

## Smoke script additions

- Verify advertised allocation capability and monetary refusal when present
- Preserve the repository's unmounted-surface skip convention

## Coverage target

Allocation accounting: at least 80%; all introduced admission/refusal states must have direct assertions. Existing runtime paths retain their package gates.

## Dependencies

StateStore conditional batches, canonical task idempotency, provider-edge safety, and existing task detail projection. No new database schema or execution pool.

## Risks / open questions

Errored streams may contain partial usage; their reported counters alone never refund an unproven remainder.

The input token estimator is not a hard bound. The first implementation reserves the trusted model's full input window plus the explicit output cap for every possible provider attempt. This is intentionally coarse and may refuse an otherwise cheap request. An exact trusted tokenizer can tighten it later without changing accounting semantics.

Bifrost's pinned transport can retry internally. The envelope includes configured network retries and the extra encrypted-reasoning repair. When intermediate usage cannot be established, the unmeasured envelope remains reserved even after the final response. A cancellation proven before the driver is entered can settle zero; cancellation after entry is uncertain unless complete measured usage is available.

The accepted allocation is a runtime limit, not a credentials grant or financial authorization. A coordinator must reserve root, specialist and later synthesis funding before issuing their respective allocations. A synthesis task uses its own immutable funding identity; retries reuse its task and cap. It must not treat a held unknown amount as refundable money.

## Trusted pricing extension contract

Phase 274 implements the extension below with explicit operator installation and
bounded initial transports. The token-only checkpoint and its historical
qualification remain the evidence for this phase; see D-490 for the extension.

The future trusted pricing manifest must define manifest ID and positive immutable revision, exact provider/model/model-version, USD currency, integer input/output micro-USD ceilings per million tokens, and an explicit includes-all-charges assertion. The trusted operator or coordinator, never task text or model output, supplies it. A monetary implementation must persist its hash with task acceptance, match the selected provider/model/version on every attempt, round reservations upward with checked arithmetic, and reserve any cache, reasoning, multimodal, per-request or ancillary charge at a declared ceiling. An unmatched, incomplete or changing tariff is unpriced and must refuse a hard cost guarantee. Missing usage retains the full monetary reservation; reported token usage without adequate price provenance does not settle money. This release specifies the manifest contract here without exporting an unused primitive, activating monetary mode or installing tariffs.

## Rollout / compatibility

All wire fields are additive. Existing tasks without allocation behave as before. Full process-restart continuity requires `tasks.driver: durable` plus SQLite/Postgres StateStore; the in-memory driver retains accounting only while that store instance lives. The allocation capability advertises the accounting mechanism, not an upgrade of volatile storage. Require the durable-task-input capability or verify the runtime driver configuration before promising restart-safe task funding. Allocated tasks require the advertised runtime capability and a driver with an explicit finite attempt bound. Caller-supplied caps cannot silently enable money enforcement. Persistence uses reserved internal StateStore kinds so external state writes cannot overwrite accounting. Unknown holds have no expiry-based refund.

## Completion evidence

Focused race qualification passed with real PostgreSQL and synthetic provider transport; see docs/notes/phase-272-local-validation.md. Combined release preflight, generated-client joins and hosted CI remain the integration owner’s gates. This phase is not a claim that application-wide financial budgets or group cancellation are complete.

## Glossary additions

- Task inference allocation: immutable cumulative token allowance shared by a canonical task and its same-identity spawned descendants
- Unknown inference liability: reserved capacity for provider work whose complete usage is unproven; it cannot be refunded merely because time passed or a task was cancelled

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage on touched packages meets target
- [ ] Cross-session and cross-process isolation gates pass
- [ ] N≥100 concurrent reservation and cancellation isolation passes under race detection
- [ ] Real runtime/provider integration gate passes
- [ ] Glossary and D-488 are integrated by the release owner

## Irreversible finality extension (D-493)

Task lifecycle and current counters do not establish final accounting. Negotiate
`task_inference_allocation_finality_v1` and inspect the additive `closed` field.
The allocation store serializes Close with all reservations using the same CAS
slot. Close never releases unresolved attempts; settlement after Close remains
permitted. When closed, zero token/money reserved and unknown totals plus no
bound breach establish the final conservative charge for that allocation.
An older field omission is unknown, not closed.

Built-in task engines close before the last terminal member is published, after
the root and every accepted descendant are terminal. Accepted background work is
not cut off merely by its parent's completion. Pending/paused members retain
funding. A late new child is refused; exact accepted retries retain their task.
Durable recovery also closes fully terminal legacy families. A failed close
keeps the last task transition retryable; a failed task write after a successful
close cannot reopen funding. The existing single active registry rule remains.

Funding is pinned before native task acceptance and during recovered-root
hydration. Totals and attempts migrate atomically to an explicit version-2
protected coordination partition keyed by tenant and full-owner/root digests.
Only an immutable funding fingerprint, counters and opaque attempt receipts
remain outside ordinary session data. Session erasure neither recreates funding
nor removes a close barrier or outstanding liability. Legacy slots receive an
invalid-for-old-readers marker, and loaded old writers lose their CAS. Histories
of 1,000 or more legacy attempts require quiesced migration maintenance; partial
migration never starts. Unknown formats fail closed.

Drain older writers before upgrade and never downgrade an active funded store.
Legacy markers are not a boot-time downgrade detector; mixed-version writers
after session erasure are unsupported because old binaries ignore the new
protected namespace.
No expiration or task-state inference refunds liability. There is no online
tenant/account delete API: explicit storage decommission must revoke/drain all
writers before removing the corresponding protected accounting partition and
legacy markers. This change adds no purge, retention timer or production action.

Required extension qualification: all three state drivers with 100 concurrent
reservations/close, late settlement and unknown holds, lost close acknowledgment,
SQLite reopen, old-reader/loaded-writer refusal, session erasure/late settlement,
protected metadata minimization, background descendant lifecycle,
terminal-write failure, native recovery, canonical projection and capability
negotiation. The full accounting and task package gates passed locally, together
with full static analysis and generated-wire joins. Complete consumer and
hosted integration checks remain open; see the candidate validation record.

The pinned Bifrost SDK has a transport-level stale-connection retry loop below
its logical counter. A loopback regression against the prior source confirmed
four fully consumed POSTs while the driver declared only three attempts. The
candidate correction multiplies `MaxRetries + 2` by fasthttp's outer five-send
cap for each audited OpenAI/Anthropic text factory. Custom OpenAI-compatible and
exact-selected OpenAI/Anthropic routes use the same audited transport for token
funding; other families and opaque/media/file request shapes fail closed. Harbor
emits no alternate-provider fallback list or raw body. Bound multiplication does
not establish actual settlement: final-only usage retains unreported physical
liability. Existing pre-repair executions cannot be retroactively certified.

The corrected bound and composed native-provider regression require their own
qualification; storage/finality results do not waive that gate. See
`docs/notes/bifrost-physical-attempt-validation.md` for the exact evidence and
remaining release limits.
