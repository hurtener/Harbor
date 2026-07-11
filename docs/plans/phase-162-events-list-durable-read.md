# Phase 162 — `events.list`: durable, time-ranged, cross-session raw-event read

## Summary

A second Protocol consumer (an operator-filed ask, 2026-07-10) wants to
open a fleet observability view
and answer "show me the raw events from 7 days ago to now, and let me scroll
them." No surface answers it today: `events.subscribe` is a forward-only live
tail, `events.aggregate` returns bucketed counts with no payloads, and
`state.history` is per-session and sequence-windowed — so a pure Protocol
client would need exactly the shadow store it must not build (D-061 read from
the client side). This phase adds an additive `events.list` method: the
existing wire `EventFilter` (which already carries `since`/`until` and the
type/session/run axes) plus a tail-first paging cursor mirroring
`state.history`, returning the SAME flat event rows the SSE and `state.history`
already project — no new row shape, no redaction change, no heavy inlining.
The D-062 consumer ships in the same phase: the Console Events page gains the
historical-window read its own empty-state copy already hints at. Full D-223
lockstep + D-209 regen (a new method + wire types).

## RFC anchor

- RFC §6.13
- RFC §5.2
- RFC §4
- RFC §7

## Briefs informing this phase

- brief 06
- brief 11

## Brief findings incorporated

- brief 06 §5 ("Two-channel split"): "When the observability record and the
  stream chunk are separate records flowing through different paths … every
  dashboard, replay tool, and Console feature has to fuse them. Lesson: unify
  on one bus from t=0." `events.list` is a READ of the one bus's durable
  record — the same redacted rows the SSE fans out — never a second record
  path or a parallel store.
- brief 11 LR-3 (time range on the events surface): "Time range — 'Last 30
  min' default. Implies the event subscription is time-bounded; replay needs
  to materialize … an older window." Brief 11 designed the Console events
  surface around an older-window replay read; this phase ships that read
  (`events.subscribe` stayed forward-only and `state.history` arrived
  per-session — the fleet-window read is the remaining gap).
- brief 11 LR-5 (Event Stream dock): the event stream view is a first-class
  Console surface fed by the Protocol — the D-062 consumer here (the Events
  page window read) is the surface brief 11 enumerated, not a new invention.

## Findings I'm departing from (if any)

None.

## Goals

- **The gap, verified (file:line).** Three surfaces exist; none answers a
  time-ranged raw-row read:
  - `events.subscribe` (`GET /v1/events`, SSE) is forward-only live tail
    (`internal/protocol/methods/methods.go:30-36`).
  - `events.aggregate` (`POST /v1/events/aggregate`) returns bucketed COUNTS
    only — no payloads (same methods block; the Console's rate sparkline is
    its consumer, `web/console/src/routes/(console)/events/+page.svelte:8-9`).
  - `state.history` returns raw rows but is per-session and
    sequence-windowed (`web/console/src/lib/protocol/state.ts:20-26`; the
    handler at `internal/protocol/transports/stream/state_history_handler.go`).
  - `search.events` is NOT this read either: it is the server-enforced
    text-search index surface (`internal/protocol/methods/methods.go:179-183`;
    mapped onto its `SearchIndex` in `indexFor`,
    `internal/protocol/search.go:115`) — it answers "find events matching
    query q," never "enumerate the window's rows, paged."
  - Meanwhile the wire filter ALREADY carries the time bounds: wire
    `EventFilter.Since`/`.Until` (inclusive-lower/exclusive-upper RFC-3339 on
    `OccurredAt`, `internal/protocol/types/events.go:46-51`; Console mirror
    `web/console/src/lib/protocol/events.ts:59-63`). Their ONLY consumers
    today are predicates over the live/count paths — the wire-filter matcher
    (`events.MatchWire`, `internal/events/filter.go:158`, time bounds at
    `:187-191`) and the aggregate count-window clamp
    (`internal/events/aggregate.go:159-164`) — no historical ROW read
    consumes them anywhere.
- **The method.** Additive `events.list`: request = the existing wire
  `EventFilter` + `limit` + an opaque backward-paging `cursor`; response =
  `{events, next_cursor, has_more, truncated}` — tail-first windowing
  mirroring `state.history`'s `next_cursor`/`has_more` semantics, and rows
  reusing the EXISTING flat event row projection (`types.StateEvent` — the
  shape `state_history_handler.go` already projects, with the same
  `payloadWireValue` pass-through and `StateArtifactRef` seed extraction).
  NO new row shape.
- **Scoping (§6 item 5 — binding).** A non-widened caller reads ONLY rows
  matching its verified identity triple (the by-id `MatchesScoped` posture);
  fleet widening follows the CLOSED TWO-SCOPE read set the reused
  `EventFilter` contract already documents and the sibling aggregate handler
  already enforces — the verified `auth.ScopeAdmin` OR
  `auth.ScopeConsoleFleet` claim (`internal/protocol/types/events.go:41-44`;
  `internal/protocol/transports/stream/handlers.go:141-143`) — so the Events
  page's live and historical feeds can never diverge on authz. Scope is
  derived SERVER-SIDE from the verified session/JWT — never the request
  body — and every widened read emits `audit.admin_scope_used`. Precedent,
  cited precisely: the identity fan-in exists on `Subscribe`/`Replay` only
  (the inmem `Replay` admin path,
  `internal/events/drivers/inmem/inmem.go:600`); `Bounds`/`Window`
  contribute only the Bounds-then-Window single-audit pattern
  (`inmem.go:748`) — the windowed fan-in itself is NEW, which is what the
  godoc amendments below record.
- **Honesty at the window edge.** The response reuses the `truncated`
  retention-gap flag `state.history` already returns
  (`internal/protocol/types/state.go:121` posture): rows older than the
  substrate's oldest retained row fell off, and the response says so.
- **Substrate + honest degradation (no capability ceremony, §4.4/§9).** The
  read extends the existing `events.HistoryReplayer` seam (the optional
  windowed-read capability interface BOTH V1 drivers already implement,
  `internal/events/events.go` — the D-254 precedent): the durable driver
  serves real windows from the persisted, global-sequence-ordered log; the
  inmem driver returns what its ring holds (`ReplayBufferSize` capacity)
  with `truncated=true` when the requested window reaches past the ring —
  honest degradation, no `Supports*` protocol, no driver-only feature.
  **The seam's recorded scope contract is deliberately amended, not
  silently violated:** the `HistoryReplayer` godoc pins "scoped to the
  named session (`MatchesScoped`), never fanning in under Admin"
  (`internal/events/events.go:562` block), and `MatchesScoped`'s godoc
  records the cross-session disclosure that rule closed
  (`internal/events/events.go:403-417`). This phase amends both godocs to
  name the ONE sanctioned exception: an authority-gated,
  explicitly-requested fleet read on a DISTINCT method (`events.list` with
  the two-scope claim + per-request audit) — categorically different from
  the silent Admin-widening-of-a-by-id-read that was the original bug. The
  by-id reads (`Bounds`/`Window` for `state.history`) keep the never-fan-in
  rule verbatim.
- **Redaction + heavy content unchanged.** Rows stay the bus-redacted
  projection (the single redactor at the bus publish boundary is preserved;
  §7 rule 6); heavy payloads stay by-reference (`artifact_ref` seeds routed
  to `artifacts.get_ref`, D-026) — `events.list` inlines nothing the SSE
  would not.
- **D-062 consumer, same phase.** The Console Events page — whose empty-state
  copy already says "run a `durable` event driver for read-back"
  (`web/console/src/routes/(console)/events/+page.svelte:249-258`) — gains
  the historical window: the existing window picker (`WINDOW_SPEC`,
  `web/console/src/lib/protocol/events.ts:197-202`) drives `events.list` for
  the table's historical rows (scroll-up pagination via `next_cursor`), the
  live SSE tail stays exactly as-is for the forward edge, and the
  `truncated` flag renders the retention-gap notice.
- **Operator latitude — exercised as NO additions.** Candidate adjacent reads
  were evaluated and declined: single-event inspect-by-id (the returned rows
  are self-contained — the row IS the full redacted projection, so a detail
  drawer renders the already-fetched row; a by-id method adds nothing);
  event-type catalog for filter dropdowns (derivable from
  `events.aggregate`'s per-type buckets over the same window — a registry
  read would be a new feature, not the same read); cross-page linking reads
  (the rows already carry the session/run identity the Console links on).

## Non-goals

- No change to `events.subscribe` (live tail) or `events.aggregate` (counts)
  — this is the third leg beside them, not a replacement.
- No new row shape, no redaction change, no heavy-payload inlining (D-026
  by-reference discipline preserved verbatim).
- No retention configuration and no retention-horizon reporting — the
  forward-looking horizon surface is the sibling honesty phase (Phase 163);
  this phase carries only the at-read `truncated` flag.
- No `search.events` change — text search and window enumeration stay
  distinct surfaces.
- No coordinator-side caching design — what a consumer caches is its own
  concern; this phase only guarantees the read is re-runnable (rebuildable
  derived views stay legal client-side).
- No Protocol version bump — additive method + additive wire types.

## Acceptance criteria

- [x] `MethodEventsList` (`events.list`, `POST /v1/events/list`) registered
  (method set + canonical registration + conformance row); wire types
  `EventsListRequest{Identity, Filter EventFilter, Cursor, Limit}` /
  `EventsListResponse{Events []StateEvent, NextCursor, HasMore, Truncated}`
  in `internal/protocol/types` (single source, §8).
- [x] Tail-first paging semantics pinned: zero cursor ⇒ newest rows; each
  page is oldest-first within the window; `next_cursor` scrolls up;
  `has_more=false` + `next_cursor=0` at the retained head — mirroring
  `state.history` (a shared test asserts the two surfaces' paging grammar
  matches).
- [x] `since`/`until` bounds honored (inclusive/exclusive per the existing
  `EventFilter` godoc); a `until < since` request fails
  `CodeInvalidRequest` (the structurally-invalid posture the filter godoc
  already names, `types/events.go:28-30`).
- [x] Scoping: a non-widened caller gets only own-triple rows — enforced
  on the caller's VERIFIED triple, not the request filter's triple. The
  cross-tenant AND cross-USER axes both elevate: naming a foreign tenant OR
  a foreign user requires the closed two-scope claim (`admin` OR
  `console:fleet`) and fails `CodeIdentityScopeRequired`/403 without it
  (the `FilterFromWire` USER-axis gate closes a cross-user disclosure the
  original review caught — a non-admin caller naming `{own-tenant,
  foreign-user, foreign-session}` must NOT receive another user's rows;
  §6). The SESSION axis deliberately does NOT elevate on a single foreign
  value — a user legitimately reads their own other sessions (the Console
  Sessions/Playground history flow); the broader cross-session-observer
  posture question is routed to the wave checkpoint. A widened read WITH
  either claim succeeds and emits `audit.admin_scope_used` exactly once per
  request; scope is never read from the request body; a `console:fleet`-only
  caller can `events.list` the SAME window it can subscribe/aggregate over
  (the live/historical authz-parity pin).
- [x] Both V1 event drivers serve the read through the extended
  `HistoryReplayer` seam: durable = real windows over the persisted log;
  inmem = ring contents + `truncated=true` past the ring head; a
  conformance scenario runs against both (no capability ceremony).
- [x] Rows are byte-identical to the `state.history` projection for the same
  events (same `payloadWireValue`, same artifact-ref seeding) — a test pins
  row-shape equality; the sentinel-redaction posture holds (no raw
  args/results in any returned payload).
- [x] Console Events page: the window picker drives `events.list` for
  historical rows (initial load + scroll-up paging); the live SSE tail is
  unchanged; `truncated` renders a retention-gap notice; the empty-state
  copy is updated to reflect that read-back now exists (durable driver) and
  what the inmem ring honestly serves.
- [x] Full lockstep in the same PR: `make protocol-ts-gen` (manifest +
  `events.ts` + `client.ts` mirrors), `make protocol-docs-gen`,
  `singlesource.CanonicalWireTypes`, generator typeindex registrations,
  `methods_test.go`. `ProtocolVersion` unbumped (additive).
- [x] `scripts/smoke/phase-162.sh` OK ≥ 3, FAIL = 0.
- [x] `-race` on touched packages; coverage ≥ 85% on touched Go packages.

## Files added or changed

- `internal/events/events.go` — the `HistoryReplayer` seam extended with the
  time-ranged, filtered, cursor-paged window read (name per implementor;
  same optional-interface shape both drivers implement — D-254 precedent);
  the seam godoc's never-fan-in contract (`events.go:562` block) amended to
  name the one sanctioned, authority-gated fleet-read exception; and
  `MatchesScoped`'s disclosure-history godoc (`events.go:403-417` — the
  same file; `MatchesScoped` lives in `events.go`, not `filter.go`) amended
  with the same why-safe-here rationale (explicit two-scope authority + a
  dedicated method + per-request audit, vs the silent widening it
  originally closed).
- `internal/events/drivers/durable/durable.go` — the persisted-log
  implementation (global-sequence order, cross-session under an
  admin-fan-in filter, `MatchesScoped` for the non-widened path).
- `internal/events/drivers/inmem/inmem.go` — the ring implementation +
  honest `truncated`.
- `internal/events/conformancetest/conformancetest.go` — the shared
  windowed-read scenario both drivers must pass.
- `internal/protocol/types/events.go` — `EventsListRequest`/`Response`
  (reusing `EventFilter` + `StateEvent`).
- `internal/protocol/methods/methods.go` (+ `methods_test.go`) —
  `MethodEventsList`.
- `internal/protocol/transports/stream/` — the `events.list` handler
  (reusing the `state_history_handler.go` projection helpers —
  `payloadWireValue` + artifact-ref seeding are shared, not copied).
- `internal/protocol/singlesource/singlesource.go` + the three generator
  typeindex files + `internal/protocol/conformance/conformance.go`.
- `web/console/src/lib/protocol/events.ts`, `client.ts`,
  `wire-manifest.gen.json` (regenerated).
- `web/console/src/routes/(console)/events/+page.svelte` (+ the events lib
  state module) — historical window read + scroll-up + truncation notice +
  empty-state copy update.
- `test/integration/phase162_events_list_test.go` (new) — real drivers,
  identity scoping, widened-read audit, ≥1 failure mode, `-race`.
- `docs/site/protocol/methods.md` / `types.md` (regenerated).
- `scripts/smoke/phase-162.sh` (new); `docs/plans/README.md`;
  `docs/decisions.md` (D-294); `docs/glossary.md`.

## Public API surface

- Wire: `events.list` method; `EventsListRequest` / `EventsListResponse`
  (rows = the existing `StateEvent` shape).
- Go: the extended `events.HistoryReplayer` windowed-read method (internal
  seam; both drivers implement).
- Console: the Events page historical read (Console-internal).

## Test plan

- **Unit:** paging grammar (zero-cursor tail, scroll-up, head termination);
  since/until bounds incl. the invalid-range 400; row-shape equality with
  `state.history`'s projection; handler scope table (non-admin own-triple,
  widened-without-claim 403, widened-with-claim + single audit emit).
- **Integration (`test/integration/phase162_events_list_test.go`):** real
  drivers — drive scripted runs across TWO identities, read back windows:
  own-scope isolation (caller A never sees B's rows — §6 rule 10), the
  admin-widened fleet read with `audit.admin_scope_used` asserted, the
  `truncated` edge on the inmem ring, the cross-driver conformance scenario,
  ≥1 failure mode (missing identity refused), `-race`.
- **Conformance:** the shared events-driver windowed-read scenario (both V1
  drivers; the events conformance suite home).
- **Concurrency / leak:** N≥100 concurrent `events.list` reads against one
  bus under concurrent publishing, `-race` — no torn pages, no cross-caller
  row bleed, goroutine baseline after close (extending the drivers' existing
  D-025 stress).

## Smoke script additions

- live-server: drive a scripted run via the `start` method
  (`POST /v1/control/start`); poll `tasks.get` to terminal; POST
  `/v1/events/list` with `since` = boot time → rows present including the
  run's task-lifecycle event types; take `next_cursor` and fetch page 2
  (paging round-trip); a request without a token is rejected 401.
- Done-definition: `OK ≥ 3, FAIL = 0`; 404/405/501 → SKIP until the phase
  ships.

## Coverage target

- `internal/events` (+ both drivers): 85%
- `internal/protocol/transports/stream` (the new handler): existing package
  target maintained or raised
- Console: vitest on the events state module's window/paging fold.

## Dependencies

- 124 (gap-free durable log — the substrate), 125 (`state.history` — the
  row shape, paging grammar, and projection helpers this reuses; D-254),
  72/72a (subscribe + aggregate — the siblings this completes), 108h (the
  Events page this phase's consumer lands on), 118 (D-223 lockstep).

## Risks / open questions

- **Cross-session scan cost on the durable driver.** The persisted log is
  per-session-keyed with a global sequence; a fleet-wide window read is a
  merge across session logs (a global-order scan). The implementor picks
  the read shape; the plan binds only the semantics (global-sequence order,
  bounded page size, cursor-stable). **As-built honest bound (corrected from
  the earlier "no unbounded scans" wording):** the durable admin fleet read
  gathers ALL candidate `(session, seq)` pairs below the cursor from the
  head records and sorts them per page — so the *candidate gather* is
  `O(events-below-cursor)` in memory/CPU, NOT bounded by `limit`. What IS
  bounded by `limit` is the **entry I/O**: the loader stops after loading
  `limit+1` MATCHING entries, so the expensive per-event StateStore reads
  never exceed the page. The head-record identity pre-filter prunes
  non-matching sessions before any entry load. This is acceptable for V1
  (documented latitude); a follow-up for a **merged global-sequence index**
  (so the candidate gather is also page-bounded) is tracked as HA-13 —
  reword, don't optimise here.
- **Admin fan-in is a disclosure edge.** The widened read reuses the
  existing `Matches` admin fan-in + mandatory audit emit; the integration
  test's two-identity isolation + widened-read legs are the guard (§6).
- **Time vs sequence ordering.** `OccurredAt` is producer-stamped and not
  strictly monotonic across sessions; the cursor is SEQUENCE-based (stable),
  `since`/`until` filter on `OccurredAt` (semantic) — the plan pins this
  split explicitly so paging never skips or duplicates rows on clock skew.

## Glossary additions

- "historical event read (`events.list`)" (docs/glossary.md, same PR).

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
      (the two-identity integration leg + the widened-read audit leg).
- [x] **Reusable-artifact concurrent-reuse:** the bus drivers' D-025 stress
      extended with concurrent windowed reads (N≥100, `-race`).
- [x] **Integration test wires real drivers end-to-end, asserts identity
      propagation, covers ≥1 failure mode, runs under `-race`** (§17.3).
- [x] Wire changes complete: `make protocol-ts-gen-check` +
      `make protocol-docs-gen-check` green with the regenerated artifacts
      committed.
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: N/A — none departed
