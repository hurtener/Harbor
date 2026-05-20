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
// Phase 74 `topology.snapshot` method (each a separate surface from
// the steering inbox).
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
