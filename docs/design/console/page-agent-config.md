# Console page — Agent Config

**Slug:** `agent-config` &middot; **Sidebar cluster:** Execution (reached via the
Agents detail "Configure" deep-link; not a standalone sidebar entry) &middot;
**Route:** `/agent-config` (in the `(console)` group, no `/console/` URL prefix)

## 1. Purpose

Agent Config is the consolidated **agent-config control panel**: ONE operator
surface that exposes the agent-config control plane (D-234 / D-235) for a
selected agent. It renders, live and versioned, the agent's revision history
with server-side diff + rollback, its skills, its MCP pause/resume + per-tool
disable policy, its layered system prompt, and an add-MCP-connection form. Every
change applies on the agent's **next run** (the next-turn projection — never
mid-flight, per D-025).

The panel is the consolidated CONSUMER of the `agent_config.*` Protocol family
shipped across the upstream backend phases (revision registry + diff/rollback,
skills, tool-exposure, prompt-layers, add-connection). It introduces **no new
Protocol surface** — RFC §7.3 "no Console page without its feeding Protocol
phase" is satisfied because every method it calls already shipped.

## 2. Where it sits in the IA

The panel is reached from an **Agents detail** page via the "Configure"
deep-link (`/agent-config?agent=<id>`), or directly by URL. It is intentionally
NOT a 15th sidebar entry — the app shell's fixed 14-page IA (CONVENTIONS.md §2)
is preserved. The panel carries its own **agent selector** (a text input)
defaulting to the connected runtime's agent — `harbor-dev-agent` in dev — so it
works against the dev runtime's synthetic default agent even when the Agent
Registry has zero registered rows.

## 3. Functionality matrix

- **Revision history + two-revision diff + rollback (92a).** `[shipped]` Lists
  `agent_config.list_revisions` newest-first; the active revision is badged. The
  operator picks two revisions → `agent_config.diff` renders a TEXT delta for
  the prompt layers and a structured SET-diff for skills / tool-exposure /
  connections; one-click `agent_config.rollback` (behind a confirm) repoints the
  active pointer.
- **Skills — list / add / remove (92c).** `[shipped]` `agent_config.skills.list`
  / `.upsert` / `.delete`. A pack-overwrite refusal surfaces as a clear inline
  error (never a silent failure — CLAUDE.md §13).
- **MCP pause/resume + per-tool disable (92d).** `[shipped]` A desired-state
  editor (paused servers + disabled tools, keyed `source_tool`) seeded from the
  active config; `agent_config.set_tool_exposure` records a revision and emits
  `mcp.connection.paused` / `.resumed`, which the panel's live event feed
  reflects.
- **Layered prompt — operator base editor + user-layer view (92e).**
  `[shipped]` `agent_config.set_prompt_layers` writes the operator base layer;
  the user layer (composed above the base in the lower-trust position) is shown
  read-only. The diff against prior revisions is visible in the Revision history
  card.
- **Add MCP connection (92f).** `[shipped]` `agent_config.add_mcp_connection`
  surfaces the terminal attach lifecycle from the response state
  (`online` / `failed` / `auth_required`) plus the live `mcp.connection.*`
  events (`pending` → `added` / `failed` / `auth_required`, the OAuth pause
  advisory). Secret auth headers are entered as `Key: value` lines and are
  **never** rendered back after submit (write-only — D-235).
- **Admin-gated writes.** `[shipped]` Every write control is disabled-with-
  tooltip + a read-only scope banner when the connection lacks the `admin` scope
  claim (the 92b `TenantDefaultOverridesCard` precedent). The runtime ALSO gates
  — a forged call fails closed with a `scope_mismatch` 403 the area's error
  state surfaces.
- **No Console-held config.** `[shipped]` The panel holds NO config of its own
  beyond UI form/loading state — every read is an `agent_config.*` snapshot + the
  live event stream (D-061).

## 4. Page anatomy

- **Sidebar / top bar / app status bar** (shared app shell, CONVENTIONS.md §2).
- **Header card** (outside the `<PageState>` boundary): title + subtitle, the
  agent selector + Load control, and the non-admin read-only scope banner.
- **Cards canvas** (inside `<PageState>`, scrolls internally — the Agents-108l /
  Events-108h viewport-locked pattern): a two-column grid of carded area
  components — Revision history, Layered prompt, Skills, MCP policy, and a
  full-width Add MCP connection card.

## 5. Components

- `routes/(console)/agent-config/+page.svelte` — the panel route; composes the
  agent selector + the five area cards inside the four-state `<PageState>`.
- `lib/agentconfig/state.svelte.ts` — `AgentConfigPanelState`, the runes state
  controller (mirrors the 92b `TenantDefaultOverridesState`): the four-state
  primary load, the `hasAdminScope` getter, the per-area data + write actions +
  saving/error flags, and the live `mcp.connection.*` subscription.
- `lib/components/agentconfig/RevisionHistoryCard.svelte` — revisions, compare,
  and rollback; composes `DiffView`.
- `lib/components/agentconfig/DiffView.svelte` — the prompt text-delta + the
  skills / exposure / connections set-diff.
- `lib/components/agentconfig/SkillsCard.svelte` — list / add / remove skills.
- `lib/components/agentconfig/McpPolicyCard.svelte` — pause/resume + per-tool
  disable + the live connection-events feed.
- `lib/components/agentconfig/PromptLayersCard.svelte` — base editor + user-layer
  view.
- `lib/components/agentconfig/AddConnectionCard.svelte` — the add-connection
  form + lifecycle state.

## 6. Console consistency (D-121 — binding)

Built against `docs/design/console/CONVENTIONS.md`:

- Routes under `(console)/` (served at `/agent-config`), no `/console/` URL
  prefix; renders inside the app shell.
- Uses the shared `components/ui/` inventory (`PageState`, `StatusChip`); forks
  no primitive that already exists. Each of the five areas is a self-contained
  page-specific component in `components/agentconfig/`.
- Routes all async state through the four-state `<PageState>`
  (disconnected / loading / error / ready) — reads via the primary boundary, the
  diff via a nested boundary.
- Talks to the Runtime only through `HarborClient` + `connection.ts` — zero
  hand-rolled `fetch`; every call goes through the typed `AgentConfigNamespace`.
- Introduces no raw token literals (tokens only); Svelte 5 runes mode (D-092).
- Admin-only writes follow the 92b precedent: disabled-with-tooltip + a scope
  message for non-admin.

## 7. Non-goals

- No new Protocol surface (the panel is a consumer only — RFC §7.3).
- The session-user (non-admin) safe-subset UI — the operator Console renders the
  admin surface; the white-label end-user client is downstream.
- The Evaluations surface (post-V1, D-064).
