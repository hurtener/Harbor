---
description: How a Harbor session holds many runs, how foreground and background work unify under one TaskID namespace, and why the typed event bus is the single canonical projection of runtime state for both UI streaming and telemetry.
---

# Sessions, tasks, and events

Three runtime concepts carry most of Harbor's runtime state, and they nest:
a **session** is a longer-lived conversation; inside it, **tasks** are the unit
of executable work — foreground turns and background jobs alike; and every
state change those tasks produce is narrated on **one typed event bus**. Get
these three right and observability, steering, and durability all fall out of
the same machinery instead of needing parallel plumbing.

This page distills RFC §6.8 (tasks), §6.9 (sessions), and §6.13 (the event
bus). For the authoritative design, defer to [the RFC](/reference/rfc).

## Session — many runs under one triple

A session is a longer-lived, multi-turn conversation. It is **not** a single
request: one session contains many runs over its lifetime, and it is scoped by
the identity triple `(tenant_id, user_id, session_id)` — with the session as
the innermost scope and the most active concurrency boundary in the runtime.

A few consequences follow directly from that scoping:

- **One user, many concurrent sessions.** A single user can hold several
  sessions at once, and they must stay fully isolated from one another. The
  isolation is structural, not advisory — see
  [Identity and isolation](/concepts/identity-and-isolation).
- **The session is where memory and state live.** Memory is session-scoped by
  default; promoting it to user or tenant scope is an explicit, audited policy,
  never a global. See [Memory and skills](/concepts/memory-and-skills).
- **`agent_id` is not part of the isolation key.** An agent runs *within* a
  `(tenant, user, session)`; it does not widen the boundary. Storage filters by
  the triple, never by which agent happened to execute.

::: tip Session vs. run
A **run** is one planner loop — one turn from input to a `Finish` decision. A
**session** is the conversation those runs belong to. The Console reflects the
same split: Live Runtime is the present-tense workbench (the run happening now),
while Sessions are the durable record of runs over time. See
[Observability](/concepts/observability).
:::

## Tasks — one TaskID namespace, one lifecycle

Most agent runtimes bolt "background jobs" on as a second-class subsystem with
its own IDs, its own status model, and its own cancellation rules. Harbor does
not. Foreground and background work unify under **one `TaskID` namespace** with
a `Kind` discriminator and a **single lifecycle**.

| Concept | What it means |
| --- | --- |
| `TaskID` | One opaque, registry-assigned identifier (a ULID) that namespaces every unit of work — foreground turns and background jobs share it. |
| `Kind` | The discriminator (`foreground` / `background`) that tells you *what shape* of work a task is, without forking the lifecycle. |
| Lifecycle | One state machine: `PENDING` → running → `COMPLETE` (or `FAILED` / `CANCELLED`), with `PAUSED` as a durable state. |

Because the lifecycle is shared, the same control surface, the same event
types, and the same provenance apply whether you are watching a user's chat
turn or a long-running batch task spawned from inside it.

### PAUSED is durable

`PAUSED` is not a soft "we stopped polling" state — it is durable, backed by a
planner checkpoint. A run that pauses for HITL approval, tool-side OAuth, an
A2A `AUTH_REQUIRED`, or an operator `PAUSE` is checkpointed so it can be resumed
later against the original pause's identity scope. That pause is **one
primitive**, not four — see
[Pause, resume, and steering](/concepts/pause-resume-and-steering).

### Idempotent spawns, cascade-or-isolate cancellation

Two properties keep background work from becoming orphan work:

- **Idempotent spawns.** Spawning a task carries an idempotency key, so a
  retried or re-delivered spawn does not fork a duplicate task — a key reused
  with a divergent request fails loudly rather than silently forking. This is
  what makes spawn-and-await safe across restarts and retries.
- **Cascade or isolate cancellation.** Cancelling a parent task cascades to the
  children it spawned by default (a breadth-first walk of descendants), or a
  child can opt into `isolate` so it survives the parent — the choice is
  explicit, not an accident of wiring. Cancelling one run's context never bleeds
  into a sibling run.

::: warning Background tasks: in-process default, durable opt-in
By default, background tasks execute **in-process** — they run inside the same
Runtime under the `inprocess` task driver, not on a separate worker fleet. An
opt-in `durable` task driver also ships: setting `tasks.driver: durable`
persists task, group, and patch records through the `StateStore` so they survive
a process restart (a task left running by a crash is recovered to a failed
terminal state on reopen). What is *not* yet shipped is fleet auto-re-drive —
claiming and re-running a persisted task on another worker — which slots in
behind the same `TaskID` lifecycle post-V1. Either way, design against the
`TaskID` lifecycle, not an external queue.
:::

::: details Why spawn and await are inseparable
A planner that can spawn a background task but cannot join it produces work the
runtime can never recover. Harbor treats the spawn/await pair as the unit of
value — the `SpawnTask` and `AwaitTask` planner decisions are exercised
together, never split across releases. See
[Runtime and planner](/concepts/runtime-and-planner) for the full decision sum.
:::

## The typed event bus — the one canonical projection

Here is the load-bearing idea: **the typed event bus is the single canonical
projection of runtime state.** Task lifecycle, planner decisions, LLM token
chunks, tool execution, pause/resume, governance, audit — all of it is narrated
on one bus.

Live UI streaming and telemetry are **not parallel paths**. The same typed bus
feeds a Console (or any Protocol client) and the operator telemetry; durable
semantic/lifecycle events are the recorded source of truth. Present-tense
completion animation is a bounded, non-durable fan-out and may be missed by a
reconnect, while the terminal task/answer state reconciles it. See
[Observability](/concepts/observability) for how that bus-first model fans out
to logs, traces, and metrics.

### The bus contract

The bus enforces a small set of non-negotiable properties on every event:

| Property | What it guarantees |
| --- | --- |
| Identity-mandatory | Every event carries `(tenant, user, session)` (plus `run` where applicable). A subscription without a valid identity scope is rejected — fail closed, never silent. |
| Server-filtered | Subscribers receive only events their identity scope authorizes. Cross-session / cross-tenant / admin views require an explicit elevated scope claim, audited unconditionally. The filter is server-side, not a client-side `if`. |
| Durable events are gap-free sequenced | Events published to durable history carry a per-bus monotonic `sequence`; that number is your reconnect cursor. Present-tense `llm.completion.chunk` animation carries `sequence: 0`, has no replay cursor, and may be missed across reconnect. |
| Audit-redacted before emit | Every payload passes the audit redactor *before* it hits the bus. Raw tool arguments and results — which routinely carry secrets — never reach a subscriber unredacted. |
| Drop-oldest on backpressure | A slow subscriber does not stall the runtime. On backpressure the bus drops the oldest buffered events and emits a `bus.dropped` notice so the consumer knows there is a gap, rather than silently lying. |

::: tip The durable sequence is your reconnect contract
Because durable events are gap-free sequenced and reconnect replays from a
cursor, a client that records the last durable `sequence` it processed can
disconnect, come back, and resume the durable history exactly where it left
off. Live completion animation is intentionally lossy; the terminal task and
answer state reconciles it. A `bus.dropped` between two durable sequences is
the honest signal that the buffer overflowed for a slow consumer.
:::

## See it on the wire

The event bus is exposed verbatim through the Harbor Protocol: REST/JSON for
control and Server-Sent Events for the stream, both server-enforced for
identity. `curl` is a complete client.

- [Streaming semantics](/protocol/streaming-semantics) — subscribe, filter,
  replay from a cursor, survive backpressure, and aggregate the stream.
- [The events reference](/protocol/events) — the full catalog of canonical
  event types and their payload shapes, including `task.spawned`,
  `bus.dropped`, and the lifecycle transitions described above.

## Build it

The fastest way to internalize the model is to watch the bus from a runtime you
embed yourself:

- [Observe an embedded runtime](/recipes/observe-an-embedded-runtime) —
  subscribe to the typed bus from Go, see task lifecycle and planner decisions
  flow past, and confirm identity filtering with your own eyes.
- [Steer and resume a run](/recipes/steer-and-resume-a-run) — drive a task
  through `PAUSED` and back, exercising the durable checkpoint.

## Related concepts

- [Runtime and planner](/concepts/runtime-and-planner) — the decision loop that
  produces the events a session records.
- [Identity and isolation](/concepts/identity-and-isolation) — the triple every
  session, task, and event is scoped by.
- [Pause, resume, and steering](/concepts/pause-resume-and-steering) — the one
  primitive behind the durable `PAUSED` state.
- [Observability](/concepts/observability) — how the one bus becomes logs,
  traces, and metrics without a second path.
