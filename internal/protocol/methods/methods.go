// Package methods is the single source of truth for Harbor Protocol
// method names (CLAUDE.md §8: "Method names live in
// internal/protocol/methods/methods.go. No hardcoded method strings
// elsewhere."). Other packages reference these constants; no Protocol
// method string is hardcoded outside this file. A single-source lint
// formalises this; the layout here lays the foundation so that lint
// is a no-op formalisation.
//
// # The set: the task control surface
//
// Harbor ships the ten canonical task-control method names (RFC §5.2
// "Task control" row): `start` plus the nine steering-control entries
// from the RFC §6.3 control taxonomy. `start` spawns a task; the nine
// controls map 1:1 onto the nine steering.ControlType values. Later
// Protocol surfaces (state snapshots, topology, artifacts, traces,
// metrics — RFC §5.2's other rows) add their method names here in their
// own phases.
//
// The wire strings are lowercase snake_case — `inject_context`,
// `user_message` — matching the RFC §5.2 table verbatim. They are NOT
// the uppercase steering.ControlType wire strings (`INJECT_CONTEXT`,
// `USER_MESSAGE`): the Protocol method name is the client-facing name,
// and the protocol.ControlSurface translates a method name into its
// steering.ControlType. Keeping the two namespaces distinct is
// deliberate — the Protocol surface owns its own method vocabulary
// ("the runtime owns the protocol it speaks").
//
// # The extension: the streaming-events method-name anchors
//
// The streaming-events extension elevates `events.subscribe` to a
// canonical method-name constant. The wire-transport route is still
// `GET /v1/events` (SSE), but the canonical method name is now the contract third-party
// Console implementations branch on — same pattern as the
// task-control nine. Harbor adds `events.aggregate`
// (`POST /v1/events/aggregate`) for time-bucketed event-type counts.
// `events.subscribe` and `events.aggregate` are streaming-events
// methods, NOT task-control methods: `IsControlMethod` returns false
// for both (the predicate stays exclusive to the steering-
// control nine) and `Methods()` returns the augmented sorted set with
// the new entries.
// and
//
// # The search cluster
//
// Harbor adds the five `search.*` methods used by the Console
// command palette and per-section search bars. `search.query` is the
// pure aggregator that fans out to the per-index methods
// (`search.sessions`, `search.tasks`, `search.events`,
// `search.artifacts`). The five methods are NOT control methods:
// `IsControlMethod` returns false for them (the steering inbox stays
// exclusive). A separate `IsSearchMethod` predicate lets transport
// adapters branch the route table. See
//
// # The posture cluster
//
// Harbor adds the five read-only `runtime.*` / `metrics.*` posture
// methods: `runtime.info` (build identity + version + uptime +
// capabilities), `runtime.health` (per-subsystem readiness rollup),
// `runtime.counters` (low-cardinality live counters), `runtime.drivers`
// (configured driver names per persistence-shaped subsystem), and
// `metrics.snapshot` (a Protocol-shaped projection over the
// MetricsRegistry). Harbor extends the cluster with the two config-
// posture methods the Console Settings page consumes: `governance.posture`
// (the read-only `IdentityTiers` view) and `llm.posture` (the
// bound LLM provider/model/region + `MockMode` flag). None of
// the seven methods are control or search methods — `IsControlMethod` /
// `IsSearchMethod` return false; a dedicated `IsPostureMethod` predicate
// routes them through the PostureSurface dispatcher, a sibling of the
// task-control surface, not an extension. They are read-only — no
// mutation counterpart ships at V1. See
//
// # The topology method
//
// Harbor adds `topology.snapshot` — the request-side surface that
// returns the Runtime engine's canonical TopologyProjection (static
// node graph + live per-edge queue depth). It is NOT a task-control
// method (`IsControlMethod` returns false), NOT a streaming-events
// method, and NOT a search method: `IsTopologyMethod` is its own
// O(1) predicate. The wire-transport route is the existing
// `POST /v1/control/{method}` REST surface. The paired in-flight
// surface — `topology.changed` — is a canonical EVENT, not a method,
// so it is not in the method registry. See
//
// # No registration escape hatch
//
// canonicalMethods is a fixed package-level map, not a write-once
// registry. The task-control set is closed; a new Protocol
// method is a new phase that declares a new constant + extends the map +
// (if reader-facing) updates the master plan / glossary — there is no
// RegisterMethod seam to drift through. This mirrors the steering
// taxonomy's fixed-enum posture.
package methods

import "sort"

// Method is the string-typed enum of canonical Harbor Protocol method
// names. The wire form is the lowercase snake_case string.
type Method string

// The ten canonical task-control method names (RFC §5.2 "Task control"
// row + RFC §6.3 control taxonomy) PLUS the two streaming-events method
// names landed in — `events.subscribe` and
// `events.aggregate` — the first non-task-control Protocol surface,
// PLUS the five `search.*` methods landed earlier.
const (
	// MethodStart asks the Runtime to spawn a new task / foreground run.
	// Maps onto tasks.TaskRegistry.Spawn.
	MethodStart Method = "start"
	// MethodCancel cancels a run (soft by default; `hard: true` in the
	// payload propagates a cancellation context). Maps onto the CANCEL
	// steering control.
	MethodCancel Method = "cancel"
	// MethodPause pauses a run at the next planner-step boundary. Maps
	// onto the PAUSE steering control; the run loop routes it through
	// the unified pauseresume.Coordinator.
	MethodPause Method = "pause"
	// MethodResume resumes a paused run. Maps onto the RESUME steering
	// control; the run loop routes it through pauseresume.Coordinator.
	MethodResume Method = "resume"
	// MethodRedirect rewrites a run's goal. Maps onto the REDIRECT
	// steering control; the new goal is the payload's `goal` string.
	MethodRedirect Method = "redirect"
	// MethodInjectContext appends operator-supplied context to a run's
	// trajectory, visible on the planner's next step. Maps onto the
	// INJECT_CONTEXT steering control.
	MethodInjectContext Method = "inject_context"
	// MethodApprove approves a HITL-gated step. Maps onto the APPROVE
	// steering control; the run loop advances the pause via
	// pauseresume.Coordinator.
	MethodApprove Method = "approve"
	// MethodReject rejects a HITL-gated step. Maps onto the REJECT
	// steering control; the run loop advances the pause and the run
	// terminates with Finish{ConstraintsConflict}.
	MethodReject Method = "reject"
	// MethodPrioritize changes a run's task priority. Maps onto the
	// PRIORITIZE steering control; the new priority is the payload's
	// `priority` number.
	MethodPrioritize Method = "prioritize"
	// MethodUserMessage injects a user-authored message into a run,
	// visible on the planner's next step. Maps onto the USER_MESSAGE
	// steering control; the message is the payload's `message` string.
	MethodUserMessage Method = "user_message"

	// MethodEventsSubscribe opens a server-filtered event subscription.
	// The wire-transport route is `GET /v1/events`
	// SSE; the canonical method name is the contract a
	// third-party Console branches on. Identity-mandatory; a request
	// with `?admin=1` (cross-tenant fan-in) requires the verified
	// `auth.ScopeAdmin` or `auth.ScopeConsoleFleet` scope claim.
	// The reject path returns the canonical
	// `errors.CodeIdentityScopeRequired` Code (HTTP 403). NOT a
	// task-control method — IsControlMethod returns false; the
	// control nine stays exclusive.
	MethodEventsSubscribe Method = "events.subscribe"

	// MethodEventsAggregate returns time-bucketed event-type counts
	// over a window. Powers the per-event-type
	// stacked-area sparkline on the Console Events page.
	// The wire-transport route is `POST /v1/events/aggregate`.
	// Identity-mandatory + cross-tenant scope rules apply (same
	// posture as MethodEventsSubscribe).
	//
	// NOT a control method: `IsControlMethod(MethodEventsAggregate)`
	// returns false.
	MethodEventsAggregate Method = "events.aggregate"

	// MethodSearchQuery — the Console palette
	// dispatcher. Pure aggregator: fans out concurrently to the
	// runtime-side per-index search methods, merges + paginates the
	// union. Carries no index of its own.
	MethodSearchQuery Method = "search.query"
	// MethodSearchSessions — Server-enforced session-index
	// search scoped to the caller's identity triple; cross-tenant
	// requires the `auth.ScopeAdmin` claim.
	MethodSearchSessions Method = "search.sessions"
	// MethodSearchTasks — Server-enforced task-index search;
	// same identity-scope contract as MethodSearchSessions.
	MethodSearchTasks Method = "search.tasks"
	// MethodSearchEvents — Server-enforced events-index
	// search (filters by event type + header fields + time window).
	// Reuses the EventFilter predicate. Substring search over
	// event payload contents is post-V1.
	MethodSearchEvents Method = "search.events"
	// MethodSearchArtifacts — Server-enforced artifact-index
	// search; rows always carry a `ref` (artifacts are by-reference by
	// construction).
	MethodSearchArtifacts Method = "search.artifacts"

	// MethodRuntimeInfo — Read-only posture method:
	// returns the Runtime's build identity (version / commit / Go
	// toolchain / build date), Protocol version, advertised
	// capabilities, uptime, instance ID, and operator-configured
	// display name. NOT a control method; dispatched by PostureSurface.
	MethodRuntimeInfo Method = "runtime.info"
	// MethodRuntimeHealth — Read-only posture method:
	// returns the per-subsystem readiness rollup (`ready` / `degraded`
	// / `unavailable`) across the runtime's registered subsystems.
	MethodRuntimeHealth Method = "runtime.health"
	// MethodRuntimeCounters — Read-only posture method:
	// returns the low-cardinality live counters the Console footer /
	// sidebar chips render (events/sec, tasks running, background jobs,
	// MCP connections, sessions active). Identity-scoped; the response
	// is the roll-up, never a per-run / per-task breakdown.
	MethodRuntimeCounters Method = "runtime.counters"
	// MethodRuntimeDrivers — Read-only posture method:
	// returns the configured driver names per persistence-shaped
	// subsystem (`state`, `artifacts`, `memory`, `eventlog`). Returns
	// the driver name + optional posture mode — never the DSN.
	MethodRuntimeDrivers Method = "runtime.drivers"
	// MethodMetricsSnapshot — Read-only posture method:
	// returns a Protocol-shaped projection over the
	// MetricsRegistry — counters, histograms, gauges as flat wire
	// values. NOT an OpenTelemetry SDK re-export.
	MethodMetricsSnapshot Method = "metrics.snapshot"

	// MethodGovernancePosture — Returns the
	// runtime's read-only governance configuration: the consolidated
	// `IdentityTiers` map (per-tier `BudgetCeilingUSD` + token-bucket
	// `RateLimit` + `MaxTokens`) plus the `DefaultTier` selector and the
	// caller-resolved tier. Identity-mandatory; cross-tenant reads
	// require the `auth.ScopeAdmin` claim. NOT a control method
	// and NOT a search method — it is a posture method (read-only
	// runtime-config projection). `IsControlMethod` / `IsSearchMethod`
	// both return false; `IsPostureMethod` returns true. See
	MethodGovernancePosture Method = "governance.posture"

	// MethodLLMPosture — Returns the
	// runtime's read-only LLM provider posture: provider name, model id,
	// region/endpoint, and a `MockMode` boolean — `true` iff the runtime
	// booted with `HARBOR_DEV_ALLOW_MOCK=1`. Identity-mandatory;
	// cross-tenant reads require the `auth.ScopeAdmin` claim. A
	// posture method, same posture as MethodGovernancePosture.
	MethodLLMPosture Method = "llm.posture"

	// MethodGovernanceSetTenantOverrides — admin verb: sets (or clears) a
	// tenant's default LLM-parameter overrides (model, additive
	// extra-instructions, temperature, max-tokens, reasoning-effort) live,
	// with no redeploy. The change is a desired-state REPLACE and lands on
	// every session in the tenant on its next run (the RFC §6.15
	// `ModelOverride` governance seam). Identity-mandatory; requires the
	// `auth.ScopeAdmin` claim (a non-admin caller is rejected with
	// CodeScopeMismatch). NOT a control / search / posture method —
	// `IsGovernanceAdminMethod` is its own O(1) predicate. The
	// wire-transport route is `POST /v1/governance/set_tenant_overrides`.
	MethodGovernanceSetTenantOverrides Method = "governance.set_tenant_overrides"

	// MethodGovernanceGetTenantOverrides — admin verb: reads a tenant's
	// current default LLM-parameter override record (the read-back the
	// Console renders before editing). Identity-mandatory; requires the
	// `auth.ScopeAdmin` claim. The wire-transport route is
	// `POST /v1/governance/get_tenant_overrides`.
	MethodGovernanceGetTenantOverrides Method = "governance.get_tenant_overrides"

	// MethodGovernanceRotateKey — admin verb: rotates the LLM provider
	// API key live, with no redeploy. The new key swaps atomically behind
	// the provider account so the next call uses it and the old key is
	// invalidated immediately. The key VALUE is a secret carried only on
	// the request leg — never logged, audited, or echoed; the response +
	// the `governance.key_rotated` event carry only a non-reversible
	// fingerprint. Identity-mandatory; requires the `auth.ScopeAdmin`
	// claim (a non-admin caller is rejected with CodeScopeMismatch). The
	// wire-transport route is `POST /v1/governance/rotate_key`.
	MethodGovernanceRotateKey Method = "governance.rotate_key"

	// MethodAgentConfigGet — reads an agent's current active config
	// revision (the read-back the Console renders before editing).
	// Identity-mandatory; requires the verified `auth.ScopeAdmin` claim
	// (reading the agent-config control plane is a privileged action).
	// The wire-transport route is `POST /v1/agent_config/get`.
	MethodAgentConfigGet Method = "agent_config.get"
	// MethodAgentConfigSetRevision — admin verb: writes a NEW immutable,
	// content-addressed config revision for an agent and advances the
	// active pointer. An idempotent re-set of byte-identical canonical
	// content is a no-op returning the existing revision. The change lands
	// on the agent's NEXT run (next-turn projection — never mid-flight).
	// Identity-mandatory; requires the `auth.ScopeAdmin` claim (a
	// non-admin caller is rejected with CodeScopeMismatch). The
	// wire-transport route is `POST /v1/agent_config/set_revision`.
	MethodAgentConfigSetRevision Method = "agent_config.set_revision"
	// MethodAgentConfigListRevisions — reads an agent's revision chain,
	// newest-first. Identity-mandatory; requires the `auth.ScopeAdmin`
	// claim. The wire-transport route is
	// `POST /v1/agent_config/list_revisions`.
	MethodAgentConfigListRevisions Method = "agent_config.list_revisions"
	// MethodAgentConfigDiff — server-side compare of two existing config
	// revisions (a structured set-diff for the structured sections).
	// Identity-mandatory; requires the `auth.ScopeAdmin` claim. The
	// wire-transport route is `POST /v1/agent_config/diff`.
	MethodAgentConfigDiff Method = "agent_config.diff"
	// MethodAgentConfigRollback — admin verb: repoints the active pointer
	// to an existing revision WITHOUT mutating or deleting any revision.
	// The repoint lands on the agent's NEXT run. Identity-mandatory;
	// requires the `auth.ScopeAdmin` claim. The wire-transport route is
	// `POST /v1/agent_config/rollback`.
	MethodAgentConfigRollback Method = "agent_config.rollback"
	// MethodAgentConfigSkillsList — reads the agent's skills (metadata
	// only) — the first consumer of the config-revision registry.
	// Identity-mandatory; requires the `auth.ScopeAdmin` claim. The
	// wire-transport route is `POST /v1/agent_config/skills/list`.
	MethodAgentConfigSkillsList Method = "agent_config.skills.list"
	// MethodAgentConfigSkillsUpsert — admin verb: upserts a skill into the
	// agent's store and records the membership change as a config
	// revision. A pack-origin skill is never silently overwritten — the
	// refusal surfaces as a typed Protocol error. Identity-mandatory;
	// requires the `auth.ScopeAdmin` claim. The wire-transport route is
	// `POST /v1/agent_config/skills/upsert`.
	MethodAgentConfigSkillsUpsert Method = "agent_config.skills.upsert"
	// MethodAgentConfigSkillsDelete — admin verb: deletes a skill from the
	// agent's store and records the membership change as a config
	// revision. Identity-mandatory; requires the `auth.ScopeAdmin` claim.
	// The wire-transport route is `POST /v1/agent_config/skills/delete`.
	MethodAgentConfigSkillsDelete Method = "agent_config.skills.delete"
	// MethodAgentConfigSetToolExposure — admin verb: sets the agent's
	// MCP-exposure / per-tool policy (paused servers + disabled tools) as a
	// desired-state replace of the tool-exposure section and records the
	// change as a config revision. Pausing a server excludes its tools from
	// the next run (the live transport stays warm) and rejects its App
	// callbacks against current state; emits `mcp.connection.paused` /
	// `.resumed`. Identity-mandatory; requires the `auth.ScopeAdmin` claim.
	// The wire-transport route is `POST /v1/agent_config/set_tool_exposure`.
	MethodAgentConfigSetToolExposure Method = "agent_config.set_tool_exposure"
	// MethodAgentConfigSetPromptLayers — admin verb: sets the agent's
	// layered system prompt (operator base and/or user layer) as a
	// desired-state replace of the prompt-layer section and records the
	// change as a config revision. The base is the run's base system prompt;
	// the user layer composes above it in the lower-trust guidance position
	// (it can extend but never replace or weaken the operator base). The
	// skills + tool-exposure sections are preserved. Applies to the agent's
	// next run. Identity-mandatory; requires the `auth.ScopeAdmin` claim.
	// The wire-transport route is `POST /v1/agent_config/set_prompt_layers`.
	MethodAgentConfigSetPromptLayers Method = "agent_config.set_prompt_layers"
	// MethodAgentConfigSetLLMParams — admin verb: sets the agent's per-agent
	// LLM-parameter section (model / temperature / max-tokens /
	// reasoning-effort) as a desired-state replace of the LLM-params section
	// and records the change as a config revision. The per-agent params
	// override the tenant-wide baseline (resolution session › per-agent ›
	// tenant-wide baseline › config default) for the agent's next run. A set
	// model is validated against the configured ModelProfiles (an unknown
	// model is rejected). The prompt-layer + skills + tool-exposure +
	// connection sections are preserved. Identity-mandatory; requires the
	// `auth.ScopeAdmin` claim. The wire-transport route is
	// `POST /v1/agent_config/set_llm_params`.
	MethodAgentConfigSetLLMParams Method = "agent_config.set_llm_params"
	// MethodAgentConfigAddMCPConnection — admin verb: adds a NEW MCP server
	// connection (the separable async-dial path). The runtime drives the real
	// attach lifecycle (dial → initialize handshake → discover → register),
	// records the non-secret descriptor as a config revision (preserving the
	// skills + tool-exposure + prompt-layer sections), and emits the
	// `mcp.connection.pending` then a terminal `mcp.connection.added` /
	// `.failed` / `.auth_required` lifecycle event. A failure records no
	// revision and never registers a half-attached server (fail loud). Adding
	// a stdio server (an RCE surface) is gated beyond admin on an operator
	// allowlist (fail-closed; argv-form only). An auth-required server parks
	// on the unified pause/resume primitive (the resume continuation that
	// re-drives the attach to online is not yet implemented — issue #375).
	// Identity-mandatory; requires the `auth.ScopeAdmin` claim. The
	// wire-transport route is `POST /v1/agent_config/add_mcp_connection`.
	MethodAgentConfigAddMCPConnection Method = "agent_config.add_mcp_connection"

	// MethodAgentConfigSessionSetUserPrompt — session-safe verb (the
	// non-admin lower tier): a session-scoped end user sets a user
	// prompt layer that composes ABOVE the operator base (it can extend the
	// operator's guidance but never precede, replace, or weaken the base —
	// the base is unwritable by a session caller). Identity-mandatory; does
	// NOT require the admin scope (a valid identity is enough). The
	// wire-transport route is `POST /v1/agent_config/session/set_user_prompt`.
	MethodAgentConfigSessionSetUserPrompt Method = "agent_config.session.set_user_prompt"
	// MethodAgentConfigSessionSetSourceDisables — session-safe verb: a
	// session-scoped end user NARROWS (never widens) source/tool enablement
	// by naming servers/tools to DISABLE within the admin-allowed set. The
	// disable set is unioned into the admin exclusion set at run start, so it
	// can only narrow the admin-allowed exposure — there is no enable path.
	// Identity-mandatory; does NOT require the admin scope. The wire-transport
	// route is `POST /v1/agent_config/session/set_source_disables`.
	MethodAgentConfigSessionSetSourceDisables Method = "agent_config.session.set_source_disables"
	// MethodAgentConfigSessionSkillsList — session-safe verb: lists the
	// session's ephemeral personal skills (metadata only) under the caller's
	// real triple. Identity-mandatory; does NOT require the admin scope. The
	// wire-transport route is
	// `POST /v1/agent_config/session/skills/list`.
	MethodAgentConfigSessionSkillsList Method = "agent_config.session.skills.list"
	// MethodAgentConfigSessionSkillsUpsert — session-safe verb: upserts an
	// ephemeral personal skill under the caller's real triple. The skill is
	// session-scoped and never promotes to the agent/tenant scope.
	// Identity-mandatory; does NOT require the admin scope. The wire-transport
	// route is `POST /v1/agent_config/session/skills/upsert`.
	MethodAgentConfigSessionSkillsUpsert Method = "agent_config.session.skills.upsert"
	// MethodAgentConfigSessionSkillsDelete — session-safe verb: deletes an
	// ephemeral personal skill under the caller's real triple.
	// Identity-mandatory; does NOT require the admin scope. The wire-transport
	// route is `POST /v1/agent_config/session/skills/delete`.
	MethodAgentConfigSessionSkillsDelete Method = "agent_config.session.skills.delete"

	// MethodAgentConfigUserGet — user-tier verb (the middle tier of the
	// authorization matrix): reads the caller's OWN durable, versioned
	// config variant — the active revision keyed under their real
	// (tenant, user). Identity-mandatory; requires the verified
	// `auth.ScopeAgentConfigUser` claim (NOT admin). The wire-transport route
	// is `POST /v1/agent_config/user/get`.
	MethodAgentConfigUserGet Method = "agent_config.user.get"
	// MethodAgentConfigUserSetRevision — user-tier verb: writes a NEW
	// immutable revision of the caller's durable config variant from a
	// structurally-bounded safe-subset payload (user prompt + narrow-only
	// disables + personal-skill names) — no base / connections / model, so a
	// user caller physically cannot widen a capability. Requires the verified
	// `auth.ScopeAgentConfigUser` claim. The wire-transport route is
	// `POST /v1/agent_config/user/set_revision`.
	MethodAgentConfigUserSetRevision Method = "agent_config.user.set_revision"
	// MethodAgentConfigUserListRevisions — user-tier verb: reads the caller's
	// own variant revision chain, newest-first. Requires the verified
	// `auth.ScopeAgentConfigUser` claim. The wire-transport route is
	// `POST /v1/agent_config/user/list_revisions`.
	MethodAgentConfigUserListRevisions Method = "agent_config.user.list_revisions"
	// MethodAgentConfigUserDiff — user-tier verb: server-side compare of two
	// existing revisions of the caller's own variant. Requires the verified
	// `auth.ScopeAgentConfigUser` claim. The wire-transport route is
	// `POST /v1/agent_config/user/diff`.
	MethodAgentConfigUserDiff Method = "agent_config.user.diff"
	// MethodAgentConfigUserRollback — user-tier verb: repoints the caller's
	// own variant active pointer to an existing revision WITHOUT mutating any
	// revision. Requires the verified `auth.ScopeAgentConfigUser` claim. The
	// wire-transport route is `POST /v1/agent_config/user/rollback`.
	MethodAgentConfigUserRollback Method = "agent_config.user.rollback"

	// MethodPauseList — the paginated,
	// identity-scope-filtered snapshot of currently-paused runs from
	// the unified pause/resume Coordinator. Read-only: it
	// does NOT mutate the registry and does NOT call Resume — resume
	// actions continue through MethodResume / MethodApprove /
	// MethodReject. The wire-transport route is
	// `POST /v1/pause/list`. Identity-mandatory; a cross-tenant filter
	// requires the verified `auth.ScopeAdmin` claim. NOT a
	// task-control method — `IsControlMethod(MethodPauseList)` returns
	// false; the control nine stays exclusive. See
	MethodPauseList Method = "pause.list"

	// MethodFlowsList — Returns the
	// paginated catalog of registered engine-graph flows with aggregate
	// run metrics (runs-in-window, p50/p95 latency, success rate, last
	// run, per-flow Budget). Identity-mandatory; a cross-tenant
	// filter requires the verified `auth.ScopeAdmin` claim. NOT a
	// task-control method — `IsFlowsMethod` returns true; `IsControlMethod`
	// returns false.
	MethodFlowsList Method = "flows.list"
	// MethodFlowsDescribe — Returns a single flow's full
	// engine-graph description: nodes + edges + per-node descriptor +
	// per-node policy + a string source reference (Go path or YAML
	// descriptor — never executable code) + live Budget
	// consumption. Identity-mandatory; an unknown flow id fails with
	// CodeNotFound.
	MethodFlowsDescribe Method = "flows.describe"
	// MethodFlowsRunsList — Returns a flow's paginated run
	// history (per-run status / trigger / timing / cost / identity).
	// Identity-mandatory; a cross-tenant filter requires the verified
	// `auth.ScopeAdmin` claim.
	MethodFlowsRunsList Method = "flows.runs.list"
	// MethodFlowsRunsDescribe — Returns a single flow run's
	// per-node execution timeline + final-output reference. Heavy outputs
	// are shipped by-reference via FlowArtifactRef — NEVER inline
	// bytes. Identity-mandatory; an unknown run id fails with
	// CodeNotFound.
	MethodFlowsRunsDescribe Method = "flows.runs.describe"
	// MethodFlowsRun — Invokes a one-shot run of a registered
	// flow. This is the ONLY mutating Flows-page method; it is gated on
	// identity AND the verified `auth.ScopeAdmin` claim (closed
	// scope set). A request without the claim is rejected with
	// CodeScopeMismatch (HTTP 403). Every other Flows-page method is
	// read-only.
	MethodFlowsRun Method = "flows.run"
	// MethodFlowsMetrics — Returns a flow's time-bucketed
	// sparkline aggregates (runs-per-bucket, p95 latency, success rate,
	// cost, budget consumption) over a window. Read-only; identity-
	// mandatory.
	MethodFlowsMetrics Method = "flows.metrics"

	// MethodTopologySnapshot — Returns the
	// canonical TopologyProjection of the Runtime's engine — the static
	// node graph + live per-edge queue depth. Request → reply (on-demand
	// cold-start surface); the paired in-flight surface is the
	// `topology.changed` canonical event. The wire-transport route is
	// the existing `POST /v1/control/{method}` REST surface. NOT a
	// task-control method (it reaches the engine's read-only Topology
	// accessor, not the steering inbox) and NOT a streaming-events or
	// search method — `IsControlMethod` / `IsStreamingEventsMethod` /
	// `IsSearchMethod` all return false; `IsTopologyMethod` returns
	// true. Identity-mandatory; a cross-tenant snapshot requires the
	// `auth.ScopeAdmin` claim.
	MethodTopologySnapshot Method = "topology.snapshot"

	// MethodArtifactsList — Returns the
	// identity-scope-filtered catalog of artifacts from the runtime's
	// content-addressed artifact store, with the filter
	// extensions (mime / source / size-range / created-range / tags)
	// applied as a Go-side projection. Identity-mandatory; a cross-tenant
	// list requires the `auth.ScopeAdmin` claim. The wire-transport
	// route is the existing `POST /v1/control/{method}` REST surface. NOT a
	// task-control, streaming-events, search, posture, pause, or topology
	// method — `IsArtifactsMethod` is its own O(1) predicate. See
	MethodArtifactsList Method = "artifacts.list"
	// MethodArtifactsPut — The Console (and
	// Playground) file-upload pipeline: accepts bytes +
	// PutOpts, routes the payload through `audit.Redactor`, stores it via
	// `artifacts.ArtifactStore.PutBytes`, and returns the canonical
	// ArtifactRef. Heavy bytes never travel inline through the LLM edge
	// the put returns a reference, never echoes the body.
	// Identity-mandatory; a body whose scope tenant disagrees with the
	// caller's verified tenant is rejected with CodeScopeMismatch.
	MethodArtifactsPut Method = "artifacts.put"
	// MethodArtifactsGetRef — The read-side
	// presigned-URL resolver: invokes `artifacts.Presigner.PresignGet` via
	// type-assertion on the underlying ArtifactStore. Drivers that do not
	// implement `Presigner` (in-mem / fs / sqlite-blob / postgres-blob)
	// return `CodePresignUnsupported` loudly — no silent fallback. The
	// Console's Preview / Download / Share / bulk-Download all route
	// through this single resolver. Identity-mandatory;
	// expiry is bounded [1m, 7d].
	MethodArtifactsGetRef Method = "artifacts.get_ref"

	// MethodArtifactsDelete — The admin-gated, audited
	// "evict an artifact" mutation: removes the keyed artifact from the
	// scope via the shipped `ArtifactStore.Delete` (idempotent). Requires
	// the verified `admin` scope claim (Delete
	// is a mutating verb, strictly more than the read scope); fails closed
	// without it. Emits `artifacts.deleted` for audit. Routes through the
	// control surface at `POST /v1/control/artifacts.delete`.
	MethodArtifactsDelete Method = "artifacts.delete"

	// MethodMemoryList — Returns the
	// paginated, identity-scope-filtered set of memory records the
	// Console Memory page renders. Read-only — it composes over the
	// shipped `MemoryStore.Snapshot` surface and the
	// `events.aggregate` counters. The wire-transport route
	// is `POST /v1/memory/list`. Identity-mandatory; a cross-tenant
	// filter requires the verified `auth.ScopeAdmin` (or
	// `auth.ScopeConsoleFleet`) claim from the closed two-scope
	// set — NO new memory scope is minted (audit B1). NOT a control /
	// search / posture / pause / topology method; `IsMemoryMethod` is
	// its own O(1) predicate. See
	MethodMemoryList Method = "memory.list"

	// MethodMemoryGet — Returns the full detail of a single
	// memory record: metadata + post-redaction value (below the
	// heavy-content threshold) OR a by-reference `MemoryArtifactRef`
	// (at or above the threshold) — NEVER inline bytes above threshold.
	// The wire-transport route is `POST /v1/memory/get`. Same identity-
	// scope contract as MethodMemoryList.
	MethodMemoryGet Method = "memory.get"

	// MethodMemoryHealth — Returns aggregate memory-health
	// counters (total records / expiring-in-1h / identity-rejected-24h
	// / recovery-dropped-24h) plus the per-scope driver mapping. The
	// 24-h-window counters derive from `events.aggregate` over the
	// `memory.*` event types. The wire-transport route is
	// `POST /v1/memory/health`. Same identity-scope contract as
	// MethodMemoryList.
	MethodMemoryHealth Method = "memory.health"

	// MethodMemoryStrategyTrace — Returns the live
	// read-only projection of how the configured memory strategy is
	// compacting the caller's session memory right now (the rolling-
	// summary text + the verbatim-turn count + the token estimate +
	// health) — the strategy's real `GetLLMContext` + `Health` output, not
	// a fabricated selection trace. The wire-transport route is
	// `POST /v1/memory/strategy_trace`. Same identity-scope contract as
	// MethodMemoryList (read scope; no admin claim).
	MethodMemoryStrategyTrace Method = "memory.strategy_trace"

	// MethodMemoryPut — The admin-gated, audited
	// "add a memory turn" mutation: appends an operator-supplied
	// conversation turn to the caller's session memory via the shipped
	// `MemoryStore.AddTurn`. Requires the verified `admin` scope claim;
	// fails closed without it. The wire-transport route is
	// `POST /v1/memory/put`.
	MethodMemoryPut Method = "memory.put"

	// MethodMemoryDelete — The admin-gated, audited
	// "evict a memory turn" mutation: removes the keyed turn from the
	// caller's session memory via a `Snapshot` → drop-turn → `Restore`
	// read-modify-write on the shipped MemoryStore. Requires the verified
	// `admin` scope claim; fails closed without it. The
	// wire-transport route is `POST /v1/memory/delete`.
	MethodMemoryDelete Method = "memory.delete"

	// The MCP-Connections-page method
	// cluster. Twelve `mcp.servers.*` methods — nine read methods and
	// three admin verbs — that back the Console MCP Connections page.
	// None are control / streaming-events / search / posture / pause /
	// topology methods: `IsMCPServersMethod` is the O(1) predicate, and
	// they route through the MCPSurface dispatcher. Identity-mandatory;
	// the three admin verbs gate on the `auth.ScopeAdmin` claim (the
	// closed-set — no new scope is minted for MCP). See

	// MethodMCPServersList — paged, filterable list of the configured
	// MCP southbound servers with live state.
	MethodMCPServersList Method = "mcp.servers.list"
	// MethodMCPServersGet — single-server detail read.
	MethodMCPServersGet Method = "mcp.servers.get"
	// MethodMCPServersResources — list of the resources a server
	// advertises.
	MethodMCPServersResources Method = "mcp.servers.resources"
	// MethodMCPServersPrompts — list of the prompts a server
	// advertises.
	MethodMCPServersPrompts Method = "mcp.servers.prompts"
	// MethodMCPServersRefreshDiscovery — control-plane verb: re-runs
	// the server's tools/resources/prompts discovery.
	MethodMCPServersRefreshDiscovery Method = "mcp.servers.refresh_discovery"
	// MethodMCPServersProbe — control-plane verb: runs a transport
	// ping / tools-list round-trip.
	MethodMCPServersProbe Method = "mcp.servers.probe"
	// MethodMCPServersHealth — handshake-latency sparkline + reconnect
	// history + transport-error rate.
	MethodMCPServersHealth Method = "mcp.servers.health"
	// MethodMCPServersBindingsList — list of a server's OAuth bindings
	// (metadata only — never token plaintext).
	MethodMCPServersBindingsList Method = "mcp.servers.bindings.list"
	// MethodMCPServersPolicy — read-only ToolPolicy projection.
	MethodMCPServersPolicy Method = "mcp.servers.policy"
	// MethodMCPServersRefreshBinding — admin verb: initiates an OAuth
	// (re)connect flow for a binding. Requires the `auth.ScopeAdmin`
	// claim.
	MethodMCPServersRefreshBinding Method = "mcp.servers.refresh_binding"
	// MethodMCPServersRevokeBinding — admin verb: revokes an OAuth
	// binding. Requires the `auth.ScopeAdmin` claim.
	MethodMCPServersRevokeBinding Method = "mcp.servers.revoke_binding"
	// MethodMCPServersSetRawHTMLTrust — admin verb: sets the per-server
	// raw-HTML opt-in flag and emits the `mcp.raw_html_trust_toggled`
	// audit event. Requires the `auth.ScopeAdmin` claim.
	MethodMCPServersSetRawHTMLTrust Method = "mcp.servers.set_raw_html_trust"

	// MethodMCPReadResource — the MCP Apps host's resource read: fetches
	// a `ui://` resource's HTML (an MCP App's UI document) scoped to the
	// request identity triple, heavy-content aware (content at or above
	// the heavy threshold rides by reference with a loud bypass event,
	// never inline). NOT a control method; routed through the MCP Apps
	// dispatcher (`IsMCPAppsMethod`). Identity-mandatory.
	MethodMCPReadResource Method = "mcp.servers.read_resource"
	// MethodMCPAppsCallTool — the MCP Apps host's app-tool-call proxy:
	// an in-iframe MCP App asks the host to invoke an MCP tool, and the
	// request re-enters the EXISTING identity + approval-gate + tool-side
	// -OAuth invocation path (no new bypass). An app call to a gated
	// tool parks on the unified pause primitive exactly as a planner call
	// does. NOT a control method; routed through the MCP Apps
	// dispatcher. Identity-mandatory.
	MethodMCPAppsCallTool Method = "mcp.apps.call_tool"
	// MethodMCPAppsToolContext — the MCP Apps host's tool-context read: a
	// rendered MCP App fetches the tool context (the input arguments + the
	// lowered result) that produced it, keyed by the server source id + the
	// stable tool-call id carried on the `mcp.app_available` discovery
	// event. The runtime captures the context at the tool-invocation site
	// (heavy-aware — a large result rides by reference); the read projects
	// it inline OR as an artifact reference. An unknown or cross-identity
	// id fails with CodeNotFound (existence is never revealed across
	// identities). NOT a control method; routed through the MCP Apps
	// dispatcher. Identity-mandatory.
	MethodMCPAppsToolContext Method = "mcp.apps.tool_context"

	// MethodToolsList — Returns the
	// catalog of registered tools visible to the caller's identity
	// scope, with optional facet filters (scope / transport / OAuth
	// status / approval policy / reliability tier) plus aggregate
	// counters (Total / Active / Pending approval / Awaiting OAuth) for
	// the filtered view. Powers the Console Tools page catalog table.
	// Identity-mandatory; a cross-tenant fan-in requires the
	// `auth.ScopeAdmin` claim. NOT a control / search /
	// posture / topology method — `IsToolsMethod` returns true. The
	// wire-transport route is `POST /v1/tools/list`. See
	MethodToolsList Method = "tools.list"
	// MethodToolsGet — Returns a single tool's catalog row
	// projection by ID. The lighter sibling of `tools.describe` — the
	// row shape the Console renders in the detail-panel header.
	MethodToolsGet Method = "tools.get"
	// MethodToolsDescribe — Returns the full manifest of a
	// registered tool descriptor: transport, version, scopes, the
	// argument / output JSON Schemas, examples, OAuth binding scope,
	// approval policy, and the reliability shell.
	// Powers the Tools-page Manifest / Inputs / Outputs tabs.
	MethodToolsDescribe Method = "tools.describe"
	// MethodToolsMetrics — Returns per-tool error-rate
	// gauges over a selectable window (1h / 24h / 7d) plus a status
	// pill (`Healthy` / `Degraded` / `Offline`). Powers the Tools-page
	// Status + Error-rate right-rail card.
	MethodToolsMetrics Method = "tools.metrics"
	// MethodToolsContentStats — Returns the per-tool
	// distribution of recent result sizes vs the heavy-content
	// threshold (RFC §6.5) plus the negotiated `DisplayMode`
	// snapshot. Powers the Tools-page Content-size card.
	MethodToolsContentStats Method = "tools.content_stats"
	// MethodToolsSetApprovalPolicy — ADMIN method: updates a
	// tool's approval policy. Requires the verified `auth.ScopeAdmin`
	// claim (there is NO `tools.admin` scope — the closed
	// two-scope set is the only admit surface). Emits an
	// `audit.admin_scope_used` event through the shipped audit.Redactor.
	MethodToolsSetApprovalPolicy Method = "tools.set_approval_policy"
	// MethodToolsRevokeOAuth — ADMIN method: revokes all
	// OAuth bindings for a tool. Requires the verified `auth.ScopeAdmin`
	// claim. Emits an `audit.admin_scope_used` event through
	// the shipped audit.Redactor.
	MethodToolsRevokeOAuth Method = "tools.revoke_oauth"

	// MethodTasksList — Returns the
	// paginated list of tasks visible to the caller's identity scope,
	// with optional facet filters (status / kind / parent-task /
	// identity / time-window / error-class / latency-above / free-text)
	// plus per-status aggregate counters (Pending / Running / Paused /
	// Failed / Complete / Cancelled) for the filtered view. Powers the
	// Console Tasks-page kanban board + list-mode table. Identity-
	// mandatory; a cross-tenant fan-in requires the `auth.ScopeAdmin`
	// claim. NOT a control / search / posture method —
	// `IsTasksMethod` returns true. The wire-transport route is
	// `POST /v1/tasks/list`. See
	MethodTasksList Method = "tasks.list"
	// MethodTasksGet — Returns the enriched detail of a
	// single task: the full Task projection (heavy values via
	// ArtifactRef), parent-session reference, parent-task
	// reference (when child), per-step cost rollup aggregated from
	// `llm.cost.recorded` events, and the planner-checkpoint reference
	// at spawn time. A cross-tenant TaskID lookup returns CodeNotFound
	// (existence is never revealed across tenants). The wire-transport
	// route is `POST /v1/tasks/get`.
	MethodTasksGet Method = "tasks.get"

	// MethodAgentsList — Returns the
	// catalog of agents registered under the caller's identity scope,
	// with optional facet filters (status / planner type / free-text
	// search) plus aggregate counters. Powers the Console Agents page
	// cards grid. Identity-mandatory; a cross-tenant fan-in requires the
	// `auth.ScopeAdmin` claim. `IsAgentsMethod` returns true.
	// The wire-transport route is `POST /v1/agents/list`.
	MethodAgentsList Method = "agents.list"
	// MethodAgentsGet — Returns one agent's full
	// registration-identity projection: agent_id / incarnation /
	// version_hash, hosting, status, health, and the
	// AgentConfig projection. Powers the Agents detail header + the
	// Identity / Autonomy tabs.
	MethodAgentsGet Method = "agents.get"
	// MethodAgentsTools — Returns the agent's tool bindings
	// joined to per-binding OAuth status. Powers the Tools tab.
	MethodAgentsTools Method = "agents.tools"
	// MethodAgentsMemory — Returns the agent's configured
	// memory strategy, TTL, and scope. Powers the Memory tab.
	MethodAgentsMemory Method = "agents.memory"
	// MethodAgentsGovernance — Returns the agent's
	// per-identity-tier cost ceilings + spend and rate-limit
	// posture. Powers the Cost tab.
	MethodAgentsGovernance Method = "agents.governance"
	// MethodAgentsSkills — Returns the agent's attached
	// skills (imported + generated skills). Powers the Skills
	// tab.
	MethodAgentsSkills Method = "agents.skills"
	// MethodAgentsPermissions — Returns the agent's
	// permission model — V1 default is implicit ("every authenticated
	// user in the tenant"); an explicit ACL surface is post-V1.
	MethodAgentsPermissions Method = "agents.permissions"
	// MethodAgentsMetrics — Returns the registry-wide rollup
	// (Active Agents / Running Tasks / Total Cost / Total Tokens) for
	// the caller's identity scope. Powers the Agents page hero numbers.
	MethodAgentsMetrics Method = "agents.metrics"

	// MethodAgentsPause — The fleet-control verb that
	// pauses an agent (stop accepting new tasks until Resume). Wraps the
	// shipped `registry.Pause` in-process control verb behind a Protocol
	// method so the Console can drive it. MUTATES registry state and
	// therefore requires the elevated `auth.ScopeAdmin` control claim;
	// emits `agent.paused`. `IsAgentsControlMethod` returns true.
	// Supersedes the earlier deferral.
	MethodAgentsPause Method = "agents.pause"
	// MethodAgentsDrain — Gracefully drains an agent (accept
	// no new tasks, finish existing). Wraps `registry.Drain`; emits
	// `agent.drained`. Control-scope gated.
	MethodAgentsDrain Method = "agents.drain"
	// MethodAgentsRestart — Restarts an agent (bump
	// incarnation; rehydrate from StateStore). Wraps `registry.Restart`;
	// emits `agent.restart_requested`. Control-scope gated.
	MethodAgentsRestart Method = "agents.restart"
	// MethodAgentsForceStop — Hard-stops an agent. Wraps
	// `registry.ForceStop`; emits `agent.force_stopped`. Control-scope
	// gated.
	MethodAgentsForceStop Method = "agents.force_stop"
	// MethodAgentsDeregister — Removes an agent from the
	// registry (irreversible). Wraps `registry.Deregister`; emits
	// `agent.deregistered`. Control-scope gated.
	MethodAgentsDeregister Method = "agents.deregister"

	// MethodAuthRotateToken — Rotates the
	// operator's current Protocol-auth token: the Runtime re-mints a
	// JWT for the caller's already-verified `(tenant, user, session)`
	// identity and returns it once (one-time-reveal). The ONLY net-new
	// Protocol method Harbor ships — the Console Settings page is
	// otherwise a pure consumer of the posture methods. ADMIN
	// method: requires the verified `auth.ScopeAdmin` claim (the
	// closed two-scope set — there is NO `auth.admin` scope). A request
	// without the claim is rejected 403 with CodeIdentityScopeRequired.
	// Every successful rotation emits a redacted `audit.admin_scope_used`
	// event. NOT a control / search / posture / pause / topology
	// method — `IsAuthMethod` is its own O(1) predicate. The
	// wire-transport route is `POST /v1/auth/rotate_token`. See
	MethodAuthRotateToken Method = "auth.rotate_token" //nolint:gosec // G101 false positive — this is a Protocol method name, not a credential.

	// MethodSessionsList — Returns the
	// paginated, identity-scope-filtered projection of the
	// SessionRegistry the Console Sessions page renders — the
	// past-and-active durable record of every Harbor execution the
	// operator has access to. Carries the full filter set (status /
	// agent / user / tenant / started-window / has-intervention /
	// has-failed-task / cost-above) plus cursor pagination. Read-only;
	// identity-mandatory. A cross-tenant filter (a `tenant_ids` entry
	// outside the caller's verified tenant) requires the verified
	// `auth.ScopeAdmin` claim (closed two-scope set — NO new
	// scope is minted). NOT a control / streaming-events / search /
	// posture / pause / topology / artifacts / memory / mcp / tools /
	// flows method — `IsSessionsMethod` is its own O(1) predicate. The
	// wire-transport route is `POST /v1/sessions/list`. See
	MethodSessionsList Method = "sessions.list"
	// MethodSessionsInspect — Returns the full per-session
	// snapshot the Console Sessions detail view renders — the
	// right-rail Session Summary projection (id / status / started /
	// duration / events / tasks / agent / user / tenant / cost / last
	// activity) plus the capped `recent_interventions` /
	// `recent_artifacts` slices. Read-only; identity-mandatory; same
	// cross-tenant scope contract as MethodSessionsList. The
	// wire-transport route is `POST /v1/sessions/inspect`.
	MethodSessionsInspect Method = "sessions.inspect"
	// MethodSessionsDelete — the identity-scoped data-lifecycle erasure
	// method: deletes a whole session and CASCADES deletion of its
	// scoped State, Memory, and Artifacts. Own-session-only — a caller
	// erases solely their own verified `(tenant, user, session)`; a body
	// identity mismatching the verified identity is rejected
	// `identity_required` before any further logic, and there is NO
	// admin / cross-tenant erasure path. A session with a RUNNING task is
	// refused fail-loud with the distinct `session_running` (409) code,
	// mirroring the GC never-reap-running invariant — no store is touched
	// on refusal. A successful erasure emits a redacted, content-free
	// `session.erased` audit event under the actor's observability scope
	// (never re-persisted under the erased triple). The mutating sibling
	// of `sessions.list` / `sessions.inspect`; it routes through the same
	// Sessions handler. Identity-mandatory. The wire-transport route is
	// `POST /v1/sessions/delete`.
	MethodSessionsDelete Method = "sessions.delete"

	// MethodRunsSetOverrides — Records the
	// reasoning-effort / temperature / max-tokens / system-prompt
	// override the Console Playground page applies to the NEXT message
	// in a session. The override is session-scoped (keyed by the
	// identity triple) and one-shot: it applies to the next
	// `user_message` / `start` and is consumed at that point — it does
	// NOT apply retroactively to past messages, and a session that sets
	// an override then never sends drops it. Identity-mandatory
	// (`session_id` required; tenant / user inferred from the verified
	// JWT); a cross-session override is rejected with CodeScopeMismatch.
	// NOT a control / streaming-events / search / posture / pause /
	// topology / artifacts / memory / mcp / tools / flows / agents /
	// sessions / tasks method — `IsRunsMethod` is its own O(1)
	// predicate. The wire-transport route is `POST /v1/runs/set_overrides`.
	MethodRunsSetOverrides Method = "runs.set_overrides"

	// MethodStateHistory — the State-snapshots surface's by-id,
	// identity-scoped, read-only TAIL-FIRST windowed read of a session's
	// durable event stream. Returns a bounded backward page of flat
	// `StateEvent` (events with `Sequence < before`, the most-recent K,
	// oldest-first within the window) plus the discovered head/tail
	// sequence and a scroll-up cursor; heavy content rides by a routable
	// `StateArtifactRef`, never inline. Reduction to chat messages stays
	// client-side. Identity-mandatory: an incomplete triple is rejected
	// with CodeIdentityRequired; an unknown or cross-identity session is
	// CodeNotFound (existence is never revealed across identities,
	// mirroring `tasks.get`); a cross-tenant read requires the verified
	// `auth.ScopeAdmin` claim on the ctx (the request body carries no
	// elevation knob). NOT a control / streaming-events / search /
	// posture / pause / topology / artifacts / memory / mcp / tools /
	// flows / agents / sessions / tasks / runs method — `IsStateMethod`
	// is its own O(1) predicate. The wire-transport route is
	// `POST /v1/state/history`.
	MethodStateHistory Method = "state.history"
)

// canonicalMethods is the registered set. It is a fixed package-level
// map (not a write-once registry) — the task-control set is
// closed; a new Protocol method is a new phase that extends this map.
// The map exists so IsValidMethod is O(1) and Methods returns a
// deterministic snapshot.
var canonicalMethods = map[Method]struct{}{
	MethodStart:                               {},
	MethodCancel:                              {},
	MethodPause:                               {},
	MethodResume:                              {},
	MethodRedirect:                            {},
	MethodInjectContext:                       {},
	MethodApprove:                             {},
	MethodReject:                              {},
	MethodPrioritize:                          {},
	MethodUserMessage:                         {},
	MethodEventsSubscribe:                     {},
	MethodEventsAggregate:                     {},
	MethodSearchQuery:                         {},
	MethodSearchSessions:                      {},
	MethodSearchTasks:                         {},
	MethodSearchEvents:                        {},
	MethodSearchArtifacts:                     {},
	MethodRuntimeInfo:                         {},
	MethodRuntimeHealth:                       {},
	MethodRuntimeCounters:                     {},
	MethodRuntimeDrivers:                      {},
	MethodMetricsSnapshot:                     {},
	MethodGovernancePosture:                   {},
	MethodLLMPosture:                          {},
	MethodGovernanceSetTenantOverrides:        {},
	MethodGovernanceGetTenantOverrides:        {},
	MethodGovernanceRotateKey:                 {},
	MethodAgentConfigGet:                      {},
	MethodAgentConfigSetRevision:              {},
	MethodAgentConfigListRevisions:            {},
	MethodAgentConfigDiff:                     {},
	MethodAgentConfigRollback:                 {},
	MethodAgentConfigSkillsList:               {},
	MethodAgentConfigSkillsUpsert:             {},
	MethodAgentConfigSkillsDelete:             {},
	MethodAgentConfigSetToolExposure:          {},
	MethodAgentConfigSetPromptLayers:          {},
	MethodAgentConfigSetLLMParams:             {},
	MethodAgentConfigAddMCPConnection:         {},
	MethodAgentConfigSessionSetUserPrompt:     {},
	MethodAgentConfigSessionSetSourceDisables: {},
	MethodAgentConfigSessionSkillsList:        {},
	MethodAgentConfigSessionSkillsUpsert:      {},
	MethodAgentConfigSessionSkillsDelete:      {},
	MethodAgentConfigUserGet:                  {},
	MethodAgentConfigUserSetRevision:          {},
	MethodAgentConfigUserListRevisions:        {},
	MethodAgentConfigUserDiff:                 {},
	MethodAgentConfigUserRollback:             {},
	MethodPauseList:                           {},
	MethodTopologySnapshot:                    {},
	MethodArtifactsList:                       {},
	MethodArtifactsPut:                        {},
	MethodArtifactsGetRef:                     {},
	MethodArtifactsDelete:                     {},
	MethodMemoryList:                          {},
	MethodMemoryGet:                           {},
	MethodMemoryHealth:                        {},
	MethodMemoryStrategyTrace:                 {},
	MethodMemoryPut:                           {},
	MethodMemoryDelete:                        {},

	MethodFlowsList:         {},
	MethodFlowsDescribe:     {},
	MethodFlowsRunsList:     {},
	MethodFlowsRunsDescribe: {},
	MethodFlowsRun:          {},
	MethodFlowsMetrics:      {},

	MethodToolsList:         {},
	MethodToolsGet:          {},
	MethodToolsDescribe:     {},
	MethodToolsMetrics:      {},
	MethodToolsContentStats: {},

	MethodToolsSetApprovalPolicy: {},
	MethodToolsRevokeOAuth:       {},

	MethodTasksList: {},
	MethodTasksGet:  {},

	MethodSessionsList:    {},
	MethodSessionsInspect: {},
	MethodSessionsDelete:  {},

	MethodRunsSetOverrides: {},

	MethodStateHistory: {},

	MethodAuthRotateToken: {},

	MethodMCPServersList:             {},
	MethodMCPServersGet:              {},
	MethodMCPServersResources:        {},
	MethodMCPServersPrompts:          {},
	MethodMCPServersRefreshDiscovery: {},
	MethodMCPServersProbe:            {},
	MethodMCPServersHealth:           {},
	MethodMCPServersBindingsList:     {},
	MethodMCPServersPolicy:           {},
	MethodMCPServersRefreshBinding:   {},
	MethodMCPServersRevokeBinding:    {},
	MethodMCPServersSetRawHTMLTrust:  {},

	MethodMCPReadResource:    {},
	MethodMCPAppsCallTool:    {},
	MethodMCPAppsToolContext: {},

	MethodAgentsList:        {},
	MethodAgentsGet:         {},
	MethodAgentsTools:       {},
	MethodAgentsMemory:      {},
	MethodAgentsGovernance:  {},
	MethodAgentsSkills:      {},
	MethodAgentsPermissions: {},
	MethodAgentsMetrics:     {},
	MethodAgentsPause:       {},
	MethodAgentsDrain:       {},
	MethodAgentsRestart:     {},
	MethodAgentsForceStop:   {},
	MethodAgentsDeregister:  {},
}

// canonicalAgentsMethods is the closed set of the thirteen `agents.*`
// methods: the eight read-only projections landed earlier
// plus the five fleet-control verbs (Pause / Drain / Restart /
// ForceStop / Deregister) landed earlier. IsAgentsMethod
// is O(1); the wire handler branches on it to route the request through
// the agents dispatcher instead of the task-control surface. The five
// control verbs wrap the shipped `registry.*` in-process control verbs
// behind a Protocol method — supersedes the earlier
// deferral that previously kept them off the Protocol surface.
var canonicalAgentsMethods = map[Method]struct{}{
	MethodAgentsList:        {},
	MethodAgentsGet:         {},
	MethodAgentsTools:       {},
	MethodAgentsMemory:      {},
	MethodAgentsGovernance:  {},
	MethodAgentsSkills:      {},
	MethodAgentsPermissions: {},
	MethodAgentsMetrics:     {},
	MethodAgentsPause:       {},
	MethodAgentsDrain:       {},
	MethodAgentsRestart:     {},
	MethodAgentsForceStop:   {},
	MethodAgentsDeregister:  {},
}

// canonicalAgentsControlMethods is the closed sub-set of the five
// `agents.*` fleet-control verbs that MUTATE registry state and
// therefore require the verified `auth.ScopeAdmin` control claim.
// The Agents wire handler uses this to gate
// the control path: a read method skips the scope check; a control
// method without the claim fails closed with CodeIdentityScopeRequired
// (HTTP 403). There is NO `agents.admin` scope — the closed two-scope
// set (`admin` + `console:fleet`) is the only admit surface.
var canonicalAgentsControlMethods = map[Method]struct{}{
	MethodAgentsPause:      {},
	MethodAgentsDrain:      {},
	MethodAgentsRestart:    {},
	MethodAgentsForceStop:  {},
	MethodAgentsDeregister: {},
}

// IsAgentsMethod reports whether m is one of the thirteen canonical
// `agents.*` methods (reads + control verbs). The wire
// handler branches on this to route the request through the agents
// dispatcher instead of the task-control / search / posture / topology
// surfaces.
func IsAgentsMethod(m Method) bool {
	_, ok := canonicalAgentsMethods[m]
	return ok
}

// IsAgentsControlMethod reports whether m is one of the five mutating
// `agents.*` fleet-control verbs (`agents.pause` / `agents.drain` /
// `agents.restart` / `agents.force_stop` / `agents.deregister`) that
// require the verified `auth.ScopeAdmin` control claim. The
// Agents wire handler uses this to decide whether to enforce the gate.
func IsAgentsControlMethod(m Method) bool {
	_, ok := canonicalAgentsControlMethods[m]
	return ok
}

// canonicalArtifactsMethods is the closed set of the artifacts methods —
// the read/put trio plus the
// admin `artifacts.delete` mutation. IsArtifactsMethod is
// O(1); the control transport branches on it to route the request
// through the artifacts dispatcher instead of the task-control surface.
var canonicalArtifactsMethods = map[Method]struct{}{
	MethodArtifactsList:   {},
	MethodArtifactsPut:    {},
	MethodArtifactsGetRef: {},
	MethodArtifactsDelete: {},
}

// IsArtifactsMethod reports whether m is one of the canonical artifacts
// methods (— `artifacts.list` / `artifacts.put` /
// `artifacts.get_ref` — plus the admin
// `artifacts.delete` mutation). The control transport branches on this to
// route the request through the artifacts dispatcher instead of the
// task-control / search / posture surfaces. NOT a control
// method — a new non-control method extends THIS predicate, never the
// steering inbox.
func IsArtifactsMethod(m Method) bool {
	_, ok := canonicalArtifactsMethods[m]
	return ok
}

// canonicalMCPServersMethods is the closed sub-set of the twelve
// `mcp.servers.*` methods landed earlier — nine
// read methods and three admin verbs. IsMCPServersMethod is O(1); the
// control transport branches on it to route the request through the
// MCPSurface dispatcher instead of the task-control surface.
var canonicalMCPServersMethods = map[Method]struct{}{
	MethodMCPServersList:             {},
	MethodMCPServersGet:              {},
	MethodMCPServersResources:        {},
	MethodMCPServersPrompts:          {},
	MethodMCPServersRefreshDiscovery: {},
	MethodMCPServersProbe:            {},
	MethodMCPServersHealth:           {},
	MethodMCPServersBindingsList:     {},
	MethodMCPServersPolicy:           {},
	MethodMCPServersRefreshBinding:   {},
	MethodMCPServersRevokeBinding:    {},
	MethodMCPServersSetRawHTMLTrust:  {},
}

// canonicalMCPAdminMethods is the closed sub-set of the three
// `mcp.servers.*` admin verbs that gate on the
// `auth.ScopeAdmin` claim. IsMCPAdminMethod is O(1); the MCPSurface
// dispatcher uses it to apply the admin-scope gate.
var canonicalMCPAdminMethods = map[Method]struct{}{
	MethodMCPServersRefreshBinding:  {},
	MethodMCPServersRevokeBinding:   {},
	MethodMCPServersSetRawHTMLTrust: {},
}

// IsMCPServersMethod reports whether m is one of the twelve canonical
// `mcp.servers.*` methods. The control transport
// branches on this to route the request through the MCPSurface
// dispatcher instead of the task-control / search / posture surfaces.
// NOT a control method — a new non-control method extends THIS
// predicate, never the steering inbox.
func IsMCPServersMethod(m Method) bool {
	_, ok := canonicalMCPServersMethods[m]
	return ok
}

// IsMCPAdminMethod reports whether m is one of the three `mcp.servers.*`
// admin verbs (`refresh_binding` / `revoke_binding` /
// `set_raw_html_trust`) that gate on the `auth.ScopeAdmin` claim.
// The MCPSurface dispatcher uses it to apply the admin gate.
func IsMCPAdminMethod(m Method) bool {
	_, ok := canonicalMCPAdminMethods[m]
	return ok
}

// canonicalGovernanceAdminMethods is the closed set of the admin-scoped
// `governance.*` mutation/read verbs that gate on the `auth.ScopeAdmin`
// claim (distinct from the read-only `governance.posture` posture method).
// IsGovernanceAdminMethod is O(1); the governance wire handler uses it to
// apply the admin-scope gate.
var canonicalGovernanceAdminMethods = map[Method]struct{}{
	MethodGovernanceSetTenantOverrides: {},
	MethodGovernanceGetTenantOverrides: {},
	MethodGovernanceRotateKey:          {},
}

// IsGovernanceAdminMethod reports whether m is one of the admin-scoped
// `governance.*` tenant-override verbs (`set_tenant_overrides` /
// `get_tenant_overrides`). The governance wire handler uses it to apply
// the admin gate. NOT a posture method — `governance.posture` is read-only
// and routed through the posture surface.
func IsGovernanceAdminMethod(m Method) bool {
	_, ok := canonicalGovernanceAdminMethods[m]
	return ok
}

// canonicalAgentConfigMethods is the closed set of the twenty-two
// `agent_config.*` methods — the five registry verbs (get / set_revision /
// list_revisions / diff / rollback), the three skills-control verbs
// (skills.list / skills.upsert / skills.delete), the MCP-exposure verb
// (set_tool_exposure), the layered-prompt verb (set_prompt_layers), the
// per-agent LLM-params verb (set_llm_params), the add-connection verb
// (add_mcp_connection), the five session safe-subset verbs, and the five
// user-tier verbs (user.get / user.set_revision / user.list_revisions /
// user.diff / user.rollback). IsAgentConfigMethod is O(1); the
// agent-config wire handler branches on the trailing path segment to
// dispatch.
var canonicalAgentConfigMethods = map[Method]struct{}{
	MethodAgentConfigGet:              {},
	MethodAgentConfigSetRevision:      {},
	MethodAgentConfigListRevisions:    {},
	MethodAgentConfigDiff:             {},
	MethodAgentConfigRollback:         {},
	MethodAgentConfigSkillsList:       {},
	MethodAgentConfigSkillsUpsert:     {},
	MethodAgentConfigSkillsDelete:     {},
	MethodAgentConfigSetToolExposure:  {},
	MethodAgentConfigSetPromptLayers:  {},
	MethodAgentConfigSetLLMParams:     {},
	MethodAgentConfigAddMCPConnection: {},
	// Session-user safe subset (the non-admin lower tier).
	MethodAgentConfigSessionSetUserPrompt:     {},
	MethodAgentConfigSessionSetSourceDisables: {},
	MethodAgentConfigSessionSkillsList:        {},
	MethodAgentConfigSessionSkillsUpsert:      {},
	MethodAgentConfigSessionSkillsDelete:      {},
	// User tier (the durable per-user variant — the middle tier).
	MethodAgentConfigUserGet:           {},
	MethodAgentConfigUserSetRevision:   {},
	MethodAgentConfigUserListRevisions: {},
	MethodAgentConfigUserDiff:          {},
	MethodAgentConfigUserRollback:      {},
}

// canonicalAgentConfigUserMethods is the closed sub-set of the five
// `agent_config.user.*` verbs — the durable per-user config-variant tier
// (the middle tier of the authorization matrix). A caller is permitted on
// these verbs iff they carry the verified `auth.ScopeAgentConfigUser` claim
// (NOT admin — an admin token does not implicitly own per-user variants).
// IsAgentConfigUserMethod is O(1); the agent-config wire handler uses it to
// apply the user-tier scope gate. Authority derives from the verified ctx,
// never the request body.
var canonicalAgentConfigUserMethods = map[Method]struct{}{
	MethodAgentConfigUserGet:           {},
	MethodAgentConfigUserSetRevision:   {},
	MethodAgentConfigUserListRevisions: {},
	MethodAgentConfigUserDiff:          {},
	MethodAgentConfigUserRollback:      {},
}

// canonicalAgentConfigSessionMethods is the closed sub-set of the
// `agent_config.session.*` SAFE-SUBSET methods — the non-admin lower tier of
// the authorization matrix. A session-scoped (non-admin) caller is permitted
// on EXACTLY these verbs: set the user prompt layer, narrow-only source/tool
// disable, and ephemeral personal-skill upsert/delete/list. Every other
// `agent_config.*` method is admin-gated. These verbs require a valid
// identity but NOT the admin scope; authority derives from the verified ctx,
// never the request body.
var canonicalAgentConfigSessionMethods = map[Method]struct{}{
	MethodAgentConfigSessionSetUserPrompt:     {},
	MethodAgentConfigSessionSetSourceDisables: {},
	MethodAgentConfigSessionSkillsList:        {},
	MethodAgentConfigSessionSkillsUpsert:      {},
	MethodAgentConfigSessionSkillsDelete:      {},
}

// canonicalAgentConfigAdminMethods is the closed sub-set of the
// `agent_config.*` methods that mutate the agent-config control plane and
// gate on the verified `auth.ScopeAdmin` claim (the agent-config authorization model). Reads
// (`agent_config.get` / `list_revisions` / `diff` / `skills.list`) are
// also admin-gated at the wire handler (the whole family is admin-scoped),
// but this set names the WRITES for callers that distinguish them.
var canonicalAgentConfigAdminMethods = map[Method]struct{}{
	MethodAgentConfigSetRevision:      {},
	MethodAgentConfigRollback:         {},
	MethodAgentConfigSkillsUpsert:     {},
	MethodAgentConfigSkillsDelete:     {},
	MethodAgentConfigSetToolExposure:  {},
	MethodAgentConfigSetPromptLayers:  {},
	MethodAgentConfigSetLLMParams:     {},
	MethodAgentConfigAddMCPConnection: {},
}

// IsAgentConfigMethod reports whether m is one of the twenty-two
// `agent_config.*` methods. The control transport / wire handler branches
// on this to route the request through the agent-config dispatcher
// instead of the task-control surface. NOT a control method — a new
// agent-config method extends THIS predicate, never the steering inbox.
func IsAgentConfigMethod(m Method) bool {
	_, ok := canonicalAgentConfigMethods[m]
	return ok
}

// IsAgentConfigUserMethod reports whether m is one of the five
// `agent_config.user.*` verbs — the durable per-user config-variant tier
// (the middle tier of the authorization matrix). The agent-config wire
// handler uses it to gate those routes on the verified
// `auth.ScopeAgentConfigUser` claim (NOT admin). A user method is always an
// agent-config method and never a session safe-subset or admin method.
func IsAgentConfigUserMethod(m Method) bool {
	_, ok := canonicalAgentConfigUserMethods[m]
	return ok
}

// IsAgentConfigAdminMethod reports whether m is one of the
// `agent_config.*` capability-mutation verbs that gate on the verified
// `auth.ScopeAdmin` claim (the agent-config authorization model).
func IsAgentConfigAdminMethod(m Method) bool {
	_, ok := canonicalAgentConfigAdminMethods[m]
	return ok
}

// IsAgentConfigSessionMethod reports whether m is one of the
// `agent_config.session.*` safe-subset verbs — the non-admin lower tier of
// the authorization matrix. A session-scoped (non-admin) caller is permitted
// on these verbs (and ONLY these among the agent-config family); every other
// `agent_config.*` method requires the admin scope.
func IsAgentConfigSessionMethod(m Method) bool {
	_, ok := canonicalAgentConfigSessionMethods[m]
	return ok
}

// canonicalMCPAppsMethods is the closed set of the three MCP Apps host
// methods — the `mcp.servers.read_resource` `ui://` document fetch, the
// `mcp.apps.call_tool` app-tool-call proxy, and the
// `mcp.apps.tool_context` tool-context read. All are identity-mandatory
// reads/proxies routed through the AppsSurface dispatcher, distinct from
// the twelve `mcp.servers.*` Console methods. IsMCPAppsMethod is O(1); the
// control transport branches on it.
var canonicalMCPAppsMethods = map[Method]struct{}{
	MethodMCPReadResource:    {},
	MethodMCPAppsCallTool:    {},
	MethodMCPAppsToolContext: {},
}

// IsMCPAppsMethod reports whether m is one of the three MCP Apps host
// methods (`mcp.servers.read_resource` / `mcp.apps.call_tool` /
// `mcp.apps.tool_context`). The control transport branches on this to
// route the request through the AppsSurface dispatcher instead of the
// task-control / Console-MCP surfaces. NOT a control method — a new MCP
// Apps method extends THIS predicate, never the steering inbox.
func IsMCPAppsMethod(m Method) bool {
	_, ok := canonicalMCPAppsMethods[m]
	return ok
}

// canonicalToolsMethods is the closed sub-set of the seven `tools.*`
// methods landed earlier — the five read
// methods (`tools.list` / `tools.get` / `tools.describe` /
// `tools.metrics` / `tools.content_stats`) plus the two admin methods
// (`tools.set_approval_policy` / `tools.revoke_oauth`). IsToolsMethod
// is O(1); a transport adapter branches on it to route the request
// through the Tools surface instead of the task-control surface.
var canonicalToolsMethods = map[Method]struct{}{
	MethodToolsList:              {},
	MethodToolsGet:               {},
	MethodToolsDescribe:          {},
	MethodToolsMetrics:           {},
	MethodToolsContentStats:      {},
	MethodToolsSetApprovalPolicy: {},
	MethodToolsRevokeOAuth:       {},
}

// canonicalToolsAdminMethods is the closed sub-set of the two `tools.*`
// methods that MUTATE runtime tool state and therefore require the
// verified `auth.ScopeAdmin` claim. The Tools wire handler
// uses this to gate the admin path: a read method skips the scope
// check; an admin method without the claim fails closed with
// CodeIdentityScopeRequired (HTTP 403). There is NO `tools.admin`
// scope — the closed two-scope set (`admin` + `console:fleet`) is the
// only admit surface.
var canonicalToolsAdminMethods = map[Method]struct{}{
	MethodToolsSetApprovalPolicy: {},
	MethodToolsRevokeOAuth:       {},
}

// IsToolsMethod reports whether m is one of the seven canonical
// `tools.*` methods. The control transport
// branches on this to route the request through the Tools dispatcher
// instead of the task-control / search / posture / topology surfaces.
// NOT a control method — a new non-control method extends THIS
// predicate, never the steering inbox.
func IsToolsMethod(m Method) bool {
	_, ok := canonicalToolsMethods[m]
	return ok
}

// IsToolsAdminMethod reports whether m is one of the two mutating
// `tools.*` methods (`tools.set_approval_policy` / `tools.revoke_oauth`)
// that require the verified `auth.ScopeAdmin` claim. The Tools
// wire handler uses this to decide whether to enforce the scope gate.
func IsToolsAdminMethod(m Method) bool {
	_, ok := canonicalToolsAdminMethods[m]
	return ok
}

// canonicalTasksMethods is the closed sub-set of the two `tasks.*`
// methods landed earlier — `tasks.list` and
// `tasks.get`. Both are READ-ONLY: the Console Tasks page consumes the
// existing task-control verbs for mutation, never a new
// `tasks.*` mutating method. IsTasksMethod is O(1); the stream
// transport branches on it to route the request through the Tasks
// dispatcher instead of the task-control surface.
var canonicalTasksMethods = map[Method]struct{}{
	MethodTasksList: {},
	MethodTasksGet:  {},
}

// IsTasksMethod reports whether m is one of the two canonical `tasks.*`
// methods (— `tasks.list`, `tasks.get`). The stream
// transport branches on this to route the request through the Tasks
// dispatcher instead of the task-control / search / posture / topology
// surfaces. NOT a control method — both are reads; the Console Tasks
// page consumes the shipped control verbs for mutation.
func IsTasksMethod(m Method) bool {
	_, ok := canonicalTasksMethods[m]
	return ok
}

// canonicalFlowsMethods is the closed sub-set of the six Flows-page
// methods landed earlier. IsFlowsMethod is O(1);
// a transport adapter uses it to branch the request onto the Flows
// dispatcher instead of the task-control / search / posture surfaces.
var canonicalFlowsMethods = map[Method]struct{}{
	MethodFlowsList:         {},
	MethodFlowsDescribe:     {},
	MethodFlowsRunsList:     {},
	MethodFlowsRunsDescribe: {},
	MethodFlowsRun:          {},
	MethodFlowsMetrics:      {},
}

// IsFlowsMethod reports whether m is one of the six Flows-page methods.
// Five are read-only; `flows.run` is the single
// mutating method. The Flows-page surface is distinct from the steering
// inbox, the streaming-events surface, the search cluster, and the
// posture surface — a transport adapter branches on this predicate to
// route the request through the Flows dispatcher.
func IsFlowsMethod(m Method) bool {
	_, ok := canonicalFlowsMethods[m]
	return ok
}

// canonicalSessionsMethods is the closed sub-set of the three Sessions-
// page methods — the two read methods `sessions.list` and
// `sessions.inspect`, plus the mutating `sessions.delete` data-lifecycle
// erasure verb. IsSessionsMethod is O(1); the stream transport branches
// on it to route the request through the Sessions handler instead of the
// task-control surface. The first two are read-only; `sessions.delete`
// mutates (it gates its own own-session-only erasure at the handler /
// service edge).
var canonicalSessionsMethods = map[Method]struct{}{
	MethodSessionsList:    {},
	MethodSessionsInspect: {},
	MethodSessionsDelete:  {},
}

// IsSessionsMethod reports whether m is one of the three canonical
// Sessions-page methods (— `sessions.list`, `sessions.inspect`,
// `sessions.delete`). The stream transport branches on this to route
// the request through the Sessions handler instead of the task-control
// / search / posture / pause / topology / artifacts / memory / mcp /
// tools / flows surfaces. NOT a control method — a new non-control
// method extends THIS predicate, never the steering inbox.
func IsSessionsMethod(m Method) bool {
	_, ok := canonicalSessionsMethods[m]
	return ok
}

// canonicalRunsMethods is the closed sub-set of the Console Playground-
// page `runs.*` methods landed earlier. Today it
// holds the single `runs.set_overrides` method — the next-message
// reasoning-effort / temperature / max-tokens / system-prompt override
// recorder. IsRunsMethod is O(1); the stream transport branches on it to
// route the request through the Runs handler instead of the task-control
// surface.
var canonicalRunsMethods = map[Method]struct{}{
	MethodRunsSetOverrides: {},
}

// IsRunsMethod reports whether m is one of the canonical `runs.*`
// methods (— today just `runs.set_overrides`). The
// stream transport branches on this to route the request through the
// Runs handler instead of the task-control / search / posture / pause /
// topology / artifacts / memory / mcp / tools / flows / agents /
// sessions surfaces. NOT a control method — a new non-control method
// extends THIS predicate, never the steering inbox.
func IsRunsMethod(m Method) bool {
	_, ok := canonicalRunsMethods[m]
	return ok
}

// canonicalStateMethods is the closed sub-set of the State-snapshots
// `state.*` methods. Today it holds the single `state.history` windowed
// event-replay read; `state.list_trajectories` and
// `state.load_planner_checkpoint` (the other two RFC §5.2 State-snapshots
// methods) land in their own phases and extend this set. IsStateMethod is
// O(1); the stream transport branches on it to route the request through
// the state-history handler instead of the task-control surface.
var canonicalStateMethods = map[Method]struct{}{
	MethodStateHistory: {},
}

// IsStateMethod reports whether m is one of the canonical State-snapshots
// `state.*` methods (— today just `state.history`). The stream transport
// branches on this to route the request through the state-history handler
// instead of the task-control / search / posture / pause / topology /
// artifacts / memory / mcp / tools / flows / agents / sessions / runs
// surfaces. NOT a control method — a new non-control state method extends
// THIS predicate, never the steering inbox.
func IsStateMethod(m Method) bool {
	_, ok := canonicalStateMethods[m]
	return ok
}

// canonicalAuthMethods is the closed sub-set of the `auth.*` methods
// landed earlier. Today it holds the single
// `auth.rotate_token` method. IsAuthMethod is O(1); the stream
// transport branches on it to route the request through the auth
// handler instead of the task-control surface.
var canonicalAuthMethods = map[Method]struct{}{
	MethodAuthRotateToken: {},
}

// IsAuthMethod reports whether m is one of the canonical `auth.*`
// methods (— today just `auth.rotate_token`). The
// stream transport branches on this to route the request through the
// auth handler instead of the task-control / search / posture
// surfaces. NOT a control method — a new non-control method extends
// THIS predicate, never the steering inbox.
func IsAuthMethod(m Method) bool {
	_, ok := canonicalAuthMethods[m]
	return ok
}

// canonicalSearchMethods is the closed sub-set of the five search.*
// methods. IsSearchMethod is O(1); a transport adapter (
// search handler) uses it to branch the route table.
var canonicalSearchMethods = map[Method]struct{}{
	MethodSearchQuery:     {},
	MethodSearchSessions:  {},
	MethodSearchTasks:     {},
	MethodSearchEvents:    {},
	MethodSearchArtifacts: {},
}

// IsSearchMethod reports whether m is one of the five canonical
// `search.*` methods. The control transport branches on this
// to route the request through the search dispatcher instead of the
// task-control surface.
func IsSearchMethod(m Method) bool {
	_, ok := canonicalSearchMethods[m]
	return ok
}

// canonicalPostureMethods is the closed sub-set of the seven posture
// methods — the five `runtime.*` / `metrics.*` reads
// plus the two `governance.posture` / `llm.posture`
// reads. IsPostureMethod is O(1); a transport adapter uses it to branch
// the route table onto the PostureSurface instead of the task-control
// ControlSurface.
var canonicalPostureMethods = map[Method]struct{}{
	MethodRuntimeInfo:       {},
	MethodRuntimeHealth:     {},
	MethodRuntimeCounters:   {},
	MethodRuntimeDrivers:    {},
	MethodMetricsSnapshot:   {},
	MethodGovernancePosture: {},
	MethodLLMPosture:        {},
}

// IsPostureMethod reports whether m is one of the seven read-only
// posture methods — the five `runtime.*` / `metrics.*` reads
// and the two `governance.posture` / `llm.posture` config
// reads. The PostureSurface uses this to branch in
// transport adapters that want a single route table over all canonical
// methods.
func IsPostureMethod(m Method) bool {
	_, ok := canonicalPostureMethods[m]
	return ok
}

// canonicalTopologyMethods is the closed sub-set of the topology-
// projection methods landed earlier. Today it
// holds the single `topology.snapshot` method; the paired
// `topology.changed` surface is an EVENT, not a method, so it is not
// in this set. IsTopologyMethod is O(1); the control transport uses
// it to branch the request onto the topology dispatcher.
var canonicalTopologyMethods = map[Method]struct{}{
	MethodTopologySnapshot: {},
}

// IsTopologyMethod reports whether m is one of the canonical
// topology-projection methods (— today just
// `topology.snapshot`). The control transport branches on this to
// route the request through the topology dispatcher instead of the
// task-control / search surfaces. NOT a control method — a new
// non-control method extends THIS predicate, never the steering inbox.
func IsTopologyMethod(m Method) bool {
	_, ok := canonicalTopologyMethods[m]
	return ok
}

// canonicalMemoryMethods is the closed set of `memory.*` methods that
// route through the memory stream handlers: the three
// read methods (`memory.list` / `memory.get` / `memory.health`),
// the read `memory.strategy_trace`, and the
// admin-gated mutation pair (`memory.put` / `memory.delete`).
// IsMemoryMethod is O(1); the transport adapter uses it to branch the
// request through the memory handlers instead of the task-control
// surface. The mutation methods gate on the verified `admin` scope claim
// at the handler edge — the predicate only governs routing.
var canonicalMemoryMethods = map[Method]struct{}{
	MethodMemoryList:          {},
	MethodMemoryGet:           {},
	MethodMemoryHealth:        {},
	MethodMemoryStrategyTrace: {},
	MethodMemoryPut:           {},
	MethodMemoryDelete:        {},
}

// IsMemoryMethod reports whether m is one of the canonical `memory.*`
// methods — the read trio plus the
// `memory.strategy_trace` read + `memory.put` / `memory.delete`
// admin mutation pair. The control transport branches on this to route the
// request through the memory handlers instead of the task-control / search
// / posture / pause / topology surfaces. NOT a control method — a new
// non-control memory method extends THIS predicate, never the steering
// inbox; the mutation methods gate on `admin` at the handler edge.
func IsMemoryMethod(m Method) bool {
	_, ok := canonicalMemoryMethods[m]
	return ok
}

// streamingEventsMethods is the closed set of canonical method names
// the Runtime classifies as streaming-events methods (the first non-
// task-control Protocol surface to land — / 72a). Used by
// IsControlMethod to keep its predicate exclusive to the
// nine, and by IsStreamingEventsMethod (below) when a caller wants
// the inverse predicate.
var streamingEventsMethods = map[Method]struct{}{
	MethodEventsSubscribe: {},
	MethodEventsAggregate: {},
}

// IsValidMethod reports whether m is one of the canonical Protocol
// method names — the task-control ten plus the
// streaming-events additions plus the search cluster.
func IsValidMethod(m Method) bool {
	_, ok := canonicalMethods[m]
	return ok
}

// IsPauseMethod reports whether m is one of the pause-snapshot methods
// landed in (currently only MethodPauseList). The
// pause-snapshot surface is a read-only projection over the unified
// pause/resume Coordinator; it is NOT a steering control,
// NOT a streaming-events method, and NOT a search method. A transport
// adapter branches on this predicate to route the request through the
// pause-list snapshot handler instead of the task-control surface.
func IsPauseMethod(m Method) bool {
	_, ok := pauseMethods[m]
	return ok
}

// pauseMethods is the closed set of canonical pause-snapshot method
// names. Used by IsControlMethod to keep its predicate
// exclusive to the nine, and by IsPauseMethod for the inverse.
var pauseMethods = map[Method]struct{}{
	MethodPauseList: {},
}

// IsControlMethod reports whether m is one of the nine steering-control
// methods — every canonical method except MethodStart AND the
// streaming-events methods AND the
// `search.*` cluster AND the `runtime.*` / `metrics.*`
// posture cluster AND the pause-snapshot method AND the
// `topology.snapshot` method AND the `memory.*`
// read cluster (each a separate surface from the steering inbox).
// The protocol.ControlSurface uses this to branch: a control method
// maps onto a steering.ControlEvent; MethodStart maps onto the task
// registry; a streaming-events method routes through the SSE /
// events-aggregate transport; a search method maps onto the
// search dispatcher; a posture method maps onto the
// PostureSurface; a pause-snapshot method maps onto the
// pause-list handler; a topology method maps onto the
// topology dispatcher. A new non-control method (state inspection,
// artifacts — future phases) extends THIS predicate, NOT
// the steering-control inbox.
func IsControlMethod(m Method) bool {
	if !IsValidMethod(m) {
		return false
	}
	if m == MethodStart {
		return false
	}
	if _, ok := streamingEventsMethods[m]; ok {
		return false
	}
	if IsSearchMethod(m) {
		return false
	}
	if IsPostureMethod(m) {
		return false
	}
	if IsPauseMethod(m) {
		return false
	}
	if IsTopologyMethod(m) {
		return false
	}
	if IsArtifactsMethod(m) {
		return false
	}
	if IsMemoryMethod(m) {
		return false
	}
	if IsMCPServersMethod(m) {
		return false
	}
	if IsMCPAppsMethod(m) {
		return false
	}
	if IsTasksMethod(m) {
		return false
	}
	if IsToolsMethod(m) {
		return false
	}
	if IsFlowsMethod(m) {
		return false
	}
	if IsAgentsMethod(m) {
		return false
	}
	if IsSessionsMethod(m) {
		return false
	}
	if IsRunsMethod(m) {
		return false
	}
	if IsStateMethod(m) {
		return false
	}
	if IsGovernanceAdminMethod(m) {
		return false
	}
	if IsAgentConfigMethod(m) {
		return false
	}
	if IsAuthMethod(m) {
		return false
	}
	return true
}

// IsStreamingEventsMethod reports whether m is one of the streaming-
// events methods landed in (MethodEventsSubscribe or
// MethodEventsAggregate). The transport-side router uses this to
// classify a method into its routing branch without re-listing the
// streaming set.
func IsStreamingEventsMethod(m Method) bool {
	_, ok := streamingEventsMethods[m]
	return ok
}

// Methods returns a deterministic, lexicographically-sorted snapshot of
// every canonical method name (the task-control ten + the
// streaming-events additions + the search cluster).
// Useful for exhaustiveness tests and for a transport adapter's route
// table.
func Methods() []Method {
	out := make([]Method, 0, len(canonicalMethods))
	for m := range canonicalMethods {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
