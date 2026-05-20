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
)

// canonicalMethods is the registered set. It is a fixed package-level
// map (not a write-once registry) — the Phase 54 task-control set is
// closed; a new Protocol method is a new phase that extends this map.
// The map exists so IsValidMethod is O(1) and Methods returns a
// deterministic snapshot.
var canonicalMethods = map[Method]struct{}{
	MethodStart:           {},
	MethodCancel:          {},
	MethodPause:           {},
	MethodResume:          {},
	MethodRedirect:        {},
	MethodInjectContext:   {},
	MethodApprove:         {},
	MethodReject:          {},
	MethodPrioritize:      {},
	MethodUserMessage:     {},
	MethodEventsSubscribe: {},
	MethodEventsAggregate: {},
	MethodSearchQuery:     {},
	MethodSearchSessions:  {},
	MethodSearchTasks:     {},
	MethodSearchEvents:    {},
	MethodSearchArtifacts: {},
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

// IsControlMethod reports whether m is one of the nine steering-control
// methods — every canonical method except MethodStart AND the
// streaming-events methods (Phase 72 / 72a) AND the Phase 72c
// `search.*` cluster (a separate surface from the steering inbox).
// The protocol.ControlSurface uses this to branch: a control method
// maps onto a steering.ControlEvent; MethodStart maps onto the task
// registry; a streaming-events method routes through the SSE /
// events-aggregate transport; a search method maps onto the Phase 72c
// search dispatcher. A new non-control method (state inspection,
// topology, artifacts — future phases) extends THIS predicate, NOT
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
