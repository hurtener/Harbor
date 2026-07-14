# Harbor Protocol Capability Map For A Generic TUI

## Executive Finding

Harbor already exposes enough Protocol surface for a useful runtime test and
control TUI. The present blocker to OpenCode-level fidelity is not basic
control breadth. It is the absence of a canonical durable **conversation turn
and ordered part projection**.

Today a client must join task snapshots and flat events into its own transcript
model. That is adequate for a functional TUI, but it allows the Console, TUI,
and third-party clients to disagree about ordering, grouping, reconnect
recovery, and unknown event behavior.

The Harbor TUI must initially use the current Protocol faithfully and keep its
reducer isolated. A future Protocol phase should decide whether a canonical
turn/part read model belongs on the wire. It should not be invented as a TUI-
private Runtime endpoint.

## 1. Core User Journeys

### 1.1 Attach And Negotiate

Sequence:

1. Collect a base URL, JWT credential source, and session identity.
2. Call `runtime.info`.
3. Compare `protocol_version` and `wire_surface_digest`.
4. Load `runtime.health`, including scoped retention horizons,
   `runtime.drivers`, `llm.posture`, and `governance.posture`.
5. Establish `events.subscribe` only after the identity/session is fixed.

Primary methods:

- `runtime.info`
- `runtime.health`
- `runtime.counters`
- `runtime.drivers`
- `metrics.snapshot`
- `llm.posture`
- `governance.posture`
- `events.subscribe`

The connect view should show the Runtime display name and instance ID,
Protocol/version compatibility, health, active LLM posture, driver names,
stream status, effective scopes, and the visible `(tenant, user, session)`
triple.

### 1.2 Create, Hydrate, Or Reactivate A Session

There is no separate session-create method. A client generates a non-empty
session ID, uses it in the authenticated session scope, and creates the first
task with `start`.

Hydration sequence for an existing session:

1. `sessions.list`
2. switch the Protocol client's session scope;
3. `sessions.inspect`;
4. `state.history` tail-first;
5. `tasks.list` to recover user query text absent from durable lifecycle
   events; and
6. `pause.list` to recover parked interventions.

Closed-session reactivation uses the same `start` method as a normal turn. A
closed, non-erased session is reopened in place, durable history is preserved,
and `session.reopened` is emitted. An erased session ID fails with HTTP 409 and
the typed `session_erased` code. There is no separate reopen method.

Session methods and lifecycle:

- `sessions.list`
- `sessions.inspect`
- `sessions.set_title`
- `sessions.delete`
- `state.history`
- `start` for closed-session reactivation
- `session.reopened`
- `session_erased`

### 1.3 Submit And Stream A Turn

Sequence:

1. render the user turn optimistically;
2. call `start`;
3. remember the returned `task_id`, which is also the steering run ID;
4. reduce relevant live events into the transcript; and
5. call `tasks.get` at terminal state for the authoritative result, terminal
   status, trajectory projection, and artifact references.

The production task enricher still emits an empty cost rollup. Cost events can
be displayed when observed live, but `tasks.get` must not be described as an
authoritative per-task or per-step cost source yet.

Important events include:

- `task.spawned`, `task.started`, `task.completed`, `task.failed`, and
  `task.cancelled`;
- `planner.decision`;
- `llm.completion.chunk` with `content` or `reasoning` kind;
- `llm.cost.recorded`;
- `tool.invoked`, `tool.completed`, and `tool.failed`;
- control and pause lifecycle events; and
- `bus.dropped` or replay-gap signals.

Every later turn is another `start` in the same session. Harbor's session
memory provides continuity; the TUI should not model chat as one permanently
running request.

### 1.4 Structured Output Testing

`StartRequest.output_schema` supports a first-class structured test mode.
Schema-constrained runs suppress speculative token streaming because retries
cannot retract output. The TUI should show the validated terminal envelope
from `tasks.get.result_inline`, and treat `output_invalid` as a failed test.

### 1.5 Artifact-Backed Input

Sequence:

1. upload with `artifacts.put`;
2. retain the returned `ArtifactRef`;
3. pass IDs through `StartRequest.input_artifact_ids`;
4. optionally set each disposition to `ref`, `inline`, `provider_native`, or
   `tool:<name>`;
5. observe disposition/provider-upload events; and
6. inspect `tasks.get.input_artifacts`.

`user_message` steering is text-only. Attachments cannot currently be added to
an already-running task.

### 1.6 Steer And Control

The task ID is placed in the run scope for:

- `cancel`
- `pause`
- `resume`
- `redirect`
- `inject_context`
- `approve`
- `reject`
- `prioritize`
- `user_message`

An HTTP success means a control was accepted for delivery, not that the
Runtime applied it. The UI must wait for `control.received`,
`control.applied`, `control.rejected`, and resulting task/pause events.

### 1.7 Intervention Recovery

On every attach and reconnect:

1. call `pause.list`;
2. subscribe to pause, approval, rejection, and tool-auth events;
3. join cause-specific events to a pause token;
4. approve/reject with the token and optional reason; and
5. clear a prompt only after event/snapshot reconciliation.

A parked run may remain `running` in task projections. `pause.list` is the
authoritative intervention snapshot.

### 1.8 One-Shot Run Overrides

`runs.set_overrides` currently supports:

- reasoning effort;
- temperature;
- max tokens;
- system-prompt override; and
- model.

The override is session-scoped and consumed by the next run. Model selection
cannot yet be rendered as a trustworthy picker because the Protocol lacks a
configured model/profile catalog.

## 2. Screen-To-Protocol Matrix

The current registry contains 116 canonical methods. This grouping accounts
for the complete surface while separating core TUI functions from advanced
administration.

### Connect And Runtime Status

Core:

- `runtime.info`
- `runtime.health`
- `runtime.counters`
- `runtime.drivers`
- `metrics.snapshot`
- `governance.posture`
- `llm.posture`

Advanced administration:

- `governance.get_tenant_overrides`
- `governance.set_tenant_overrides`
- `governance.rotate_key`
- `auth.rotate_token`

`auth.rotate_token` is canonical but not mounted by production `harbor serve`
or `sdk/server` unless an auth surface is explicitly composed.

### Conversation And Run Control

- `start`
- `cancel`
- `pause`
- `resume`
- `redirect`
- `inject_context`
- `approve`
- `reject`
- `prioritize`
- `user_message`

### Sessions, History, And Overrides

- `sessions.list`
- `sessions.inspect`
- `sessions.set_title`
- `sessions.delete`
- `state.history`
- `runs.set_overrides`

### Tasks And Interventions

- `tasks.list`
- `tasks.get`
- `pause.list`

### Events And Search

- `events.subscribe`
- `events.aggregate`
- `events.list`
- `search.query`
- `search.sessions`
- `search.tasks`
- `search.events`
- `search.artifacts`

### Artifacts

- `artifacts.list`
- `artifacts.put`
- `artifacts.get_ref`
- `artifacts.delete`

### Tools

- `tools.list`
- `tools.get`
- `tools.describe`
- `tools.metrics`
- `tools.content_stats`
- `tools.set_approval_policy`
- `tools.revoke_oauth`

When `runtime.info` advertises `tool_annotations`, production projections can
carry real OAuth state, approval policy, last-used time, error-rate metrics,
offloaded-content statistics, approval-policy writes, and OAuth revocation.
`aggregates_partial` indicates annotator unavailability, but not bounded
event-scan truncation. Busy-session analytics must be labelled best-effort.

### MCP Servers And Apps

- `mcp.servers.list`
- `mcp.servers.get`
- `mcp.servers.resources`
- `mcp.servers.prompts`
- `mcp.servers.refresh_discovery`
- `mcp.servers.probe`
- `mcp.servers.health`
- `mcp.servers.bindings.list`
- `mcp.servers.policy`
- `mcp.servers.refresh_binding`
- `mcp.servers.revoke_binding`
- `mcp.servers.set_raw_html_trust`
- `mcp.servers.read_resource`
- `mcp.apps.call_tool`
- `mcp.apps.tool_context`

Arbitrary MCP App HTML cannot be faithfully hosted in a portable terminal.
The TUI should show metadata, context, and source-safe summaries, and offer an
external browser action where appropriate.

### Agent Discovery And Lifecycle

Reads:

- `agents.list`
- `agents.get`
- `agents.tools`
- `agents.memory`
- `agents.governance`
- `agents.skills`
- `agents.permissions`
- `agents.metrics`

Advanced controls:

- `agents.pause`
- `agents.drain`
- `agents.restart`
- `agents.force_stop`
- `agents.deregister`

The Protocol can inspect agents but `StartRequest` cannot select an agent.

### Agent Configuration

Administrative revision/config methods:

- `agent_config.get`
- `agent_config.set_revision`
- `agent_config.list_revisions`
- `agent_config.diff`
- `agent_config.rollback`
- `agent_config.skills.list`
- `agent_config.skills.upsert`
- `agent_config.skills.delete`
- `agent_config.set_tool_exposure`
- `agent_config.set_prompt_layers`
- `agent_config.set_llm_params`
- `agent_config.add_mcp_connection`
- `agent_config.remove_mcp_connection`
- `agent_config.set_mcp_discovery_origins`
- `agent_config.set_oauth_provider`
- `agent_config.remove_oauth_provider`

Session-safe narrowing and personalization:

- `agent_config.session.set_user_prompt`
- `agent_config.session.set_source_disables`
- `agent_config.session.skills.list`
- `agent_config.session.skills.upsert`
- `agent_config.session.skills.delete`

Durable per-user variants:

- `agent_config.user.get`
- `agent_config.user.set_revision`
- `agent_config.user.list_revisions`
- `agent_config.user.diff`
- `agent_config.user.rollback`

### Memory

- `memory.list`
- `memory.get`
- `memory.health`
- `memory.strategy_trace`
- `memory.put`
- `memory.delete`

### Flows And Topology

- `flows.list`
- `flows.describe`
- `flows.runs.list`
- `flows.runs.describe`
- `flows.run`
- `flows.metrics`
- `topology.snapshot`

The current stock serve composition advertises topology as unavailable, so
this screen must be capability-gated.

## 3. Fidelity Gaps

### P0: Canonical Ordered Conversation Projection

Harbor has no wire type equivalent to a stable ordered list of:

- user messages;
- answer-text parts;
- reasoning parts;
- planner-decision parts;
- tool-call parts with stable IDs and lifecycle;
- artifact references;
- pause/intervention parts; and
- terminal result/error parts.

The Console currently owns a `ChatMessage`/`ChatToolCall`/`ReasoningStep`
reducer. A TUI would otherwise reimplement it. This risks divergent order,
grouping, replay behavior, and handling of future events.

Implementation options to investigate in a future phase plan:

1. keep flat canonical events but publish one reusable Go client reducer;
2. add a Protocol transcript/turn read model derived from events; or
3. add durable canonical conversation-part events and a paginated projection.

The TUI must not solve this with a private endpoint.

### P0: Durable User Turns

`state.history` omits user query text. The Console joins `tasks.list` rows by
run ID, which is paginated and not atomic with event retention. A canonical
redacted user-message event or transcript projection is needed for reliable
long-term rehydration.

### P0: Agent Selection

The Protocol exposes agent discovery but `StartRequest` has no `agent_id`,
planner selector, or flow selector. This blocks generic multi-agent testing,
although a generated single-agent binary can still provide a focused target.

### P1: Tool Call Detail

Current events expose tool name, transport, timing, attempts, and errors, but
not a stable generic call record carrying redacted typed input, result preview
or artifact reference, parallel grouping, planner-step parent, and replay ID.
The first TUI can render lifecycle cards, but not OpenCode-grade expandable
generic tool traces.

The `tool_annotations` capability narrows catalog-level posture gaps but does
not close this per-invocation replay/detail gap.

### P1: Tool Analytics Completeness

Production tool analytics scan a bounded event window, but `ToolMetrics` and
`ToolContentStats` have no truncation marker for that bound.
`aggregates_partial` only reports annotator availability. The TUI should label
these analytics best-effort or a future Protocol phase should add an honest
completeness marker.

### P1: Durable Follow-Up Queue

Queueing is currently client-local. The TUI must label it accordingly and
persist only local intent. A future server primitive would need idempotent
enqueue, list, cancel, promotion, and multi-client reconciliation.

### P1: Model Catalog

`runs.set_overrides` accepts a model, but the Protocol cannot enumerate
configured/allowed model profiles or their capabilities. A free-text field is
possible; a trustworthy OpenCode-style picker is not.

### P1: Generic Direct Tool Invocation

The tool catalog is inspectable, but no canonical `tools.invoke` exists.
Therefore a schema-generated tool lab must remain indirect through an agent or
flow until the Runtime exposes a policy-preserving invocation method.

### P1: Artifact Retrieval

`artifacts.get_ref` depends on a presigner. Default local/blob stores may
return `presign_unsupported`, and no authenticated byte-stream fallback
exists. This prevents portable preview/download of generated binary or heavy
artifacts.

### P1: Mid-Run Attachments

`start` accepts artifacts; `user_message` accepts text only. The TUI cannot add
a document or image to an active turn.

### P2: Capability Granularity

The handshake now includes `tool_annotations`, narrowing the tool-posture gap,
but it still does not advertise every shipped method family. The client must
combine capability checks with typed unknown-method/404/405/501 degradation.

### P2: Pause Deadline

`PauseSnapshot` exposes `paused_at` but no authoritative deadline/max-park
instant. The UI can show age, not an honest countdown.

### Deferred Product Features

No Protocol primitive currently supports session fork/branch, historical edit
and replay, regenerate-from-turn, share links, joined run comparison, or saved
evaluation cases. These are useful generic testing features, but not necessary
for the first operational TUI.

## 4. Local And Remote Identity Posture

Local co-launch is not a trust bypass. Every request still needs:

- a bearer JWT;
- verified tenant and user claims;
- a selected session; and
- a run ID for run-scoped control.

One token may drive multiple isolated sessions. The TUI must change the
session scope when switching conversations rather than treating session as a
global singleton.

`harbor serve` and scaffolded `sdk/server` binaries mint no token. The local
on-ramp is the same as production testing: configure a JWKS source and mint a
token using `harbor token`, or obtain one from an external identity provider.

Credentials should enter the TUI through a secure prompt, protected token
file, environment variable, or credential helper. They should not be written
to prompt history, debug logs, terminal titles, or ordinary preference files.

Owner-user controls are permitted for the caller's own run; cross-tenant or
privileged controls require effective scopes derived from the verified token.
Request bodies cannot grant scope.

## 5. Optional Serve Composition

Recommended process model:

```text
stock harbor serve or scaffolded agent binary
                  |
                  | authenticated Harbor Protocol: REST + SSE
                  v
       optional Bubble Tea TUI client
```

One TUI client implementation should support:

1. remote attach;
2. stock `harbor serve --tui`; and
3. scaffolded `--with-server --tui` binaries.

The latter two are orchestration conveniences only:

1. open the normal Protocol server;
2. wait for an authoritative bound address and readiness;
3. start the client with ordinary credentials;
4. define whether leaving the TUI leaves the server running or cancels the
   shared process; and
5. drain and restore the terminal on shutdown.

The TUI must never import Runtime internals to inspect the served handle,
access the event bus directly, read planner state, mount a debug endpoint, or
disable JWT validation on loopback.

The public `sdk/server.Handle` already has `Serve`, `Close`, and `BindAddr`.
An explicit ready/bound notification may be needed for clean ephemeral-port
co-launch without polling.

## 6. Implementation Priority

### First Usable Slice

- connect/compatibility screen;
- one session with text turn streaming;
- task terminal reconciliation;
- closed-session resume and erased-session remediation;
- pause/approval inbox;
- session list/switch/new/rename;
- artifact upload for initial turns;
- cancel/pause/resume/redirect/user-message controls;
- generic tool lifecycle cards;
- raw event diagnostics; and
- local prompt history/drafts/preferences.

### Fidelity Slice

- robust replay and dropped-event recovery;
- reasoning collapse and streaming cadence;
- task/result side panel and observed live cost without claiming durable task
  cost completeness;
- scoped retention horizons, `counters_partial`, and aggregate truncation;
- capability-gated tool annotations with bounded-analytics labelling;
- command palette, leader keys, and which-key overlay;
- responsive sidebar/overlay behavior;
- tool/result renderer registry;
- unknown-part fallback; and
- theme parity and golden terminal snapshots.

### Protocol-Gap Candidates

- canonical conversation projection;
- durable user turns and pending turns;
- agent selection on start;
- model profile discovery;
- generic policy-preserving tool invocation;
- portable artifact byte retrieval;
- mid-run artifact steering; and
- pause deadlines.

These are research outcomes, not authorization to add Protocol methods. Any
wire change requires its own RFC/phase process and a same-wave consumer.

## 7. Primary Sources

- `internal/protocol/methods/methods.go`
- `internal/protocol/types/control.go`
- `internal/protocol/types/tasks.go`
- `internal/protocol/types/sessions.go`
- `internal/protocol/types/state.go`
- `internal/protocol/types/pause.go`
- `internal/protocol/types/runs.go`
- `internal/protocol/types/tools.go`
- `internal/protocol/types/artifacts.go`
- `internal/protocol/types/agents.go`
- `internal/protocol/types/version.go`
- `internal/runtime/serve/serve.go`
- `internal/runtime/serve/mux.go`
- `sdk/server/server.go`
- `cmd/harbor/cmd_serve.go`
- `cmd/harbor/scaffold/templates/minimal-react/cmd_main.go.tmpl`
- `docs/site/protocol/methods.md`
- `docs/site/protocol/events.md`
- `docs/site/protocol/streaming-semantics.md`
- `docs/site/protocol/auth-and-identity.md`
- `docs/site/protocol/pause-model.md`
- `docs/skills/use-the-harbor-protocol/SKILL.md`
- `web/console/src/lib/sessions/history.ts`
- `web/console/src/lib/chat/types.ts`
- `web/console/src/routes/(console)/playground/[session_id]/+page.svelte`
