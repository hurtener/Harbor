# Phase 247 — Durable observability rollups (HA-65)

## Summary

Add a first-class durable observability rollup projection — an indexed,
rebuildable materialization of aggregate measures over canonical Harbor
events — behind its own typed interface and §4.4 driver seam (in-memory,
SQLite, Postgres). Rollups are best-effort aggregates over successfully
persisted canonical events, never a billing-exact ledger; they answer
administrative cost/token/outcome questions without scanning the raw event
log at read time. D-426 is the phase authority and narrowly amends D-296:
this rebuildable projection is allowed while a general-purpose Harbor TSDB and
identity-labelled OTel metrics remain rejected.

## RFC anchor

- RFC §5.2
- RFC §5.5
- RFC §6.9
- RFC §6.13
- RFC §6.14
- RFC §6.15
- RFC §7
- RFC §9

## Briefs informing this phase

- brief 03
- brief 05
- brief 06
- brief 11

## Brief findings incorporated

- brief 03 §8: cost/usage events carry the identity quadruple; the LLM call
  surface reports tokens and cost that canonical events persist.
- brief 05 §4: the durable event log is the canonical projection substrate;
  StateStore idempotency (EventID keys) and the persistence triad + conformance
  suite are the floor.
- brief 06 §3/§5: one typed bus, protocol-grade; metrics derive from events
  with a strict cardinality firewall — identity components never become metric
  labels (the `Event.Type`/`Extra`-only derivation rule).
- brief 06 §4: bounded reads and honest completeness; an unavailable aggregate
  must never masquerade as zero.
- brief 11 §LR-2: per-session aggregates (total cost, tokens, p50/p95 latency,
  tool-call counts) are Console features backed by protocol data.

## Findings I'm departing from (if any)

- D-296 decided Harbor does NOT become a counters/metrics TSDB and that trend
  series derive from the durable event log. This phase does not reverse that:
  it keeps the durable event log as the source of truth and rejects
  high-cardinality identity-labelled OTel metrics, but it materializes a
  rebuildable indexed projection so administrative queries do not scan the log
  at read time. The narrow amendment is recorded in D-426.
- D-309/Phase 174's per-session counter enrichment scans up to 10,000 events
  per visible session row and reports honest partials. This phase keeps that
  seam but lets the session enricher read the rollup projection when current,
  with the honest partial-scan fallback when the projection is unavailable or
  stale. The read-time scan remains, not as the primary path but as the honest
  fallback.

## Goals

- Build a durable observability rollup projection behind its own typed
  interface and §4.4 seam with in-memory, SQLite, and Postgres drivers and one
  conformance suite (the indexed triad).
- Consume canonical Harbor outcomes incrementally from the existing local
  durable event log: `llm.cost.recorded`, task lifecycle events, and other
  canonical outcome events — existing source-backed measures only, with no new
  canonical events or payload fields added merely to fill analytics.
- Store aggregate rows, never duplicate raw event payloads; rebuild fully from
  the durable event log.
- Expose fixed UTC MINUTE storage buckets (a query may coarsen) with
  authoritative dimensions and existing source-backed measures only, and
  explicit freshness/completeness (`current` / `catching_up` /
  `unavailable` plus an observed watermark and retention quality) on every
  query response.
- Back the session enricher's counters from the projection when current, with
  the existing honest partial-scan fallback.
- Ship ONE bounded Protocol-owned administrative query surface and a minimal
  Console consumer (the Sessions page counter read through the typed Protocol
  surface).
- Participate in Harbor's deletion semantics: session erasure removes or
  tombstones every aggregate attributable to that session and reconciles
  parent user/tenant totals; a rebuild never resurrects erased aggregates.

## Non-goals

- No billing-exact ledger: rollups are best-effort aggregates over
  successfully persisted canonical events; exact per-call billing remains the
  event log's authority and governance's cost accumulator.
- No outbox, no new canonical event ID, no active-active exactly-once: the
  projection uses the existing per-runtime
  durable event sequence for idempotency and a durable applied-through
  watermark; the fail-loud LLM publication contract is unchanged and
  projection application failures are best-effort; multi-replica application
  is documented as at-least-once
  idempotent on that local sequence, never claimed exactly-once across
  active-active replicas.
- No general-purpose Harbor TSDB and no identity-labelled OTel metrics:
  D-296's decided-NO on both stands (D-426 records the narrow amendment).
- No opaque aggregate blobs on the generic StateStore floor recovered through
  `ListKind` scans; no high-cardinality metric labels derived from
  tenant/user/session/run identity.
- No raw event-payload duplication, no per-read raw-event scanning for the
  supported query shapes, and no operator analytics beyond the one bounded
  administrative query surface.
- No new impersonation/content-read authority: widened queries require the
  existing server-derived admin / `console:fleet` claims from the request
  context (never the body) with the established widened-read audit evidence.

## v1.29.4 production correction and release evidence

The v1.29.4 correction makes rollup lost-wake polling compare the cheap source
watermark with the durable projection checkpoint before opening a bounded
source page. A current idle projection performs no global event-head scan;
stale and catching-up projections retain the existing bounded replay and
explicit completeness semantics. This stays within D-426's existing indexed,
rebuildable, best-effort projection contract and adds no new canonical event,
Protocol method, or analytics authority.

Implementation PR #733 merged at
`90f5f8ce96f83f994462e33cdfeccc77c535ca7e`; hosted candidate run
`32620015889` completed successfully, including the live preflight,
PostgreSQL conformance, both Go platforms, Playwright, isolation, leak,
chaos, lint, docs, and examples. The immutable annotated `v1.29.4` tag
object is `d85ca3928171cbf5c72e890f7c4b622e4b2cf1ff` and peels to
`90f5f8ce96f83f994462e33cdfeccc77c535ca7e`; release workflow `32622414573`
succeeded, publishing [13 release assets](https://github.com/hurtener/Harbor/releases/tag/v1.29.4)
with verified aggregate `checksums.txt`, six sidecar checksums, and six
GitHub attestations. The native darwin/arm64 artifact reports Harbor v1.29.4,
Protocol 0.1.0, build `90f5f8ce96f83f994462e33cdfeccc77c535ca7e`; module
provenance records `Sum=h1:GNQ902D6ddXlYtiOmC+wGMN7LSbE7VQilFb5HggKUyU=`,
`GoModSum=h1:mlX6OoauN4FzVO6Bw2PZTvb3l1tf3y4WHYRzudiTkYg=`,
`Origin.Hash=90f5f8ce96f83f994462e33cdfeccc77c535ca7e`, and
`Origin.Ref=refs/tags/v1.29.4`. The post-tag scaffold pin and golden
fixtures are complete. Focused local
`go test ./cmd/harbor -run TestScaffold_Golden` and `make drift-audit`
passed; local `make preflight` was not run. No downstream runtime, fleet,
or database mutation is claimed.

## Acceptance criteria

- [ ] A session emits more than 10,000 events; session and admin usage queries
      still return exact projection-backed totals without `counters_partial`
      and without scanning those events at read time.
- [ ] Cost and all supported token dimensions reconcile with canonical
      `llm.cost.recorded` fixtures, including sub-cent calls and cache-token
      fields, under the declared best-effort contract (a successfully persisted
      event is always reflected; the projection never invents a value it did
      not observe).
- [ ] Queries group correctly by tenant, user, session, and model across
      multiple users, concurrent sessions, and models, with no identity bleed.
      The storage dimension set is exactly the fixed UTC MINUTE bucket plus
      the authoritative
      `(tenant_id, user_id, session_id, model)` — `agent_id` is not a rollup
      dimension, not even conditionally, no other entity dimension is added,
      and a query may coarsen the bucket.
- [ ] Successful LLM completions (`llm.cost.recorded`) and task
      completed/failed/cancelled counts are distinct source-backed measures
      backed by existing canonical events; attempts, failed LLM calls,
      retry/downgrade, task-spawned, and user-message counts are unsupported
      and reported unavailable — never mandated, inferred, or backed by new
      canonical events. Unsupported measures are omitted or marked
      unavailable, never synthesized.
- [ ] The projection consumes only successfully persisted canonical events
      from the existing local durable sequence; there is no outbox and no new
      canonical event ID. The fail-loud LLM publication contract is unchanged
      and projection application failures are best-effort. Replaying the same
      source event is idempotent; restart catch-up, a crash between source
      persistence and projection application, and concurrent replica
      application do not lose or double-count values under the documented
      at-least-once idempotent contract (never an active-active exactly-once
      claim).
- [ ] Every query response carries an observed watermark/freshness stamp and an
      explicit completeness state: `current`, `catching_up`, or `unavailable`
      (with `rebuilding` and retention-quality signals where applicable). A
      query never returns zero as a substitute for "projection unavailable";
      the session enricher's projection-backed counters fall back to the
      existing honest partial scan (`CountersPartial`) when the projection is
      unavailable or stale.
- [ ] Fixed UTC MINUTE storage buckets are the base grain (a query may
      coarsen); the base dimension set is exactly
      `(tenant_id, user_id, session_id, model, time_bucket)` with existing
      source-backed
      measures only: the `llm.cost.recorded` successful-completion count,
      exact integer/decimal cost, prompt/completion/reasoning/cache-read/
      cache-write/total tokens, latency count/sum/min/max, and task
      completed/failed/cancelled counts. Attempts, failed LLM calls, retry/
      downgrade, task-spawned, and user-message counts are unsupported and
      reported unavailable, never synthesized.
- [ ] A verified fleet caller can run widened grouped queries and produces
      exactly the required audit evidence (`audit.admin_scope_used` on the
      widened fan-in); an ordinary caller cannot enumerate another user,
      session, or tenant, and the request body never supplies tenant/user/
      session identity for widening.
- [ ] Session erasure removes the session's rows and reconciles every
      higher-level grouping; no unretractable pre-summed total survives, and a
      full rebuild does not resurrect erased aggregates. Retention policy and
      the rebuildable event-log horizon are explicit; if source events needed
      for a full rebuild have been removed, the rebuilt projection exposes that
      historical incompleteness (retention quality).
- [ ] SQLite and Postgres query plans use bounded indexed access for the
      supported filters/groupings; acceptance includes a large fixture proving
      query work is independent of the total raw-event count.
- [ ] The existing session enricher either reads this projection or remains an
      honest fallback — this phase ships the projection-backed path with the
      honest fallback, and `sessions.list` / `sessions.inspect` never regress
      to a false-empty page (D-309's WARN-3 rule holds).
- [ ] ONE bounded Protocol query surface ships (`observability.query`):
      mandatory time window,
      server-authorized filters over tenant/user/session/model (where
      authoritative) and outcome, a closed `group_by` set, bounded bucket
      sizes, pagination and deterministic sorting for ranked results, exact or
      explicitly partial/freshness-marked results, and a maximum
      result/bucket budget that fails loudly. No second
      administrative query surface is added.
- [ ] A minimal Console consumer uses only the typed Protocol surface: the
      Sessions page counter read is projection-backed through
      `sessions.list`/`sessions.inspect` (the enricher reads the projection);
      the Console never reads the projection database directly and never
      maintains the only historical copy.
- [ ] Driver conformance, concurrent-reuse, cross-isolation, restart, replay,
      failure-injection, and real Protocol integration tests ship with the
      feature; N>=100 concurrent mixed-identity projection writes and reads
      pass under `-race` with no data race, context bleed, cancellation
      cross-talk, or goroutine leak.
- [ ] Canonical wire types/methods/errors, body-scope registration, transport,
      client helper, generated docs, operator skill, and Console TypeScript
      lockstep land together without a `ProtocolVersion` bump. D-296's
      rejection of a general-purpose Harbor TSDB and identity-labelled OTel
      metrics is retained and recorded in D-426.

## Files added or changed

- `internal/observability/rollup/` (or the natural subsystem home) — typed
  interface, incremental applier, watermark, bucket/shape builders,
  conformance suite
- `internal/observability/rollup/drivers/{inmem,sqlite,postgres}/` — the
  indexed triad behind the §4.4 seam
- `internal/sessions/protocol/enricher.go` — projection-backed counters with
  the honest partial fallback
- `internal/protocol/{types,methods,errors,bodyscope,singlesource,transports}/`
  — the one bounded administrative query surface
- `internal/protocol/client/` and `web/console/src/lib/protocol/` lockstep
- generated Protocol docs (`make protocol-docs-gen`) and the operator skill
- `web/console/src/lib/sessions/` — minimal counter read through the typed
  surface
- `test/integration/observability_rollups_test.go`
- `docs/glossary.md`, `docs/decisions.md` (D-426, including the D-296
  amendment), `docs/plans/README.md`, `RFC-001-Harbor.md`, `docs/skills/`,
  and `CHANGELOG.md`
- `scripts/smoke/phase-247.sh`

## Public API surface

- One administrative query method (`observability.query`): a
  mandatory time window, server-authorized filters, a closed `group_by` set,
  bounded buckets, pagination with deterministic ordering, and a
  freshness/completeness block (`state: current|catching_up|unavailable`,
  `watermark`, retention quality) on every response.
- The rollup projection behind one typed Go interface with in-memory, SQLite,
  and Postgres drivers; no new optional driver capability (D-296 amendment
  records why this projection is allowed while a general TSDB is not).
- Session counter backfill: `sessions.list` / `sessions.inspect` counters are
  projection-backed when the projection is current, with the existing
  `CountersPartial` honest fallback; wire shape unchanged.

## Test plan

- **Unit:** bucket/dimension/measure shape; fixed UTC minute bucketing and
  coarsening; sub-cent and
  cache-token fidelity; source-backed measure mapping (no synthesis);
  unsupported measures (attempts, failed LLM calls, retry/downgrade,
  task-spawned, user-message counts) reported unavailable; closed
  `group_by` rejection; watermark and completeness transitions
  (current/catching_up/unavailable/rebuilding); retention-quality marking;
  idempotent replay on the local durable sequence; erasure fence and parent
  reconciliation; budget-limit loud failure; identity-scope enforcement and
  widened-read audit.
- **Integration:** real durable drivers with a >10,000-event session fixture;
  projection-backed session counters with the honest fallback under a forced
  projection failure; crash-between-persist-and-apply and restart catch-up;
  concurrent replica application (at-least-once idempotent, no active-active
  exactly-once claim); session erasure removes rows and reconciles groupings;
  rebuild does not resurrect erased aggregates; a large fixture proving query
  work is independent of the raw-event count; N>=100 concurrent mixed
  identities under `-race`.
- **Conformance:** all three rollup drivers pass one conformance suite
  (indexed query shapes, idempotency, watermark, erasure fence); Protocol
  integration across driver combinations owns authority and audit assertions.
- **Concurrency / leak:** N>=100 concurrent applier/query/erase operations on
  one shared projection under `-race`, with cancellation barriers and a final
  goroutine baseline.
- **Fuzz:** request decoding, group_by/filter shapes, and bucket windows with
  bounded allocations and no panics.

## Smoke script additions

- Produce a >10,000-event durable session, run the one
  administrative query `observability.query`, and assert projection-backed
  totals without
  `counters_partial` and without read-time scans; assert a stale/unavailable
  projection surfaces `catching_up`/`unavailable` (never zero) and the session
  enricher falls back honestly; assert a widened fleet query emits the
  established audit evidence and an ordinary caller cannot enumerate another
  identity.

## Coverage target

- Rollup projection package: 90%; touched drivers: 90%; session enricher:
  measured floors with 100% on the new projection-backed/fallback branches;
  new Protocol authority paths: 100%; integration package: 85%.

## Dependencies

- Depends on Phases 36a, 57, 120, 130, 163, 171, 174, and 205.
- Consumed by the session enricher (Phase 174 seam) and a minimal Console
  counter read; gates no later phase in this wave.

## Risks / open questions

- Every measure must map to an existing canonical event payload; a measure
  with no existing carrier is unsupported and reported unavailable — never
  synthesized and never the occasion for a new canonical event. The exact
  mapping is a planning-time decision against the shipped event types:
  supported measures are the `llm.cost.recorded` successful-completion
  count, exact integer/decimal cost, prompt/completion/reasoning/cache-read/
  cache-write/total tokens, latency count/sum/min/max, and task
  completed/failed/cancelled counts; attempts, failed LLM calls, retry/
  downgrade, task-spawned, and user-message counts are unsupported.
- The durable applied-through watermark is per-runtime (the existing local
  durable sequence); operators running multiple replicas share one storage
  backend, so replica application must be at-least-once idempotent on that
  sequence and the missing exactly-once property must be stated, not claimed.
- The indexed triad's query plan must be proven bounded by fixture; a
  group_by/filter combination outside the indexed set fails loudly rather than
  degrading to a scan.
- Session erasure reconciliation of parent user/tenant totals must be tested
  against concurrent writes; the erasure fence and the rebuild path must agree
  on what "erased" means (rows removed, never resurrected).

## Glossary additions

- **Observability rollup projection**
- **Rollup watermark**
- **Rollup completeness state**

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages >= stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] Concurrent-reuse test passes with N>=100 under `-race`, including no
      data races, context bleed, cancellation cross-talk, or goroutine leaks.
- [ ] Real-driver integration wires the >10,000-event fixture, identity
      propagation, erasure reconciliation, and a failure mode under `-race`.
- [ ] If new vocabulary: glossary updated
- [ ] The D-296 amendment and the best-effort/watermark contract are recorded
      in D-426 before implementation merges.
