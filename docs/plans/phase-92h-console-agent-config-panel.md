# Phase 92h — Console: agent-config control panel (the consolidated consumer)

## Summary

The operator-facing Console consumer of the agent-config control plane: ONE consolidated agent-config control panel that renders the revision history with server-side **diff + rollback** (92a), **skills** management (92c), **MCP pause/resume + per-tool disable** (92d), the **layered prompt** editor (92e), and **add-MCP-connection** (92f). It is a pure Protocol client — every read is a state snapshot + canonical events, every write goes through the typed client; no Console-held config, no hand-rolled fetch. Admin-gated (disabled-with-tooltip for non-admin), four-state async, tokens only — per the binding Console conventions (D-121).

## RFC anchor

- RFC §7 — Console layer (runtime-lens principle; the Console is a Protocol client).
- RFC §6.16 — Agent Registry (the Agents page is a lens over the registry; agent-config is part of an agent's surface).
- RFC §6.4 — Tool catalog (the MCP Connections surface the pause/policy/add controls relate to).

## Briefs informing this phase

- brief 11
- brief 12

## Brief findings incorporated

- **brief 11 (console feature surface):** the Console is a fleet/agent management lens, not a standalone app; config control is rendered from canonical events + registry snapshots, never a Console-local source of truth. This panel renders `agent.config.revised`/`reverted` + `mcp.connection.*` events + the 92a read surface, holding nothing itself (D-061).
- **brief 12 (console deployment + shared UI):** the Console anchors on the shared component inventory + the one token surface; pages compose the shared `<PageState>` four-state contract. This panel reuses the shared UI primitives + the typed `HarborClient`, adds no bespoke fetch, and lands raw literals nowhere.

## Findings I'm departing from (if any)

None.

## Console consistency (D-121 — binding)

This page is built against the shared foundation per `docs/design/console/CONVENTIONS.md`: the single `(console)` route group (no `/console/` URL prefix), the shared app shell, the `web/console/src/lib/components/ui/` inventory, the four-state `<PageState>` async contract, the unified `HarborClient` + `connection.ts`, and the one reconciled `tokens.css` scale. Admin-only controls follow the 92b `TenantDefaultOverridesCard` precedent (disabled-with-tooltip + a scope message for non-admin). No raw color/spacing literals; no hand-rolled `fetch`; Svelte 5 runes mode.

## Goals

- A consolidated agent-config control panel (a section/route within the Agents surface or a dedicated `(console)` route) presenting, for a selected agent:
  - **Revision history + diff + rollback** (92a): list revisions newest-first, render a two-revision diff (text for prompt, set-diff for skills/exposure), one-click rollback (repoint).
  - **Skills** (92c): list / add / remove agent skills; surface pack-overwrite refusal as a clear inline error.
  - **MCP pause/resume + per-tool disable** (92d): per-server pause toggle + per-tool disable, with the "paused" state surfaced; reflects `mcp.connection.paused/resumed`.
  - **Layered prompt** (92e): edit the operator base layer + view the user layer; diff against prior revisions.
  - **Add MCP connection** (92f): a form to add a server, surfacing the pending/failed/auth-required (OAuth) lifecycle via the canonical events + the unified pause/resume advisory ("paused by an administrator" / "awaiting authorization").
- Admin-gated writes (disabled-with-tooltip for non-admin); reads follow the four-state contract.
- All Protocol calls via the typed `AgentConfigNamespace` (extended across 92a–f); zero hand-rolled fetch.

## Non-goals

- Any new Protocol surface — 92h is a CONSUMER only; every method it calls already shipped in 92a–f (RFC §7.3 satisfied).
- The session-user (non-admin) end-user UI — the operator Console renders the admin surface; a white-label end-user client is downstream (not this repo).
- The Evaluations surface (post-V1, D-064).

## Acceptance criteria

- [ ] The panel renders revision history + a two-revision diff + rollback against the live `agent_config.{list_revisions,diff,rollback,get}` methods.
- [ ] Skills management (list/upsert/delete) via `agent_config.skills.*`; pack-overwrite refusal renders as a clear inline error (not a silent failure).
- [ ] MCP pause/resume + per-tool disable via `agent_config.set_tool_exposure`; the paused state + `mcp.connection.paused/resumed` events reflected live.
- [ ] Layered prompt base editor + user-layer view via `agent_config.set_prompt_layers`; diff shows prompt deltas.
- [ ] Add-MCP-connection form via `agent_config.add_mcp_connection`; the pending/failed/auth-required lifecycle surfaced from canonical events (incl. the OAuth pause/resume advisory).
- [ ] Admin-gated writes: non-admin sees disabled controls + a scope message (the 92b card precedent); reads use `<PageState>` four states.
- [ ] No raw literals; no hand-rolled fetch; Svelte 5 runes; `svelte-check --fail-on-warnings` + `npm run lint` (incl. the protocol-ts lockstep + chat-encapsulation guards) clean.
- [ ] Playwright spec covers the panel's load + admin/non-admin states; `scripts/smoke/phase-92h.sh` green.

## Files added or changed

- `web/console/src/routes/(console)/...` — the agent-config panel route/section (within Agents or a dedicated route per CONVENTIONS.md).
- `web/console/src/lib/components/agentconfig/` — the panel components (revision-history, diff-view, skills, mcp-policy, prompt-editor, add-connection) built from the shared `ui/` inventory.
- `web/console/src/lib/agentconfig/state.svelte.ts` — the page state (runes; load/save; admin-scope getter), mirroring the 92b `TenantDefaultOverridesState`.
- `web/console/src/lib/protocol/agentconfig.ts` + `client.ts` — any read-method client additions (most shipped in 92a–f; this phase only consumes).
- `web/console/tests/agent-config-page.spec.ts` — Playwright spec.
- `scripts/smoke/phase-92h.sh`.
- `docs/design/console/page-*.md` — the per-page spec for the panel (or an extension of the Agents page spec).

## Public API surface

- N/A (Console-only; consumes the existing `agent_config.*` Protocol surface).

## Test plan

- **Unit (vitest):** the typed-client calls for each agent_config method (route + payload + error→ProtocolError); the page state transitions (idle/loading/ready/saving/error); the admin-scope gate (disabled when non-admin).
- **Integration (Playwright):** the panel loads against a live `harbor dev` (the established harness): revision history renders, a diff renders, a rollback round-trips, skills add/remove, a pause toggle reflects, the prompt editor saves, the add-connection form surfaces a lifecycle state; admin vs non-admin (disabled) states. Real Protocol round-trips (no mocked transport at the seam).
- **Conformance:** N/A.
- **Concurrency / leak:** N/A (Console page).

## Smoke script additions

- `scripts/smoke/phase-92h.sh`: static — the panel route + components present; the typed client covers the agent_config read+write methods; the page is in the `(console)` group; live (skip-if-404) — the panel's feeding methods (`agent_config.list_revisions` etc.) answer on the dev server.

## Coverage target

- `web/console` touched modules: meet the Console `svelte-check`/lint gates (no Go coverage target — Console phase).

## Dependencies

- 92a, 92c, 92d, 92e, 92f (every Protocol method the panel consumes must have shipped — RFC §7.3). Console foundation: D-121 (CONVENTIONS.md), the shared `ui/` inventory, `HarborClient`.

## Risks / open questions

- **Surface size.** Consolidating five feature areas into one panel is a large Console phase; the plan keeps each area a self-contained component over the shared inventory so the panel composes them without bespoke primitives (brief 12). If it proves too large in one PR, split by component along the 92a–f boundaries (each consumer is independently shippable behind its already-merged Protocol method) — but the default is one coherent panel.
- **Diff rendering fidelity.** The diff must render text (prompt) and structured set-diff (skills/exposure) clearly; reuse any existing diff/code component in the inventory rather than hand-rolling.
- **Lifecycle/event surfacing.** The add-connection + pause states are event-driven; the panel subscribes via the typed client's event stream, never polling a Console-local store.

## Glossary additions

- **agent-config control panel** — the consolidated Console surface (Phase 92h) that renders the agent-config control plane (revision history + diff + rollback, skills, MCP pause/policy, layered prompt, add-connection) as a pure Protocol client.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] `svelte-check --fail-on-warnings` + `npm run lint` + `npm run build` clean
- [ ] Admin-gated writes verified (non-admin disabled-with-tooltip)
- [ ] No raw color/spacing literals; no hand-rolled fetch; Svelte 5 runes
- [ ] Playwright spec passes; screenshots captured for the graphic-quality check
- [ ] If new vocabulary: glossary updated

<!-- This is a Console (no-Go) phase: the Go-specific concurrent-reuse + Go-integration checklist items are N/A; the binding gates are the Console ones above (D-121 + brief 12). -->
