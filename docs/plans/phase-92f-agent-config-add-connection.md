# Phase 92f — agent-config control plane: add a new MCP connection (dial + handshake + OAuth)

## Summary

The separable hard piece of the agent-config control plane: live-add a NEW MCP server connection over the Protocol. Unlike pause/resume (92d, a projection-time flag on an already-attached server), adding a connection requires an async dial + the MCP `initialize` handshake + possible OAuth — the OAuth path reuses the EXISTING unified pause/resume primitive (no new auth dance). The connection descriptor is recorded as an agent-config revision (diff/rollback via 92a); the dial outcome is fail-loud (record `failed`, never silently drop). Admin-scoped, with adding a stdio server (an RCE surface) the most privileged action.

## RFC anchor

- RFC §6.4 — Tool catalog and transports (the MCP southbound driver + attach lifecycle this drives).
- RFC §7.4 — Out of scope V1 / the unified pause/resume primitive the OAuth path reuses.
- RFC §6.16 — Agent Registry (MCP exposure is part of the agent's config; an add is a config revision).

## Briefs informing this phase

- brief 14
- brief 09

## Brief findings incorporated

- **brief 14 (MCP client/host compliance):** attaching an MCP server is Connect → `initialize` handshake → Discover (tools/resources/prompts) → register descriptors. This phase drives that real sequence on an admin add, surfacing capabilities, and fails loud on a handshake/version mismatch rather than registering a half-attached server.
- **brief 09 (MCP OAuth from bifrost):** a server requiring auth raises an OAuth need that the runtime parks on the unified pause/resume primitive (tool-side OAuth, agent-bound token keyed by the registration `agent_id`). This phase routes a new connection's OAuth requirement through that existing primitive — no new auth coordination.

## Findings I'm departing from (if any)

None.

## Goals

- Admin-scoped Protocol method to add a new MCP server connection (transport + endpoint/command + auth config) — recorded as an agent-config revision (the connection descriptor lives in the registry; diff/rollback work).
- Drive the real attach sequence: async dial → `initialize` handshake → Discover → register; fail loud on any step (record a `failed` connection state with the reason, never a silent half-attach).
- OAuth-required connections park on the unified pause/resume primitive. (As-built, wave-end audit 2026-06-22: the parking half shipped; the resume continuation that re-drives the attach to `online` is deferred to issue #375 — resume currently only releases the pause.)
- Adding a stdio server (RCE surface) is the most privileged action: beyond admin scope it is allowlist-gated and/or approval-gated via the pause/resume primitive (D-235).
- Emit `mcp.connection.added` (+ a `failed`/`pending` lifecycle event) so the Console (92h) renders the add + its OAuth/failed state.

## Non-goals

- Remove/edit an existing connection's transport (pause/resume is 92d; a destructive remove is a later refinement if needed).
- The Console add-connection form (92h).
- Non-MCP transports (HTTP/A2A add) — MCP only this phase.
- Persisting provider credentials at rest (the OAuth token lifecycle reuses the existing tool-side OAuth storage; no new secret store here).

## Acceptance criteria

- [ ] Admin-scoped Protocol method (e.g. `agent_config.add_mcp_connection`) records the connection descriptor as a revision (REPLACING/extending only the MCP-connection section, preserving Skills/ToolExposure/PromptLayers).
- [ ] The runtime drives dial → `initialize` → Discover → register; a successful add surfaces the server + its tools; a dial/handshake failure records a `failed` state with the reason and emits a loud event — never a silent drop (§13).
- [ ] An OAuth-required server parks on the unified pause/resume primitive (NOT a new mechanism); the agent-bound token keys by the registration `agent_id`. (The resume continuation that completes the attach to `online` is deferred to issue #375 — see the as-built clarification on D-237 §2.)
- [ ] Adding a stdio server is gated beyond plain admin: allowlist and/or approval via pause/resume (D-235, §7); the gate is fail-closed.
- [ ] `mcp.connection.added` + the lifecycle (`pending`/`failed`) events emitted, redacted (no secrets/tokens).
- [ ] Identity scoped by the triple; admin authority from verified ctx; the live transport for OTHER servers is untouched by an add.
- [ ] TS manifest + typed client + generated docs regenerated; `scripts/smoke/phase-92f.sh` green.

## Files added or changed

- `internal/runtime/agentcfg/protocol/addconnection.go` — the add-connection service (record revision, drive attach via the MCP driver, route OAuth to pause/resume, emit lifecycle events).
- `internal/agentcfg/agentcfg.go` — extend `ConfigPayload` (or `ToolExposure`) with the connection-descriptor section (transport/endpoint/auth) if not already representable.
- `internal/tools/drivers/mcp/` — expose a guarded runtime-attach entry point (the boot `Attach` exists; this needs a post-boot, identity-scoped, fail-loud attach the service drives) — keep the driver unaware of the registry (the service orchestrates).
- `internal/protocol/{types/agentconfig.go,methods/methods.go,singlesource/singlesource.go,transports/stream/agentconfig_handler.go}` + generator typeindex files.
- `cmd/harbor/cmd_dev.go` + `harbortest/devstack/devstack.go` — wire the add-connection service + the pause/resume + allowlist gate (D-094 twin).
- `web/console/src/lib/protocol/agentconfig.ts` + `client.ts`; `docs/site/protocol/*`; `docs/skills/...`.
- `scripts/smoke/phase-92f.sh`.

## Public API surface

```go
// Protocol: agent_config.add_mcp_connection {name, transport, command/endpoint, auth} → revision + attach
// Lifecycle: states pending → online | failed | auth_required(parked on pause/resume).
```

## Test plan

- **Unit:** add records a revision (siblings preserved); a successful attach surfaces tools; a dial/handshake failure records `failed` + emits a loud event (no silent drop); the stdio allowlist/approval gate is fail-closed; OAuth requirement parks on pause/resume (not a new path).
- **Integration:** `test/integration/agentcfg_add_connection_test.go` — drive a REAL MCP server fixture (env-gated live stdio per §17.8; a captured-transcript fixture otherwise) through the real add path: add → initialize → discover → tools appear in the next run's catalog → diff/rollback; a failing dial → `failed` recorded; non-admin 403; stdio without allowlist → rejected; under `-race`.
- **Conformance:** reuses the 92a `agentcfg` driver conformance.
- **Concurrency / leak:** concurrent adds + the existing servers' runs under `-race`; no transport leak; baseline goroutines restored.

## Smoke script additions

- `scripts/smoke/phase-92f.sh`: static — the add_mcp_connection method + the lifecycle events + the pause/resume reuse + the stdio gate symbol + typed client + generated-docs rows; live (skip-if-404) — admin add of a known fixture server returns a pending/attached descriptor; non-admin rejected; stdio-without-allowlist rejected.

## Coverage target

- `internal/runtime/agentcfg/protocol` (add-connection methods): 85%

## Dependencies

- 92a (registry + diff/rollback), 92d (the MCP-exposure section + the projection it slots into), 28 (tools/mcp driver + attach), 30 (tool-side OAuth + the unified pause/resume primitive), 50 (pause/resume coordinator).

## Risks / open questions

- **The hard part — async attach lifecycle.** Dial + handshake is genuinely async and can fail many ways (unreachable, version mismatch, auth required, malformed tools). The service must model an explicit lifecycle (`pending`/`online`/`failed`/`auth_required`) with loud events at each transition; a half-attached server is never registered. This is why 92f is the separable, last backend sub-phase.
- **stdio RCE surface.** Adding a stdio server runs an operator-supplied command — the most dangerous action in the band. The gate (allowlist + approval via pause/resume) is fail-closed and audited; the plan must NOT ship a path where a plain-admin token spawns an arbitrary process without the allowlist/approval (D-235, §7 rule 8 no shell-form exec).
- **Live-test realism (§17.8).** The fixture MUST derive from a real MCP server (a stdio binary in dev or a captured transcript) — a hand fixture that can't tell a real handshake from a wrong one is a rubber stamp.

## Glossary additions

- **runtime MCP attach (control-plane)** — the admin-driven, post-boot, identity-scoped add of a new MCP server connection: dial → `initialize` → Discover → register, fail-loud, with OAuth parked on the unified pause/resume primitive. Distinct from boot-time `Attach` (config-declared) and from pause/resume (92d, a flag on an already-attached server).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **Concurrent-reuse test passes (N≥100 against one shared registry under `-race`).**
- [ ] **Integration test exists, real MCP server fixture (§17.8) + registry + bus, identity propagation, ≥1 failure mode (failed dial / non-admin / stdio-gate), `-race`.**
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
