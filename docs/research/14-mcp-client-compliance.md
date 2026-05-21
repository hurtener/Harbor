# Brief 14 — MCP client/host compliance (spec 2025-11-25)

**Date:** 2026-05-21
**Subsystem:** `internal/tools/drivers/mcp` (+ `internal/tools/auth`, `internal/mcpconsole`, `web/console`)
**RFC anchors:** §6.4 (Tools), §6.5 (LLM client), §7 (Console)
**Companions:** brief 03 (tools + integrations + LLM client), brief 09 (MCP OAuth — lessons from bifrost)
**Phases authored from this brief:** 85a–85j (post-V1; the prioritised first post-V1 band — see `docs/plans/README.md`).

## 0. TL;DR

Harbor consumes MCP servers as a *client* through the Phase 28 southbound driver (`internal/tools/drivers/mcp/`), a thin wrapper over the official Go SDK `github.com/modelcontextprotocol/go-sdk v1.6.0`. The SDK negotiates spec **2025-11-25** and handles all base-protocol mechanics for free (JSON-RPC, lifecycle, version negotiation, ping, cancellation, session headers, transports). Tool / resource / prompt *consumption* is solid; content lowering is genuinely good.

But measured against the MCP capability matrix, Harbor is **not cleanly "core compliant"** today — two defects break the spec's own bar of *"correct behaviour for every capability you advertise"*:

1. **Roots honesty violation.** Harbor advertises `roots.listChanged` (the SDK's nil-`Capabilities` default) but wires no roots provider. A server calling `roots/list` gets nothing.
2. **Pagination truncation bug.** `Discover` reads only the first page of `tools/list` / `resources/list` / `prompts/list`; servers with >1 page silently lose entries.

Everything else is *missing feature*, not *broken*: no MCP HTTP OAuth (the existing `auth.Provider` is not wired into the MCP driver at all), no sampling, no elicitation, no real roots, no completions / logging / resource-templates / progress, no Apps, no Tasks.

This brief inventories the gap against the matrix, fixes the framing of what Harbor can *claim*, and decomposes the closure into ten lettered phases (85a–85j). The predecessor's Phase 85 ("Skills Portico provider driver") is **deleted** — Portico is an MCP gateway; the generic MCP client driver *is* the Portico client, so a Portico-specific driver is an anti-pattern.

## 1. Where the MCP client code lives

| Area | Location | Role |
|---|---|---|
| MCP southbound client driver | `internal/tools/drivers/mcp/` (`mcp.go`, `auto.go`, `content.go`, `events.go`, `registry.go`, `transport_{stdio,sse,streamable}.go`) | The `*Provider` implements `tools.ToolProvider`; exposes a remote server's tools/resources/prompts as Harbor `Tool` descriptors. |
| Tool-side OAuth | `internal/tools/auth/` (Phase 30) | PKCE + RFC 7591 + refresh + encrypted store. **Not imported by the MCP driver.** |
| Console MCP observability | `internal/mcpconsole/`, `internal/protocol/mcp.go`, `internal/protocol/types/mcp_servers.go` | `mcp.servers.*` read methods — observability, not the wire path. |
| Official SDK | `github.com/modelcontextprotocol/go-sdk v1.6.0` | Negotiates spec 2025-11-25; owns base-protocol mechanics. |

The SDK distinction matters throughout: a row classified **SDK-handled** is compliant for free; a row classified **Wired** / **Partial** / **Absent** is Harbor's responsibility.

## 2. Compliance matrix — Harbor's current state

Classification: **SDK-handled** (inherited) · **Wired** (Harbor uses it) · **Partial** (incomplete) · **Absent**.

### Base protocol

| # | Capability | State | Note |
|---|---|---|---|
| 1 | JSON-RPC 2.0 | SDK-handled | Harbor never touches frames. |
| 2 | Version negotiation | SDK-handled | Negotiates `2025-11-25`; Harbor does not pin/override. |
| 3 | Lifecycle (`initialize` → `notifications/initialized`) | SDK-handled | `client.Connect` runs the handshake. |
| 4 | Capability negotiation | **Partial** | Only `roots.listChanged` advertised — and dishonestly (see §3). No `sampling` / `elicitation` / `tasks` capabilities. |
| 5 | stdio transport | Wired | `transport_stdio.go` — argv-only command transport. |
| 6 | Streamable HTTP transport | Wired | `transport_streamable.go`. |
| 7 | `MCP-Session-Id` | SDK-handled | SDK manages the header. |
| 8 | `MCP-Protocol-Version` header | SDK-handled | SDK sends it post-init. |
| 9 | OAuth for HTTP servers | **Absent (for MCP)** | `auth.Provider` exists but is not wired into the MCP driver. MCP HTTP auth = static `Headers` only. No RFC 9728, no RFC 8707. |
| 10 | Access-token safety | Partial | Static bearer via header; never query-string — but no resource binding / audience check. |
| 11 | Ping | SDK-handled | `KeepAlive` option set. |
| 12 | Cancellation | SDK-handled | `ctx` cancel → `notifications/cancelled`. |
| 13 | Progress | **Absent** | No `_meta.progressToken` sent; no `notifications/progress` handler. |
| 14 | Pagination | **Partial — BUG** | `Discover` reads page 1 only; `NextCursor` ignored. See §3. |
| 15 | Error handling | Wired | Typed `ErrTransportFailed` / `ErrMCPToolError` / `ErrSchemaInvalid`. |
| 16 | Timeouts | Wired | `tools.RunWithPolicy` shell wraps every Invoke. |

### Server features consumed

| # | Capability | State | Note |
|---|---|---|---|
| 17 | Tools discovery | **Partial** | `tools/list` wired; **no `ToolListChangedHandler`** — catalog never refreshes. |
| 18 | Tool invocation + content types | Wired | All five 2025-11-25 content kinds lowered (`content.go`); JSON fallback for unknown kinds. |
| 19 | Structured output | Wired | `OutputSchema` + `StructuredContent` preserved. |
| 20 | Resources list / read | Wired | `ListResources` / `ReadResource`. |
| 21 | Resource templates | **Absent** | `resources/templates/list` never called. |
| 22 | Resource subscriptions | **Partial** | `Subscribe` + `ResourceUpdatedHandler` → `mcp.resource_updated` event. **No `Unsubscribe`** — subscription leak on Close. |
| 23 | Prompts list / get | Wired | — |
| 24 | Completions | **Absent** | `completion/complete` never called. |
| 25 | Logging | **Absent** | No `logging/setLevel`, no `notifications/message` handler. |

### Client features exposed to servers

| # | Capability | State | Note |
|---|---|---|---|
| 26 | Roots | **Partial — DISHONEST** | Capability advertised; no provider wired. See §3. |
| 27 | Sampling | **Absent** | No `CreateMessageHandler` — servers cannot use Harbor's LLM. |
| 28 | Elicitation | **Absent** | No `ElicitationHandler` — form & URL modes both unsupported. |

### Extensions

| # | Capability | State | Note |
|---|---|---|---|
| 29 | MCP Tasks | **Absent** | go-sdk v1.6.0 exposes no `tasks/*`. Harbor must hand-transcribe — see §5. |
| 30 | MCP Apps (`ui://`) | **Absent (runtime)** | `registry.go` carries `DisplayModes` / `RawHTMLTrust` projection fields for a Console renderer that does not exist — a §13 primitive-without-consumer smell already in the tree. |
| 31 | Extension negotiation | **Absent** | `ClientCapabilities.Extensions` never populated. |
| 32 | OAuth Client-Credentials / Enterprise-Managed Authorization extensions | **Absent** | Enterprise-tier; not scoped in this band. |

## 3. The two correctness defects (cheap, must-fix-first)

These are not missing features — they are bugs in shipped Phase 28 code.

**Roots honesty violation.** The SDK, given a nil `Capabilities` in `ClientOptions`, advertises `roots.listChanged` by default. Harbor never sets `Capabilities` and never wires a roots provider, so it advertises a capability it cannot service. By the spec's compliance bar — *"correct behaviour for every capability you advertise"* — this disqualifies Harbor from a clean "core compliant" claim. Fix: either explicitly advertise an empty `Capabilities` (stop claiming roots) until 85e wires a real provider, OR ship 85e's provider. Phase 85a takes the stopgap (stop advertising); 85e flips it on properly.

**Pagination truncation.** `Discover` calls `ListTools` / `ListResources` / `ListPrompts` with `nil` params exactly once and ignores `NextCursor`. A server exposing >1 page of tools silently loses every tool past page 1 — a *silent correctness* failure, the worst kind. The SDK exposes auto-paginating iterators (`.Tools()` etc.) that Harbor simply isn't using. Fix is mechanical (Phase 85a).

Both are §17.6-class bugs: discovered by an audit, rooted in a previously-shipped phase, fixed in the band that surfaces them.

## 4. What Harbor can honestly claim

The matrix has three claim tiers. Harbor's honest position:

| Claim | Bar | Harbor today | Harbor after 85a–j |
|---|---|---|---|
| "MCP protocol compliant client" | Base protocol + ≥1 transport + correct behaviour for advertised capabilities | ❌ — blocked by the two §3 defects | ✅ after 85a |
| "Full core MCP client" | + stdio + Streamable HTTP + OAuth + tools/resources/prompts/completions/logging + roots/sampling/elicitation + progress/pagination | ❌ | ✅ after 85a–f |
| "Ecosystem-complete MCP host" | + Apps + Tasks + extension negotiation | ❌ | ✅ after 85a–j |

**Binding wording rule for Harbor docs and marketing:** never write "fully MCP compliant" unscoped. The correct phrasing once the band lands is *"MCP 2025-11-25 core-compliant, with stdio + Streamable HTTP transports, OAuth for remote servers, Roots, Sampling, Elicitation, Tasks, and MCP Apps support."* Phase 85j produces the conformance harness that *substantiates* that sentence; until 85j passes, the sentence is a claim, not a fact.

## 5. MCP Tasks — the hand-transcription path

go-sdk v1.6.0 exposes **no** `tasks/*` surface. This does **not** block Harbor: Tasks is hand-transcribed from the 2025-11-25 spec, the same pattern Harbor already used for the A2A v1 Go shapes (`internal/distributed/a2a/` — "hand-transcribed from proto", AGENTS.md §3). A sibling ecosystem framework — Dockyard, Harbor's MCP-*server* framework — has already retrofitted Tasks compatibility in Go by implementing the spec ahead of the SDK; its Go shapes are the reference implementation for the transcription.

The Tasks surface (spec `basic/utilities/tasks`, **experimental** in 2025-11-25):

**Capability shape.** Both parties declare a `tasks` capability, structured by request category:

```text
capabilities.tasks = {
  list:    {}                      # supports tasks/list
  cancel:  {}                      # supports tasks/cancel
  requests: {
    tools:      { call:          {} }   # server-side: task-augmentable tools/call
    sampling:   { createMessage: {} }   # client-side: task-augmentable sampling/createMessage
    elicitation:{ create:        {} }   # client-side: task-augmentable elicitation/create
  }
}
```

The `requests` set is **exhaustive** — a request type absent from it does not support task-augmentation.

**Methods:** `tasks/get` (poll), `tasks/result` (blocking result retrieval — returns exactly what the underlying request would have returned, success or JSON-RPC error), `tasks/cancel`, `tasks/list` (paginated). Plus the optional `notifications/tasks/status`.

**Task object:** `taskId` (receiver-generated string, unique), `status`, `statusMessage?`, `createdAt`, `lastUpdatedAt` (both ISO 8601), `ttl` (ms; `null` = unlimited), `pollInterval?` (ms).

**Status lifecycle:** `working` (initial) → `input_required` ⇄ `working` → terminal (`completed` / `failed` / `cancelled`). Terminal states never transition.

**Two-phase response:** a task-augmented request returns a `CreateTaskResult` (task data only) immediately; the real result arrives later via `tasks/result`.

**Tool-level negotiation:** `tools/list` results carry `execution.taskSupport ∈ {required, optional, forbidden}` per tool. Absent or `forbidden` → client MUST NOT task-augment (server returns `-32601`). `required` → client MUST task-augment. This is a finer layer *on top of* the `tasks.requests.tools.call` capability.

**Related-task metadata:** every request/response/notification tied to a task carries `_meta["io.modelcontextprotocol/related-task"] = {taskId}` — **except** `tasks/get` / `tasks/list` / `tasks/cancel` (the `taskId` is already a param) and `notifications/tasks/status`. `tasks/result` MUST carry it (the result structure has no task ID otherwise).

**Error codes:** `-32602` for bad/expired `taskId`, bad cursor, or cancelling a terminal task; `-32603` internal; `-32600` when a receiver requires task-augmentation and the requestor didn't task-augment.

**Harbor's two roles.** Harbor is a *requestor* toward MCP servers (task-augmenting `tools/call`, polling) — that is the 85i client. But once Harbor wires sampling (85c) and elicitation (85d), servers can task-augment *those* requests, making Harbor a *receiver* that must run task state machines. 85c/85d ship the non-task path; task-augmented sampling/elicitation reception is gated on 85h/85i and called out as a cross-reference in those plans.

**Security — context binding.** The spec mandates: when an authorization context exists, receivers MUST bind tasks to it and reject cross-context `tasks/get|result|cancel`. For Harbor this maps directly onto the identity triple `(tenant, user, session)` — a task created under one identity is invisible to another. This is non-negotiable for Harbor and lands as a hard acceptance criterion in 85i (and 85h's wire types must carry the binding field).

## 6. MCP Apps — the Console-side surface

MCP Apps (`io.modelcontextprotocol/ui`) lets a tool declare an interactive HTML UI resource (`ui://...`) that renders inline in the conversation. This is **Console** work, not runtime-driver work — it touches `web/console`, not `internal/tools/drivers/mcp`. The pieces:

- Fetch `ui://` resources referenced by `_meta.ui.resourceUri`; preload before tool result.
- Render in a **sandboxed iframe** — no parent DOM / cookie / localStorage access; strict CSP; host-controlled permissions.
- Implement the AppBridge `postMessage` JSON-RPC dialect (`ui/initialize`, tool-call proxying, model-context updates, host-pushed data).

This is also where `registry.go`'s orphan `DisplayModes` / `RawHTMLTrust` / `set_raw_html_trust` projection fields finally get a consumer — Phase 85g closes that primitive-without-consumer gap. 85g is large and Console-shaped; when authored per §16 it may itself decompose (as the Console wave did), but the band carries it as one row for now.

## 7. Phase mapping (85a–85j)

| Phase | Slug | One-line goal | RFC § | Deps |
|---|---|---|---|---|
| 85a | `mcp-client-core-compliance` | Fix the two §3 defects + add `*ListChanged` handlers + resource `Unsubscribe`. Gets Harbor to a clean, honest "core compliant" claim. | §6.4 | 28 |
| 85b | `mcp-http-oauth` | Wire `auth.Provider` into the MCP driver; RFC 9728 protected-resource-metadata discovery; `WWW-Authenticate` 401 step-up; RFC 8707 resource indicators. Interactive flow via the unified pause/resume primitive. | §6.4, §3.3 | 28, 30, 50 |
| 85c | `mcp-sampling-provider` | `sampling/createMessage` handler backed by `llm.LLMClient`; `modelPreferences` mapping; multimodal content; tool-enabled sampling; approval via pause/resume. | §6.4, §6.5, §3.3 | 28, 32, 50 |
| 85d | `mcp-elicitation-provider` | `elicitation/create` form mode (restricted JSON Schema) + URL mode; HITL via pause/resume; Console surface for the prompt. | §6.4, §3.3 | 28, 50 |
| 85e | `mcp-roots-provider` | Real filesystem/workspace roots; path-traversal safety; `roots/list_changed`. Flips the honest-empty capability from 85a to a real provider. | §6.4 | 28, 85a |
| 85f | `mcp-remaining-server-features` | Completions (`completion/complete`), logging (`logging/setLevel` + `notifications/message`), resource templates (`resources/templates/list`), progress (`_meta.progressToken` + `notifications/progress`). | §6.4 | 28, 85a |
| 85g | `mcp-apps-host` | Console-side `ui://` resource renderer: sandboxed iframe, CSP, AppBridge `postMessage` dialect. Closes the `registry.go` primitive-without-consumer gap. | §6.4, §7 | 28, 85a |
| 85h | `mcp-tasks-wire-types` | Pre-phase: hand-transcribe the `tasks/*` types + capability shapes from the 2025-11-25 spec (Dockyard's Go retrofit as reference). No client logic — wire types + capability negotiation surface only. | §6.4 | 28 |
| 85i | `mcp-tasks-client` | Consume the four `tasks/*` methods (get / result / cancel / list); task-augmented `tools/call` honouring `execution.taskSupport`; related-task `_meta`; identity-bound task isolation; polling honouring `pollInterval` / `ttl`. | §6.4 | 85h, 28 |
| 85j | `mcp-client-conformance` | Conformance harness — mock MCP servers exercising every capability the band added — plus the scoped, substantiated compliance statement. | §6.4 | 85a–85i |

Ordering: 85a is the foundation (ship first — it unblocks the honest "core compliant" claim). 85b–85g have no inter-dependencies beyond 85a and can be staged in parallel. 85h precedes 85i (wire types before client). 85j is last — it conforms the whole band.

## 8. Decisions / departures

- **Phase 85 "Skills Portico provider driver" is deleted.** Portico is an MCP gateway; it speaks MCP like any other server. A Portico-*specific* skill provider driver would duplicate the generic MCP client driver and couple Harbor to one ecosystem tool. The generic MCP client driver consumes Portico exactly as it consumes any MCP server. The deletion is recorded as a decisions-log entry at implementation time; the master plan's Phase 85 row and the post-V1 bullet are replaced by the 85a–j band.
- **MCP Tasks is not SDK-blocked.** The audit's initial read ("SDK-gap as much as Harbor-gap") is superseded: Harbor hand-transcribes the spec (§5), precedent being the A2A wire shapes. 85h is the transcription pre-phase.
- **No departure from briefs 03 / 09.** Brief 09's bifrost OAuth shapes inform 85b directly; this brief extends, does not contradict it.
- **Decision numbers are not pre-assigned.** Unlike the 83-band (which pre-assigned D-105/106/107), the 85-band plans reference decisions as "filed at implementation time" — the band is post-V1 and far enough out that pre-assigned numbers would likely collide with intervening work. Each 85x phase files its decision entry when the phase is picked up, per §16.

## 9. Glossary additions

These land in `docs/glossary.md` in the PR that ships Phase 85a (the foundation phase):

- **MCP capability negotiation** — the `initialize`-time exchange where client and server each advertise only the MCP capabilities they actually service. Advertising an unserviced capability is a soft protocol violation (see the roots honesty defect, brief 14 §3).
- **MCP Tasks** — the experimental 2025-11-25 mechanism for durable, pollable, deferred-result requests. A task is a receiver-generated `taskId` plus a `working → input_required ⇄ working → terminal` state machine. Hand-transcribed into Harbor Go types in Phase 85h.
- **Task-augmented request** — an MCP request carrying a `params.task` field; the receiver returns a `CreateTaskResult` immediately and the real result is fetched later via `tasks/result`.
- **MCP Apps** — the `io.modelcontextprotocol/ui` extension: a tool declares a `ui://` HTML UI resource that the host renders inline in a sandboxed iframe. Harbor's consumer is the Console (Phase 85g).
- **Related-task metadata** — `_meta["io.modelcontextprotocol/related-task"] = {taskId}`, the field that associates every task-lifecycle message with its task.
