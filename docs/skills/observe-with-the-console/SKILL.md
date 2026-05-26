---
name: observe-with-the-console
description: "Tour the Console's observability pages — Tasks, Events, Tools, Memory, Skills, Sessions, Artifacts, Traces, Metrics, Agents, Audit, Health, Topology, Playground. Use when debugging an agent's behavior, hunting a regression, or building intuition for what the runtime is actually doing under the hood."
license: Apache-2.0
metadata:
  framework: harbor
  surface: console
  verbs: "console"
---

# Observe with the Console

The Console is a Protocol client — it never reads internal Runtime objects, only canonical Protocol events, state snapshots, topology, artifacts, traces, metrics. Everything you see in the UI is something a third-party UI (yours, mine, a TUI) could also see. That property is what makes the Console teach you the runtime: if it's visible, it's a Protocol surface.

This skill tours the V1.1 pages and what each is for.

## The nav — 14 pages

```text
Connection · Playground · Tasks · Events · Tools · Skills · Memory · Sessions · Artifacts · Traces · Metrics · Agents · Audit · Health · Topology
```

Plus a footer connection indicator + a global identity-triple chip.

## 1. Playground — chat against your agent

Covered in depth in [`drive-the-playground`](../drive-the-playground/SKILL.md). Where you actually use the agent.

## 2. Tasks — the request-level view

Every chat message creates a Task. Every background spawn creates a Task. Every steer creates a Task. The Tasks page is the master list — filter by status (running / paused / completed / failed), by session, by identity.

Click a task → the **Task detail page**. This is the most useful debugging surface in the Console. It has tabs for:

- **Overview** — status, started/ended, duration, identity triple, planner used, LLM model, token usage, tool calls count, total cost (when governance is wired).
- **Events** — the canonical event stream for THIS task (every `tool.invoked`, `llm.call`, `pause.requested`, `pause.resumed`, etc.) in order.
- **LLM** — every prompt + completion verbatim. Click a turn to expand the system prompt + tool definitions + memory replay.
- **Tools** — every tool invocation with args, result, latency, error chain.
- **Memory** — what the planner read from memory each turn.
- **Pauses** — any `RequestPause` events + their resume reasons.
- **Trace** — the OTel trace tree for the task.

When something goes wrong, start here. The Task detail page tells the whole story.

## 3. Events — the live stream

Every event the runtime emits, in real time, across ALL tasks the attached identity has scope for. Filter by event type, by identity, by task. Pause/resume the stream.

Useful when you want a system-level view ("what's happening RIGHT NOW") instead of a per-task view. The Tasks page is "this run"; Events is "every run."

The event types you see most often:

- `task.created` / `task.completed` / `task.failed`
- `tool.invoked` / `tool.result` / `tool.failed`
- `llm.call` / `llm.completion` / `llm.context_leak` (the heavy-output guard firing — RFC §6.5)
- `pause.requested` / `pause.resumed`
- `memory.read` / `memory.written`
- `agent.registered` / `agent.deregistered`

## 4. Tools — what's registered

A registry view — every tool the runtime has loaded, with source (in-process / HTTP / MCP / A2A), spec, schema, cost classification. Click a tool → invocation history across tasks.

When the planner "doesn't pick a tool" you expect, check here first — confirm the tool is registered + the spec/description matches what you intended.

## 5. Skills — the runtime skill catalog

The DB-backed skill catalog. Browse, view body, see which tasks have pulled which skill. Distinct from the operator skills you're reading; this is `internal/skills/` (RFC §6.7) — see [`configure-memory-and-skills`](../configure-memory-and-skills/SKILL.md).

## 6. Memory — per-session inspection

Pick a session → see the memory state the planner has access to. Useful when debugging "the agent should know X but it's behaving like it doesn't" — confirm X is actually in memory.

## 7. Sessions — the per-user lifecycle view

Every session for the attached identity. Idle TTL countdown, hard-cap countdown, status (active / idle / expired). Sweep events when sessions are reaped.

## 8. Artifacts — the artifact store browser

Every artifact in the store the attached identity has scope for. Filter by MIME, by size, by task. Click an artifact → preview (image/PDF/text) + the ref ID + which tools have touched it.

When a tool persists a heavy output, this is where it lands. The Playground's file uploads also land here.

## 9. Traces — the OTel tree

OpenTelemetry traces for every task. Drill in to see per-tool, per-LLM-call spans. Same data your real OTel collector sees if you've wired one.

## 10. Metrics — the metrics dashboard

Prometheus-style metrics — task throughput, latency P50/P95/P99, tool invocation count, LLM token usage, error rate, governance violations. The Console pulls these from the Runtime's `/metrics` endpoint.

For production observability, point a real Prometheus at the Runtime; the Console's view is convenient for dev.

## 11. Agents — the registry

The Agent Registry (RFC §6.16). Every agent that's registered with this Runtime, its `agent_id` (registration identity, NOT isolation identity — see CLAUDE.md §6 clarification), its capabilities, last-seen, register/deregister events.

Useful for multi-agent deployments where one Runtime hosts many agents. For a single-agent setup, you see one row.

## 12. Audit — the redacted audit log

Every audit-emitting action with `audit.Redactor` already applied. Tool args/results redacted per the redaction policy. Filter by tenant, user, session, action.

For compliance: this is what you'd grep for "user X did action Y on date Z."

## 13. Health — the heartbeat

`/healthz` proxied. Green = Runtime healthy + all configured drivers reachable. Red = something's down (SQLite locked, Postgres unreachable, LLM provider timing out).

## 14. Topology — the runtime's wiring diagram

A live graph of the runtime's components: Bifrost LLM client, Tool catalog (with each tool node), Memory driver, State driver, Artifact store, Event bus, Skill catalog. Edges show data flow.

The topology snapshot is a Protocol method (`topology_snapshot`, V1.1 phase 84a) — third-party UIs can read it too. The graph re-renders on hot reload.

## Per-instance capabilities

Every page reads `runtime.info.capabilities` once at attach to discover which Protocol methods this Runtime instance advertises. A capability that's "off" hides the corresponding page (or shows a "not available" notice on this Runtime). Use this when you've stripped down a Runtime for an embedded deployment — the Console gracefully degrades.

## The `<PageState>` contract

Every page has a four-state async contract:

- **Loading** — initial fetch in flight.
- **Loaded** — data fetched, rendering.
- **Empty** — fetch returned no data (no tasks yet, no events yet, etc.) — page shows a helpful empty-state with a CTA back to the relevant action.
- **Error** — fetch failed; page shows the error + a Retry button.

If a page shows infinite Loading, the underlying Protocol call is hung — check the Runtime stderr. The "no events for SSE" gotcha (`payload.TaskID` capital-T) historically caused this; the current Console handles it correctly.

## Common failure modes

- **Connection footer flips to "Disconnected" mid-session.** Token expired or rotated. See [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md) §4.
- **Events page shows nothing but the stream is "active".** Identity scope mismatch — you're scoped to `tenant=A` but tasks are running as `tenant=B`. Update the identity-triple chip OR the localStorage seed.
- **A page reads "not available on this Runtime".** Capability disabled — runtime advertised that the method isn't supported. Either it's an intentional deployment (embedded runtime, stripped down) or the runtime version is older than the Console.
- **Topology page shows a graph that doesn't match my yaml.** Stale snapshot — hot reload landed but the page hasn't refetched. Click the refresh icon top-right.

## See also

- [`drive-the-playground`](../drive-the-playground/SKILL.md) — chat against the agent; the Tasks page links from there.
- [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md) — boot Runtime + Console.
- [`use-the-harbor-protocol`](../use-the-harbor-protocol/SKILL.md) — every Console page maps 1:1 to a Protocol method; build your own UI from the same surface.
- `docs/design/console/CONVENTIONS.md` (D-121) — the design system every Console page is built against.
- RFC §6.16 — the Agent Registry.
- RFC §6.5 — the context-window safety net (`llm.context_leak`).
