# Phase 245 — Lifecycle-only session catalog and inspection projection (HA-63)

## Summary

Add an explicit lifecycle-only projection selector to the existing
`sessions.list` and `sessions.inspect` methods. The selector returns only
session lifecycle metadata with zero counter enrichment; the full projection
remains the default and is unchanged; counter-dependent filters and sorts
paired with the lifecycle selector fail as a typed invalid request rather than
silently switching to the expensive projection.

## RFC anchor

- RFC §5.2
- RFC §5.5
- RFC §6.9
- RFC §6.13
- RFC §7
- RFC §9

## Briefs informing this phase

- brief 05
- brief 06
- brief 11

## Brief findings incorporated

- brief 05 §4: the session registry is a lifecycle record (open/close/GC) and
  identity is captured and immutable; the registry projection is pure.
- brief 06 §4: identity-triple filtering is server-enforced and bounded reads
  never masquerade as complete ones (D-311 honesty: absence is representable).
- brief 11 §"Sessions view": a catalog list needs lifecycle metadata and
  drill-down; counter columns (cost/tokens/events) are a separate concern that
  must not price every catalog read.

## Findings I'm departing from (if any)

- D-309/Phase 174 wired a read-time `Enricher` seam so every `sessions.list` /
  `sessions.inspect` row is counter-enriched by a bounded per-session history
  scan (`internal/sessions/protocol/enricher.go` — `perSessionScanBound =
  10_000`, truncation surfaced as `SessionRow.CountersPartial`). That seam
  stays; this phase adds a lifecycle-only projection that deliberately does
  NOT invoke it, because a catalog/chat-open read needs only lifecycle fields.
  D-309's honest-partial contract for the full projection is preserved
  unchanged.

## Goals

- Add one additive `projection` selector to both `sessions.list` and
  `sessions.inspect`; `"full"` (the current shape, counter-enriched) is the
  default and remains explicitly selectable; `"lifecycle"` returns lifecycle
  metadata only.
- The lifecycle path performs ZERO enrichment: no event-history replayer reads,
  no task/pause/artifact/App lookups, no counter scans. Work is bounded by the
  page size, before and after restart, independent of total event/turn
  cardinality.
- Return, per row: session id, lifecycle status, title and title source,
  start/update/completion/last-activity timestamps where authoritative,
  duration where derivable without enrichment, and the effective/default agent
  id only where Harbor can represent it honestly.
- Counter fields (events, tasks, cost, tokens, pending-intervention,
  failed-task flags) are absent or explicitly marked unavailable in the
  lifecycle shape; zero never means "not computed".
- Reject, as a typed invalid request, a counter-dependent filter or sort
  (`cost_above_cents`, `has_failed_task`, `has_intervention`, `cost_desc`)
  paired with the lifecycle selector. Lifecycle filters/sorts (status, title,
  started/last-activity order) keep their existing semantics.
- Keep the full projection compatible and explicitly selectable for operator
  surfaces that display counters.

## Non-goals

- No change to the full projection's counter exactness, `CountersPartial`
  honesty contract, or the enricher seam.
- No retention change, no coordinator cache, no new lifecycle fields, no
  second session data source, no counter recomputation path.
- No new authority: the selector changes cost and shape, never authority.
  Each method preserves its own widening claims and audit behavior exactly;
  this phase does not unify or broaden the admin/fleet gates.
- Raw events and the full session projection remain the diagnostic surfaces;
  this selector does not replace full operational inspection.

## Acceptance criteria

- [ ] `sessions.list` and `sessions.inspect` both accept an additive
      `projection` selector whose default value is `"full"` and whose
      `"lifecycle"` value is documented; `ProtocolVersion` stays `0.1.0`
      (additive field).
- [ ] With a real durable session containing more than 100,000 events,
      lifecycle list and inspect perform zero history-replayer/enricher reads;
      request work remains bounded by the page size before and after restart.
      Prove the enricher seam is not invoked by instrumentation (a
      spy/not-called assertion), not by timing.
- [ ] A page of N session rows never runs N counter scans on the lifecycle
      path; existing full reads still return exact/partial counters with their
      present honesty contract (`CountersPartial`).
- [ ] The lifecycle row carries: session id; lifecycle status; title and title
      source (`unset | auto | manual`); started/updated/completed/last-activity
      timestamps where authoritative; duration where derivable without
      enrichment; and the effective/default agent id only where honestly
      representable (otherwise absent or explicitly unavailable). Counter
      fields are absent or explicitly marked unavailable — a zero value never
      means "not computed".
- [ ] Cursor, lifecycle filter, and lifecycle sort behavior is stable and
      unchanged in semantics; a counter-dependent filter or sort
      (`cost_above_cents`, `has_failed_task`, `has_intervention`, `cost_desc`)
      paired with `projection: "lifecycle"` fails as a typed invalid request
      and never falls back to enrichment.
- [ ] Same-user, foreign-user, cross-tenant, signed-session-reach, admin/fleet,
      and erased-session cases pass on every durable driver; cross-identity
      denial does not become an existence oracle (a foreign lifecycle read
      returns the same non-oracular not-found/denied posture as the existing
      session surface).
- [ ] The lifecycle projection registers with the projection-completeness gate
      (Phase 177) so its intentionally-absent counter fields are allow-listed
      with reasons and no counter filter/sort can silently operate over them.
- [ ] Protocol manifest, generated clients, protocol docs, the operator skill,
      and Harbor's own consumer chat catalog use the new projection; the first
      consumer is the two-read chat-open path specified by Phase 246 plus a
      minimal Console catalog read, per D-062.
- [ ] Full-project behavior is byte-compatible for existing callers: a request
      without the selector returns exactly what it returns today.
- [ ] N>=100 concurrent mixed-identity lifecycle list/inspect calls on one
      shared projector pass under `-race` with no data race, context bleed,
      cancellation cross-talk, goroutine leak, or cross-identity row.

## Files added or changed

- `internal/protocol/types/sessions.go` — additive `projection` request field
  and lifecycle row shape (absence-representable counters)
- `internal/protocol/{methods,singlesource,errors}/` — canonical method
  constants/errors for the typed counter-filter rejection
- `internal/sessions/protocol/{filter.go,protocol.go,lister_projector.go,
  cursor.go,projection_register.go}` — lifecycle selector, counter-filter
  rejection, lifecycle projector, register with the projection gate
- `internal/runtime/serve/` — wiring only; the enricher is NOT invoked on the
  lifecycle path
- `internal/protocol/client/` and `web/console/src/lib/protocol/` lockstep
  (hand-maintained mirror + `make protocol-ts-gen`)
- generated Protocol docs (`make protocol-docs-gen`) and the operator skill
  `use-the-harbor-protocol`
- `test/integration/session_lifecycle_projection_test.go`
- `docs/glossary.md`, `docs/decisions.md`, `docs/plans/README.md`,
  `RFC-001-Harbor.md`, `docs/skills/`, and `CHANGELOG.md`
- `scripts/smoke/phase-245.sh`

## Public API surface

- `sessions.list` / `sessions.inspect` accept an additive optional
  `projection` field: `"full"` (default) or `"lifecycle"`.
- Lifecycle row shape: `session_id`, `status`, `title`, `title_source`,
  `started_at`, `updated_at`, `completed_at`, `last_activity_at`, `duration`,
  `agent_id` (only when honestly representable), plus an explicit
  `counters: "unavailable"` marker when the caller asked for lifecycle (or
  absent counter fields — never zero-as-not-computed).
- A counter-dependent filter/sort with `projection: "lifecycle"` returns the
  canonical `invalid_request` typed error; no new error code beyond the
  existing registry unless the closed set lacks a fit.

## Test plan

- **Unit:** selector parsing and default; lifecycle row shape (absent vs zero
  counters); counter-filter/sort rejection matrix for every counter-dependent
  filter and `cost_desc`; lifecycle filter/sort semantics unchanged; cursor
  stability; enricher-not-invoked spy on the lifecycle path; full-projection
  byte compatibility.
- **Integration:** real durable sessions (SQLite + Postgres where available)
  with a >100,000-event fixture: lifecycle list/inspect perform zero enricher
  reads and are bounded by page size before and after restart; N-row pages do
  not run N scans; same-user/foreign/cross-tenant/signed-reach/admin/fleet and
  erased-session cases run across every durable driver; cross-identity denial
  is non-oracular; a forced enricher failure leaves the lifecycle path
  unaffected (it is not invoked).
- **Conformance:** every registered sessions/state driver combination passes
  the lifecycle and full projections through the Protocol integration suite;
  driver conformance does not claim method auth.
- **Concurrency / leak:** N>=100 mixed-identity lifecycle calls on one shared
  projector and handler set under `-race`, with cancellation barriers and a
  final goroutine baseline.

## Smoke script additions

- Before implementation, a pending static skeleton records this plan.
- When implemented, list a durable session with the lifecycle selector and
  assert the response carries lifecycle fields and no counter fields; assert a
  `cost_desc` sort or `cost_above_cents` filter with the lifecycle selector
  fails with the canonical typed error; assert the default projection still
  returns counters; assert cross-identity lifecycle reads are not-found.

## Coverage target

- `internal/sessions/protocol`: 90%; new Protocol authority/selector paths:
  100%; touched `internal/protocol` packages: measured floors with 100% on new
  branches; integration package: 85%.

## Dependencies

- Depends on Phases 130, 163, 174, 177, 205, and 232.
- Gates Phase 246 (the two-read chat-open consumer); the minimal Console
  catalog read lands with the same wave.

## Risks / open questions

- The lifecycle shape must choose the honest absence mechanism for counter
  fields (field omission vs an explicit `counters: "unavailable"` marker);
  either satisfies the "zero never means not computed" rule, but the choice is
  a wire-shape decision to pin before implementation (additive either way).
- The effective/default agent id is not a single-valued session binding today;
  the lifecycle row must represent honest absence rather than fabricating a
  binding (the D-309 agent-field class rule applies).
- The projection-completeness gate (Phase 177) must accept the lifecycle
  projection's allow-list entries; a counter filter over the lifecycle shape
  must fail at the gate level as well as at the surface level.

## Glossary additions

- **Session lifecycle projection**

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages >= stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] Concurrent-reuse test passes with N>=100 under `-race`, including no
      data races, context bleed, cancellation cross-talk, or goroutine leaks.
- [ ] Real-driver integration wires durable sessions with a >100,000-event
      fixture, identity propagation, and a failure mode (forced enricher
      failure) under `-race`.
- [ ] If new vocabulary: glossary updated
- [ ] The selector/absence decision and the D-309 extension are recorded in
      D-424 before implementation merges.
