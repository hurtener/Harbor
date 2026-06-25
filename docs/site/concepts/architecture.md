---
description: Harbor splits into four deliberately separated layers — Runtime, Protocol, Console, and CLI — anchored by three non-negotiable product properties. A deep-linkable explainer of the seams and why they pay off.
---

# The four-layer architecture

Harbor is a Go-native runtime SDK for durable, steerable, event-driven AI agents. Its shape is not an accident of growth — it is four layers separated on purpose, so that the orchestration kernel stays headless and *everything else observes it over a versioned wire*. That single decision is what unlocks remote attach, third-party clients, and headless embedding.

This page distills the architecture and the three non-negotiable properties so they have a citable home. For design authority, defer to the [RFC](/reference/rfc).

## The four layers at a glance

| Layer | What it owns | Import direction |
| --- | --- | --- |
| **Runtime** | Tasks, planner runtime, tools, memory, sessions, events, skills, artifacts, the unified pause/resume primitive | Imports nothing above it; never imports the Console |
| **Protocol** | The canonical event/state contract — streaming events, task control, snapshots, topology, artifacts-by-reference, traces/metrics | Versioned independently of the product release |
| **Console** | Observability + control UI (a SvelteKit product) | A Protocol client only; never reads internal Runtime objects |
| **CLI** | The single `harbor` binary (`harbor dev`, deploy, scaffold, validate) | A Protocol client over the same path a remote Console would use |

The load-bearing rule across the table: the **CLI and the Console are both Protocol clients**. `harbor dev` talks to the Runtime over the same wire a browser-attached Console would — there is no privileged internal view. The Runtime never imports the Console, and the Console never reads internal Runtime objects.

## Runtime — the headless orchestration kernel

The Runtime is the orchestration kernel. It owns *mechanism*: sessions and runs, foreground and background tasks, the typed event bus, streaming, retries and timeouts, the unified pause/resume primitive, artifacts, tool dispatch, memory injection, scheduling, and provenance.

What it deliberately does **not** own is reasoning policy — that belongs to the Planner. The Runtime executes whatever a planner decides; it has no opinion on *how* the next action was chosen. See [Runtime vs. Planner](/concepts/runtime-and-planner) for the full seam.

The Runtime is headless. It emits canonical events and exposes a network surface (the Protocol); it ships no UI of its own. To watch a Runtime work, see [Observability and the Console](/concepts/observability).

## Protocol — the canonical, independently-versioned wire contract

The Harbor Protocol is the contract between the Runtime and anything observing or controlling it. It carries identity-filtered streaming events, the task-control surface (start, cancel, pause, resume, redirect, inject), state snapshots, topology, artifacts by reference, and traces/metrics.

The wire is deliberately small: **REST/JSON for control plus SSE for the event stream**, both server-enforced for identity. There is no gRPC and no WebSocket shim at V1 — `curl` is a complete client — and the `internal/protocol/transports/` seam keeps alternates additive rather than a migration.

::: tip Versioned on its own clock
The Protocol version is pinned at `0.1.0` and moves independently of the product release (currently v1.6). Bumping it is an RFC change with a deprecation window, so third-party clients are never whipsawed.
:::

Start at the [Protocol track](/protocol/) for the wire reference, the [quickstart](/protocol/quickstart), and the [build-a-client](/protocol/build-a-client) guide.

## Console — architecturally just a Protocol client

The Console is a separate SvelteKit product: a 14-page observability and control plane organized into five clusters — Runtime, Execution, Resources, Evaluation, and Settings. Every page is a *runtime lens* — a projection over canonical Protocol state — never a standalone feature and never a privileged hook into the kernel.

::: warning The decoupling rule is strict
The Console never reads internal Runtime objects. A Protocol method that maps 1:1 to an internal Go signature is a reject-on-sight smell, because it leaks internals the wire is supposed to hide. This is what lets a remote browser tab — or a third-party console, IDE, or TUI — attach to a running Runtime as a first-class client.
:::

See [Observe with the Console](/skills/observe-with-the-console/SKILL).

## CLI — the single `harbor` binary

The `harbor` binary drives the local loop: `harbor dev` boots a Runtime plus Console with hot reload and dynamic agent scaffolding, and the same binary handles deploy, scaffold, and validate.

The point worth repeating: `harbor dev` is a Protocol client. The local dev experience uses the same wire path a remote Console would, so what you debug locally is exactly what a remote operator observes. Walk it in [Run the dev loop](/skills/run-the-dev-loop/SKILL).

## The three non-negotiable properties

Three properties are non-negotiable from V1. If a change would weaken any of them, that is an RFC conversation, not a patch.

### 1. Multi-isolation from V1

Every Runtime context carries the mandatory identity triple `(tenant_id, user_id, session_id)`, with the session as the innermost scope and the most active concurrency boundary. One user can hold multiple concurrent sessions that stay fully isolated. Every identity-scoped storage method requires the full triple, and a missing component **fails closed** — the runtime raises an identity-required error and audits the rejection — with no opt-out knob. Cross-session, cross-tenant, and admin reads require an explicit elevated scope claim and are audited unconditionally.

→ [Identity and isolation](/concepts/identity-and-isolation)

### 2. The Console is a Protocol client

The Runtime is headless and emits canonical events; the Console consumes them and nothing else. This is the import rule from the table above, restated as a product guarantee: no shortcut debug endpoint, no shadow source of truth, no privileged read path — even "just for dev."

→ [Observability and the Console](/concepts/observability)

### 3. The Planner is swappable

The Runtime owns mechanism; the Planner owns reasoning policy. The contract is one interface:

```go
type Planner interface {
    Next(ctx context.Context, run RunContext) (Decision, error)
}
```

`Decision` is a sum type — `CallTool`, `CallParallel`, `SpawnTask`, `AwaitTask`, `RequestPause`, `Finish`. The Runtime executes the decision; the planner reaches Runtime features only through a pre-filtered `RunContext` view, never via package imports. Two concretes ship on identical primitives to prove the seam is real — the reference `react` planner (the default) and a `deterministic` planner. Further planners (Plan-Execute, Workflow, Graph, Supervisor, MultiAgent, HumanApproval) are post-V1 against the same interface.

→ [Runtime vs. Planner](/concepts/runtime-and-planner)

## Why the seams pay off

The layering is a means, not an end. Drawing the Protocol seam — the one most agent frameworks never draw — is what makes these capabilities fall out for free:

- **Remote attach.** Because the Runtime is headless and speaks a versioned wire, a Console (or any client) can attach to a Runtime running on another machine without special-casing.
- **Third-party clients.** A small SSE+REST surface means an IDE plugin, a TUI, or a bespoke dashboard can be a full client with no SDK lock-in. `curl` already is one.
- **Headless embedding.** The Runtime is a library first; you can embed it in your own Go service and observe it over the same Protocol. See [Embed Harbor headless](/recipes/embed-harbor-headless) and [Observe an embedded runtime](/recipes/observe-an-embedded-runtime).

::: details A worked through-line: a paused run, seen from every layer
A run pauses for HITL approval. The **Runtime** owns the pause coordinator and mints a resume token scoped to the original identity. The **Protocol** surfaces the pause as a canonical event and accepts the resume as a control instruction. The **Console** renders the pending approval as a lens over that event — it invents nothing. The **CLI** could drive the exact same resume over the exact same wire. One primitive, four layers, no parallel implementations.
:::

## Go deeper

- [RFC — the full architecture and the non-negotiable properties](/reference/rfc)
- [The Harbor Protocol track](/protocol/)
- [Runtime vs. Planner](/concepts/runtime-and-planner) · [Identity and isolation](/concepts/identity-and-isolation) · [Observability](/concepts/observability)
- [Run the dev loop](/skills/run-the-dev-loop/SKILL) · [Embed Harbor headless](/recipes/embed-harbor-headless)
