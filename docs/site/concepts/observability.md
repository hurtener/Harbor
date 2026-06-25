---
description: How Harbor makes a running agent legible — one typed event bus drives slog, OpenTelemetry, and a built-in Prometheus endpoint, and the Console renders the same events as a pure Protocol client.
---

# Observability and the Console

Most agent frameworks bolt observability on after the fact: a logging call here, a tracing decorator there, a dashboard that reaches into internal objects to scrape state. Harbor draws the seam the other way around. The Runtime emits one typed event stream, and **everything that watches the system — logs, traces, metrics, and the Console — is a projection of that one stream.** There is no privileged backdoor into the Runtime, because there is nothing to privilege: the events are the source.

This page distills the two halves of that design — bus-first telemetry (RFC §6.14) and the Console as a Protocol client (RFC §7). For the authoritative treatment, defer to the [RFC](/reference/rfc).

## Bus-first telemetry

Harbor has a single canonical projection of runtime state: the [typed event bus](/concepts/sessions-tasks-and-events). The same events that stream a live turn to a UI also drive the operator-facing telemetry. Logging, tracing, and metrics are **derived from that stream, not parallel paths bolted alongside it.**

| Surface | What it is | Default behavior |
| --- | --- | --- |
| `slog` records | Structured logs derived from bus events | JSON handler in production, text handler in dev, with a standard identity attribute set (`tenant_id`, `user_id`, `session_id`, `run_id`, `task_id`, `trace_id`, `span_id`, `tool`) |
| OpenTelemetry | Spans and metrics from the same events | OTLP is the default metrics exporter; trace context propagates across run, task, and tool boundaries |
| Prometheus | Pull-based metrics scrape | A built-in `/metrics` endpoint ships at V1 alongside the OTLP default |

The practical payoff: a single event carries the full identity quadruple, so a log line, a span, and a metric label all agree on which `(tenant, user, session, run)` produced them. You correlate across the three surfaces by the same keys, because they came from the same source.

::: tip One stream, many lenses
Because telemetry and live UI streaming consume the same bus, what you see in the Console during a turn is exactly what your logs and traces recorded for it. There is no "observability version" of runtime state that can drift from the real one.
:::

The bus itself is identity-mandatory and server-filtered: subscriptions are scoped to the caller's identity, the canonical sequence is monotonic and gap-free, and **every payload runs through the audit redactor before emit** — so secrets and raw tool arguments never reach a log, a trace, or a Console pane. Under backpressure the bus drops oldest and emits a `bus.dropped` notice (carrying the dropped sequence range) rather than blocking a run. The full event catalog lives in the [events reference](/protocol/events).

## The Console — a Protocol client, not a backdoor

The Console is a **separate SvelteKit product** (built with `adapter-static`). Architecturally it is just a [Protocol](/protocol/) client: it talks to the Runtime over the same versioned wire that any third-party client uses. See the [architecture overview](/concepts/architecture) for how the four layers fit together.

::: warning No privileged access
The Console never reads internal Runtime objects. A Protocol method that maps 1:1 to an internal Go signature is a reject-on-sight smell. This is a hard rule, not a guideline — it is what lets a browser on another machine, an IDE plugin, or a `curl` script observe a Runtime with exactly the Console's fidelity.
:::

The consequence is that **every Console page is a runtime lens** — a rendering of canonical Protocol state — never a standalone feature with its own private hook into the Runtime. If a page can show you something, it is because the Protocol exposes it; if the Protocol exposes it, any client can show it too.

## The page clusters

The Console organizes its pages into clusters, each a different angle on the same canonical state:

| Cluster | What it lenses |
| --- | --- |
| **Runtime** | The live, present-tense view of the Runtime — active runs, topology, the planner runtime in motion |
| **Execution** | The work the Runtime has done and is doing — Sessions, tasks, and their traces, plus the agents, tools, and events that produced them |
| **Resources** | The catalogs and stores the Runtime resolves against — flows, memory, MCP connections, artifacts |
| **Settings** | Configuration and operator-facing control |

::: tip Evaluation is post-V1
The RFC's information architecture reserves a fifth cluster, **Evaluation** (quality and correctness surfaces for agent behavior), but it is a post-V1 subsystem — not a shipped Console cluster today. The shipped shell carries the four clusters above.
:::

## Load-bearing distinctions

Two distinctions in the Console's vocabulary are easy to get wrong and carry real design weight. Get them straight and the rest of the surface reads correctly.

::: details Live Runtime is the workbench. Sessions are the records.
**Live Runtime** is the present-tense workbench — what the Runtime is doing *right now*: active runs, in-flight tool calls, the planner reasoning step by step. It is where you watch and steer work as it happens.

**Sessions** are durable records — the multi-turn history of conversations, each containing many runs, replayable after the fact. They are where you go to understand what *already* happened.

They are not two views of the same data with a time toggle; they are two different products of the bus — one live, one persisted. Conflating them ("just show the live view, scrolled back") loses the durability guarantee Sessions exist to provide.
:::

::: details Agents are execution entities, not chatbots.
An Agent in Harbor is a **runtime execution entity** — a configured unit of work with a registration identity, a planner, a tool catalog, memory and skill policy. It is not a chat persona. The Console's Agent surfaces show you the *execution* of that entity: which runs it spawned, which tools it called, where it paused.

Treating an Agent as "a chatbot you talk to" mis-frames everything downstream — it suggests one conversation per agent, when in fact one agent runs across many isolated sessions, and a session may engage many agents. Identity isolation is on the `(tenant, user, session)` triple, never on the agent.
:::

## Observe it

| Goal | Start here |
| --- | --- |
| Watch and steer a live Runtime through the UI | [Observe with the Console](/skills/observe-with-the-console/SKILL) |
| Pull events and telemetry from a Runtime you embed in Go | [Observe an embedded runtime](/recipes/observe-an-embedded-runtime) |
| Understand the exact event shapes on the wire | [Events reference](/protocol/events) and [streaming semantics](/protocol/streaming-semantics) |
| Build your own observing client | [Use the Harbor Protocol](/skills/use-the-harbor-protocol/SKILL) |

For the design authority behind everything above, see the [RFC](/reference/rfc) (§6.14 telemetry, §7 Console). The Protocol that both the Console and your own clients speak is versioned independently of the product release — see the [changelog](/reference/changelog) for where it stands.
