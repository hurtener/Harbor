# Phase 163 — Windowed-reads honesty pair: `flows.runs.list` time filter + retention horizons as Protocol data

## Summary

Two small, bundled honesty asks (operator-filed, 2026-07-10; a second
Protocol consumer rendering fleet views over a `{from, to}` window). First (LOW): `flows.runs.list` is the one run-history
read with no time bound — `FlowRunsListRequest` filters only by
`flow_id`/`tenants`/`page`/`page_size` — so a 7-day flow-run view is a
full-history walk with a client-side date filter; this phase adds optional
`since`/`until` mirroring `TaskFilter` exactly. Second (MEDIUM): a consumer
promising "the last 7 days" cannot learn a runtime's retention BEFORE
reading — the only signal is the per-read `truncated` flag after the edge is
hit; this phase surfaces per-surface retention horizons as additive Protocol
data. The ask's premise is corrected against the tree: NO retention config
exists today (the durable event log is "gap-free and untrimmed in V1",
`durable.go:776`), so the honest v1 shape is the OBSERVED
oldest-retained-timestamp per durable surface, not a configured duration.
The deliberately-rejected sibling is re-recorded so it is not re-opened:
Harbor does NOT become a time-series DB for counters/metrics. Both changes
are additive wire (full D-223/D-209 regen); consumers ship same-phase.

## RFC anchor

- RFC §5.2
- RFC §6.1
- RFC §6.13
- RFC §6.14
- RFC §7

## Briefs informing this phase

- brief 06
- brief 11

## Brief findings incorporated

- brief 06 §5 ("Metrics cardinality footgun" + the one-bus lesson): metrics
  derive from bounded-cardinality event metadata and `runtime.counters` /
  `metrics.snapshot` are point-in-time roll-ups — brief 06's design never
  made them a history substrate. The TSDB rejection this phase re-records
  (see Non-goals) is that posture read forward: trends derive from the
  durable event log; counters are never sampled into a history table.
- brief 11 LR-3 (time range on observability views): "Time range — 'Last 30
  min' default" — brief 11 designed every observability view around a
  time-bounded read. Tasks and sessions got their bounds
  (`TaskFilter.Since/Until`, `SessionFilter` started-window); the flow-run
  history read is the straggler this phase brings level.
- brief 11 (Flows view): "Per-flow: visual DAG …, source-of-truth view …,
  test history" — the flow detail page's run-history table is a designed
  surface; its date filter is the natural consumer of the new bounds.

## Findings I'm departing from (if any)

- **The filed ask's premise "the events `durable` driver already has a
  retention setting; this just surfaces it over the Protocol" is FALSE
  against the tree** — verified: no retention/prune knob exists anywhere
  (`internal/config/config.go:814-823` `EventsConfig` carries only
  driver/buffer/idle/drop-window/replay-buffer/state-DSN keys; the durable
  log is explicitly "gap-free and untrimmed in V1",
  `internal/events/drivers/durable/durable.go:776`; the inmem driver's
  horizon is its ring capacity, `ReplayBufferSize`). The plan therefore
  surfaces the OBSERVED oldest-retained-timestamp (always truthful,
  derivable today) instead of echoing a configured duration that does not
  exist. A configured-retention field becomes additive IF a retention knob
  ever ships (a separate lifecycle feature, deliberately out of scope
  here). Recorded in D-296.

## Goals

- **HA — flow-run time filter (verified anchors).** `FlowRunsListRequest`
  (`internal/protocol/types/flows.go:303-320`; Console mirror
  `web/console/src/lib/flows/types.ts:133-140`) gains optional `since` /
  `until` (RFC-3339 UTC, inclusive-lower / exclusive-upper on the run's
  `StartedAt` — the field the run rows already carry and sort by,
  `flows.go:289-290`, `:323`), mirroring `TaskFilter.Since`/`.Until`
  EXACTLY (`internal/protocol/types/tasks.go:211-214`; Console
  `tasks.ts:107-108`). Additive: both optional, absent ⇒ unbounded — zero
  behavior change for existing callers. Same tenant/admin scope rules the
  request already carries (`Tenants` beyond the caller's own requires the
  admin claim; scope derived server-side).
- **HA — retention horizons as data (premise-corrected shape).** Additive
  `retention` block on the `runtime.health` response
  (`internal/protocol/types/posture.go:119` `RuntimeHealth`): one entry per
  durable surface — `events`, `tasks`, `sessions` — each carrying the
  OBSERVED `oldest_retained_at` (RFC-3339; zero/absent when the surface
  holds no rows yet). `state.history`'s substrate IS the event log, so the
  `events` entry covers it (documented on the field). Placement rationale:
  `runtime.health` over `runtime.info` because horizons are OPERATIONAL
  facts that advance over time and health is the polled operational
  surface; `runtime.info` stays identity/build/capability-shaped
  (`posture.go:40-70`).
- **Pairing.** The forward-looking horizon composes with the at-read
  `truncated` flag (`state.history` today; `events.list` after Phase 162):
  horizon = "expect gaps past X" before reading; `truncated` = "this read
  hit the gap" after.
- **Consumers, same phase (D-062):**
  - Flow detail page: the run-history table
    (`web/console/src/routes/(console)/flows/[flow_id]/+page.svelte:147`,
    fed by `web/console/src/lib/flows/detail.svelte.ts`) gains a date-range
    filter driving the new bounds — server-side filtering replaces the
    walk-and-filter interim.
  - Console Events page: a window-edge honesty banner — when the picked
    window (`WINDOW_SPEC`, `events.ts:197-202`) starts before the runtime's
    `events` `oldest_retained_at`, the page says "this runtime retains
    events only back to X" instead of implying a complete window (composing
    with Phase 162's historical read and its `truncated` notice).

## Non-goals

- **No TSDB, re-recorded as decided-NO so it is not re-opened**
  (operator-decided, 2026-07-10; the full rationale is encoded here and in
  D-296 so this document is self-contained):
  `runtime.counters` / `metrics.snapshot` are now-only snapshots (single
  `snapshot_at` — `internal/protocol/types/posture.go` `RuntimeCounters.
  SnapshotAt`; Console `posture.ts:41`, `:131`) and STAY that way. Harbor
  is not asked to store counter/metric time-series; trends derive from the
  durable event log (rebuildable by any consumer), and metrics route to an
  out-of-band scrape (`MetricsRegistry` → Prometheus-style TSDB, RFC
  §6.14). A consumer sampling snapshots into its own history table would
  hold the only copy — the exact shadow-store shape both sides' rules
  forbid.
- No retention CONFIGURATION (no pruning knob, no lifecycle change) — the
  durable log stays untrimmed in V1; this phase only reports what is
  observably retained. A future retention knob is a separate phase.
- No horizon entry for non-durable/capability surfaces (memory is a
  capability server's data, not Harbor core).
- No `sessions.list`/`tasks.list` filter changes (their bounds already
  exist); no Protocol version bump (all additive).

## Acceptance criteria

- [ ] `FlowRunsListRequest.Since`/`.Until` (additive, `omitempty`,
  RFC-3339): bounds filter run rows on `StartedAt`
  (inclusive/exclusive); absent ⇒ unbounded; `until < since` fails
  `CodeInvalidRequest` (matching the tasks-filter posture); paging
  interacts correctly (bounds applied before pagination; page counts
  reflect the bounded set).
- [ ] Scope discipline unchanged and re-pinned: a `Tenants` widening beyond
  the caller's tenant still requires the verified admin claim; scope never
  read from the request body.
- [ ] `RuntimeHealth` gains the additive `retention` block: per-surface
  `oldest_retained_at` for `events` / `tasks` / `sessions`, derived from
  each store's oldest retained row (events: the log/ring head's
  `OccurredAt`; tasks/sessions: the oldest retained record's start/open
  time); absent/zero when a surface holds no rows; values are OBSERVED,
  never configured claims.
- [ ] Driver honesty: on the inmem events driver the `events` horizon
  reflects the ring head (advances as the ring evicts); on the durable
  driver it reflects the persisted head. No capability ceremony — both
  drivers answer through the same seam.
- [ ] Flow detail page: date-range filter drives the new bounds server-side;
  clearing the filter restores unbounded paging; vitest covers the filter
  fold.
- [ ] Events page: the window-edge banner renders when the picked window
  predates the `events` horizon; hidden otherwise; vitest covers the
  compare.
- [ ] Full lockstep in the same PR: `make protocol-ts-gen` (manifest +
  `flows` + posture TS mirrors), `make protocol-docs-gen`,
  `singlesource.CanonicalWireTypes` + typeindex rows. `ProtocolVersion`
  unbumped.
- [ ] `scripts/smoke/phase-163.sh` OK ≥ 2, FAIL = 0.
- [ ] `-race`; coverage ≥ 85% on touched Go packages.

## Files added or changed

- `internal/protocol/types/flows.go` (`:303-320`) — `Since`/`Until` on
  `FlowRunsListRequest`.
- `internal/protocol/types/posture.go` — the `retention` block on
  `RuntimeHealth` (+ the per-surface entry type).
- The flows protocol service (the `flows.runs.list` projector) — bounds
  applied on `StartedAt` before pagination.
- The posture/health service — the per-surface oldest-retained derivation
  (events via the bus seam's head; tasks/sessions via their registries'
  oldest-row reads; the implementor picks the cheap query per store).
- `internal/protocol/singlesource/singlesource.go` + generator typeindex
  files; `internal/protocol/conformance/conformance.go` row updates if the
  method table changes (none expected — no new method).
- `web/console/src/lib/flows/types.ts` (`:133-140`), `flows` client mirror,
  `web/console/src/lib/protocol/posture.ts`, `wire-manifest.gen.json`
  (regenerated).
- `web/console/src/lib/flows/detail.svelte.ts` +
  `web/console/src/routes/(console)/flows/[flow_id]/+page.svelte` — the
  date-range filter.
- `web/console/src/routes/(console)/events/+page.svelte` — the window-edge
  honesty banner.
- `test/integration/phase163_windowed_honesty_test.go` (new).
- `docs/site/protocol/types.md` (regenerated); `scripts/smoke/phase-163.sh`
  (new); `docs/plans/README.md`; `docs/decisions.md` (D-295 + D-296);
  `docs/glossary.md`.

## Public API surface

- Wire: `FlowRunsListRequest.since`/`.until` (additive);
  `RuntimeHealth.retention` (additive per-surface
  `oldest_retained_at` block). No new methods.
- Console: flow-run date filter + events window-edge banner
  (Console-internal).

## Test plan

- **Unit:** flows bounds table (absent/one-sided/both/invalid-range/paging
  interaction); retention derivation per store (empty store ⇒ absent; ring
  eviction advances the events horizon; durable head stable); posture
  handler additive-shape round-trip (old clients unaffected — additive
  JSON).
- **Integration (`test/integration/phase163_windowed_honesty_test.go`):**
  real drivers — seed flow runs across a time spread, read bounded windows
  server-side and assert the client-side-filter interim equivalence; read
  `runtime.health` and assert the `events` horizon matches the actual
  oldest retained event (both drivers); identity propagation + a
  cross-tenant refusal failure mode; `-race`.
- **Conformance:** N/A — no driver-interface change beyond the derivation
  read (covered by the integration legs on both events drivers).
- **Concurrency / leak:** horizon reads under concurrent publish/evict
  (N≥100, `-race`) never race the ring (extends the drivers' D-025 stress);
  no goroutine growth.

## Smoke script additions

- live-server, TWO health-side assertions so the done-definition is
  meetable even when the dev config declares no flows: (1) the
  `runtime.health` response carries the `retention` block; (2) after a
  scripted run (the `start` method, `POST /v1/control/start`) the block's
  `events` entry carries a non-empty, RFC-3339-parsable
  `oldest_retained_at`. Third, flows leg: `flows.runs.list` accepts
  `since`/`until` without `invalid_request` (`skip_if_404` when the dev
  config declares no flows — the bounds semantics are integration-covered).
- Done-definition: `OK ≥ 2, FAIL = 0` (achievable from the two health
  assertions alone); 404/405/501 → SKIP until the phase ships.

## Coverage target

- `internal/protocol/types` + the flows/posture service packages: 85%
- `internal/events` (the horizon derivation): existing target maintained
- Console: vitest suites named above.

## Dependencies

- 26a (flows + run history), 108p (the flows page the consumer lands on),
  72f (runtime posture surfaces), 108h + 162 (the Events page window work
  the banner composes with — 162 lands first in staging), 118 (D-223
  lockstep), 125 (the `truncated` at-read flag this pairs with).

## Risks / open questions

- **Observed-horizon derivation cost.** Oldest-row reads must be cheap
  (head record / min-index queries, never table scans on the hot path);
  health is polled — the implementor may cache the horizon briefly
  (seconds) since it only advances. Named so the review checks the query
  shape.
- **Sessions GC interplay.** The session registry's GC reaps idle sessions,
  so the sessions horizon is "oldest retained", not "oldest ever" — exactly
  the honest semantics; the godoc must say so to preempt confusion.
- **The premise correction is load-bearing.** If a reviewer expects
  "surface the existing retention config", the plan's Findings-departure +
  D-296 carry the verified evidence that no such config exists; do not
  silently re-introduce a phantom config echo.

## Glossary additions

- "retention horizon" (docs/glossary.md, same PR).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
      (the flows-bounds cross-tenant refusal leg).
- [ ] **Reusable-artifact concurrent-reuse:** the events drivers' D-025
      stress extended with concurrent horizon reads (N≥100, `-race`).
- [ ] **Integration test wires real drivers end-to-end, asserts identity
      propagation, covers ≥1 failure mode, runs under `-race`** (§17.3).
- [ ] Wire changes complete: `make protocol-ts-gen-check` +
      `make protocol-docs-gen-check` green with regenerated artifacts
      committed.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above (the
      retention-config premise correction) + recorded in D-296.
