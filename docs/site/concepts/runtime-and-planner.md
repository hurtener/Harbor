---
description: How Harbor separates mechanism from policy — one Runtime that owns execution, one swappable Planner interface whose Next returns a Decision sum type, and the RunContext view the Planner reads.
---

# Runtime and Planner

Harbor draws one of its load-bearing lines between **mechanism** and **policy**. The Runtime owns mechanism — everything about *how* an agent actually runs. The Planner owns policy — *what* to do next. The two meet at a single, deliberately small interface, which is what makes planners swappable without touching the kernel.

This page distills RFC sections 3.2 and 6.2. The RFC is the design authority; when this page and the [RFC](/reference/rfc) disagree, the RFC wins.

## Mechanism vs. policy

The split is not cosmetic. The Runtime is a headless kernel that knows how to execute, persist, isolate, observe, and recover work. The Planner is a reasoning policy that decides the next move and nothing else. Drawing the line here means a new planning strategy never has to re-implement retries, pause/resume, identity isolation, or artifact handling.

| The Runtime owns (mechanism) | The Planner owns (policy) |
| --- | --- |
| Sessions, runs, and the unified task lifecycle | Reasoning and next-action selection |
| Tool execution and dispatch across transports | Choosing *which* tool to call, with what arguments |
| Streaming, retries, timeouts, backoff | When to call tools in parallel vs. sequentially |
| The unified pause/resume primitive | When to request a pause (HITL, auth, approval) |
| Memory injection and skill retrieval | How to use injected context to decide |
| Artifacts, provenance, scheduling | When a run is finished |
| The `(tenant, user, session)` identity triple and its isolation | (reads identity for context; never enforces isolation itself) |

Because mechanism lives entirely in the Runtime, the Planner stays small and testable. See [Tools](/concepts/tools) for how dispatch is the Runtime's job rather than the LLM provider's, and [Pause, resume, and steering](/concepts/pause-resume-and-steering) for the one primitive every pause reason backs onto.

## The Planner contract

The entire contract is one method:

```go
// Planner owns reasoning policy. The Runtime owns everything else.
type Planner interface {
    // Next inspects the pre-filtered RunContext and returns the next
    // Decision for the Runtime to execute. It never executes anything
    // itself, and it never reaches Runtime features via package imports.
    Next(ctx context.Context, run RunContext) (Decision, error)
}
```

The loop is simple and the responsibilities are clean: the Planner returns a `Decision`, the Runtime executes it, the Runtime updates the run, and it calls `Next` again. Errors are explicit — a planner that cannot decide returns an error; it never silently degrades.

## The Decision sum type

`Next` returns a `Decision`, a sealed sum type. The Runtime knows how to execute each variant; the Planner only has to choose one.

| Decision | What the Runtime does |
| --- | --- |
| `CallTool` | Dispatch one tool through the catalog and feed the result back into the run |
| `CallParallel` | Dispatch several tools concurrently and join their results |
| `SpawnTask` | Start background work under a new `TaskID` |
| `AwaitTask` | Join previously spawned background work |
| `RequestPause` | Hand control to the unified pause primitive (HITL, OAuth, approval) |
| `Finish` | Complete the run with a final result |

::: tip SpawnTask and AwaitTask travel together
A planner that can spawn background work but cannot join it produces orphan work the Runtime cannot recover. In Harbor the spawn/await pair is the unit of value — the emission paths land together, never split across releases. Likewise, `RequestPause` is only meaningful because a real consumer (HITL, tool-side OAuth, A2A `AUTH_REQUIRED`) exercises it; see [Pause, resume, and steering](/concepts/pause-resume-and-steering).
:::

## RunContext — the Planner's view

The Planner never imports Runtime packages and never reaches into kernel internals. It reads exactly one thing: the `RunContext` the Runtime hands it on every `Next` call.

`RunContext` is a **pre-filtered view** — the Runtime has already applied identity isolation, injected the memory and skills the policy declared, and resolved heavy outputs to references before the Planner ever sees them. Concretely, that means:

- Isolation is enforced upstream. The Planner reads the run's identity quadruple for context but never performs the isolation filtering itself — storage is already scoped to `(tenant, user, session)` before the Planner runs; see [Identity and isolation](/concepts/identity-and-isolation).
- Heavy tool outputs arrive as artifact references, not bytes. The runtime-wide context-window safety net guarantees no raw heavy content reaches the model — see [Artifacts and context safety](/concepts/artifacts-and-context-safety).
- Memory and skills are already retrieved and budgeted by the time the Planner reads them — see [Memory and skills](/concepts/memory-and-skills).

This is why the seam holds: the Planner is a pure function of `(ctx, RunContext)` to `Decision`. There is no back channel.

## The V1 planners

Two concrete planners ship on these identical primitives. The second exists specifically to **prove the seam** — a single concrete would let the interface quietly grow assumptions only one implementation needs.

- **`react` (the reference planner, default).** The reasoning-and-acting loop most agents want out of the box. It is what you get with zero configuration.
- **`deterministic`.** A fixed-policy planner that walks an ordered set of decision steps. It demonstrates that the Runtime treats the Planner as a black box behind `Next`, and it makes planner-swap behavior testable without a model in the loop.

Further planners — Plan-Execute, Workflow, Graph, Supervisor, MultiAgent, HumanApproval — are post-V1 work on the *same* interface. They are new policies, not new kernels.

::: details Which planner should I use?
Start with **`react`**. It is the default for a reason: it covers the broad case of tool-using, reasoning agents, and it is the most exercised path.

Reach for **`deterministic`** when you want a fixed, auditable decision sequence — fixtures, tests, or workflows where the steps are known ahead of time and you do not want model variance.

Everything else (**Plan-Execute** for explicit plan-then-execute separation, **Workflow** and **Graph** for declarative topologies, **Supervisor** and **MultiAgent** for delegation, **HumanApproval** for gated execution) is post-V1. When those land, they slot in behind the same `Next(ctx, RunContext) (Decision, error)` contract — adopting one will be a configuration change, not a rewrite.
:::

## Build it / go deeper

- **Configure a planner** — wire `react` (or swap in `deterministic`) for an agent: [configure a planner](/recipes/configure-a-planner).
- **Embed Harbor headless** — drive the Runtime and a Planner directly from Go, with no Console or CLI: [embed Harbor headless](/recipes/embed-harbor-headless).
- **Adjacent concepts** — [Tools](/concepts/tools) (how `CallTool` is dispatched), [Pause, resume, and steering](/concepts/pause-resume-and-steering) (how `RequestPause` is handled).
- **Design authority** — RFC sections 3.2 and 6.2 in the [full RFC](/reference/rfc).
