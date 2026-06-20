# Phase 92d — agent-config control plane: MCP pause/resume + per-tool policy

## Summary

The headline operator control of the agent-config plane: pause/resume an MCP server and set per-individual-tool policy (active / deferred / disabled), recorded as agent-config revisions (diff + rollback via 92a) and applied **next-turn** via projection — no transport teardown, no draining. Pausing a server sets a desired-state flag the next run's `tools.NewPlannerView` projection honours (excluding that server's tools); per-tool policy maps directly onto the existing `LoadingMode`. The transport stays WARM — resume is an instant flag flip. The intentional planner-snapshot / app-call-current asymmetry (D-234) is enforced here: a paused server's MCP **App** callbacks are rejected against current state, surfacing the operator-legible "paused by a system administrator" overlay.

## RFC anchor

- RFC §6.4 — Tool catalog and transports (the MCP southbound driver + the catalog projection this gates next-turn).
- RFC §6.16 — Agent Registry (MCP exposure + per-tool policy are part of the agent's content-hashed config; a policy edit is a config revision).
- RFC §6.15 — Governance (the admin-verb pattern).

## Briefs informing this phase

- brief 15
- brief 14
- brief 03

## Brief findings incorporated

- **brief 15 (native tool-calling + deferred loading + tag scoping):** the `LoadingMode` (active / deferred) + the run-start catalog projection (`NewPlannerView` over a `CatalogFilter`) is the mechanism for controlling which tools enter the planner prompt. This phase extends per-tool policy onto that existing seam — `disabled` (never projected, never in context) and `deferred` (discoverable, out of the initial context budget) — so the control is projection-time, not a transport operation.
- **brief 14 (mcp client/host compliance):** an MCP server is a live session; tools are named `<source>_<tool>`. This phase pauses at projection time (the live transport stays warm) rather than tearing down the session, so resume is instant and spec-compliant, and gates the app→host callback path against current server state.
- **brief 03 (tools + integrations):** the runtime owns tool dispatch; the catalog is the planner-addressable registry. Per-tool policy and server pause are catalog-projection concerns, not driver concerns — this phase keeps the MCP driver unaware of the desired-state registry; the projection reads the registry, the driver stays a transport.

## Findings I'm departing from (if any)

None.

## Goals

- Admin-scoped Protocol surface to pause/resume an MCP server and set per-tool policy, recorded as agent-config revisions.
- Run-start projection: the active revision's MCP-exposure + per-tool-policy map is resolved into the `CatalogFilter` / `LoadingMode` at run start — a paused server's tools are excluded; per-tool `disabled`/`deferred` honoured. Next-turn only (D-025).
- The transport stays warm on pause (no teardown); resume is a flag flip.
- The app→host callback gate (D-234 asymmetry): `internal/mcpconsole.AppsAccessor.CallTool` checks CURRENT server desired-state BEFORE `desc.Invoke()` and rejects a paused server's App callbacks with a typed authorization error; emit `mcp.connection.paused` so the Console/client renders the "paused by admin" overlay.

## Non-goals

- The registry primitive (92a); skills (92c); the layered prompt (92e); add-new-connection (92f); session-user safe subset (92g).
- Adding/removing a server connection (pause/resume is a flag on an already-attached server; ADD is 92f).
- Mid-flight teardown / draining of a running tool call (forbidden by D-025; in-flight planner calls keep their snapshot).

## Acceptance criteria

- [ ] Admin-scoped Protocol methods to set MCP-exposure (pause/resume per server) + per-tool policy, on the `/v1/agent_config/` family (single-source), nil-safe to 501.
- [ ] A mutation records an `agent.config.revised` revision (MCP-exposure / per-tool-policy delta in `ConfigPayload.ToolExposure`) + emits `mcp.connection.paused` / `.resumed`.
- [ ] Run-start projection: a paused server's tools are absent from the planner view; a `disabled` tool is never projected; a `deferred` tool is discoverable but out of the initial budget — verified against the real `NewPlannerView`/`CatalogFilter`/`LoadingMode` seam.
- [ ] The transport stays warm on pause (no `Provider.Close`); resume restores projection without a re-dial.
- [ ] **App→host asymmetry:** with a server paused, an `mcp.apps.call_tool` callback to that server's tool is rejected against CURRENT state (a typed authorization error) while an in-flight planner snapshot is undisturbed — covered by a test that pauses between run start and an App callback.
- [ ] `agent_config.diff` shows the structured MCP-exposure/policy set-diff; rollback repoints.
- [ ] Identity scoped by the triple; `agent_id` keys the registry (§6).
- [ ] TS manifest + typed module + generated docs regenerated; `scripts/smoke/phase-92d.sh` green.

## Files added or changed

- `internal/agentcfg/agentcfg.go` — extend `ConfigPayload` with `ToolExposure` (per-server pause flag + per-tool `LoadingMode`/disabled map).
- `internal/agentcfg/protocol/mcppolicy.go` — the pause/resume + per-tool-policy service methods (record revision, emit events).
- `internal/protocol/methods/methods.go` + `types/agentconfig.go` + `singlesource.go` + `manifest.go` — the new methods + wire types.
- `internal/protocol/transports/stream/agentconfig_handler.go` — dispatch.
- `internal/tools/planner_view.go` / `tools.go` — extend `CatalogFilter` to honour the projected desired-state (paused-server exclusion + per-tool policy) at run start.
- `internal/tools/drivers/mcp/registry.go` — a `GetServerState`/current-state read for the app-call gate.
- `internal/mcpconsole/apps.go` — the app→host current-state gate before `desc.Invoke()` + the `mcp.connection.paused` overlay event.
- `cmd/harbor/cmd_dev_runloop.go` + `harbortest/devstack` — project the active MCP-exposure/policy into the run-start `CatalogFilter` (D-094 twin).
- `web/console/src/lib/protocol/agentconfig.ts` + `client.ts`; `docs/site/protocol/*`; `docs/skills/...`.
- `scripts/smoke/phase-92d.sh`.

## Public API surface

```go
// agentcfg.ConfigPayload extension:
type ToolExposure struct {
    // PausedServers is the set of MCP source ids excluded from the next
    // run's projection (transport stays warm).
    PausedServers []string `json:"paused_servers,omitempty"`
    // ToolPolicy maps "<source>_<tool>" → loading policy (active/deferred/disabled)
    // for next-run projection.
    ToolPolicy map[string]string `json:"tool_policy,omitempty"`
}
```

## Test plan

- **Unit:** the projection honours paused-server exclusion + per-tool policy against a real `CatalogFilter`/`LoadingMode`; pause records a revision + emits `mcp.connection.paused`; the app-call gate rejects a paused server's callback; the transport is NOT closed on pause.
- **Integration:** `test/integration/agentcfg_mcp_policy_test.go` — real MCP (a fixture/stdio server, env-gated live per §17.8) + real registry + real catalog + real bus: pause a server via Protocol → next run's planner view excludes its tools → an in-flight snapshot still has them → an app→host callback to the paused server is rejected against current state → resume restores projection without re-dial → diff/rollback round-trip; non-admin rejected; under `-race`.
- **Conformance:** reuses the 92a `agentcfg` driver conformance.
- **Concurrency / leak:** N≥100 concurrent projections + policy edits against one shared registry + catalog under `-race`; no cross-run bleed; transports not leaked; baseline goroutines restored.

## Smoke script additions

- `scripts/smoke/phase-92d.sh`: static — the pause/resume + per-tool-policy method constants + the `ToolExposure` payload section + the app-call gate symbol + the `mcp.connection.paused` event + the typed module + generated-docs rows; live (skip-if-404) — pause a server through the admin-gated route, verify a subsequent projection/list reflects exclusion, a non-admin token is rejected, resume restores.

## Coverage target

- `internal/agentcfg/protocol` (mcp-policy methods): 85%
- `internal/tools` (projection extension): meet/raise the existing package target.

## Dependencies

- 92a (registry + projection seam + diff/rollback), 110a (`NewPlannerView`/`CatalogFilter`/`LoadingMode`), 107c/26b (deferred-loading + per-source policy), 28 (tools/mcp), 109i/D-173 (the app tool-context surface the asymmetry gate protects).

## Risks / open questions

- **App-call gate placement.** The gate lives in `internal/mcpconsole.AppsAccessor.CallTool` before `desc.Invoke()` (it holds the MCP registry handle + can extract `<source>`), as defence-in-depth ahead of the approval gate. Risk: a future second app-call entry path must inherit the same gate — flagged so it is not bypassed; the integration test pins the behaviour, not just the call site.
- **Per-tool policy key stability.** Policy keys on `<source>_<tool>`; a server that renames a tool orphans its policy entry. The plan pins: an orphaned policy entry is inert (the tool is absent), never a hard error, and is surfaced in the diff so an operator can prune it.
- **"Paused by admin" UX is an authorization rejection, not cosmetic.** The overlay is driven by the canonical event; the runtime emits state, the client renders — it never reaches into the App (D-061). Pinned so the Console phase that renders it stays a Protocol client.

## Glossary additions

- **next-turn projection** — resolving an agent's desired-state config (paused servers, per-tool policy, skills, prompt) into a run's immutable snapshot at run start, so a config edit applies only to the next run (D-025-aligned).
- **planner-snapshot / app-call-current asymmetry** — in-flight planner tool calls use the run's run-start snapshot; an MCP App's app→host callback (a new invocation after the run) is gated against current desired-state — the basis for the "paused by admin" rejection.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **Concurrent-reuse test passes (N≥100 against one shared registry + catalog under `-race`).**
- [ ] **Integration test exists (`test/integration/agentcfg_mcp_policy_test.go`), real MCP + registry + catalog + bus, identity propagation, the app-call asymmetry + ≥1 failure mode, `-race`.** External-protocol fixture derives from a real server (§17.8).
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
