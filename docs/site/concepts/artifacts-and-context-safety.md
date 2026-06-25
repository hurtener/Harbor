---
description: How Harbor routes heavy outputs through the ArtifactStore by reference, enforces a runtime-wide invariant that no raw heavy content reaches the LLM, and treats attachment disposition as policy rather than a hardcoded MIME map.
---

# Artifacts and the context-window safety net

Agents produce heavy things: a 4 MB CSV from a query tool, a generated PDF, a base64 image, a multi-megabyte HTTP response. Naively, those bytes flow straight into the next prompt — and the run dies on a context-window error, or worse, silently truncates and the planner reasons over garbage. Harbor closes that failure mode structurally. Heavy outputs route through the **ArtifactStore** by reference, and a runtime-wide invariant guarantees that no raw heavy content ever reaches the LLM.

This page distills the design. For the binding contract, defer to the [RFC](/reference/rfc) — §6.10 covers the ArtifactStore, §6.5 covers the context-window safety net.

::: tip Where this sits
Artifacts are one of the persistence-shaped subsystems — the ArtifactStore ships behind the same in-memory / SQLite / Postgres triad as state and memory. Tools never see raw bytes flowing back into the planner; they hand heavy results to the store and the planner sees a reference. See [Tools and transports](/concepts/tools).
:::

## Artifacts: content-addressed, identity-stamped, scope-checked

An artifact is a stored blob plus metadata. Two properties make it load-bearing:

- **Content-addressed IDs.** An artifact's identity derives from its content, so the same bytes deduplicate and references are stable and verifiable.
- **A per-task scoped facade.** Tools and producers do not touch the raw store. They go through a per-task facade that **auto-stamps the identity triple** `(tenant, user, session)` onto every write and **scope-checks every read**. A tool cannot read another session's artifact, and it cannot forget to attribute one — the facade does it.

This is the same fail-closed identity discipline the rest of the Runtime enforces: a missing identity component is an error, not a silently global read. See [Identity and isolation](/concepts/identity-and-isolation).

When a tool produces a heavy result, the planner receives an `ArtifactRef` — a typed pointer to the stored blob — not the bytes. The planner can reason about *what* was produced (kind, size, origin) and pass the reference to a downstream tool, without the payload ever entering the prompt.

## The heavy-output threshold

What counts as "heavy" is a single, explicit number:

| Knob | Value |
| --- | --- |
| Default heavy-output threshold | **32 KB** |
| Global override | runtime-configurable in `harbor.yaml` |
| Per-tool override | a tool that always returns large blobs can lower its own bar |

A result at or above the threshold MUST be offloaded to the ArtifactStore. This is not advisory and it is not opt-in:

::: warning No opt-in flag, no NoOp fallback
There is no `enable_artifacts: true` switch and no NoOp store that quietly drops the offload. Offloading heavy output is the runtime's behavior, not a feature an operator remembers to turn on. A subsystem that produces heavy content always has a real ArtifactStore behind it. (This is the same "fail loudly, no silent degradation" principle that governs identity and pause/resume.)
:::

Configure the threshold and the store driver in [`harbor.yaml`](/reference/config).

## The context-window safety net

The threshold tells producers when to offload. The **safety net** guarantees they actually did — at the one place it matters, the edge of the LLM client.

Harbor enforces a runtime-wide invariant: **no message reaching the LLM carries raw heavy content.** Two mechanisms uphold it:

1. **Producers offload to `ArtifactStub`s.** Anywhere heavy content could enter the message stream — tool results, tool arguments, injected context — the producer substitutes a lightweight stub that references the stored artifact instead of inlining the bytes.
2. **A single enforcement pass at the LLM-client edge.** Just before a request is handed to the LLM driver, one pass scans the assembled message set. If any raw heavy payload slipped through, it does not truncate and it does not pray — it **fails loudly**.

The failure modes are explicit, typed errors:

| Error | Fires when | Signal |
| --- | --- | --- |
| `ErrContextLeak` | A message reaching the LLM still carries raw heavy content (a producer skipped the offload) | also emits an `llm.context_leak` event on the bus |
| `ErrContextWindowExceeded` | The assembled request approaches the model's context limit even after offload | the request is rejected before it is sent |

`ErrContextLeak` is a *bug signal*, not a recoverable runtime condition: it means a producer failed to offload, and the emitted `llm.context_leak` event surfaces it for observation rather than letting the run continue on corrupted input. Because every event flows through the typed bus, a leak is visible in telemetry and the Console, not buried. See [Observability](/concepts/observability).

::: details Why enforce at the edge instead of trusting producers?
Trusting every producer to offload correctly is exactly the assumption that breaks under refactor: a new tool transport, a new injection path, a planner that builds messages a slightly different way. A single enforcement pass at the LLM-client boundary is the one chokepoint every request passes through, so the invariant holds no matter how the message was assembled. This is the design recorded as D-026, in the RFC's §6.5; consult the [RFC](/reference/rfc) for the authoritative statement.
:::

## Attachment disposition is policy, not a hardcoded MIME map

When a message *does* legitimately carry an attachment — an image, a document, an audio clip — the question is *how* it should reach the model. Harbor does not bake that decision into a fixed table keyed on MIME type. Disposition is **policy**, with four values:

| Disposition | Meaning |
| --- | --- |
| `ref` | Pass a reference; the model receives a pointer, not the bytes. **Default.** |
| `inline` | Embed the content directly in the message (small, prompt-safe payloads). |
| `provider_native` | Upload to the provider's native file/attachment API and reference it the provider's way. Opt-in. |
| `tool:<name>` | Route the attachment to a named tool for handling. |

The default is `ref` — the conservative choice that keeps payloads out of the prompt unless something explicitly asks otherwise. Disposition resolves by precedence:

```text
per-attachment hint  >  per-agent policy  >  runtime default (ref)
```

So an operator sets a sensible per-agent policy, a specific call can override it with a per-attachment hint, and absent both, the runtime defaults to `ref`. Provider-native upload is opt-in because it sends bytes to a third party and that should be a deliberate choice, never an accident of MIME sniffing. (This is recorded as D-189; the [RFC](/reference/rfc) §6.5 is authoritative.)

## Build it

Two recipes turn the model above into working configuration:

- **[Control attachment disposition](/recipes/control-attachment-disposition)** — set the per-agent policy and per-attachment hints so attachments reach the model as `ref`, `inline`, `provider_native`, or `tool:<name>`.
- **[Provider-native attachments](/recipes/provider-native-attachments)** — opt in to uploading attachments through a provider's native file API.

Related concepts and references:

- [Tools and transports](/concepts/tools) — why heavy tool results come back as an `ArtifactRef`.
- [Persistence](/concepts/persistence) — the in-memory / SQLite / Postgres triad the ArtifactStore ships on.
- [`harbor.yaml` configuration](/reference/config) — the heavy-output threshold and artifact store driver keys.
- [RFC](/reference/rfc) — the binding design: §6.10 (ArtifactStore) and §6.5 (the context-window safety net, D-026; attachment disposition, D-189).
