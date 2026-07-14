---
name: observe-with-the-console
description: "Tour the Console's observability pages — Overview, Live Runtime, Sessions, Tasks, Agents, Tools, Events, Background Jobs, Playground, Flows, Memory, MCP Connections, Artifacts, Settings. Use when debugging an agent's behavior, hunting a regression, or building intuition for what the runtime is actually doing under the hood."
license: Apache-2.0
metadata:
  framework: harbor
  surface: console
  verbs: "console"
---

# Observe with the Console

The Console is a Protocol client — it never reads internal Runtime objects, only canonical Protocol events, state snapshots, topology, artifacts, traces, metrics. Everything you see in the UI is something a third-party UI (yours, mine, a TUI) could also see. That property is what makes the Console teach you the runtime: if it's visible, it's a Protocol surface.

This skill tours the pages and what each is for.

## The nav — four clusters

The sidebar groups pages into four labelled clusters:

```text
Runtime    · Overview · Live Runtime
Execution  · Sessions · Tasks · Agents · Tools · Events · Background Jobs · Playground
Resources  · Flows · Memory · MCP Connections · Artifacts
Settings   · Settings
```

A persistent top bar (breadcrumb, ⌘K search, identity avatar) sits above the content, and one global status bar runs along the bottom — connection state, Protocol version, live-event count, Console version.

Many panels are **capability-adaptive**: every page reads `runtime.info.capabilities` once at attach to discover which Protocol methods this Runtime instance advertises. A capability that's "off" hides the corresponding panel (or shows a "not available" notice). Use this when you've stripped down a Runtime for an embedded deployment — the Console gracefully degrades.

---

## Runtime

### Overview — the at-a-glance hub

The operator's landing page, composed entirely over the shipped data layer (no Overview-specific Protocol method). It folds several surfaces into one canvas:

- **Context + health row** — the active identity context plus a runtime-health pill (`runtime.health`).
- **Audit ribbon** — a recent-actions strip folded from the event stream (this is where the redacted audit view lives; there is no standalone Audit page).
- **KPI cards** — four counters (`runtime.counters`) sampled into a client-side ring buffer for real trend sparklines + deltas. This is the closest thing to a metrics dashboard; for production observability, point a real Prometheus at the Runtime.
- **Interventions queue** — pending pauses (`pause.list`) with approve/reject inline.
- **Cost rollup** (by model) + an **alerts strip** + a **recent-activity** feed, all derived client-side from `events.subscribe`.

### Live Runtime — the operations cockpit

A single-runtime drill-down whose composition is a pure function of the runtime's advertised capabilities. It renders an always-present **spine** — posture header, activity counters, needs-attention, live events, active sessions, health, cost — plus capability-gated panels.

This page absorbs several things that are NOT standalone nav pages:

- **Health** — the heartbeat lives in the spine's health panel (and is mirrored in the bottom status bar's connection indicator). Green = Runtime healthy + configured drivers reachable; red = something's down (SQLite locked, Postgres unreachable, LLM provider timing out).
- **Traces** — toggle the trace overlay on, then select a run/node to see its OTel span tree. Same data your real OTel collector sees if you've wired one.
- **Topology** — a capability-gated panel (gated on the `topology.snapshot` method). It draws a live graph of the runtime's wiring: LLM client, tool catalog (one node per tool), memory driver, state driver, artifact store, event bus, skill catalog, with edges for data flow. `topology.snapshot` is a Protocol method — third-party UIs can read it too. On a planner/RunLoop runtime that doesn't advertise topology, the cockpit fills the viewport with the other spine panels instead of a topology void.

---

## Execution

### Sessions — the per-user lifecycle view

Every session for the attached identity. Idle TTL countdown, hard-cap countdown, status (active / idle / expired). Sweep events when sessions are reaped. Click a session for its detail.

A session row shows its **title** when one is set (truncated, with the full title + id in a tooltip) and falls back to the bare id otherwise. Click **Rename** inline to set/change/clear it — this calls `sessions.set_title` (D-288) and can name any session of your own `(tenant, user)`, not just the one you're currently attached to.

The **"Most expensive"** sort and the **cost-above** facet chip now operate on TRUTHFUL per-session cost — the runtime populates `total_cost_cents` / `total_tokens` / `tasks_count` / `events_count` at the source (D-309), so sorting by cost actually reorders by real spend instead of the old permanently-zero placeholder. The Events cell shows a **"≥"** prefix when a row's counts are an honest lower bound (its per-session scan hit its bound). The Agent column reads `—` when no single agent is bound to the session (there is no one authoritative agent for a multi-agent session), and the page carries no agent-id filter chip — a programmatic `filter.agent_ids` call fails loud rather than lying with an empty page.

### Tasks — the request-level view

Every chat message creates a Task. Every background spawn creates a Task. Every steer creates a Task. The Tasks page is the master list — filter by status (running / paused / completed / failed), by session, by identity.

Select a row and the page swaps into **detail mode** — same page, an in-page detail header plus a bottom-dock tab strip. This is the most useful debugging surface in the Console:

- **Details / Input / Output** — status, started/ended, duration, identity triple, planner used, LLM model, token usage, the request input and final output (`tasks.get`).
- **Events** — the canonical event stream for THIS task (every `tool.invoked`, `llm.call`, `pause.requested`, `pause.resumed`, etc.) in order.
- **Tools** — every tool invocation for the task with args, result, latency, error chain.

When something goes wrong, start here. For the OTel span tree of a task, flip the trace overlay on in **Live Runtime**.

The **"Has pending approval"** facet now narrows to REAL rows — the runtime populates `has_pending_approval` from the pause/approval registry scoped to each task's run (D-313), so filtering for it returns the tasks actually blocked on a HITL gate instead of an empty page. In the right-rail detail, the **Parent-Session** card renders the parent session's real status + start/last-activity timestamps (a fresh `serve.Enricher` read); the Agent line reads `—` where no single agent is bound (same honest-omission as Sessions), and the per-task Cost-breakdown rollup is still pending its backend (Phase 178).

### Agents — the registry

The Agent Registry (RFC §6.16). Every agent registered with this Runtime, its `agent_id` (registration identity, NOT isolation identity — see CLAUDE.md §6 clarification), capabilities, last-seen, register/deregister events. Useful for multi-agent deployments where one Runtime hosts many agents; for a single-agent setup, you see one row.

Click an agent for its detail page, which carries the per-agent tabs: **Identity**, **Autonomy**, **Tools** (connected tools), **Memory**, **Cost**, and **Skills**. The Skills tab is where you inspect the DB-backed runtime skill catalog this agent draws on (`internal/skills/`, RFC §6.7) — there is no standalone Skills nav page. See [`configure-memory-and-skills`](../configure-memory-and-skills/SKILL.md).

### Tools — what's registered

A registry view — every tool the runtime has loaded, with source (in-process / HTTP / MCP / A2A), spec, schema, cost classification. Click a tool → invocation history across tasks.

When the planner "doesn't pick a tool" you expect, check here first — confirm the tool is registered + the spec/description matches what you intended.

The catalog overview's **In-flight / Pending-approval / Awaiting-OAuth** counters render **"unavailable"** (not `0`) until the per-tool annotator backend ships (Phase 178) — an honest `aggregates_partial` marker rather than a fabricated zero (D-313). The OAuth-status and approval-policy facet chips are disabled for the same reason (a programmatic `filter.oauth_statuses` / `filter.approval_policies` call loud-rejects with `invalid_request` rather than lying with an empty page). The tool total, transport, scope, and reliability facets are real and work today.

### Events — the live stream

Every event the runtime emits, in real time, across ALL tasks the attached identity has scope for. Filter by event type, by identity, by task. Pause/resume the stream.

The table composes TWO feeds: the live `events.subscribe` tail (forward edge) AND a durable **historical window** read back over `events.list` (D-294), driven by the time-range picker — so you can scroll UP into events from before you attached ("Load older events"), not just watch new ones arrive. On a `durable` event driver the window is complete; on the default in-memory driver the ring only retains recent events and a retention-gap notice appears when older rows were evicted. A cross-tenant (fleet) window needs the `admin` or `console:fleet` scope, exactly like the live feed.

Useful when you want a system-level view ("what's happening RIGHT NOW") instead of a per-task view. The Tasks page is "this run"; Events is "every run." It's also where you'd filter for audit-shaped activity across the fleet.

The event types you see most often:

- `task.created` / `task.completed` / `task.failed`
- `tool.invoked` / `tool.result` / `tool.failed`
- `llm.call` / `llm.completion` / `llm.context_leak` (the heavy-output guard firing — RFC §6.5)
- `pause.requested` / `pause.resumed`
- `memory.read` / `memory.written`
- `agent.registered` / `agent.deregistered`

### Background Jobs — the spawn queue

The queue view for planner-spawned background tasks — a focused `tasks.list` projection filtered to background kinds. A table on the left, a detail rail on the right showing the selected job's Details, parent task, and live progress (`task.progress` + derived ETA). Includes orphan detection for spawned work that lost its parent.

### Playground — chat against your agent

Covered in depth in [`drive-the-playground`](../drive-the-playground/SKILL.md). Where you actually use the agent; the Tasks page links from here.

---

## Resources

### Flows — the engine-graph catalog

A searchable catalog of registered flows (`flows.list`) with a Flow Metrics card for the selected flow. A flow is a composable engine-graph DAG registered as a tool and invocable directly via `flows.run` — it is NOT planner-bound. The page is view-only; `Run flow` is the only mutating action and it's admin-gated. The Budget meter's **tokens** bar now renders real per-run token consumption (`tokens_used`, summed symmetrically with `cost_usd_used`) instead of a flat 0 (D-313).

### Memory — per-session inspection

Pick a session → see the memory state the planner has access to. Useful when debugging "the agent should know X but it's behaving like it doesn't" — confirm X is actually in memory. V1 memory has no TTL, so the old "expiring soon" TTL facet chip + the "Expiring in 1h" health counter are gone (they were structurally always empty — D-313); a programmatic `filter.agent_ids` loud-rejects with `invalid_request` because a V1 memory record carries no producer identity to filter on.

### MCP Connections — the southbound control plane

The configured MCP servers that supply tools / resources / prompts to the runtime's agents (`mcp.servers.*`). A servers table on the left, a right-rail server detail on the right. Where you confirm an MCP server is connected and what it's contributing to the tool catalog. When a server advertises an OAuth requirement (a `401` + `WWW-Authenticate` step-up), running **Test connection** (`mcp.servers.probe`) triggers requirement discovery and the detail rail shows the discovered authorization server(s), endpoints, scopes, and PKCE posture — marked **unverified (advertised by the connected server)**. Harbor only reports it: it never runs the OAuth flow, holds a token, or dials a discovered endpoint.

### Artifacts — the artifact store browser

Every artifact in the store the attached identity has scope for. Filter by MIME, by size, by task. Click an artifact → preview (image/PDF/text) + the ref ID + which tools have touched it.

When a tool persists a heavy output, this is where it lands. The Playground's file uploads also land here.

---

## Settings

A calm "sub-nav rail + one section at a time" surface. The left rail lists Console-local sections (preferences, saved state) first, then a Runtime sub-heading for the read-only posture sections (runtime / governance / LLM posture). The one net-new Protocol method it owns is `auth.rotate_token`, behind the Per-Runtime Auth card's "Rotate token."

---

## The `<PageState>` contract

Every page has a four-state async contract:

- **Loading** — initial fetch in flight.
- **Loaded** — data fetched, rendering.
- **Empty** — fetch returned no data (no tasks yet, no events yet, etc.) — page shows a helpful empty-state with a CTA back to the relevant action.
- **Error** — fetch failed; page shows the error + a Retry button.

If a page shows infinite Loading, the underlying Protocol call is hung — check the Runtime stderr.

## Common failure modes

- **Status bar / connection indicator flips to "Disconnected" mid-session.** Token expired or rotated. See [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md) §4.
- **Events page shows nothing but the stream is "active".** Identity scope mismatch — you're scoped to `tenant=A` but tasks are running as `tenant=B`. Update the identity context OR the localStorage seed.
- **A panel reads "not available on this Runtime".** Capability disabled — the runtime advertised that the method isn't supported. Either it's an intentional deployment (embedded runtime, stripped down) or the runtime version is older than the Console.
- **Live Runtime's topology panel is missing.** The runtime doesn't advertise `topology.snapshot` (e.g. a planner/RunLoop runtime). The cockpit fills the gap with its other spine panels rather than showing an empty graph.

## See also

- [`drive-the-playground`](../drive-the-playground/SKILL.md) — chat against the agent; the Tasks page links from there.
- [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md) — boot Runtime + Console.
- [`use-the-harbor-protocol`](../use-the-harbor-protocol/SKILL.md) — every Console page maps onto a Protocol method; build your own UI from the same surface.
- `docs/design/console/CONVENTIONS.md` — the design system every Console page is built against.
- RFC §6.16 — the Agent Registry.
- RFC §6.5 — the context-window safety net (`llm.context_leak`).
