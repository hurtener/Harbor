---
description: One Tool concept across in-process Go, HTTP, MCP, A2A, and Flow — the catalog hides the transport, the Runtime owns dispatch, tools read identity from context, and heavy results route to artifacts.
---

# Tools and transports

A planner should reason about *what it can do*, not *how the doing is wired*. Harbor makes that literal: there is exactly one `Tool` concept, and the catalog hides whether a given tool is local Go code, an HTTP endpoint, an MCP server, a remote A2A agent, or an internal Flow. The planner emits a decision to call a tool; the Runtime figures out the rest.

This page distills RFC §6.4. For the authoritative design, defer to the [RFC](/reference/rfc). To build something, jump to [add an in-process tool](/skills/add-an-in-process-tool/SKILL) or the [define-a-tool recipe](/recipes/define-a-tool).

## What a Tool is

A `Tool` is a typed, self-describing capability. It carries everything the Runtime and the planner need to call it safely and the LLM needs to choose it well:

| Facet | What it is | Why it matters |
| --- | --- | --- |
| Schemas | Typed input and output shapes | The planner is given the input schema; the Runtime validates calls against it |
| Side-effect class | A declared class — `pure`, `read`, `write`, `external`, or `stateful` | Lets governance and HITL gates reason about consequences |
| Auth scopes | The credentials a call requires | Drives tool-side OAuth and the pause/resume primitive when a scope is missing |
| Hints | Advisory cost / latency / safety metadata for selection | Helps the planner pick the right tool without trial and error |
| `ToolPolicy` | Timeout, retries, backoff, validate mode | A resilience contract enforced by the Runtime, not hand-rolled per tool |

The `ToolPolicy` is the resilience seam: a flaky HTTP endpoint and a fast local function are the *same concept* with different timeout and retry settings. You declare the policy; the Runtime applies it on every invocation. When `ToolPolicy` is left zero-valued, sensible defaults fire — so the common case ("register this function") is production-resilient with zero ceremony.

## The catalog and TransportKind

The `ToolCatalog` registers, resolves, and lists tools. Behind a single `TransportKind` it hides where the work actually happens:

| TransportKind | Backed by | Reach for it when |
| --- | --- | --- |
| `InProcess` | Go code in your binary | Your first tools and anything that is just a function |
| `HTTP` | A JSON-over-HTTP endpoint | Wrapping an existing internal or third-party API |
| `MCP` | A Model Context Protocol server | Consuming the growing MCP tool ecosystem |
| `A2A` | A remote agent over Agent-to-Agent | Delegating to another agent as a callable capability |
| `Flow` | An internal Harbor subflow | Composing a multi-step playbook as one callable tool |

To the planner, all five are identical: a name, an input schema, and a result. Swapping a tool from an in-process prototype to a hardened HTTP service is a catalog change, not a planner change.

## Dispatch is the Runtime's job, not the LLM provider's

This is the load-bearing distinction. In many stacks, "tools" are a feature of the LLM provider's API — the model returns a provider-specific tool-call shape and your code adapts to whatever each vendor emits. Harbor inverts that.

The planner emits a `CallTool` decision (one arm of the `Decision` sum type returned by `Next`). The Runtime — not the LLM provider — invokes the tool through the dispatcher, applies the `ToolPolicy`, validates against the schema, captures provenance, and routes the result. Provider differences disappear because the LLM client carries no tool surface at all: its single `Complete` method has no `Tools` or `ToolChoice` field.

::: tip Why this matters
Because dispatch lives in the Runtime, every transport, every provider, and every planner share one execution path — with consistent retries, timeouts, identity capture, and artifact handling. There is no per-provider tool adapter to drift. See [Runtime vs. Planner](/concepts/runtime-and-planner) for the seam this rests on.
:::

## Tools and identity

Tools run inside the mandatory identity triple `(tenant, user, session)`, but they never handle raw scopes. Identity flows through `context.Context`, and a tool reads what it needs from there. The Runtime captures the full triple in the call's provenance regardless of what the tool does.

Artifact and scoped-storage access goes through a **per-task scoped facade** that auto-stamps the identity triple on writes and scope-checks every read. A tool cannot reach across sessions or tenants by accident — the facade closes that path by construction, and a missing identity component fails closed rather than degrading silently. The isolation model is covered in full under [Identity and isolation](/concepts/identity-and-isolation).

::: warning Tools never see raw scopes
A tool that tries to read or pass an identity scope directly is working against the grain. Read identity from `ctx`; reach storage through the injected scoped facade. The facade is the contract, not a convenience.
:::

## Heavy results route to the ArtifactStore

A tool that returns a 4 MB report must not stuff those bytes into the planner's reasoning context. Harbor enforces this: heavy outputs route through the `ArtifactStore`, and the planner sees an `ArtifactRef` (a content-addressed handle), not the bytes.

- The heavy-output threshold defaults to **128 KB** and is runtime-configurable. It is a runtime-wide invariant — there are no per-tool overrides. It bounds what may enter a model's context window; Console-facing Protocol replies keep a separate, pinned **32 KB** inline bound.
- There is **no opt-in flag and no `NoOp` fallback** — offloading is the behavior, not a setting.
- A runtime-wide safety net guarantees no raw heavy content ever reaches the LLM: a single enforcement pass at the LLM-client edge fails loudly with `ErrContextLeak` rather than quietly truncating.

The planner can then decide to fetch, summarize, or hand the reference to another tool — all by reference. The full mechanism, including attachment disposition policy, lives under [Artifacts and context safety](/concepts/artifacts-and-context-safety).

::: details Which transport should I pick?
**Start in-process.** Your first tools are almost always plain Go functions — fastest to write, easiest to test, no network. Register them in the catalog as `InProcess`.

Reach for an external transport when the capability already lives outside your binary:

- **HTTP** — you have, or want, a JSON API boundary. Good for tools owned by another team or service.
- **MCP** — you want to consume an existing MCP server from the ecosystem instead of reimplementing it.
- **A2A** — the capability is itself an agent, and delegating to it (rather than copying its logic) is the right factoring.
- **Flow** — the "tool" is really a multi-step Harbor subflow you want to expose under one name.

Because the planner sees one `Tool` concept, you can prototype `InProcess` and promote to `HTTP`/`MCP`/`A2A` later without touching planner logic — only the catalog registration changes.
:::

## Build it

- **[Add an in-process tool](/skills/add-an-in-process-tool/SKILL)** — the operator playbook for registering your first Go-backed tool end to end.
- **[Define a tool](/recipes/define-a-tool)** — the recipe: schemas, side-effect class, `ToolPolicy`, and catalog registration in code.
- **[Artifacts and context safety](/concepts/artifacts-and-context-safety)** — where heavy results go and how the context safety net enforces it.
- **[RFC §6.4](/reference/rfc)** — the authoritative design for the tool catalog and transports.
