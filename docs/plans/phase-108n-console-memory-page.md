# Phase 108n — Console Memory page

## Summary

Phase 108n brings the Console **Memory** page to the Phase 108 page-polish
acceptance bar AND lands the real Protocol surface it needed to stop deferring
features. Console half: a carded, viewport-locked master-detail rebuild + a
king-file refactor into a `MemoryPageState` controller + pure `derive.ts`. Go
half (the §13 primitive-with-consumer pairing, like D-184): three new
`memory.*` methods — `memory.strategy_trace` (read) + the admin-gated
`memory.put` / `memory.delete` mutation pair — all backed by the existing
`MemoryStore` interface (no driver-seam change). It also fixes a real
wire-shape bug: the `memory.get` value is base64 (`[]byte`), which the
pre-chrome viewer rendered as gibberish.

## RFC anchor

- RFC §6.6 (Memory subsystem)
- RFC §7 (Console layer — the runtime-lens Protocol-client principle)

## Briefs informing this phase

- brief 11 (Console feature surface — §"Memory view")
- brief 12 (Console deployment and shared UI — §"The two-surface model")

## Brief findings incorporated

- brief 11 §"Memory view": the strategy debugger is a real feature — "render
  how a strategy selected items … the why". Phase 108n ships the honest,
  buildable form: `memory.strategy_trace` projects the strategy's LIVE
  `GetLLMContext` (rolling summary + verbatim-turn count + token estimate) +
  `Health`. The `rolling_summary` strategy summarises (it does not
  select-and-reject candidates), so the trace is the real compaction state, not
  a fabricated rejection list (§13).
- brief 11 §"Memory view": memory is session-scoped by default; mutating verbs
  require strictly more than read — `memory.put` / `memory.delete` gate on the
  verified `admin` claim (D-079) and emit an audit event.
- brief 12 §"The two-surface model": every datum round-trips through the
  Protocol; the Console DB holds only the saved-view chips (D-061).

## Findings I'm departing from (if any)

`memory.promotions` (the cross-session promotion viewer, brief 11 §"Memory
view") is NOT shipped: cross-session memory promotion is unimplemented in the
runtime (only a doc comment in `types/memory.go`), so a `memory.promotions`
method would be a hollow always-empty stub (§13 / PAGE-POLISH §1 forbid
shipping a stub for an absent runtime source). It stays an honest finding until
the promotion subsystem lands (an RFC-level addition). Documented per AGENTS.md
§15.

## Goals

- Retheme the page to the carded (`.panel.card`), viewport-locked master-detail
  composition: a filter card + a records TABLE (sticky `<thead>`, internal
  scroll) + a stacked right rail (health / strategy-trace / live-events /
  add-memory / selected-item). Drop the per-page PageHeader.
- King-file refactor into `MemoryPageState` + pure `derive.ts` + focused
  `MemoryTable` / `MemoryEventsCard` / `StrategyTraceCard` / `AddMemoryComposer`
  components.
- Land `memory.strategy_trace` (read) + `memory.put` / `memory.delete`
  (admin-gated, audited) — real, backed by `MemoryStore.{GetLLMContext,Health,
  AddTurn,Snapshot,Restore}`; wire the Console consumers in the same wave (§13).
- Upgrade the deferred event-feed card to a LIVE `events.subscribe` projection
  (the three `memory.*` event types are shipped).
- Fix the `memory.get` value viewer: base64 → UTF-8 → pretty JSON.

## Non-goals

- No cross-session promotion subsystem / `memory.promotions` (see departures).
- No driver-seam change — the new methods compose the existing `MemoryStore`
  interface; all three V1 drivers back them with zero per-driver work.
- No per-record TTL refresh / pin (D-065 — no priority dimension).

## Acceptance criteria

- [x] Carded + viewport-locked: `scrollHeight == innerHeight`, no horizontal
      overflow at 1512×945 (verified live).
- [x] The per-page PageHeader is gone.
- [x] `memory.strategy_trace` returns the strategy's live compaction projection
      (verified live: real rolling summary, 4 verbatim turns, 643 tokens,
      healthy).
- [x] `memory.put` appends a turn (admin-gated, audited); `memory.delete` evicts
      a turn by key (admin-gated, audited, lossless re-marshal incl. summary);
      both fail closed without `admin` (D-079) and on a missing identity (D-001).
- [x] The selected-item value viewer base64-decodes the value (verified live:
      decoded JSON, not base64).
- [x] The event-feed card is a LIVE `events.subscribe` projection, honest-empty.
- [x] All four `PageState` branches + disconnected redirect preserved.
- [x] `make conformance` count 76→79; Go tests (service + handler) under `-race`;
      `npm run check` 0/0, lint clean, the unit + e2e suites green.

## Files added or changed

- `internal/protocol/types/memory.go` — strategy-trace + put/delete wire types.
- `internal/protocol/methods/methods.go` — 3 method constants + predicate/registry.
- `internal/memory/wire.go` — `Record.Summary` (lossless delete round-trip).
- `internal/memory/events.go` — `memory.item_put` / `memory.item_deleted` audit events.
- `internal/memory/protocol/mutate.go` — `StrategyTrace` / `Put` / `Delete` service.
- `internal/protocol/transports/stream/memory_handler.go` — 3 routes + admin gate + audit.
- `internal/protocol/transports/transports.go` — mount the 3 routes + wire the bus.
- `internal/protocol/singlesource/singlesource.go` + `conformance/conformance.go` — counts.
- `internal/memory/protocol/mutate_test.go` + `transports/stream/memory_handler_test.go` — Go tests.
- `web/console/src/routes/(console)/memory/+page.svelte` — rebuilt.
- `web/console/src/lib/memory/{state.svelte.ts,derive.ts}` — controller + projections.
- `web/console/src/lib/components/memory/{MemoryTable,MemoryEventsCard,StrategyTraceCard,AddMemoryComposer,SelectedItemDetail}.svelte`.
- `web/console/src/lib/protocol/{memory-types.ts,client.ts}` — wire types + namespace methods.
- `web/console/src/lib/memory/tests/derive.test.ts` + `web/console/tests/memory-page.spec.ts`.
- `scripts/smoke/phase-108n.sh` — live-server + static guard.
- `docs/plans/phase-108n-console-memory-page.md` / `docs/decisions.md` (D-186) / `docs/design/console/page-memory.md` (§13).

## Public API surface

Three new Protocol methods (single-sourced in `internal/protocol/methods` +
`internal/protocol/types`):

- `memory.strategy_trace` → `MemoryStrategyTraceResponse{trace, protocol_version}` (read scope).
- `memory.put(turn)` → `MemoryPutResponse{key, protocol_version}` (admin scope, D-079).
- `memory.delete(key)` → `MemoryDeleteResponse{deleted, remaining_turns, protocol_version}` (admin scope).

## Test plan

- **Unit:** `internal/memory/protocol/mutate_test.go` (StrategyTrace projection;
  Put appends + returns a resolvable key + emits the audit event; Delete evicts
  by key + emits the audit event; not-found; identity-required; the
  `Record.Summary` round-trip that proves the delete is lossless). Console
  `web/console/src/lib/memory/tests/derive.test.ts` (the base64 `decodeMemoryValue`
  plus the PascalCase event projection).
- **Integration:** `internal/protocol/transports/stream/memory_handler_test.go`
  (the 3 routes through the real handler over a real `MemoryStore` + identity +
  admin-gating; 401 / 403 / 400 failure modes; under `-race`). The end-to-end
  Console↔method seam is covered by the live smoke + the Playwright spec.
- **Conformance:** the `methods.Methods()` exhaustiveness + count (76→79) +
  the conformance `wantSet`.
- **Concurrency / leak:** the existing memory/protocol concurrent-reuse suite
  covers the store; the new functions are stateless pure functions over it.

## Smoke script additions

`scripts/smoke/phase-108n.sh` (live-server): the 3 new routes mounted; no-bearer
→ 401; `strategy_trace` → 200 + `trace.strategy`; `put` → 200/403 (admin-gated);
`delete` empty-key → 400/403. Plus the static Console guard (PageHeader gone,
carded vocabulary, the controller + derive.ts, the 4 new components, the live
event feed, the client methods, the Save-view N7 contract, no hand-rolled fetch).

## Coverage target

- `internal/memory/protocol`: the new service functions are unit-covered.
- `internal/protocol/transports/stream`: the 3 handlers covered (happy + gating + failure).
- `web/console/src/lib/memory`: `derive.ts` unit-covered; the controller four-state via the existing seam + the e2e spec.

## Dependencies

- Phase 23–25 (the `MemoryStore` interface + drivers the new methods compose).
- Phase 73j / D-118 (the `memory.{list,get,health}` surface this rebuilds).
- Phase 73g (`events.subscribe` — the live feed).
- Phase 108b (chrome) + Phase 108k / D-183 / 108m / D-185 (the carded
  master-detail pattern + controller refactor it mirrors).
- Phase 105 (the disconnected redirect).

## Risks / open questions

- A per-turn `memory.delete` re-ordinals the remaining turns, so their content-
  addressed keys change on the next `memory.list`. Acceptable — keys are opaque
  and the Console re-lists after a mutation; documented in the method godoc.
- `memory.promotions` is intentionally absent (see departures) — surfaced as an
  honest finding, not a stub.

## Glossary additions

None — the memory vocabulary is already in `docs/glossary.md`; the new
`memory.strategy_trace` / `memory.put` / `memory.delete` methods extend the
shipped `memory.*` family without new domain terms.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes — the new methods are identity-mandatory (validated) + admin-gated; covered by the handler tests' 401/403 modes.
- [x] **If this phase builds a reusable artifact:** N/A — the new service functions are stateless pure functions over the existing D-025-safe `MemoryStore`; no new reusable artifact.
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam:** the handler tests wire the real `MemoryStore` + identity + admin end-to-end under `-race`; the Console↔method seam is covered by the live smoke + Playwright.
- [x] If new vocabulary: glossary updated — N/A.
- [x] If a brief finding was departed from: justified above + decisions.md entry filed — yes (promotions, D-186).
