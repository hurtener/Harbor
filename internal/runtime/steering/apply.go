package steering

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tasks"
)

// stepControl is the mutable per-step accumulator the RunLoop builds by
// draining the inbox and applying each event's side effect. It is NOT a
// compiled-artifact field — a fresh stepControl is allocated per step
// boundary, lives on the run's own goroutine stack, and is projected
// onto planner.RunContext.Control before the next Planner.Next call. It
// is therefore single-goroutine by construction (the run's own loop) and
// needs no synchronisation.
type stepControl struct {
	// signals is the planner-visible projection — exactly the shape the
	// planner reads via RunContext.Control (Phase 42).
	signals planner.ControlSignals
	// goal carries a REDIRECT's new goal so the RunLoop can update
	// RunContext.Goal in addition to Control.RedirectGoal. Empty when no
	// REDIRECT was drained this step.
	goal string
	// hardCancel is true when a CANCEL with payload.hard == true was
	// drained — the RunLoop additionally fires the hard-cancel hook.
	hardCancel bool
	// pauseRequested mirrors signals.PauseRequested; kept distinct so the
	// RunLoop can branch on "a PAUSE control arrived" without re-reading
	// the projection.
	pauseRequested bool
	// resumeRequested / resumeKind record that a RESUME / APPROVE /
	// REJECT was drained this step. resumeKind records which of the
	// three it was so the RunLoop branches correctly (APPROVE / RESUME
	// re-enter the planner; REJECT terminates the run). The resume
	// payload itself is NOT stashed here — advancePause reads it from
	// the drained ControlEvent directly.
	resumeRequested bool
	resumeKind      ControlType
	// prioritize carries a PRIORITIZE's new priority + a "was set" flag.
	prioritizeSet bool
	prioritizeVal int
}

// applier holds the runtime dependencies the per-control-type side-effect
// functions need. It is constructed once per RunLoop (the dependencies
// are immutable after construction — D-025) and shared across every run;
// the per-run / per-step state lives in stepControl + ctx, never here.
type applier struct {
	coord          pauseresume.Coordinator
	taskRegistry   tasks.TaskRegistry                            // optional; nil ⇒ PRIORITIZE fails loud
	hardCancelHook func(ctx context.Context, runID string) error // optional
}

// applyEvent dispatches one drained ControlEvent to its side-effect
// function, mutating sc in place. It returns a non-nil error only when
// the side effect itself failed (a missing task for PRIORITIZE, a
// Coordinator.Resume failure for RESUME / APPROVE / REJECT); a structural
// problem with the event was already rejected at Inbox.Enqueue time
// (Phase 52), so applyEvent trusts the event's Type / Identity / Payload.
//
// The accumulating events (INJECT_CONTEXT / USER_MESSAGE / REDIRECT /
// CANCEL soft / PAUSE) never fail — they only mutate sc. The acting
// events (PRIORITIZE, RESUME / APPROVE / REJECT, CANCEL hard) reach a
// runtime dependency and CAN fail; their failure is surfaced loud, never
// swallowed (CLAUDE.md §5).
func (a *applier) applyEvent(ctx context.Context, sc *stepControl, ev ControlEvent, outstandingToken pauseresume.Token) error {
	switch ev.Type {
	case ControlInjectContext:
		// Append the operator-supplied context map; the planner merges it
		// into its next prompt build (RFC §6.3).
		if ev.Payload != nil {
			sc.signals.InjectedContext = append(sc.signals.InjectedContext, ev.Payload)
		}
		return nil

	case ControlUserMessage:
		// USER_MESSAGE carries a user-authored message string under the
		// "message" key. A missing / non-string "message" is tolerated as
		// an empty append rather than an error — Phase 52's ValidatePayload
		// already bounded the payload; the shape is a Protocol-edge
		// convention, not a hard contract Phase 53 re-enforces.
		if msg, ok := stringFromPayload(ev.Payload, "message"); ok {
			sc.signals.UserMessages = append(sc.signals.UserMessages, msg)
		}
		return nil

	case ControlRedirect:
		// REDIRECT rewrites the run's goal. The new goal is the payload's
		// "goal" string. The RunLoop reads sc.goal to update
		// RunContext.Goal AND projects sc.signals.RedirectGoal so the
		// planner sees the redirect explicitly.
		if g, ok := stringFromPayload(ev.Payload, "goal"); ok && g != "" {
			sc.goal = g
			sc.signals.RedirectGoal = g
		}
		return nil

	case ControlCancel:
		// CANCEL — soft by default. The RunLoop projects
		// Control.Cancelled; the planner returns Finish{Cancelled} at the
		// next boundary. When payload.hard == true, the RunLoop ALSO fires
		// the hard-cancel hook to propagate a cancellation context into an
		// in-flight decision execution (brief 02 §6).
		sc.signals.Cancelled = true
		if boolFromPayload(ev.Payload, "hard") {
			sc.hardCancel = true
		}
		return nil

	case ControlPause:
		// PAUSE — the RunLoop projects Control.PauseRequested; the planner
		// returns RequestPause{AwaitInput} at the next boundary, which the
		// RunLoop routes through the unified Coordinator. PAUSE does NOT
		// itself call Coordinator.Request — the planner's RequestPause
		// decision does, so the trajectory + reason are planner-attested.
		sc.signals.PauseRequested = true
		sc.pauseRequested = true
		return nil

	case ControlResume, ControlApprove, ControlReject:
		// RESUME / APPROVE / REJECT all advance an outstanding pause via
		// the ONE Coordinator (CLAUDE.md §7 rule 4). The RunLoop records
		// which one so it branches correctly: APPROVE / RESUME re-enter
		// the planner; REJECT terminates the run with
		// Finish{ConstraintsConflict}. The actual Coordinator.Resume call
		// happens here so a failure (no outstanding pause, already
		// resumed, scope mismatch) is surfaced loud at apply time.
		sc.resumeRequested = true
		sc.resumeKind = ev.Type
		return a.advancePause(ctx, ev, outstandingToken)

	case ControlPrioritize:
		// PRIORITIZE updates the run's task priority via the TaskRegistry.
		// The new priority is the payload's "priority" number. A nil
		// TaskRegistry, a run with no TaskID, or a Prioritize failure is
		// surfaced loud.
		pri, ok := intFromPayload(ev.Payload, "priority")
		if !ok {
			return fmt.Errorf("%w: PRIORITIZE payload missing an integer 'priority'", ErrPayloadInvalid)
		}
		sc.prioritizeSet = true
		sc.prioritizeVal = pri
		return nil // the RunLoop calls a.prioritize once the run's TaskID is known

	default:
		// Unreachable: Inbox.Enqueue already rejected any non-canonical
		// type with ErrUnknownControlType. Fail loud rather than silently
		// skip — a drained event of an unknown type is a Phase 52
		// invariant violation.
		return fmt.Errorf("%w: %q reached applyEvent (Phase 52 Enqueue invariant violated)",
			ErrUnknownControlType, string(ev.Type))
	}
}

// advancePause calls Coordinator.Resume for a RESUME / APPROVE / REJECT
// control event. A REJECT carries a rejected:true marker into the resume
// payload so a Coordinator subscriber (and the pause record) can tell an
// approval from a rejection. A run with no outstanding pause Token fails
// loud with ErrNoOutstandingPause.
func (a *applier) advancePause(ctx context.Context, ev ControlEvent, token pauseresume.Token) error {
	if token == "" {
		return fmt.Errorf("%w: %s control for run %q",
			ErrNoOutstandingPause, ev.Type, ev.Identity.RunID)
	}
	// Build the resume payload. For REJECT, stamp rejected:true so the
	// distinction survives into the pause record. Clone so a caller's
	// later mutation cannot reach the Coordinator's recorded state.
	payload := make(map[string]any, len(ev.Payload)+1)
	for k, v := range ev.Payload {
		payload[k] = v
	}
	if ev.Type == ControlReject {
		payload["rejected"] = true
	}

	// Coordinator.Resume reads the resuming identity from ctx; the
	// RunLoop hands applyEvent a ctx carrying the run's quadruple, so the
	// scope check inside Resume sees the right triple.
	if err := a.coord.Resume(ctx, token, payload); err != nil {
		// pauseresume sentinels (ErrAlreadyResumed, ErrScopeMismatch,
		// ErrPauseNotFound, trajectory.ErrToolContextLost) propagate
		// verbatim — the caller reaches them via errors.Is / errors.As.
		// No silent swallow.
		return fmt.Errorf("steering: advancing pause via Coordinator.Resume (%s): %w", ev.Type, err)
	}
	return nil
}

// prioritize applies a PRIORITIZE side effect: it calls
// tasks.TaskRegistry.Prioritize for the run's task. Called by the RunLoop
// after the step drain, once the run's TaskID is in scope. A nil
// TaskRegistry or an empty TaskID fails loud — PRIORITIZE that cannot
// reach a task is a misconfiguration, not a no-op.
func (a *applier) prioritize(ctx context.Context, taskID tasks.TaskID, priority int) error {
	if a.taskRegistry == nil {
		return fmt.Errorf("%w: PRIORITIZE requires a TaskRegistry (wire WithTaskRegistry)", ErrRunLoopMisconfigured)
	}
	if taskID == "" {
		return fmt.Errorf("%w: PRIORITIZE for a run with no TaskID", ErrPayloadInvalid)
	}
	if _, err := a.taskRegistry.Prioritize(ctx, taskID, priority); err != nil {
		return fmt.Errorf("steering: applying PRIORITIZE via TaskRegistry.Prioritize: %w", err)
	}
	return nil
}

// hardCancel fires the hard-cancel hook to propagate a cancellation
// context into an in-flight decision execution (brief 02 §6 — "hard=true
// propagates a cancellation context to the in-flight tool"). A nil hook
// is tolerated: a soft CANCEL still set Control.Cancelled, so the run
// still terminates at the next boundary — the hook only accelerates an
// in-flight tool's teardown. The nil-hook case is logged by the RunLoop,
// not failed, because a RunLoop wired without an engine still honours
// CANCEL correctly (just not instantly).
func (a *applier) hardCancel(ctx context.Context, runID string) error {
	if a.hardCancelHook == nil {
		return nil
	}
	if err := a.hardCancelHook(ctx, runID); err != nil {
		return fmt.Errorf("steering: hard-cancel hook for run %q: %w", runID, err)
	}
	return nil
}

// ctxWithIdentity attaches the run's identity to ctx under BOTH the
// triple key (identity.With → identity.From — what the TaskRegistry and
// the pauseresume.Coordinator read) AND the quadruple key
// (identity.WithRun → identity.QuadrupleFrom — what run-scoped consumers
// read). The two keys are independent (see internal/identity), so a ctx
// must carry both for every downstream identity.From / QuadrupleFrom
// pathway to resolve. Returns the bare ctx when the identity is
// incomplete (the RunLoop's identity-mandatory pre-check would already
// have rejected such a run).
func ctxWithIdentity(ctx context.Context, q identity.Quadruple) context.Context {
	if err := identity.Validate(q.Identity); err != nil {
		return ctx
	}
	withID, err := identity.With(ctx, q.Identity)
	if err != nil {
		return ctx
	}
	if q.RunID == "" {
		return withID
	}
	withRun, err := identity.WithRun(withID, q.Identity, q.RunID)
	if err != nil {
		return withID
	}
	return withRun
}

// stringFromPayload returns the string value at key in m and a presence
// bool. A missing key or a non-string value returns ("", false).
func stringFromPayload(m map[string]any, key string) (string, bool) {
	if m == nil {
		return "", false
	}
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

// boolFromPayload returns the bool value at key in m. A missing key or a
// non-bool value returns false.
func boolFromPayload(m map[string]any, key string) bool {
	if m == nil {
		return false
	}
	v, ok := m[key]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// intFromPayload returns the integer value at key in m and a presence
// bool. JSON numbers decode as float64, so a float64 with no fractional
// part is accepted; a plain int is also accepted (in-process callers).
// A missing key, a non-numeric value, or a non-integral float returns
// (0, false).
func intFromPayload(m map[string]any, key string) (int, bool) {
	if m == nil {
		return 0, false
	}
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}
		return 0, false
	default:
		return 0, false
	}
}

// classifyApplyErr maps an apply-side-effect error to a stable,
// low-cardinality, redaction-safe string for the control.applied event's
// Err field. The raw error message is never used — it may quote caller
// data. An unclassified error is "apply_failed" (the catch-all).
func classifyApplyErr(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrNoOutstandingPause):
		return "no_outstanding_pause"
	case errors.Is(err, ErrRunLoopMisconfigured):
		return "misconfigured"
	case errors.Is(err, ErrPayloadInvalid):
		return "payload_invalid"
	case errors.Is(err, pauseresume.ErrAlreadyResumed):
		return "already_resumed"
	case errors.Is(err, pauseresume.ErrScopeMismatch):
		return "scope_mismatch"
	case errors.Is(err, pauseresume.ErrPauseNotFound):
		return "pause_not_found"
	default:
		return "apply_failed"
	}
}
