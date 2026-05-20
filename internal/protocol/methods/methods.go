// Package methods is the single source of truth for Harbor Protocol
// method names (CLAUDE.md §8: "Method names live in
// internal/protocol/methods/methods.go. No hardcoded method strings
// elsewhere."). Other packages reference these constants; no Protocol
// method string is hardcoded outside this file. The Phase 58 lint
// formalises this — Phase 54 lays the foundation so that lint is a no-op
// formalisation.
//
// # The Phase 54 set: the task control surface
//
// Phase 54 ships the ten canonical task-control method names (RFC §5.2
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
// (brief 07's "the runtime owns the protocol it speaks").
//
// # The Wave 13 extension: the streaming-events method-name anchors
//
// Phase 72 elevates `events.subscribe` to a canonical method-name
// constant. The wire-transport route is still `GET /v1/events` (Phase
// 60 SSE), but the canonical method name is now the contract third-party
// Console implementations branch on — same pattern as the Phase 54
// task-control nine. Phase 72a adds `events.aggregate`
// (`POST /v1/events/aggregate`) for time-bucketed event-type counts.
// `events.subscribe` and `events.aggregate` are streaming-events
// methods, NOT task-control methods: `IsControlMethod` returns false
// for both (the predicate stays exclusive to the Phase 54 steering-
// control nine) and `Methods()` returns the augmented sorted set with
// the new entries. See `docs/plans/phase-72-console-subscription-scope.md`
// and `docs/plans/phase-72a-events-filter-and-aggregate.md`.
//
// # The Wave 13 search cluster (Phase 72c / D-108)
//
// Phase 72c adds the five `search.*` methods used by the Console
// command palette and per-section search bars. `search.query` is the
// pure aggregator that fans out to the per-index methods
// (`search.sessions`, `search.tasks`, `search.events`,
// `search.artifacts`). The five methods are NOT control methods:
// `IsControlMethod` returns false for them (the steering inbox stays
// exclusive). A separate `IsSearchMethod` predicate lets transport
// adapters branch the route table. See
// `docs/plans/phase-72c-search-cluster.md`.
//
// # The Wave 13 posture cluster (Phase 72f / D-111 + Phase 72g / D-112)
//
// Phase 72f adds the five read-only `runtime.*` / `metrics.*` posture
// methods: `runtime.info` (build identity + version + uptime +
// capabilities), `runtime.health` (per-subsystem readiness rollup),
// `runtime.counters` (low-cardinality live counters), `runtime.drivers`
// (configured driver names per persistence-shaped subsystem), and
// `metrics.snapshot` (a Protocol-shaped projection over the Phase 56
// MetricsRegistry). Phase 72g extends the cluster with the two config-
// posture methods the Console Settings page consumes: `governance.posture`
// (the read-only D-081 `IdentityTiers` view) and `llm.posture` (the
// bound LLM provider/model/region + `MockMode` flag per D-089). None of
// the seven methods are control or search methods — `IsControlMethod` /
// `IsSearchMethod` return false; a dedicated `IsPostureMethod` predicate
// routes them through the PostureSurface dispatcher, a sibling of the
// task-control surface, not an extension. They are read-only — no
// mutation counterpart ships at V1. See
// `docs/plans/phase-72f-runtime-posture.md` and
// `docs/plans/phase-72g-governance-llm-posture.md`.
//
// # The Wave 13 topology method (Phase 74 / D-114)
//
// Phase 74 adds `topology.snapshot` — the request-side surface that
// returns the Runtime engine's canonical TopologyProjection (static
// node graph + live per-edge queue depth). It is NOT a task-control
// method (`IsControlMethod` returns false), NOT a streaming-events
// method, and NOT a search method: `IsTopologyMethod` is its own
// O(1) predicate. The wire-transport route is the existing
// `POST /v1/control/{method}` REST surface. The paired in-flight
// surface — `topology.changed` — is a canonical EVENT, not a method,
// so it is not in the method registry. See
// `docs/plans/phase-74-console-topology.md`.
//
// # No registration escape hatch
//
// canonicalMethods is a fixed package-level map, not a write-once
// registry. The Phase 54 task-control set is closed; a new Protocol
// method is a new phase that declares a new constant + extends the map +
// (if reader-facing) updates the master plan / glossary — there is no
// RegisterMethod seam to drift through. This mirrors the steering
// taxonomy's fixed-enum posture (D-070 §2).
package methods

import "sort"

// Method is the string-typed enum of canonical Harbor Protocol method
// names. The wire form is the lowercase snake_case string.
type Method string

// The ten canonical task-control method names (RFC §5.2 "Task control"
// row + RFC §6.3 control taxonomy) PLUS the two streaming-events method
// names landed in Wave 13 (Phase 72 / 72a) — `events.subscribe` and
// `events.aggregate` — the first non-task-control Protocol surface,
// PLUS the five `search.*` methods landed in Phase 72c (D-108).
const (
	// MethodStart asks the Runtime to spawn a new task / foreground run.
	// Maps onto tasks.TaskRegistry.Spawn (Phase 20).
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

	// MethodEventsSubscribe opens a server-filtered event subscription
	// (Phase 72 / D-105). The wire-transport route is `GET /v1/events`
	// SSE (Phase 60); the canonical method name is the contract a
	// third-party Console branches on. Identity-mandatory; a request
	// with `?admin=1` (cross-tenant fan-in) requires the verified
	// `auth.ScopeAdmin` or `auth.ScopeConsoleFleet` scope claim
	// (D-079). The reject path returns the canonical
	// `errors.CodeIdentityScopeRequired` Code (HTTP 403). NOT a
	// task-control method — IsControlMethod returns false; the Phase
	// 54 control nine stays exclusive.
	MethodEventsSubscribe Method = "events.subscribe"

	// MethodEventsAggregate returns time-bucketed event-type counts
	// over a window (Phase 72a / D-106). Powers the per-event-type
	// stacked-area sparkline on the Console Events page (Phase 73g).
	// The wire-transport route is `POST /v1/events/aggregate`.
	// Identity-mandatory + D-079 cross-tenant scope rules apply (same
	// posture as MethodEventsSubscribe).
	//
	// NOT a control method: `IsControlMethod(MethodEventsAggregate)`
	// returns false.
	MethodEventsAggregate Method = "events.aggregate"

	// MethodSearchQuery — Phase 72c (Wave 13) the Console palette
	// dispatcher. Pure aggregator: fans out concurrently to the
	// runtime-side per-index search methods, merges + paginates the
	// union. Carries no index of its own. See `docs/plans/phase-72c-search-cluster.md`.
	MethodSearchQuery Method = "search.query"
	// MethodSearchSessions — Phase 72c. Server-enforced session-index
	// search scoped to the caller's identity triple; cross-tenant
	// requires the `auth.ScopeAdmin` claim (D-079).
	MethodSearchSessions Method = "search.sessions"
	// MethodSearchTasks — Phase 72c. Server-enforced task-index search;
	// same identity-scope contract as MethodSearchSessions.
	MethodSearchTasks Method = "search.tasks"
	// MethodSearchEvents — Phase 72c. Server-enforced events-index
	// search (filters by event type + header fields + time window).
	// Reuses the Phase 72a EventFilter predicate. Substring search over
	// event payload contents is post-V1.
	MethodSearchEvents Method = "search.events"
	// MethodSearchArtifacts — Phase 72c. Server-enforced artifact-index
	// search; rows always carry a `ref` (artifacts are by-reference by
	// construction per D-026).
	MethodSearchArtifacts Method = "search.artifacts"

	// MethodRuntimeInfo — Phase 72f (D-111). Read-only posture method:
	// returns the Runtime's build identity (version / commit / Go
	// toolchain / build date), Protocol version, advertised
	// capabilities, uptime, instance ID, and operator-configured
	// display name. NOT a control method; dispatched by PostureSurface.
	MethodRuntimeInfo Method = "runtime.info"
	// MethodRuntimeHealth — Phase 72f. Read-only posture method:
	// returns the per-subsystem readiness rollup (`ready` / `degraded`
	// / `unavailable`) across the runtime's registered subsystems.
	MethodRuntimeHealth Method = "runtime.health"
	// MethodRuntimeCounters — Phase 72f. Read-only posture method:
	// returns the low-cardinality live counters the Console footer /
	// sidebar chips render (events/sec, tasks running, background jobs,
	// MCP connections, sessions active). Identity-scoped; the response
	// is the roll-up, never a per-run / per-task breakdown.
	MethodRuntimeCounters Method = "runtime.counters"
	// MethodRuntimeDrivers — Phase 72f. Read-only posture method:
	// returns the configured driver names per persistence-shaped
	// subsystem (`state`, `artifacts`, `memory`, `eventlog`). Returns
	// the driver name + optional posture mode — never the DSN.
	MethodRuntimeDrivers Method = "runtime.drivers"
	// MethodMetricsSnapshot — Phase 72f. Read-only posture method:
	// returns a Protocol-shaped projection over the Phase 56
	// MetricsRegistry — counters, histograms, gauges as flat wire
	// values. NOT an OpenTelemetry SDK re-export.
	MethodMetricsSnapshot Method = "metrics.snapshot"

	// MethodGovernancePosture — Phase 72g (Wave 13; D-112). Returns the
	// runtime's read-only governance configuration: the D-081
	// `IdentityTiers` map (per-tier `BudgetCeilingUSD` + token-bucket
	// `RateLimit` + `MaxTokens`) plus the `DefaultTier` selector and the
	// caller-resolved tier. Identity-mandatory; cross-tenant reads
	// require the `auth.ScopeAdmin` claim (D-079). NOT a control method
	// and NOT a search method — it is a posture method (read-only
	// runtime-config projection). `IsControlMethod` / `IsSearchMethod`
	// both return false; `IsPostureMethod` returns true. See
	// `docs/plans/phase-72g-governance-llm-posture.md`.
	MethodGovernancePosture Method = "governance.posture"

	// MethodLLMPosture — Phase 72g (Wave 13; D-112). Returns the
	// runtime's read-only LLM provider posture: provider name, model id,
	// region/endpoint, and a `MockMode` boolean — `true` iff the runtime
	// booted with `HARBOR_DEV_ALLOW_MOCK=1` (D-089). Identity-mandatory;
	// cross-tenant reads require the `auth.ScopeAdmin` claim (D-079). A
	// posture method, same posture as MethodGovernancePosture.
	MethodLLMPosture Method = "llm.posture"

	// MethodPauseList — Phase 72e (Wave 13; D-110) the paginated,
	// identity-scope-filtered snapshot of currently-paused runs from
	// the unified pause/resume Coordinator (Phase 50). Read-only: it
	// does NOT mutate the registry and does NOT call Resume — resume
	// actions continue through MethodResume / MethodApprove /
	// MethodReject. The wire-transport route is
	// `POST /v1/pause/list`. Identity-mandatory; a cross-tenant filter
	// requires the verified `auth.ScopeAdmin` claim (D-079). NOT a
	// task-control method — `IsControlMethod(MethodPauseList)` returns
	// false; the Phase 54 control nine stays exclusive. See
	// `docs/plans/phase-72e-pause-list-snapshot.md`.
	MethodPauseList Method = "pause.list"

	// MethodFlowsList — Phase 73i (Wave 13 / D-117). Returns the
	// paginated catalog of registered engine-graph flows with aggregate
	// run metrics (runs-in-window, p50/p95 latency, success rate, last
	// run, per-flow Budget per D-023). Identity-mandatory; a cross-tenant
	// filter requires the verified `auth.ScopeAdmin` claim (D-079). NOT a
	// task-control method — `IsFlowsMethod` returns true; `IsControlMethod`
	// returns false. See `docs/plans/phase-73i-console-flows-page.md`.
	MethodFlowsList Method = "flows.list"
	// MethodFlowsDescribe — Phase 73i. Returns a single flow's full
	// engine-graph description: nodes + edges + per-node descriptor +
	// per-node policy + a string source reference (Go path or YAML
	// descriptor per D-023 — never executable code) + live Budget
	// consumption. Identity-mandatory; an unknown flow id fails with
	// CodeNotFound.
	MethodFlowsDescribe Method = "flows.describe"
	// MethodFlowsRunsList — Phase 73i. Returns a flow's paginated run
	// history (per-run status / trigger / timing / cost / identity).
	// Identity-mandatory; a cross-tenant filter requires the verified
	// `auth.ScopeAdmin` claim (D-079).
	MethodFlowsRunsList Method = "flows.runs.list"
	// MethodFlowsRunsDescribe — Phase 73i. Returns a single flow run's
	// per-node execution timeline + final-output reference. Heavy outputs
	// are shipped by-reference via FlowArtifactRef (D-026) — NEVER inline
	// bytes. Identity-mandatory; an unknown run id fails with
	// CodeNotFound.
	MethodFlowsRunsDescribe Method = "flows.runs.describe"
	// MethodFlowsRun — Phase 73i. Invokes a one-shot run of a registered
	// flow. This is the ONLY mutating Flows-page method; it is gated on
	// identity AND the verified `auth.ScopeAdmin` claim (D-079 closed
	// scope set). A request without the claim is rejected with
	// CodeScopeMismatch (HTTP 403). Every other Flows-page method is
	// read-only.
	MethodFlowsRun Method = "flows.run"
	// MethodFlowsMetrics — Phase 73i. Returns a flow's time-bucketed
	// sparkline aggregates (runs-per-bucket, p95 latency, success rate,
	// cost, budget consumption) over a window. Read-only; identity-
	// mandatory.
	MethodFlowsMetrics Method = "flows.metrics"

	// MethodTopologySnapshot — Phase 74 (Wave 13 / D-114). Returns the
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
	// `auth.ScopeAdmin` claim (D-079). See `docs/plans/phase-74-console-topology.md`.
	MethodTopologySnapshot Method = "topology.snapshot"

	// MethodArtifactsList — Phase 73l (Wave 13 / D-120). Returns the
	// identity-scope-filtered catalog of artifacts from the runtime's
	// content-addressed artifact store, with the Phase 73l filter
	// extensions (mime / source / size-range / created-range / tags)
	// applied as a Go-side projection. Identity-mandatory; a cross-tenant
	// list requires the `auth.ScopeAdmin` claim (D-079). The wire-transport
	// route is the existing `POST /v1/control/{method}` REST surface. NOT a
	// task-control, streaming-events, search, posture, pause, or topology
	// method — `IsArtifactsMethod` is its own O(1) predicate. See
	// `docs/plans/phase-73l-console-artifacts-page.md`.
	MethodArtifactsList Method = "artifacts.list"
	// MethodArtifactsPut — Phase 73l (Wave 13 / D-120). The Console (and
	// Playground) file-upload pipeline per Brief 11 §PG-2: accepts bytes +
	// PutOpts, routes the payload through `audit.Redactor`, stores it via
	// `artifacts.ArtifactStore.PutBytes`, and returns the canonical
	// ArtifactRef. Heavy bytes never travel inline through the LLM edge
	// (D-026) — the put returns a reference, never echoes the body.
	// Identity-mandatory; a body whose scope tenant disagrees with the
	// caller's verified tenant is rejected with CodeScopeMismatch.
	MethodArtifactsPut Method = "artifacts.put"
	// MethodArtifactsGetRef — Phase 73l (Wave 13 / D-120). The read-side
	// presigned-URL resolver: invokes `artifacts.Presigner.PresignGet` via
	// type-assertion on the underlying ArtifactStore. Drivers that do not
	// implement `Presigner` (in-mem / fs / sqlite-blob / postgres-blob)
	// return `CodePresignUnsupported` loudly — no silent fallback. The
	// Console's Preview / Download / Share / bulk-Download all route
	// through this single resolver per D-022 / D-026. Identity-mandatory;
	// expiry is bounded [1m, 7d].
	MethodArtifactsGetRef Method = "artifacts.get_ref"

	// MethodMemoryList — Phase 73j (Wave 13 / D-118). Returns the
	// paginated, identity-scope-filtered set of memory records the
	// Console Memory page renders. Read-only — it composes over the
	// shipped `MemoryStore.Snapshot` surface (Phases 23–25) and the
	// `events.aggregate` counters (Phase 72a). The wire-transport route
	// is `POST /v1/memory/list`. Identity-mandatory; a cross-tenant
	// filter requires the verified `auth.ScopeAdmin` (or
	// `auth.ScopeConsoleFleet`) claim from the D-079 closed two-scope
	// set — NO new memory scope is minted (audit B1). NOT a control /
	// search / posture / pause / topology method; `IsMemoryMethod` is
	// its own O(1) predicate. See
	// `docs/plans/phase-73j-console-memory-page.md`.
	MethodMemoryList Method = "memory.list"

	// MethodMemoryGet — Phase 73j. Returns the full detail of a single
	// memory record: metadata + post-redaction value (below the D-026
	// heavy-content threshold) OR a by-reference `MemoryArtifactRef`
	// (at or above the threshold) — NEVER inline bytes above threshold.
	// The wire-transport route is `POST /v1/memory/get`. Same identity-
	// scope contract as MethodMemoryList.
	MethodMemoryGet Method = "memory.get"

	// MethodMemoryHealth — Phase 73j. Returns aggregate memory-health
	// counters (total records / expiring-in-1h / identity-rejected-24h
	// / recovery-dropped-24h) plus the per-scope driver mapping. The
	// 24-h-window counters derive from `events.aggregate` over the
	// `memory.*` event types. The wire-transport route is
	// `POST /v1/memory/health`. Same identity-scope contract as
	// MethodMemoryList.
	MethodMemoryHealth Method = "memory.health"

	// The Wave 13 (Phase 73k / D-119) MCP-Connections-page method
	// cluster. Twelve `mcp.servers.*` methods — nine read methods and
	// three admin verbs — that back the Console MCP Connections page.
	// None are control / streaming-events / search / posture / pause /
	// topology methods: `IsMCPServersMethod` is the O(1) predicate, and
	// they route through the MCPSurface dispatcher. Identity-mandatory;
	// the three admin verbs gate on the `auth.ScopeAdmin` claim (D-079
	// closed-set — no new scope is minted for MCP). See
	// `docs/plans/phase-73k-console-mcp-connections-page.md`.

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
	// (metadata only — never token plaintext, D-083).
	MethodMCPServersBindingsList Method = "mcp.servers.bindings.list"
	// MethodMCPServersPolicy — read-only ToolPolicy projection.
	MethodMCPServersPolicy Method = "mcp.servers.policy"
	// MethodMCPServersRefreshBinding — admin verb: initiates an OAuth
	// (re)connect flow for a binding. Requires the `auth.ScopeAdmin`
	// claim (D-079).
	MethodMCPServersRefreshBinding Method = "mcp.servers.refresh_binding"
	// MethodMCPServersRevokeBinding — admin verb: revokes an OAuth
	// binding. Requires the `auth.ScopeAdmin` claim (D-079).
	MethodMCPServersRevokeBinding Method = "mcp.servers.revoke_binding"
	// MethodMCPServersSetRawHTMLTrust — admin verb: sets the per-server
	// raw-HTML opt-in flag and emits the `mcp.raw_html_trust_toggled`
	// audit event. Requires the `auth.ScopeAdmin` claim (D-079).
	MethodMCPServersSetRawHTMLTrust Method = "mcp.servers.set_raw_html_trust"

	// MethodToolsList — Phase 73f (Wave 13 / D-116). Returns the
	// catalog of registered tools visible to the caller's identity
	// scope, with optional facet filters (scope / transport / OAuth
	// status / approval policy / reliability tier) plus aggregate
	// counters (Total / Active / Pending approval / Awaiting OAuth) for
	// the filtered view. Powers the Console Tools page catalog table.
	// Identity-mandatory; a cross-tenant fan-in requires the
	// `auth.ScopeAdmin` claim (D-079). NOT a control / search /
	// posture / topology method — `IsToolsMethod` returns true. The
	// wire-transport route is `POST /v1/tools/list`. See
	// `docs/plans/phase-73f-console-tools-page.md`.
	MethodToolsList Method = "tools.list"
	// MethodToolsGet — Phase 73f. Returns a single tool's catalog row
	// projection by ID. The lighter sibling of `tools.describe` — the
	// row shape the Console renders in the detail-panel header.
	MethodToolsGet Method = "tools.get"
	// MethodToolsDescribe — Phase 73f. Returns the full manifest of a
	// registered tool descriptor: transport, version, scopes, the
	// argument / output JSON Schemas, examples, OAuth binding scope
	// (D-083), approval policy (D-086), and the reliability shell
	// (D-024). Powers the Tools-page Manifest / Inputs / Outputs tabs.
	MethodToolsDescribe Method = "tools.describe"
	// MethodToolsMetrics — Phase 73f. Returns per-tool error-rate
	// gauges over a selectable window (1h / 24h / 7d) plus a status
	// pill (`Healthy` / `Degraded` / `Offline`). Powers the Tools-page
	// Status + Error-rate right-rail card.
	MethodToolsMetrics Method = "tools.metrics"
	// MethodToolsContentStats — Phase 73f. Returns the per-tool
	// distribution of recent result sizes vs the heavy-content
	// threshold (RFC §6.5 / D-026) plus the negotiated `DisplayMode`
	// snapshot (D-062). Powers the Tools-page Content-size card.
	MethodToolsContentStats Method = "tools.content_stats"
	// MethodToolsSetApprovalPolicy — Phase 73f. ADMIN method: updates a
	// tool's approval policy. Requires the verified `auth.ScopeAdmin`
	// claim (D-079; there is NO `tools.admin` scope — the closed
	// two-scope set is the only admit surface). Emits an
	// `audit.admin_scope_used` event through the shipped audit.Redactor.
	MethodToolsSetApprovalPolicy Method = "tools.set_approval_policy"
	// MethodToolsRevokeOAuth — Phase 73f. ADMIN method: revokes all
	// OAuth bindings for a tool. Requires the verified `auth.ScopeAdmin`
	// claim (D-079). Emits an `audit.admin_scope_used` event through
	// the shipped audit.Redactor.
	MethodToolsRevokeOAuth Method = "tools.revoke_oauth"

	// MethodTasksList — Phase 73d (Wave 13 / D-123). Returns the
	// paginated list of tasks visible to the caller's identity scope,
	// with optional facet filters (status / kind / parent-task /
	// identity / time-window / error-class / latency-above / free-text)
	// plus per-status aggregate counters (Pending / Running / Paused /
	// Failed / Complete / Cancelled) for the filtered view. Powers the
	// Console Tasks-page kanban board + list-mode table. Identity-
	// mandatory; a cross-tenant fan-in requires the `auth.ScopeAdmin`
	// claim (D-079). NOT a control / search / posture method —
	// `IsTasksMethod` returns true. The wire-transport route is
	// `POST /v1/tasks/list`. See
	// `docs/plans/phase-73d-console-tasks-page.md`.
	MethodTasksList Method = "tasks.list"
	// MethodTasksGet — Phase 73d. Returns the enriched detail of a
	// single task: the full Task projection (heavy values via
	// ArtifactRef per D-026), parent-session reference, parent-task
	// reference (when child), per-step cost rollup aggregated from
	// `llm.cost.recorded` events, and the planner-checkpoint reference
	// at spawn time. A cross-tenant TaskID lookup returns CodeNotFound
	// (existence is never revealed across tenants). The wire-transport
	// route is `POST /v1/tasks/get`.
	MethodTasksGet Method = "tasks.get"
)

// canonicalMethods is the registered set. It is a fixed package-level
// map (not a write-once registry) — the Phase 54 task-control set is
// closed; a new Protocol method is a new phase that extends this map.
// The map exists so IsValidMethod is O(1) and Methods returns a
// deterministic snapshot.
var canonicalMethods = map[Method]struct{}{
	MethodStart:             {},
	MethodCancel:            {},
	MethodPause:             {},
	MethodResume:            {},
	MethodRedirect:          {},
	MethodInjectContext:     {},
	MethodApprove:           {},
	MethodReject:            {},
	MethodPrioritize:        {},
	MethodUserMessage:       {},
	MethodEventsSubscribe:   {},
	MethodEventsAggregate:   {},
	MethodSearchQuery:       {},
	MethodSearchSessions:    {},
	MethodSearchTasks:       {},
	MethodSearchEvents:      {},
	MethodSearchArtifacts:   {},
	MethodRuntimeInfo:       {},
	MethodRuntimeHealth:     {},
	MethodRuntimeCounters:   {},
	MethodRuntimeDrivers:    {},
	MethodMetricsSnapshot:   {},
	MethodGovernancePosture: {},
	MethodLLMPosture:        {},
	MethodPauseList:         {},
	MethodTopologySnapshot:  {},
	MethodArtifactsList:     {},
	MethodArtifactsPut:      {},
	MethodArtifactsGetRef:   {},
	MethodMemoryList:        {},
	MethodMemoryGet:         {},
	MethodMemoryHealth:      {},

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

// canonicalArtifactsMethods is the closed sub-set of the three artifacts
// methods landed in Phase 73l (Wave 13 / D-120). IsArtifactsMethod is
// O(1); the control transport branches on it to route the request
// through the artifacts dispatcher instead of the task-control surface.
var canonicalArtifactsMethods = map[Method]struct{}{
	MethodArtifactsList:   {},
	MethodArtifactsPut:    {},
	MethodArtifactsGetRef: {},
}

// IsArtifactsMethod reports whether m is one of the three canonical
// artifacts methods (Phase 73l / D-120 — `artifacts.list`,
// `artifacts.put`, `artifacts.get_ref`). The control transport branches
// on this to route the request through the artifacts dispatcher instead
// of the task-control / search / posture surfaces. NOT a control
// method — a new non-control method extends THIS predicate, never the
// steering inbox.
func IsArtifactsMethod(m Method) bool {
	_, ok := canonicalArtifactsMethods[m]
	return ok
}

// canonicalMCPServersMethods is the closed sub-set of the twelve
// `mcp.servers.*` methods landed in Phase 73k (Wave 13 / D-119) — nine
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
// `mcp.servers.*` admin verbs (Phase 73k / D-119) that gate on the
// `auth.ScopeAdmin` claim. IsMCPAdminMethod is O(1); the MCPSurface
// dispatcher uses it to apply the admin-scope gate.
var canonicalMCPAdminMethods = map[Method]struct{}{
	MethodMCPServersRefreshBinding:  {},
	MethodMCPServersRevokeBinding:   {},
	MethodMCPServersSetRawHTMLTrust: {},
}

// IsMCPServersMethod reports whether m is one of the twelve canonical
// `mcp.servers.*` methods (Phase 73k / D-119). The control transport
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
// `set_raw_html_trust`) that gate on the `auth.ScopeAdmin` claim
// (D-079). The MCPSurface dispatcher uses it to apply the admin gate.
func IsMCPAdminMethod(m Method) bool {
	_, ok := canonicalMCPAdminMethods[m]
	return ok
}

// canonicalToolsMethods is the closed sub-set of the seven `tools.*`
// methods landed in Phase 73f (Wave 13 / D-116) — the five read
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
// verified `auth.ScopeAdmin` claim (D-079). The Tools wire handler
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
// `tools.*` methods (Phase 73f / D-116). The control transport
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
// that require the verified `auth.ScopeAdmin` claim (D-079). The Tools
// wire handler uses this to decide whether to enforce the scope gate.
func IsToolsAdminMethod(m Method) bool {
	_, ok := canonicalToolsAdminMethods[m]
	return ok
}

// canonicalTasksMethods is the closed sub-set of the two `tasks.*`
// methods landed in Phase 73d (Wave 13 / D-123) — `tasks.list` and
// `tasks.get`. Both are READ-ONLY: the Console Tasks page consumes the
// existing Phase 54 task-control verbs for mutation, never a new
// `tasks.*` mutating method. IsTasksMethod is O(1); the stream
// transport branches on it to route the request through the Tasks
// dispatcher instead of the task-control surface.
var canonicalTasksMethods = map[Method]struct{}{
	MethodTasksList: {},
	MethodTasksGet:  {},
}

// IsTasksMethod reports whether m is one of the two canonical `tasks.*`
// methods (Phase 73d / D-123 — `tasks.list`, `tasks.get`). The stream
// transport branches on this to route the request through the Tasks
// dispatcher instead of the task-control / search / posture / topology
// surfaces. NOT a control method — both are reads; the Console Tasks
// page consumes the shipped Phase 54 control verbs for mutation.
func IsTasksMethod(m Method) bool {
	_, ok := canonicalTasksMethods[m]
	return ok
}

// canonicalFlowsMethods is the closed sub-set of the six Flows-page
// methods landed in Phase 73i (Wave 13 / D-117). IsFlowsMethod is O(1);
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

// IsFlowsMethod reports whether m is one of the six Flows-page methods
// (Phase 73i / D-117). Five are read-only; `flows.run` is the single
// mutating method. The Flows-page surface is distinct from the steering
// inbox, the streaming-events surface, the search cluster, and the
// posture surface — a transport adapter branches on this predicate to
// route the request through the Flows dispatcher.
func IsFlowsMethod(m Method) bool {
	_, ok := canonicalFlowsMethods[m]
	return ok
}

// canonicalSearchMethods is the closed sub-set of the five search.*
// methods. IsSearchMethod is O(1); a transport adapter (Phase 72c
// search handler) uses it to branch the route table.
var canonicalSearchMethods = map[Method]struct{}{
	MethodSearchQuery:     {},
	MethodSearchSessions:  {},
	MethodSearchTasks:     {},
	MethodSearchEvents:    {},
	MethodSearchArtifacts: {},
}

// IsSearchMethod reports whether m is one of the five canonical
// `search.*` methods. The Phase 72c control transport branches on this
// to route the request through the search dispatcher instead of the
// task-control surface.
func IsSearchMethod(m Method) bool {
	_, ok := canonicalSearchMethods[m]
	return ok
}

// canonicalPostureMethods is the closed sub-set of the seven posture
// methods — the five Phase 72f (D-111) `runtime.*` / `metrics.*` reads
// plus the two Phase 72g (D-112) `governance.posture` / `llm.posture`
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
// posture methods — the five `runtime.*` / `metrics.*` reads (Phase 72f
// / D-111) and the two `governance.posture` / `llm.posture` config
// reads (Phase 72g / D-112). The PostureSurface uses this to branch in
// transport adapters that want a single route table over all canonical
// methods.
func IsPostureMethod(m Method) bool {
	_, ok := canonicalPostureMethods[m]
	return ok
}

// canonicalTopologyMethods is the closed sub-set of the topology-
// projection methods landed in Phase 74 (Wave 13 / D-114). Today it
// holds the single `topology.snapshot` method; the paired
// `topology.changed` surface is an EVENT, not a method, so it is not
// in this set. IsTopologyMethod is O(1); the control transport uses
// it to branch the request onto the topology dispatcher.
var canonicalTopologyMethods = map[Method]struct{}{
	MethodTopologySnapshot: {},
}

// IsTopologyMethod reports whether m is one of the canonical
// topology-projection methods (Phase 74 / D-114 — today just
// `topology.snapshot`). The control transport branches on this to
// route the request through the topology dispatcher instead of the
// task-control / search surfaces. NOT a control method — a new
// non-control method extends THIS predicate, never the steering inbox.
func IsTopologyMethod(m Method) bool {
	_, ok := canonicalTopologyMethods[m]
	return ok
}

// canonicalMemoryMethods is the closed sub-set of the three Phase 73j
// (Wave 13 / D-118) `memory.*` read methods. IsMemoryMethod is O(1);
// the transport adapter uses it to branch the request through the
// memory-inspection handlers instead of the task-control surface. The
// set is closed — the V1 memory-page surface is read-only (`memory.list`
// / `memory.get` / `memory.health`); the mutation methods (`memory.put`
// / `memory.delete`) are deferred to Phase 73 / post-V1.
var canonicalMemoryMethods = map[Method]struct{}{
	MethodMemoryList:   {},
	MethodMemoryGet:    {},
	MethodMemoryHealth: {},
}

// IsMemoryMethod reports whether m is one of the three canonical
// `memory.*` read methods landed in Phase 73j (Wave 13 / D-118). The
// control transport branches on this to route the request through the
// memory-inspection handlers instead of the task-control / search /
// posture / pause / topology surfaces. NOT a control method — a new
// non-control method extends THIS predicate, never the steering inbox.
func IsMemoryMethod(m Method) bool {
	_, ok := canonicalMemoryMethods[m]
	return ok
}

// streamingEventsMethods is the closed set of canonical method names
// the Runtime classifies as streaming-events methods (the first non-
// task-control Protocol surface to land — Phase 72 / 72a). Used by
// IsControlMethod to keep its predicate exclusive to the Phase 54
// nine, and by IsStreamingEventsMethod (below) when a caller wants
// the inverse predicate.
var streamingEventsMethods = map[Method]struct{}{
	MethodEventsSubscribe: {},
	MethodEventsAggregate: {},
}

// IsValidMethod reports whether m is one of the canonical Protocol
// method names — the Phase 54 task-control ten plus the Wave 13
// streaming-events additions plus the Phase 72c search cluster.
func IsValidMethod(m Method) bool {
	_, ok := canonicalMethods[m]
	return ok
}

// IsPauseMethod reports whether m is one of the pause-snapshot methods
// landed in Wave 13 (Phase 72e — currently only MethodPauseList). The
// pause-snapshot surface is a read-only projection over the unified
// pause/resume Coordinator (Phase 50); it is NOT a steering control,
// NOT a streaming-events method, and NOT a search method. A transport
// adapter branches on this predicate to route the request through the
// pause-list snapshot handler instead of the task-control surface.
func IsPauseMethod(m Method) bool {
	_, ok := pauseMethods[m]
	return ok
}

// pauseMethods is the closed set of canonical pause-snapshot method
// names (Phase 72e). Used by IsControlMethod to keep its predicate
// exclusive to the Phase 54 nine, and by IsPauseMethod for the inverse.
var pauseMethods = map[Method]struct{}{
	MethodPauseList: {},
}

// IsControlMethod reports whether m is one of the nine steering-control
// methods — every canonical method except MethodStart AND the
// streaming-events methods (Phase 72 / 72a) AND the Phase 72c
// `search.*` cluster AND the Phase 72f `runtime.*` / `metrics.*`
// posture cluster AND the Phase 72e pause-snapshot method AND the
// Phase 74 `topology.snapshot` method AND the Phase 73j `memory.*`
// read cluster (each a separate surface from the steering inbox).
// The protocol.ControlSurface uses this to branch: a control method
// maps onto a steering.ControlEvent; MethodStart maps onto the task
// registry; a streaming-events method routes through the SSE /
// events-aggregate transport; a search method maps onto the Phase 72c
// search dispatcher; a posture method maps onto the Phase 72f
// PostureSurface; a pause-snapshot method maps onto the Phase 72e
// pause-list handler; a topology method maps onto the Phase 74
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
	if IsTasksMethod(m) {
		return false
	}
	if IsToolsMethod(m) {
		return false
	}
	if IsFlowsMethod(m) {
		return false
	}
	return true
}

// IsStreamingEventsMethod reports whether m is one of the streaming-
// events methods landed in Wave 13 (MethodEventsSubscribe or
// MethodEventsAggregate). The transport-side router uses this to
// classify a method into its routing branch without re-listing the
// streaming set.
func IsStreamingEventsMethod(m Method) bool {
	_, ok := streamingEventsMethods[m]
	return ok
}

// Methods returns a deterministic, lexicographically-sorted snapshot of
// every canonical method name (the Phase 54 task-control ten + the
// Wave 13 streaming-events additions + the Phase 72c search cluster).
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
