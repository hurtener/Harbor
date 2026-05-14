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
// row + RFC §6.3 control taxonomy).
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
)

// canonicalMethods is the registered set. It is a fixed package-level
// map (not a write-once registry) — the Phase 54 task-control set is
// closed; a new Protocol method is a new phase that extends this map.
// The map exists so IsValidMethod is O(1) and Methods returns a
// deterministic snapshot.
var canonicalMethods = map[Method]struct{}{
	MethodStart:         {},
	MethodCancel:        {},
	MethodPause:         {},
	MethodResume:        {},
	MethodRedirect:      {},
	MethodInjectContext: {},
	MethodApprove:       {},
	MethodReject:        {},
	MethodPrioritize:    {},
	MethodUserMessage:   {},
}

// IsValidMethod reports whether m is one of the ten canonical
// task-control method names.
func IsValidMethod(m Method) bool {
	_, ok := canonicalMethods[m]
	return ok
}

// IsControlMethod reports whether m is one of the nine steering-control
// methods — every canonical method except MethodStart. The
// protocol.ControlSurface uses this to branch: a control method maps
// onto a steering.ControlEvent; MethodStart maps onto the task registry.
func IsControlMethod(m Method) bool {
	return IsValidMethod(m) && m != MethodStart
}

// Methods returns a deterministic, lexicographically-sorted snapshot of
// the ten canonical method names. Useful for exhaustiveness tests and
// for a transport adapter's route table.
func Methods() []Method {
	out := make([]Method, 0, len(canonicalMethods))
	for m := range canonicalMethods {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
