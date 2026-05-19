# Console page — Agents

**Slug:** `agents` &middot; **Sidebar cluster:** Execution &middot; **Route:** `/console/agents`
**Mockup:** `docs/rfc/assets/console-agents-page.png` (canonical)

## 1. Purpose

Agents is the fleet-management page for the runtime's registered agents. The page renders the Agent Registry as a runtime lens — every agent is a Harbor execution entity with planner, tool bindings, memory bindings, model policy, cost ceiling, OAuth bindings, recent activity, and operational health. Agents are NOT chatbots and NOT personas (D-062). The page answers: "which agents are registered?", "what's the health of the `customer-support` agent?", "which users / sessions are running which agent?", "are this agent's agent-bound OAuth tokens current?", "drain agent X for maintenance," "force-stop agent Y because it's runaway." The mockup's "Active Agents" rollup, per-agent cards with planner / model / tool count, and detail-card with identity / autonomy / planner-config / tools-tab / topology / cost / activity is the canonical shape.

## 2. Where it sits in the IA

Agents sits third under the **Execution** cluster (Execution → Sessions, Tasks, Agents, Tools, Events, Background Jobs). The operator reaches it from the global search palette, from a Session detail's "Agent" link, from a Task detail's "Agent" link, from the Overview "Agents" quick link, or directly from the sidebar. From a per-agent detail, the operator drills into: agent's recent sessions (deep-link to Sessions filtered), agent's recent tasks (deep-link to Tasks filtered), agent's configured tool attachments (deep-link to Tools detail per tool), agent's MCP connections (deep-link to MCP Connections per server), agent's OAuth bindings (deep-link to Settings → API tokens for user-bound; in-page configurator for agent-bound). Breadcrumb: `<runtime> / Agents` (list) and `<runtime> / Agents / <agent-id>` (detail).

## 3. Functionality matrix

- **Top metrics rollup — Active Agents / Running Tasks / Total Cost / Total Tokens (the mockup's hero numbers).** `[wave-13-extends]` Requires `agents.metrics` snapshot method (NEW) aggregating across the registry.
- **Agent list — cards or grid of registered agents with name, planner, model, tools count, status, recent activity sparkline.** `[wave-13-extends]` Requires `agents.list` Protocol method (NEW; D-060: Agent Registry is in-process per-runtime). The method's payload mirrors `registry.AgentSnapshot` (the runtime-side snapshot type — but the wire shape is the flat Protocol projection per RFC §5.1, not a 1:1 re-export).
- **Per-agent card metadata — agent_id (truncated + copy), name, planner type, model, configured-tools count, configured-MCP-servers count, sessions-today count, users-authorized count, health badge (Healthy / Degraded / Paused / Drained / Force-Stopped).** `[wave-13-extends]` `agents.list` payload.
- **Agent detail header — full agent_id, name, description, version_hash (D-059), incarnation, owner (admin who registered), status, health.** `[wave-13-extends]` `agents.get` Protocol method (NEW; per Brief 11 §"Agents view" + Brief 12 §"Open architectural questions" resolutions — required for the wave).
- **Identity tab — registration identity (agent_id / incarnation / version_hash), hosting (locally-hosted vs connect-to-remote per D-060), remote A2A AgentCard reference when remote.** `[wave-13-extends]` `agents.get` extended fields.
- **Autonomy & Planner tab — configured planner choice (ReAct / Deterministic / future Plan-Execute / Workflow / etc.), planner config (MaxSteps, repair policy, etc.).** `[wave-13-extends]` `agents.get` returns `AgentConfig` (the Protocol projection).
- **Tools tab — configured tool attachments per agent: name, source (in-process / HTTP / MCP / A2A / flow), transport, auth status (no auth / headers / OAuth user-bound / OAuth agent-bound + token freshness).** `[wave-13-extends]` `agents.tools` Protocol method (NEW) joining the agent's binding to `tools.ToolDescriptor`-shaped projections.
- **Per-tool-binding OAuth configurator — Connect / Reconnect / Revoke buttons, per binding scope (`auth.BindingScope` = `user` / `agent`).** `[shipped]` Triggers `tool.auth_required` event flow: the runtime emits `auth.ToolAuthRequiredPayload` with `AuthorizeURL` + `State` + `PauseToken` + `BindingScope`; the Console renders the page-targeted button. Token storage lives in `tools/auth` (D-083) — Console does not store the token.
- **Memory tab — configured memory strategy (Phase 24), TTLs, scope (session / user / tenant).** `[wave-13-extends]` `agents.memory` Protocol method (NEW) or extension on `agents.get`.
- **Cost ceilings tab — configured per-identity-tier ceilings (Phase 36a), current spend, rate-limit posture (Phase 36b).** `[wave-13-extends]` `agents.governance` Protocol method (NEW) or extension on `agents.get`.
- **Skills tab — agent-attached skills (Phase 38 + Phase 41 generated skills).** `[wave-13-extends]` `agents.skills` Protocol method (NEW) or join over Skills catalog.
- **Topology mini-graph (mockup right-card) — agent's typical tool-graph shape.** `[wave-13-extends]` Derived from `topology.snapshot` aggregated over recent runs (NEW projection).
- **Recent activity (mockup right column) — agent's last N events (`agent.registered`, `agent.restarted`, `agent.health`, `agent.drained`, `agent.deregistered`, `agent.paused`, `agent.restart_requested`, `agent.force_stopped`).** `[shipped]` Subscribe filtered to events whose payload's agent id matches.
- **Recent sessions for this agent.** `[wave-13-extends]` `sessions.list` (NEW) filtered by agent id; deep-link to Sessions page.
- **Permissions sub-panel — which users can invoke this agent.** `[wave-13-extends]` `agents.permissions` Protocol method (NEW) — depends on whether permissions are explicit (an agent ACL) or implicit (every authenticated user in the tenant). For V1 likely implicit; Wave 13 must pin.
- **"Open in Playground" — opens the Playground (which is part of Live Runtime here) with this agent pre-selected.** `[shipped]` Local navigation; Live Runtime's Start composer accepts the agent id.
- **Drain agent (graceful) — accept no new tasks, finish existing.** `[shipped]` Invoke `agent.drained` control via the registry (`registry.Drain`; D-066 control-claim gated; emits `agent.drained` event).
- **Restart agent — bump incarnation; rehydrate from StateStore.** `[shipped]` Invoke `registry.Restart`; emits `agent.restart_requested` + `agent.restarted` events.
- **Pause agent — stop accepting new tasks until Resume.** `[shipped]` Invoke `registry.Pause`; emits `agent.paused` event.
- **Force-stop agent — hard-stop (control-claim required).** `[shipped]` Invoke `registry.ForceStop`; emits `agent.force_stopped` event.
- **Deregister agent — remove from registry (irreversible).** `[shipped]` Invoke `registry.Deregister`; emits `agent.deregistered` event.
- **Search / filter agents (by name, planner, model, status).** `[wave-13-extends]` Console-side index per Brief 11 §CC-4 recommendation (agents are slow-moving catalog data).
- **No Priority field on agent cards or detail.** `[deferred]` D-065 dropped session-level priority from V1; agents do not carry a priority dimension either.
- **No "create new agent" / "edit agent" in the Console.** `[deferred]` Authoring agents lives in `harbor dev` + CLI per RFC §7.4; Console is inspector, not editor.

## 4. Page anatomy

- **Sidebar** (shared).
- **Top bar** (shared).
- **Main canvas** (per-page, list mode — matches mockup):
  - Row 1 — top metrics rollup (Active Agents / Running Tasks / Cost / Tokens).
  - Row 2 — filter bar + search box.
  - Row 3 — agent cards grid (planner / model / tools count badges, recent-activity sparkline).
- **Main canvas** (per-page, detail mode — matches mockup):
  - Row 1 — agent detail header (name + status badge + version_hash + control-action buttons: Pause / Drain / Restart / Force-Stop / Deregister, all control-claim gated).
  - Row 2 — left column tab strip: Identity | Autonomy | Tools | Memory | Cost | Skills.
  - Row 2 — center column: topology mini-graph + per-tool status indicators.
  - Row 2 — right column: Recent activity feed + Connected tools panel + Memory strategy summary.
- **Right rail** (per-page): not used in detail (mockup uses the three-column main canvas instead).
- **Bottom dock** (per-page): empty.
- **Footer** (shared).

## 5. Components — data in / actions out

| Component | Data in (Protocol source) | User actions (out) | Tag |
|---|---|---|---|
| Top metrics rollup | `agents.metrics` (NEW) | Click rollup → its detail page | `[wave-13-extends]` |
| Agent cards grid | `agents.list` (NEW) | Click card → detail; hover for tooltip; right-click for control-action menu | `[wave-13-extends]` |
| Filter bar / search | local UI state → Console-side index | Apply / Clear | `[wave-13-extends]` |
| Agent detail header | `agents.get` (NEW) | Copy id; click status badge → Health timeline; control buttons → `registry.Pause` / `.Drain` / `.Restart` / `.ForceStop` / `.Deregister` | `[wave-13-extends]` |
| Identity tab | `agents.get` returning `registry.AgentSnapshot`-shaped projection | Copy id / incarnation / version_hash | `[wave-13-extends]` |
| Autonomy & Planner tab | `agents.get` (returns `AgentConfig` projection) | none in V1 (read-only inspector) | `[wave-13-extends]` |
| Tools tab | `agents.tools` (NEW) | Click tool → Tools page detail; Connect / Reconnect / Revoke per OAuth binding | `[wave-13-extends]` |
| Memory tab | `agents.memory` (NEW) | Click "Inspect memory" → Memory page filtered | `[wave-13-extends]` |
| Cost ceilings tab | `agents.governance` (NEW) | Click "View ceilings" → Settings → Governance | `[wave-13-extends]` |
| Skills tab | `agents.skills` (NEW) | Click skill → Skills detail (out of V1 Skills-page surface; see page-settings.md) | `[wave-13-extends]` |
| Topology mini-graph | `topology.snapshot` aggregated over recent runs (NEW projection) | Click → Live Runtime topology for the latest run | `[wave-13-extends]` |
| Recent activity panel | `agent.registered` / `agent.restarted` / `agent.health` (`registry.AgentHealthPayload`) / `agent.drained` / `agent.deregistered` / `agent.paused` / `agent.restart_requested` / `agent.force_stopped` events filtered to this agent | Click row → expand | `[shipped]` |
| Recent sessions list | `sessions.list` (NEW) filtered by agent | Click → Sessions page detail | `[wave-13-extends]` |
| Permissions sub-panel | `agents.permissions` (NEW) | Edit (admin-only) | `[wave-13-extends]` |
| "Open in Playground" | local nav | Navigate to `/console/live-runtime?agent=<id>` | `[shipped]` |
| Pause button | `registry.Pause` (registry control method) | Submit; emits `agent.paused` | `[shipped]` |
| Drain button | `registry.Drain` | Submit; emits `agent.drained` | `[shipped]` |
| Restart button | `registry.Restart` | Submit; emits `agent.restart_requested` + `agent.restarted` | `[shipped]` |
| Force-Stop button | `registry.ForceStop` | Submit; emits `agent.force_stopped` | `[shipped]` |
| Deregister button | `registry.Deregister` | Submit; emits `agent.deregistered` | `[shipped]` |
| OAuth Connect / Reconnect / Revoke | `tool.auth_required` event flow (initiates) + `tool.auth_completed` event (confirms) | Open `AuthorizeURL` in popup; on callback, Console refreshes status | `[shipped]` |

## 6. Controls + actions

- **Toolbar:** filter bar + search.
- **Card-action (list):** click → detail; right-click → Pause / Drain / Restart (control-claim).
- **Header-action (detail):** Pause / Drain / Restart / Force-Stop / Deregister buttons; Open in Playground; Copy id.
- **Tab-action (detail Tools):** Connect / Reconnect / Revoke per binding.
- **Keyboard shortcuts:** `g a` Agents; `j` / `k` next / previous card; `Enter` open detail; `Esc` back; `p` Pause; `d` Drain; `r` Restart (all gated on control claim).

## 7. Empty / loading / error / unauthorized states

| State | Trigger | What renders | Recovery action |
|---|---|---|---|
| Empty registry | No agents registered | Empty-state: "No agents yet — scaffold one with `harbor scaffold` and `harbor dev`" + link to docs | Visit docs / CLI |
| Filtered empty | Filters yield zero | "No agents match these filters" + Clear | Clear |
| Initial loading | `agents.list` in flight | Skeleton cards | Auto |
| Protocol error — `CodeNotFound` on detail | Agent id missing (deregistered) | "Agent not found — perhaps it was deregistered"; back link | Back |
| Protocol error — `CodeScopeMismatch` on control action | Operator submitted Pause / Drain / Restart / Force-Stop / Deregister without control claim | Inline error on the button | Request elevated scope |
| Protocol error — `CodeIdentityRequired` / `CodeAuthRejected` | Identity / auth dropped | Banner + recover | Re-attach |
| OAuth flow rejected | User cancelled or upstream returned error | Inline error on the binding row; "Retry" button | Retry |

## 8. Multi-tenant / multi-runtime nuances

The Agent Registry is per-runtime-instance per D-060 — each runtime has its own. The Console renders only the registry for the active runtime; switching runtimes via the multi-runtime switcher per D-091 swaps the entire registry view. Within a runtime, default scope renders agents accessible to the operator's `(tenant, user)`; `admin` elevates to all agents in the runtime. Important: `agent_id` is NOT an isolation principal (D-059, CLAUDE.md §6 Clarifying note) — the isolation tuple is still `(tenant, user, session)`. The page filters by the operator's identity tuple, not by `agent_id` as an isolation key.

## 9. Identity scope claims required

- Default `(tenant, user, session)` triple — list / inspect agents the operator can use; render their identity / planner / tools / health.
- `admin` — list every agent in the runtime regardless of tenant; edit permissions.
- `console:fleet` — post-V1 fleet aggregator across runtimes.
- **Control-plane verbs (Pause / Drain / Restart / Force-Stop / Deregister)** require the control-scope claim per D-066 — strictly higher than ordinary identity scope; "a leaked read-only token must not be able to force-stop a fleet." Every control command is audit-redacted and emitted (per the Agent Registry's `agent.*` taxonomy).

## 10. Out of V1 (deferred)

- **Authoring agents in the Console (create / edit).** RFC §7.4 — scaffolding is in `harbor dev` + CLI; Console is inspector only.
- **Versioning & rollback (success-rate-over-version_hash, baseline promotion).** D-064 — foundational for post-V1 Evaluations; not in V1.
- **Permissions ACL editor** (when permissions become explicit). Brief 11 §"Agents view" — surfaced when the `agents.permissions` Protocol method matures.
- **Cross-runtime agent aggregator.** D-091 — post-V1.
- **Priority field on cards / detail.** D-065 dropped from V1.
- **Per-agent theming / branding (Brief 11 §"Open architectural questions" #9).** Post-V1.

## 11. References

- Brief 11 §"Agents view" (per-agent fields), §"Agent Detail view" sub-sections, §"Open architectural questions" #1 (agent as Protocol-addressable principal).
- Brief 12 §"Open architectural questions Brief 11 raised, resolved here" (Agent management surface required).
- RFC-001-Harbor.md §6.4 (tool catalog), §6.16 (Agent Registry), §7 (Console), §7.2 (Agents ≠ chatbots).
- Decisions: D-059 (three-ID model; `agent_id` is not an isolation principal), D-060 (Agent Registry is in-process per-runtime), D-061 (Console DB local-only), D-062 (Agents ≠ chatbots), D-066 (control-scope claim), D-068 (`version_hash` = SHA-256 over canonical JSON of `AgentConfig`), D-083 (tool-side OAuth — `auth.BindingScope`), D-091 (Console deployment posture).
- Phase plan: phase 30 (tool-side OAuth — `Shipped`), phase 53a (Agent Registry — `Shipped`), phase 73 (state inspection — `Pending`).
- Glossary terms used: `Agent Registry`, `agent_id`, `incarnation`, `version_hash`, `auth.BindingScope`, `Fleet control / fleet observation`, `control-scope claim`, `Console`, `Runtime lens`.
