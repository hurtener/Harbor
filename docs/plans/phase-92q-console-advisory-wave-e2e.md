# Phase 92q — Console advisory + wave-end live E2E + §17.5 audit

## Summary

The wave-end deliverable for the Runtime MCP tool-side OAuth wave (see `docs/plans/wave-mcp-oauth-decomposition.md`). It (a) extends the consolidated agent-config Console panel (92h) so a connection in `auth_required` renders an "awaiting authorization / paused by an administrator" advisory — a pure Protocol client reading only canonical events/state (D-061), never reaching into the flow; (b) bundles `test/integration/wavemcpoauth_test.go` — real drivers across the wave's surface, identity propagation, a denied-flow failure mode, N≥10 concurrency stress; (c) adds an env-gated `HARBOR_LIVE_*` probe against a real OAuth-capable MCP server (CI-skipped, §17.8); and (d) runs the wave-end §17.5 checkpoint audit, landing its punch list as one `chore(checkpoint)` PR before any follow-on wave.

## RFC anchor

- RFC §6.16 — Agent Registry (the agent-bound binding for runtime-added connections; the Console renders the connection lifecycle as a lens over the registry).
- RFC §6.4 — Tool catalog / tool transports (the MCP connection whose OAuth `auth_required` state the advisory surfaces).

## Briefs informing this phase

- brief 09
- brief 14

## Brief findings incorporated

- **brief 09 (MCP OAuth lessons):** the agent-bound token lifecycle is the load-bearing seam — an `auth_required` state is a parked OAuth flow awaiting out-of-band consent, not an error. The advisory communicates exactly that operator-actionable state ("awaiting authorization") instead of rendering a generic failure, so the operator knows the next step is to complete consent (not to retry the add).
- **brief 14 (MCP client/host compliance, spec 2025-11-25):** conformance fixtures must derive from the real spec / a real server transcript, never a hand-authored self-consistent blob (§17.8). The wave-end live probe targets a real OAuth-capable MCP server through the agent-bound flow; CI skips it via the `HARBOR_LIVE_*` env gate while dev runs it against the genuine server.

## Findings I'm departing from (if any)

None.

## Console consistency (D-121 — binding)

This is a Console page change, so it is built against the shared foundation per `docs/design/console/CONVENTIONS.md`: the single `(console)` route group (no `/console/` URL prefix), the shared app shell, the `web/console/src/lib/components/ui/` inventory, the four-state `<PageState>` async contract, the unified `HarborClient` + `connection.ts`, and the one reconciled `tokens.css` scale. The advisory extends the 92h agent-config panel's add-connection area; it follows the 92b disabled-with-tooltip admin precedent for any control it gates. No raw color/spacing literals, no hand-rolled `fetch`, Svelte 5 runes mode. The advisory derives entirely from canonical `mcp.connection.*` + `pause.*` events + the agent-config read surface — it never imports a Runtime-internal Go type and never reaches into the flow (D-061).

## Goals

- Extend the 92h panel's add-connection area so a connection whose lifecycle event reports `auth_required` renders a clear advisory: "awaiting authorization" (an OAuth flow is parked, the operator must complete consent out-of-band) / "paused by an administrator" — sourced from the canonical `mcp.connection.*` + unified `pause.*` events, with the authorize URL surfaced when present.
- Bundle the wave-end Go integration test `test/integration/wavemcpoauth_test.go` exercising the wave's surface end-to-end with real drivers.
- Ship an env-gated `HARBOR_LIVE_*` probe that authenticates against a real OAuth-capable MCP server; CI skips it.
- Run the wave-end §17.5 checkpoint audit (read-only) over 92k–92p and land its punch list as one `chore(checkpoint)` PR before any follow-on wave is scoped.

## Non-goals

- Any new Protocol surface — 92q is a CONSUMER only; it reads the canonical events/state and methods shipped across 92k–92p (RFC §7 runtime-lens satisfied).
- The runtime OAuth mechanism itself (the provider seam, the transport token injection, the resume bridge, the reconcile, the discovery) — all shipped by earlier wave phases (92k–92p).
- Detach-on-rollback (deferred wave-wide, D-240); the end-user (non-admin) client (downstream, not this repo).

## Acceptance criteria

- [ ] The agent-config panel renders the `auth_required` advisory ("awaiting authorization" / "paused by an administrator", with the authorize URL when present) driven by canonical `mcp.connection.*` + `pause.*` events; a Playwright spec covers the advisory render.
- [ ] `test/integration/wavemcpoauth_test.go` is green: real drivers across the wave's surface (no mocks at the seam), identity-triple propagation asserted through every layer, a denied-flow failure mode via `DenyFlow`, and an N≥10 concurrency stress with no cross-talk / no goroutine leak after teardown; runs under `-race`.
- [ ] The `HARBOR_LIVE_*`-gated probe against a real OAuth-capable MCP server is present, env-gated, and CI-skipped (§17.8).
- [ ] The Console reads ONLY canonical Protocol events/state — no Runtime-internal Go type is imported into `web/console/`; no hand-rolled `fetch`; Svelte 5 runes; `svelte-check --fail-on-warnings` + `npm run lint` clean.
- [ ] The wave-end §17.5 checkpoint audit over 92k–92p is completed and its punch list lands as one `chore(checkpoint)` PR before any follow-on wave is scoped.

## Files added or changed

- `web/console/src/lib/components/agentconfig/` — the add-connection advisory extension (an `auth_required` state branch built from the shared `ui/` inventory + the existing 92h add-connection component).
- `web/console/src/lib/agentconfig/state.svelte.ts` — the page state gains the `auth_required` lifecycle reflection (runes; derived from the event stream, no Console-local source of truth).
- `web/console/tests/agent-config-page.spec.ts` — Playwright assertions for the advisory.
- `test/integration/wavemcpoauth_test.go` — the wave-end E2E (real drivers, identity propagation, `DenyFlow` failure mode, N≥10 stress) + the `HARBOR_LIVE_*`-gated live probe.
- `scripts/smoke/phase-92q.sh` — the smoke skeleton.

## Public API surface

- N/A (Console-only consumer + an integration test; consumes the existing `agent_config.*` Protocol surface and the wave's canonical `mcp.connection.*` + `pause.*` events).

## Test plan

- **Unit (vitest):** the page-state transition into the `auth_required` advisory branch from a canonical lifecycle event; the advisory copy + the authorize-URL surfacing when present vs absent.
- **Integration:** Playwright — the panel loads against a live `harbor dev`, an `auth_required` connection renders the advisory (real Protocol round-trip, no mocked transport at the seam). Go — `test/integration/wavemcpoauth_test.go` wires real drivers across the wave's surface (the OAuth provider with a registered agent-bound config, the MCP transport's token-aware attach, the resume bridge, the run-start reconcile), asserts identity-triple propagation through every layer, drives a denied flow via `DenyFlow` as the ≥1 failure mode, and runs an N≥10 concurrency stress asserting no cross-talk and no goroutine leak after teardown; under `-race`.
- **Conformance:** the `HARBOR_LIVE_*`-gated probe derives its fixture from a real OAuth-capable MCP server (or a committed transcript), never a hand-authored blob (§17.8).
- **Concurrency / leak:** the N≥10 stress in the Go integration test (baseline goroutine count restored after teardown).

## Smoke script additions

- `scripts/smoke/phase-92q.sh`: static — the advisory component + the `auth_required` state branch present in the panel; the integration test file present; the live probe is env-gated (`HARBOR_LIVE_*`) and skips by default. Live (skip-if-404) — the panel's feeding `agent_config.*` methods answer on the dev server. The skeleton ships with a single `skip` until the surface lands.

## Coverage target

- `web/console` touched modules: meet the Console `svelte-check --fail-on-warnings` + `npm run lint` + Playwright gates — no Go coverage percentage applies to Console pages (Console phase, mirroring 92h).
- `test/integration/wavemcpoauth_test.go`: the wave-end integration test is the binding gate (real drivers, identity propagation, `DenyFlow` failure mode, N≥10 stress under `-race`); it is a cross-subsystem E2E rather than a per-package coverage target.

## Dependencies

- 92k, 92l, 92m, 92n, 92o, 92p — every runtime OAuth surface the advisory reflects and the integration test exercises must have shipped (RFC §7 runtime-lens; the Console is a Protocol client of the wave's surface). Console foundation: D-121 (CONVENTIONS.md), the shared `ui/` inventory, `HarborClient`, the 92h panel this extends.

## Risks / open questions

- **Live OAuth MCP fixture.** A real OAuth-capable MCP server is required for the §17.8 conformance probe. If none can be driven in CI, a captured transcript is committed and the `HARBOR_LIVE_*`-gated probe targets a real server in dev only (RFC §11 wave risk 4 / D-240).
- **Advisory state fidelity.** The `auth_required` advisory must reflect the live event stream (never a Console-local poll); it derives from the typed client's canonical `mcp.connection.*` + `pause.*` events so it tracks the parked flow accurately.
- **Audit blast radius.** The §17.5 audit may surface a cross-phase bug in an earlier wave phase; per §17.6 it is fixed in the same `chore(checkpoint)` PR (grep production for the same call site when fixing a test-side bug shape), not deferred.

## Glossary additions

- **auth-required advisory** — the Console agent-config panel surface (Phase 92q) that renders the "awaiting authorization / paused by an administrator" state of a runtime-added MCP connection whose OAuth flow is parked, derived purely from canonical `mcp.connection.*` + `pause.*` events.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] `svelte-check --fail-on-warnings` + `npm run lint` + `npm run build` clean
- [ ] No Runtime-internal Go type imported into `web/console/`; no hand-rolled fetch; Svelte 5 runes
- [ ] `test/integration/wavemcpoauth_test.go` green under `-race` (real drivers, identity propagation, `DenyFlow` failure mode, N≥10 stress, no goroutine leak after teardown)
- [ ] The `HARBOR_LIVE_*` probe is env-gated + CI-skipped (§17.8)
- [ ] The §17.5 checkpoint audit over 92k–92p is completed and its punch list landed as one `chore(checkpoint)` PR
- [ ] Playwright spec passes; screenshots captured for the graphic-quality check
- [ ] If new vocabulary: glossary updated

<!-- This is a Console-page + wave-end-integration phase: the Go concurrent-reuse checklist item is exercised by the integration test's N≥10 stress; the Console-side binding gates are the svelte-check/lint/Playwright ones above (D-121 + brief 12 precedent from 92h). -->
