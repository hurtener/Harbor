package steering

import "sort"

// ControlType is the string-typed enum of the nine canonical control
// event types (RFC §6.3 — Settled). The wire strings are the RFC's
// verbatim uppercase identifiers; the Protocol projection (Phase 54)
// accepts exactly these.
type ControlType string

// The nine canonical control types (RFC §6.3, brief 02 §2). Adding a
// tenth is an RFC change — the taxonomy is Settled.
const (
	// ControlInjectContext — append operator-supplied context to the
	// run's trajectory; visible on the planner's next step.
	ControlInjectContext ControlType = "INJECT_CONTEXT"
	// ControlRedirect — rewrite the run's goal. Requires the user
	// (the agent's owner) — RFC §6.3.
	ControlRedirect ControlType = "REDIRECT"
	// ControlCancel — cancel the run (hard or soft; the soft/hard
	// distinction is a Phase 53 payload concern).
	ControlCancel ControlType = "CANCEL"
	// ControlPrioritize — change the run's task priority. Requires
	// admin — RFC §6.3.
	ControlPrioritize ControlType = "PRIORITIZE"
	// ControlPause — pause the run at the next planner-step boundary.
	// Phase 53 wires this onto the unified pause/resume primitive.
	ControlPause ControlType = "PAUSE"
	// ControlResume — resume a paused run. Phase 53 wires this onto
	// the unified pause/resume primitive.
	ControlResume ControlType = "RESUME"
	// ControlApprove — approve a HITL-gated step. Phase 53 wires this
	// onto the unified pause/resume primitive (advance a pause).
	ControlApprove ControlType = "APPROVE"
	// ControlReject — reject a HITL-gated step. Phase 53 wires this
	// onto the unified pause/resume primitive.
	ControlReject ControlType = "REJECT"
	// ControlUserMessage — inject a user-authored message into the
	// run; visible on the planner's next step.
	ControlUserMessage ControlType = "USER_MESSAGE"
)

// canonicalControlTypes is the registered set. Phase 52's taxonomy is
// fixed (RFC §6.3 — nine types, Settled); there is no RegisterControlType
// escape hatch because a tenth type is an RFC change, not a phase
// addition. The map exists so IsValidControlType is O(1) and
// ControlTypes returns a deterministic snapshot.
var canonicalControlTypes = map[ControlType]struct{}{
	ControlInjectContext: {},
	ControlRedirect:      {},
	ControlCancel:        {},
	ControlPrioritize:    {},
	ControlPause:         {},
	ControlResume:        {},
	ControlApprove:       {},
	ControlReject:        {},
	ControlUserMessage:   {},
}

// IsValidControlType reports whether t is one of the nine canonical
// control types.
func IsValidControlType(t ControlType) bool {
	_, ok := canonicalControlTypes[t]
	return ok
}

// ControlTypes returns a deterministic, lexicographically-sorted
// snapshot of the nine canonical control types. Useful for the
// Protocol projection's allow-list and for exhaustiveness tests.
func ControlTypes() []ControlType {
	out := make([]ControlType, 0, len(canonicalControlTypes))
	for t := range canonicalControlTypes {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
