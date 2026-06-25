---
description: The mental-model hub for Harbor — learn the identity triple and the Runtime/Planner split once, then navigate the concept encyclopedia, each page linking down to the RFC and its matching skill or recipe.
---

# Concepts

This section explains *how Harbor thinks* — the ideas you carry in your head while you build, not the steps you type at the keyboard. It is the explanation layer that sits between the quickstart and the wire-level reference.

::: tip How to read this section
These pages teach **mental models**, not tasks.

- Want to *do* something — scaffold an agent, add a tool, drive the Console? Start at [Get Started](/get-started) and the [Guides](/guides/).
- Need the *exact wire shapes* — methods, events, errors, types? Go to the [Protocol reference](/protocol/).
- Want the *binding design authority*? Everything here distills [the RFC](/reference/rfc), which always wins when an explanation and the spec disagree.
:::

## The one mental model

Harbor has many subsystems, but almost all of them become legible once you internalize **two ideas**. Learn these and the rest of the encyclopedia reads as variations on a theme.

### 1. The identity triple

Every Runtime context carries a mandatory triple: `(tenant_id, user_id, session_id)`. The **session** is the innermost scope and the most active concurrency boundary — one user can hold many concurrent sessions, and they must stay fully isolated.

This triple is not a convenience parameter you can omit in dev. Every identity-scoped storage method requires the full triple, and a missing component **fails closed** — there is no opt-out knob. Cross-session, cross-tenant, and admin reads demand an explicit elevated scope claim and are always audited.

Once you see the triple as *the* isolation key, a lot of the design follows: why memory is session-scoped by default, why the event bus is server-filtered, why tools never read identity directly. Full treatment in [Identity and multi-isolation](/concepts/identity-and-isolation).

### 2. The Runtime / Planner split

The **Runtime owns mechanism**; the **Planner owns policy**.

| The Runtime owns (mechanism) | The Planner owns (policy) |
| --- | --- |
| sessions, runs, tasks, scheduling | reasoning and next-action selection |
| events, streaming, retries | which tool to call, when to finish |
| pause/resume, artifacts, provenance | when to spawn or await background work |
| tool execution, memory injection | when to request a pause |

The contract is a single interface. The planner's `Next(ctx, RunContext)` returns a `Decision` — a sum type of `CallTool`, `CallParallel`, `SpawnTask`, `AwaitTask`, `RequestPause`, and `Finish` — and the Runtime executes it. The Planner only reaches Runtime features through a pre-filtered `RunContext` view, never through package imports. To prove the seam is real, V1 ships two concretes on identical primitives: the reference `react` planner (default) and a `deterministic` planner. More planners come post-V1 against the same interface.

Read these two pages first, then everything else clicks: [Runtime and Planner](/concepts/runtime-and-planner) and [Identity and multi-isolation](/concepts/identity-and-isolation).

## Foundations

The load-bearing structure. Start here.

### [The four-layer architecture](/concepts/architecture)

Runtime, Protocol, Console, and CLI are separated on purpose. The Runtime is **headless** and emits canonical events; everything else observes it over a versioned wire. The CLI and the Console are both Protocol clients — `harbor dev` serves the same Protocol surface a remote browser-attached Console would attach to, so there is no privileged internal view.
Links down to [the RFC](/reference/rfc), the [Protocol overview](/protocol/), and the [dev-loop skill](/skills/run-the-dev-loop/SKILL).

### [Runtime and Planner](/concepts/runtime-and-planner)

The mechanism/policy split in depth: the `Next` interface, the `Decision` sum type, and the `RunContext` view that keeps planners from importing the kernel. The swappable-planner seam is proven by shipping two concretes at V1.
See also [Configure a planner](/recipes/configure-a-planner) and [Embed Harbor headless](/recipes/embed-harbor-headless).

### [Identity and multi-isolation](/concepts/identity-and-isolation)

The triple as the one isolation key, fail-closed semantics, elevated scope claims for cross-scope reads, and the conformance suites that prove no cross-session or cross-tenant leak under concurrent stress.
See also [Auth and identity](/protocol/auth-and-identity) and the [glossary](/reference/glossary).

### [Pause/resume and steering](/concepts/pause-resume-and-steering)

A run can pause for reasons that *look* different — HITL approval, tool-side OAuth, A2A `AUTH_REQUIRED`/`INPUT_REQUIRED`, operator `PAUSE` — but they are **one** Runtime primitive, not four parallel implementations. Steering exposes a settled nine-instruction control taxonomy (`INJECT_CONTEXT`, `REDIRECT`, `CANCEL`, `PRIORITIZE`, `PAUSE`, `RESUME`, `APPROVE`, `REJECT`, `USER_MESSAGE`) and four pause reasons; a rejected HITL gate is terminal.
See also [Steer and resume a run](/recipes/steer-and-resume-a-run), the [pause model](/protocol/pause-model), and [task control](/protocol/task-control).

## Subsystems

The capabilities that hang off the foundations. Each is identity-scoped and, where it persists, sits behind one interface with three drivers.

### [Tools and transports](/concepts/tools)

The planner reasons about one concept — a `Tool` — and the catalog hides whether it is in-process Go, HTTP, MCP, or A2A. Tool dispatch is the Runtime's job, not the LLM provider's: the planner emits `CallTool` and the Runtime invokes through the dispatcher, so provider differences disappear. Tools read identity from `context.Context` and never see raw scopes.
See also [Add an in-process tool](/skills/add-an-in-process-tool/SKILL) and [Define a tool](/recipes/define-a-tool).

### [Memory and skills](/concepts/memory-and-skills)

Two distinct subsystems that share the identity-scoped, three-driver shape.

- **Memory** is declared-policy with three strategies — `none`, `truncation`, `rolling_summary` — plus an opt-in `retrieval: semantic` mode that layers embedding-similarity search via an injected `Embedder` (and fails loudly if none is wired).
- **Skills** are token-savvy, DB-backed runtime knowledge with a `Skills.md` importer and an in-runtime generator that *persists* agent-authored skills. The planner consults them through ordinary catalog tools (`skill_search`, `skill_get`, `skill_list`, `skill_propose`).

::: warning Two meanings of "skill"
The **runtime** skill subsystem described here is not the operator-skill playbooks the docs site ships under [Skills](/skills/). Same word, different layer.
:::
See also [Configure memory and skills](/skills/configure-memory-and-skills/SKILL), [Use memory and skills from Go](/recipes/use-memory-and-skills-from-go), and [Embed and retrieve](/recipes/embed-and-retrieve).

### [Sessions, tasks, and events](/concepts/sessions-tasks-and-events)

A **session** is a longer-lived, multi-turn conversation containing many runs. Foreground and background work unify under one `TaskID` namespace with a `Kind` discriminator and a single lifecycle (idempotent spawns, cascade/isolate cancellation; background tasks run in-process at V1 with the durable seam ready). The **typed event bus** is the one canonical projection of runtime state — identity-mandatory, server-filtered, gap-free sequenced, audit-redacted before emit, drop-oldest on backpressure.
See also [streaming semantics](/protocol/streaming-semantics), [events](/protocol/events), and [Observe an embedded runtime](/recipes/observe-an-embedded-runtime).

### [Artifacts and the context safety net](/concepts/artifacts-and-context-safety)

Heavy outputs **must** route through the `ArtifactStore` — no opt-in flag, no NoOp fallback. IDs are content-addressed; access goes through a per-task scoped facade. The heavy-output threshold is 32 KB by default, runtime-configurable and per-tool overridable. A runtime-wide invariant guarantees no message reaching the LLM carries raw heavy content: producers offload to `ArtifactStub`s and a single edge pass fails loudly with `ErrContextLeak` (emitting `llm.context_leak`), plus `ErrContextWindowExceeded` when the assembled request nears the model limit. Attachment disposition (`ref` / `inline` / `provider_native` / `tool:<name>`, default `ref`) is policy, not a hardcoded MIME map.
See also [Control attachment disposition](/recipes/control-attachment-disposition) and [Provider-native attachments](/recipes/provider-native-attachments).

### [The persistence triad](/concepts/persistence)

Three drivers sit behind every persistence-shaped interface (`StateStore`, `ArtifactStore`, `MemoryStore`, `SkillStore`) and pass **one** conformance suite:

| Driver | Backed by | Use |
| --- | --- | --- |
| in-memory | zero dependencies | dev, embedding |
| SQLite | `modernc.org/sqlite` (CGo-free) | single binary, single node |
| Postgres | `pgx` | multi-node deployments |

There are no Postgres-only or SQLite-only features; a new optional capability is a new interface method plus a new conformance scenario, never per-driver hand-waving. Migrations are forward-only; SQLite uses WAL.
See also the [config reference](/reference/config) and the [productionization playbook](/reference/productionization-playbook).

### [Governance and security](/concepts/governance-and-security)

Governance is middleware between the Runtime and the LLM driver, owning identity-scoped policies the LLM substrate cannot know about: cost accumulators and ceilings, per-identity rate limits, and per-call `MaxTokens` — enforced via PreCall/PostCall hooks that fail loudly (`ErrBudgetExceeded`, `ErrRateLimited`, `ErrMaxTokensExceeded`) and emit bus events. On the security side, JWT validation is **asymmetric algorithms only** (RS/ES family; never HS\* or `none`), secrets are never hardcoded or logged, every payload passes the audit redactor, and credential passthrough is off by default.
See also the [config reference](/reference/config), [auth and identity](/protocol/auth-and-identity), and the [productionization playbook](/reference/productionization-playbook).

### [Observability and the Console](/concepts/observability)

Observability is **bus-first**: the same typed events drive both `slog` records (JSON in prod, text in dev) and OpenTelemetry spans/metrics — logging and OTel are not parallel paths. A built-in Prometheus `/metrics` endpoint ships alongside the OTLP default. The Console is a separate SvelteKit product that talks **only** over the Protocol, so every page is a runtime lens — a projection over canonical state, never a privileged hook.
See also [Observe with the Console](/skills/observe-with-the-console/SKILL), [Observe an embedded runtime](/recipes/observe-an-embedded-runtime), and [events](/protocol/events).

## The Protocol — its own track

The seam most agent frameworks never draw, drawn deliberately here. The [Harbor Protocol](/protocol/) is the canonical, independently-versioned contract between the Runtime and anything observing or controlling it: streaming events, the nine steering instructions, state snapshots, topology, artifacts-by-reference, and traces/metrics.

The wire is intentionally small — REST/JSON for control and SSE for the event stream, both server-enforced for identity — so `curl` is a complete client. The decoupling rule is strict: the Console never reads internal Runtime objects, and a method that maps 1:1 to an internal Go signature is a reject-on-sight smell. The Protocol version is pinned (currently `0.1.0`) separately from the product release; bumping it is an RFC change with a deprecation window.

::: details Start a Protocol client
The [Protocol overview](/protocol/) is the front door. From there: the [quickstart](/protocol/quickstart) runs real `curl` steps, [Build a client](/protocol/build-a-client) walks the full surface, and [Use the Harbor Protocol](/skills/use-the-harbor-protocol/SKILL) is the operator playbook.
:::

## Where to go next

| You want to… | Go to |
| --- | --- |
| build your first agent | [Get Started](/get-started) |
| follow a task-shaped walkthrough | [Guides](/guides/) |
| read the binding design source | [the RFC](/reference/rfc) |
| see the wire-level contract | [the Protocol](/protocol/) |

::: tip One last orientation
When two explanations disagree, the priority chain is **RFC → phase plans → these concept pages**. These pages distill; the [RFC](/reference/rfc) decides.
:::
