# Phase 84a — Runtime-capability gate + session aggregates (Round-8 F1 + F8 closeout)

## Summary

Closes the two remaining round-8 V1.1-readiness items so Console and Runtime work with 1:1 feature parity. **F1** (topology.snapshot 404 logged in browser on Live Runtime + Playground) gets closed by making the existing `runtime.info` capability list dynamic per-runtime + adding a `topology_snapshot` capability the Console gates the fetch behind. **F8** (`tasks_count` / `events_count` / `total_cost_cents` / `total_tokens` always zero on session rows) gets closed by Console-side enrichment from the existing `tasks.list` + `events.aggregate` wires — D-122 compliant (no shadow aggregation store; the wire still ships zero, the Console computes).

## RFC anchor

- RFC §5.3 — Protocol versioning + capability advertisement (minor-class additive change)
- RFC §6.4 — control surface (`topology.snapshot` is the V1 engine-graph projection)
- RFC §7 — Console as a Protocol client + 1:1 feature parity

## Briefs informing this phase

- brief 14 — V1.1 round-N walkthrough findings consolidation (round-6 F1, round-8 F1/F8)

## Brief findings incorporated

- **F1 root cause (round-6 catalog).** `harbor dev` is planner/RunLoop-shaped; `ControlSurface.topology` is nil; the runtime returns `CodeUnknownMethod` mapped to HTTP 404. The Console's `isUnknownMethod(err)` catch renders the right info banner (D-164), but the browser-level fetch logger still emits `[ERROR]` because non-2xx responses auto-log. The only way to suppress is to not make the call.
- **F8 design constraint (D-122).** "Cost / token / task / event counters and the agent binding are NOT modelled on the Phase 08 Session record … the Sessions page surfaces them from the live event stream … this keeps `sessions.list` a pure registry projection (no shadow aggregation store — D-061)." The Go side stays a pure registry projection; the Console enriches.
- **Capability-surface convention.** `internal/protocol/types/version.go` declares the canonical capability set as a static map (currently `{task_control, events_subscribe, runtime_posture}`). The static set is the *Protocol-version surface*; what a *specific runtime instance* advertises in `runtime.info` is the *wired subset*. They overlap today because every dev runtime wires all three. The fix is making the per-instance list reflect actual wiring.

## Findings I'm departing from (if any)

None.

## Goals

- Browser console on Live Runtime + Playground is clean (zero 4xx logged) on a planner/RunLoop runtime; the friendly info banner UX stays.
- Sessions list + detail pages render truthful counters (tasks / events / cost / tokens) per row, sourced from the Console-side enrichment.
- The runtime's capability advertisement reflects what's actually wired (topology iff the engine-graph projection accessor is present).
- 1:1 Console↔Runtime feature parity is mechanically enforced — capability discovery happens at attach; pages with optional surfaces consult capabilities before fetching.

## Non-goals

- SSE-based live counter updates on the Sessions pages — V1.1 ships one-shot fetch on page load + a Refresh button. SSE counter wiring is a V1.2/V1.3 evolution (alongside the live observability surfaces).
- New capability strings beyond `topology_snapshot`. Future per-surface capabilities (engine, distributed bus, etc.) gain their own additive PRs.
- Adding `sessions.aggregates` as a batched wire method. V1.1 iterates `tasks.list` / `events.aggregate` per session; a batched aggregator can land in V1.3 if the per-page fetch cost becomes measurable.

## Acceptance criteria

- [ ] `internal/protocol/types/version.go` declares `CapTopologySnapshot Capability = "topology_snapshot"` and registers it in `canonicalCapabilities`.
- [ ] The `runtime.info` response builder includes `topology_snapshot` in `capabilities` **iff** the underlying `ControlSurface.topology` accessor is non-nil. Static `Capabilities()` function unchanged (returns the canonical list for handshake purposes); only the per-instance wire field is conditional.
- [ ] D-094 mirror: `cmd/harbor/cmd_dev.go` and `harbortest/devstack/devstack.go` both wire the capability advertiser the same way.
- [ ] Console `client.runtime.info()` is fetched once per `RuntimeConnection` (cached on the connection) and exposes a typed `caps.has('topology_snapshot')` helper.
- [ ] `web/console/src/routes/(console)/live-runtime/+page.svelte::load` calls `client.topology.snapshot()` only when `caps.has('topology_snapshot')`; otherwise sets the existing `pageInfo` info banner directly.
- [ ] `web/console/src/routes/(console)/playground/[session_id]/+page.svelte::load` does the same gating.
- [ ] Playwright spec asserts a planner/RunLoop runtime emits **zero** `[ERROR]` browser console messages on either page load — pinning the F1 regression.
- [ ] Sessions list page enriches each visible row's `tasks_count` from `tasks.list` filtered by `session_id`, and `events_count` / `total_cost_cents` / `total_tokens` from `events.aggregate` scoped per session. Renders the live numbers, not the wire's zeros.
- [ ] Sessions detail page (`[id]/+page.svelte`) does the same enrichment for its right-rail counters.
- [ ] Playwright spec asserts a session with N spawned tasks shows `N` in the counter, not `0`.
- [ ] Acceptance test exists for both fixes (Playwright + Go-side unit test for the conditional `runtime.info.capabilities`).

## Files added or changed

- `internal/protocol/types/version.go` — register `CapTopologySnapshot`; godoc.
- `internal/protocol/types/posture.go` — `RuntimeInfo.Capabilities` documented as the wired subset (clarification comment).
- `internal/protocol/runtime_info.go` (or wherever the builder lives) — conditional inclusion based on the wired surface.
- `cmd/harbor/cmd_dev.go::bootDevStack` — pass the topology presence flag (or the accessor itself) into the info builder.
- `harbortest/devstack/devstack.go` — D-094 mirror.
- `web/console/src/lib/protocol/client.ts` — `RuntimeNamespace.info()` typed accessor; `HarborClient.capabilities` cached field; `caps.has(name)` helper.
- `web/console/src/lib/connection.ts` (or attach helper) — populate capabilities at attach time; refresh on reconnect.
- `web/console/src/routes/(console)/live-runtime/+page.svelte` — capability gate around the topology fetch.
- `web/console/src/routes/(console)/playground/[session_id]/+page.svelte` — capability gate.
- `web/console/src/routes/(console)/sessions/+page.svelte` — per-row enrichment from `tasks.list` + `events.aggregate`.
- `web/console/src/routes/(console)/sessions/[id]/+page.svelte` — enrichment for the detail right-rail.
- `web/console/tests/live-runtime-page.spec.ts` — zero-browser-error assertion on attach.
- `web/console/tests/sessions-page.spec.ts` — truthful counter assertion.
- `internal/protocol/types/posture_test.go` — table test: with/without topology accessor → `capabilities` field includes/omits `topology_snapshot`.

## Public API surface

- `types.CapTopologySnapshot` (new constant, value `"topology_snapshot"`).
- `RuntimeInfoBuilder` (internal) gains a `WithTopologyAvailable(bool)` option, or an existing surface accessor exposes the presence flag.
- TS: `HarborClient.capabilities: ReadonlySet<string>`, `caps.has(name: string): boolean`.

## Test plan

- **Unit:** posture_test asserts the conditional capability inclusion; client.ts `caps.has` returns the right shape; connection cache invalidates on reconnect.
- **Integration:** `harbortest/devstack` boot with no engine → `runtime.info.capabilities` excludes `topology_snapshot`; same boot with the engine wired → includes it.
- **Conformance:** N/A — no new driver surface.
- **Concurrency / leak:** `caps.has` is read-only on a frozen Set; safe by construction. The per-row enrichment fetches use the existing HarborClient transport — already concurrent-safe.
- **Playwright:** browser-console-error assertion on Live Runtime + Playground; counter assertion on Sessions list + detail.

## Smoke script additions

`scripts/smoke/phase-84a.sh` — assertions:

- `runtime.info` response shape includes `capabilities` array.
- On a planner/RunLoop runtime (the dev posture), `topology_snapshot` is **absent** from `capabilities`.
- `topology.snapshot` invocation still returns 404 / `unknown_method` (regression: confirm the capability gate is the gating mechanism, not a behaviour change at the wire).

## Coverage target

- `internal/protocol/types/`: 90% (existing posture/version tests + new conditional-capability table test).
- `web/console/src/lib/protocol/`: clean Svelte-check; the capability helper is small enough to be exercised entirely via Playwright e2e.

## Dependencies

- 72f (runtime.info / capabilities — already shipped)
- 73c (sessions.list / sessions.inspect — already shipped)
- 73d (tasks.list — already shipped)
- 72b (events.aggregate — already shipped)
- 83w (D-164 `unknown_method` info-banner branch — already shipped)

## Risks / open questions

- **Capability cache freshness.** A runtime can in principle add/remove capabilities at runtime (a config hot-reload that wires the engine). V1.1 fetches once at attach; if the runtime gains the topology accessor mid-session the Console won't notice until next attach. Acceptable for V1.1 — flag for V1.3 alongside the SSE live-update wave.
- **`runtime.info` admin-scope.** If the capability list is admin-gated (it isn't today), the per-tenant Console attach won't see capabilities and would have to fall back to "best-effort, attempt the call and catch unknown_method" — i.e. the current behaviour. Verify the access scope at implementation time.
- **Sessions list enrichment performance.** N sessions → N `tasks.list` + N `events.aggregate` round-trips. For dev / V1.1 (few sessions) acceptable; if production has 1000+ sessions per tenant, a batched `sessions.aggregates` wire method becomes necessary. Tracked as a V1.3 follow-up.

## Glossary additions

- **Wired capability** — the per-runtime-instance subset of canonical Protocol capabilities that the instance has actually constructed (e.g. `topology_snapshot` is wired iff the engine-graph projection accessor is non-nil). Distinct from the **canonical capability set**, which is the Protocol-version surface a handshake negotiates against. Add this distinction to `docs/glossary.md` in the same PR.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (N/A here — runtime.info is identity-scoped already; sessions enrichment uses identity-scoped wires)
- [ ] N/A — this phase wires a Console-side helper + a Go-side conditional builder; neither is a reusable artifact in the D-025 sense.
- [ ] Integration test covers the seam (devstack boot with/without topology) per AGENTS.md §17.
- [ ] Glossary updated (wired-vs-canonical capabilities distinction)
- [ ] If a brief finding was departed from: N/A
