# Phase 121 — console-surface-runtime-gauges

## Summary

Surfaces the Phase 120 runtime gauges (active runs, engine capacity-map size, governance cache size, events-dropped total, and Go-runtime liveness) in the Console's **existing** Live Runtime health panel — without inventing a colliding Protocol method or a second panel. The runtime-health Protocol surface already ships: `runtime.health` (`methods.go:198`), `types.RuntimeHealth` (a per-subsystem readiness rollup), `runtime.counters`, and `metrics.snapshot` all landed in Phase 72f (D-111) and are already consumed by the Console Overview + Live Runtime cockpit (`live-runtime/health-panel.svelte`, `live-runtime/panels.ts`). This phase routes the new gauges through the shipped `metrics.snapshot` projection (gauges registered on the telemetry `MetricsRegistry` cross the wire with no new method) and extends the existing panel to render them, adding go-runtime liveness fields to the existing posture cluster only if they genuinely need the wire.

## RFC anchor

- RFC §5.2
- RFC §7.1
- RFC §7.2

## Briefs informing this phase

- brief 06
- brief 11

## Brief findings incorporated

- brief 06 §"Observability — what belongs on the wire": product-facing health (goroutines, heap, GC, active runs, internal sizes) is wire-appropriate; raw profiles are not. The split keeps the Protocol surface small — and here it is *already* small and shipped (`metrics.snapshot`), so the right move is to feed it, not to add a parallel method.
- brief 11 §"Console feature surface — Live Runtime cockpit": runtime liveness belongs as a panel on the capability-adaptive Live Runtime cockpit (D-177 / Phase 108e), composed through the `panels.ts` capability→panel registry, not hand-mounted.
- brief 06 §"Playground anti-pattern / runtime-lens principle": the Console is a lens over canonical Protocol state; the panel reads the shipped wire types and holds no runtime entity (only a client-side trend buffer).

## Findings I'm departing from (if any)

None — this phase exists *because* the original 121a/121b plans departed from the shipped surface (they redefined `runtime.health`); this version is corrected to extend, not duplicate.

## Goals

- The Phase 120 Harbor runtime gauges are registered on the telemetry `MetricsRegistry` (the source `metrics.snapshot` projects, `types/posture.go:215`), so they cross the Protocol through the **shipped** `metrics.snapshot` with no new method.
- If Go-runtime liveness (goroutines / heap-in-use / GC pause) is not already retrievable through `metrics.snapshot.Gauges` after Phase 120, it is added as fields on the **existing** posture cluster (extend `runtime.counters` or add named gauges) — one surface, per §13 — never as a new or redefined `runtime.health` method/type. Any change to a shipped wire shape carries a §8 Protocol version bump + migrates the existing consumers in this same wave.
- The existing `web/console/src/lib/components/live-runtime/health-panel.svelte` is extended (or a sibling panel is registered in `live-runtime/panels.ts`) to render the new gauges, with a goroutine/heap trend sparkline (client-side ring buffer); no new top-level panel component duplicating the shipped one.
- The panel obeys the four-state `<PageState>` contract, the design tokens, Svelte 5 runes, and the typed `HarborClient` (no hand-rolled `fetch`); `svelte-check --fail-on-warnings` + the stylelint token rule pass.
- Scope behaviour matches the **implemented** posture gate (`posture.go:301-312`): process-global metrics are returned to authenticated callers / gated behind `console:fleet`/`admin` exactly as the existing posture reads are; this phase invents no new "session view" of process-global counters.

## Non-goals

- A new `runtime.health` method or a new `RuntimeHealth` type — both ship (Phase 72f). Redefining either is the §13 "two parallel implementations" violation this plan exists to avoid.
- A new health panel component — `live-runtime/health-panel.svelte` exists and is registered; this phase extends it.
- Raw pprof over the Protocol or in the Console — pprof is the off-Protocol loopback listener (Phase 120) only.
- Server-side metric history — the trend is a bounded client-side ring buffer.

## Acceptance criteria

- [ ] The Phase 120 gauges (`active_runs`, engine capacity-map size, governance cache size, events-dropped) are retrievable via the shipped `metrics.snapshot` method (asserted: a `metrics.snapshot` call returns named gauges for each), with no new Protocol method registered for them.
- [ ] If Go-runtime liveness fields are added, they extend an existing posture type/method (no new method name; `runtime.health`/`RuntimeHealth` are untouched in shape, OR a §8 version bump + consumer migration is included in this PR) — verified by `make protocol-ts-gen-check` + `make protocol-docs-gen-check` staying green.
- [ ] The existing Live Runtime health panel renders the new gauges via the typed client; no new panel component duplicates it; no hand-rolled `fetch` in any `.svelte` file.
- [ ] The panel renders all four `<PageState>` branches (loading / loaded / empty / error) and a goroutine/heap trend sparkline from a bounded client-side buffer; the refresh timer is cleared on unmount.
- [ ] An `admin`/`console:fleet` caller and a plain authenticated caller both get a coherent panel consistent with the existing posture gate's behaviour (no new scope semantics invented).
- [ ] No raw color/spacing/type-scale literals in touched `.svelte` files; `svelte-check --fail-on-warnings` + `npm run lint` + `npm run build` pass; Svelte 5 runes only.

## Console consistency

Per CLAUDE.md §4.5 item 12 + `docs/design/console/CONVENTIONS.md` (binding). This phase builds against the shared foundation: the `(console)/live-runtime/` route group, the app shell, the shared `ui/` inventory, the four-state `<PageState>` contract, the unified `HarborClient` + `connection.ts`, and the one `tokens.css` scale. The panel is composed through the D-177 / Phase 108e capability→panel registry (`live-runtime/panels.ts::resolvePanels`), matching `docs/design/console/page-live-runtime.md`. It extends the shipped `live-runtime/health-panel.svelte`; it does not add a parallel page or panel.

## Files added or changed

- `internal/telemetry/` — register the Phase 120 Harbor gauges on the `MetricsRegistry` so `metrics.snapshot` projects them (the seam Phase 120 establishes; this phase consumes it). Possibly a posture-cluster field addition if Go-liveness needs the wire.
- `internal/protocol/types/posture.go` — only if Go-liveness fields are added to an existing posture type (additive; with the lockstep gates re-run).
- `web/console/src/lib/components/live-runtime/health-panel.svelte` — extend to render the new gauges + sparkline.
- `web/console/src/lib/live-runtime/panels.ts` — adjust the registration / panel data wiring if a sibling panel is warranted.
- `web/console/src/lib/protocol/posture.ts` — only if a posture type gained fields (hand-mirrored; manifest regenerated).
- `scripts/smoke/phase-121.sh`.

## Public API surface

- No new Protocol method. At most additive fields on an existing posture type, projected through the already-shipped `metrics.snapshot` / `runtime.counters`. Console components are internal.

## Test plan

- **Unit:** the gauge registration surfaces named gauges in a `metrics.snapshot` projection; the panel renders each `<PageState>` branch from fixtures; the trend buffer caps length.
- **Integration:** against a running Runtime, `metrics.snapshot` returns the new gauges and the panel populates + refreshes; the §17.8 fixture is captured from the **real** `metrics.snapshot`/posture response shape (not hand-authored); an unauthorized/insufficient-scope response routes to the error state; under `-race` on the Go side.
- **Conformance:** the Protocol lockstep gates (`protocol-ts-gen-check`, `protocol-docs-gen-check`) stay green; if a posture type changed, the Go manifest + TS scan see it.
- **Concurrency / leak:** N/A new artifact (Console + a metrics registration); the gauge accessors' concurrency is covered by Phase 120's concurrent-scrape guard.

## Smoke script additions

- `scripts/smoke/phase-121.sh`:
  - (live-server) `protocol_call metrics.snapshot` returns 200 and includes the new gauge series once they land (SKIP via 404/501 until then).
  - (static-only) assert NO new `runtime.health`/`RuntimeHealth` definition is introduced (grep the diff region for a second registration); assert the touched `.svelte` files contain no raw color/spacing literals and no hand-rolled `fetch(`.

## Coverage target

- `internal/telemetry`: maintain (additive gauge registration).
- Console (Vitest where present): the four `<PageState>` branches + trend-buffer logic.

## Dependencies

- 72f (the SHIPPED posture surface — `runtime.health`, `runtime.counters`, `metrics.snapshot`, D-111 — that this phase feeds and extends)
- 120 (the runtime gauges; and the `MetricsRegistry` registration seam)
- 108e (the capability-adaptive Live Runtime cockpit + `panels.ts` registry, D-177 — supersedes 108d)
- 117 (chat/Console theming contract the panel respects)
- 118 / 113a (the TS-lockstep + generated-docs gates any posture-type change must satisfy)

## Risks / open questions

- **Is a new wire field even needed?** If Phase 120 registers the Harbor gauges (and the Go/Process collector values it can mirror) on the `MetricsRegistry`, `metrics.snapshot` carries them with **zero** Protocol change — the ideal outcome. Confirm during Phase 120 whether `go_goroutines`/heap/GC can be mirrored into the `MetricsRegistry` (vs living only on the raw prometheus registry); if they can, this phase is Console-only. **Proposed D-252** records the decision: surface runtime gauges through the shipped `metrics.snapshot`/posture cluster, never a new or redefined `runtime.health`; the Console extends the shipped health panel.
- **Scope correctness:** match `posture.go:301-312` exactly (RFC §5.5 + the cross-tenant gate); do not invent session-partitioning of process-global counters (goroutines/heap are process-global by nature).
- **Panel crowding:** the Live Runtime cockpit already carries spine + capability panels; the gauges extend the existing health panel rather than adding a competing one (D-177 registry).
- RFC §11: none directly.

## Glossary additions

- None — `metrics.snapshot`, `runtime.counters`, `RuntimeHealth`, `<PageState>`, and the Live Runtime cockpit are existing vocabulary.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: the posture scope gate is unchanged; if a posture type gained fields, the cross-tenant gate test still passes
- [ ] **If this phase builds a reusable artifact:** N/A — a metrics-registry registration + Console components; gauge-accessor concurrency covered by Phase 120.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam:** the panel consumes `metrics.snapshot`; the integration/component test seeds from a real snapshot fixture (§17.8), covers the unauthorized failure mode, and asserts the refresh timer clears on unmount.
- [ ] If Protocol types changed: `make protocol-ts-gen-check` + `make protocol-docs-gen-check` green; no second `runtime.health`/`RuntimeHealth`
- [ ] If new vocabulary: glossary updated — N/A
- [ ] If a brief finding was departed from: justified + decisions.md entry — N/A; proposed D-252 filed at implementation
- [ ] `npm run check && npm run lint && npm run build` clean in `web/console/`
