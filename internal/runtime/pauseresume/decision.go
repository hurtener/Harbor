package pauseresume

// Decision is the typed marker carried on a `pause.resumed` event so
// wire consumers can distinguish the *kind* of resume (approve vs.
// reject vs. generic resume vs. timeout) without parsing free-form
// `Reason` strings.
//
// The Phase 31 approval gate already owns its own narrower enum
// (`approval.ApprovalDecision` — approve / reject / pending) for the
// gate-internal resolution channel; that enum is approval-specific and
// stays where it lives. This `pauseresume.Decision` is the broader
// runtime-level enum that ALSO covers tool-side OAuth completion
// (`DecisionResume`) and deadline-driven resumes (`DecisionTimeout`).
// A single enum on `Coordinator.Resume` would force either pollution of
// `approval.ApprovalDecision` with non-approval values or a parallel
// "ApprovalDecision vs PauseResumeDecision" split — both are §13
// "two parallel implementations of the same conceptual feature"
// smells. Keeping the gate-internal enum narrow and the
// coordinator-edge enum broad is the right factoring.
//
// Wire consumers (the Console, third-party clients, integration tests)
// switch on this typed value rather than parsing `Reason` strings —
// the audit-flagged anti-pattern issue #113 / D-096 closes.
type Decision string

const (
	// DecisionApprove — the gate's approver said yes (HITL approval).
	// Mirrors the steering inbox's `ControlApprove` and the approval
	// package's `DecisionApprove`.
	DecisionApprove Decision = "approve"

	// DecisionReject — the gate's approver said no (HITL rejection).
	// Mirrors the steering inbox's `ControlReject` and the approval
	// package's `DecisionReject`. A REJECT terminates the run with
	// `Finish{ConstraintsConflict}` in the RunLoop (D-071).
	DecisionReject Decision = "reject"

	// DecisionResume — a generic resume of a non-approval pause. The
	// canonical producer is the tool-side OAuth provider completing a
	// flow (Phase 30), but any future non-approval resume (a steering
	// `RESUME` not tied to an APPROVE / REJECT, an A2A `INPUT_REQUIRED`
	// fulfilled) lands here.
	DecisionResume Decision = "resume"

	// DecisionTimeout — a deadline-driven resume (the pause's max-park
	// window elapsed and the runtime resumed it to surface a
	// constraint-conflict). Forward-looking: Phase 50 does not yet
	// emit this, but the enum value is reserved so a future
	// pause-deadline implementor does not have to widen the wire
	// shape.
	DecisionTimeout Decision = "timeout"
)

// IsValidDecision reports whether d is one of the four canonical
// pause-resume Decision values. An empty Decision is rejected loud — a
// `pause.resumed` event without a Decision is the bug shape this enum
// closes.
func IsValidDecision(d Decision) bool {
	switch d {
	case DecisionApprove, DecisionReject, DecisionResume, DecisionTimeout:
		return true
	default:
		return false
	}
}
