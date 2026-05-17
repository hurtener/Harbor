package steering

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools/approval"
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
	// gates is the catalog-applied approval-gate map keyed by tool
	// name (D-090 `AppliedGates`). Nil / empty disables the
	// steering→gate bridge — the apply path falls back to the direct
	// Coordinator.Resume call (the pre-D-097 behaviour). Per WithApprovalGates'
	// godoc: when an APPROVE/REJECT drains carrying a `token`, the
	// applier tries each gate's ResolveApproval; the gate that owns
	// the token resumes its waiter (which itself calls
	// Coordinator.Resume — option A, the gate-owned resume path).
	// When no gate owns the token, the applier falls through to the
	// direct Coordinator.Resume.
	gates map[string]*approval.ApprovalGate
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

// advancePause routes a RESUME / APPROVE / REJECT control event to
// the unified pause/resume primitive. Two paths:
//
//  1. Gate-owned (D-097). When the applier was constructed with
//     `WithApprovalGates(gates)` AND an APPROVE / REJECT carries a
//     `token` matching one of the gates' pending entries, the bridge
//     calls `gate.ResolveApproval(ctx, token, decision, reason)` on
//     the owning gate. `ResolveApproval` itself calls
//     `Coordinator.Resume`, so the direct call below is SKIPPED for
//     this path (option A — gate-owned resume; documented in D-097
//     to avoid the ErrAlreadyResumed double-resume that option B
//     would trigger). The gate's resolve channel delivers the
//     decision to the blocked `RunGuarded` waiter so the wrapped
//     tool's `Invoke` unblocks.
//
//  2. Direct Resume (the pre-D-097 path, retained as the fall-back).
//     A plain RESUME, an APPROVE / REJECT targeting an OAuth pause
//     (or any pause whose Token is NOT in any gate's pending map),
//     or an APPROVE / REJECT when no gates are wired — all flow
//     through the direct `Coordinator.Resume` call. The typed
//     `pauseresume.Decision` is derived from the ControlType
//     (RESUME→Resume, APPROVE→Approve, REJECT→Reject) and carried on
//     the emitted `pause.resumed` event so wire consumers can
//     distinguish the resume kind without parsing free-form `Reason`
//     strings (issue #113, D-096). A run with no outstanding pause
//     Token fails loud with ErrNoOutstandingPause.
//
// REJECT additionally stamps `rejected: true` on the resume payload
// map for backward-compatible payload observers; the typed Decision
// is the load-bearing channel.
func (a *applier) advancePause(ctx context.Context, ev ControlEvent, token pauseresume.Token) error {
	if token == "" {
		return fmt.Errorf("%w: %s control for run %q",
			ErrNoOutstandingPause, ev.Type, ev.Identity.RunID)
	}

	// Path 1 — gate-owned resume (D-097). The bridge fires only for
	// APPROVE / REJECT controls. RESUME is the operator-level "advance
	// the pause" verb that has no approval semantics; it stays on the
	// direct Coordinator.Resume path even when gates are wired.
	//
	// The gate's pause-token is carried on the wire under
	// `ev.Payload["token"]` — the gate minted it via Coordinator.Request
	// from inside RunGuarded, NOT from the RunLoop's own outstanding
	// pause (which is what the `token` param above carries — the
	// planner's `RequestPause` token, distinct from the gate's). When
	// the wire payload carries no `token` key, the APPROVE / REJECT
	// targets the RunLoop's own outstanding pause (the planner's
	// `RequestPause`); the bridge does not fire and the direct
	// Coordinator.Resume path runs with the RunLoop's token. This is
	// the canonical OAuth / A2A AUTH_REQUIRED shape: those flows park
	// via the planner's RequestPause, not via a gate.
	if len(a.gates) > 0 && (ev.Type == ControlApprove || ev.Type == ControlReject) {
		wireToken, hasWireToken := wireGateTokenFromPayload(ev.Payload)
		if hasWireToken {
			routed, err := a.routeThroughGate(ctx, ev, wireToken)
			if err != nil {
				// A gate-resolution error is loud — never silently
				// degrade to the direct Resume path (CLAUDE.md §13). A
				// "wrong gate" error from `ResolveApproval` is the
				// gate-owned `ErrApprovalNotFound` sentinel which the
				// routing code interprets as "this gate doesn't own the
				// token, try the next one." A genuine gate-side error
				// (scope mismatch, gate closed, coordinator error)
				// propagates verbatim.
				return err
			}
			if routed {
				// The owning gate called Coordinator.Resume on the
				// gate's pause (`wireToken`). The RunLoop's own
				// outstanding pause (`token`) is a DIFFERENT pause —
				// it gets resumed below via the direct Coordinator.
				// Resume path so the planner re-enters. (When the
				// gate's pause IS the RunLoop's pause — i.e. the
				// planner itself emitted `RequestPause(
				// ApprovalRequired)` and a gate happened to also park
				// on the same token — `wireToken == token` and the
				// "double-resume" below becomes ErrAlreadyResumed,
				// which the direct path surfaces loud. That case is
				// pathological — the planner shouldn't both
				// RequestPause AND wrap its own tool call in an
				// approval gate; the typical shape is one or the
				// other.)
				//
				// For the common shape (planner runs idle, gate's
				// pause is independent), the bridge resolves the
				// gate's pause and the direct path below resolves
				// the RunLoop's own pause — two distinct Coordinator.
				// Resume calls against two distinct Tokens. No
				// double-resume.
				if wireToken == token {
					// Edge case: the gate's pause IS the RunLoop's
					// outstanding pause (same Token). The gate
					// already resumed it; skip the direct path to
					// avoid ErrAlreadyResumed.
					return nil
				}
				// Fall through to the direct path so the RunLoop's
				// own outstanding pause resumes too.
			}
			// If routed == false: no gate owned wireToken (typo on the
			// wire? OAuth pause? unknown). The direct path below
			// resumes the RunLoop's own pause with the wire-side
			// payload — preserving the pre-D-097 behaviour for
			// non-gate APPROVE/REJECT events.
		}
	}

	// Path 2 — direct Coordinator.Resume (the pre-D-097 path).
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

	// Map ControlType → typed Decision. The mapping is exhaustive over
	// the three ControlType values applyEvent dispatches here; an
	// unreachable default fails loud rather than silently emitting a
	// `pause.resumed` event without a Decision (the §13 fail-loudly
	// contract the typed marker exists to enforce).
	decision, err := decisionFromControlType(ev.Type)
	if err != nil {
		return fmt.Errorf("steering: advancing pause: %w", err)
	}

	// Coordinator.Resume reads the resuming identity from ctx; the
	// RunLoop hands applyEvent a ctx carrying the run's quadruple, so the
	// scope check inside Resume sees the right triple.
	if err := a.coord.Resume(ctx, token, decision, payload); err != nil {
		// pauseresume sentinels (ErrAlreadyResumed, ErrScopeMismatch,
		// ErrPauseNotFound, trajectory.ErrToolContextLost) propagate
		// verbatim — the caller reaches them via errors.Is / errors.As.
		// No silent swallow.
		return fmt.Errorf("steering: advancing pause via Coordinator.Resume (%s): %w", ev.Type, err)
	}
	return nil
}

// routeThroughGate is the D-097 steering→gate bridge. It iterates the
// configured gates and tries `gate.ResolveApproval(ctx, token, ...)`
// on each; the first gate that does not return `ErrApprovalNotFound`
// is the owning gate (a token can only ever be in ONE gate's pending
// map because the gate constructs the token via Coordinator.Request
// and registers it under its own pending key). The function returns:
//
//   - (true, nil) — the owning gate resolved the token. The gate
//     itself called Coordinator.Resume; the caller MUST NOT
//     double-resume (option A; D-097).
//   - (false, nil) — every gate returned ErrApprovalNotFound. The
//     token belongs to a non-gate pause (an OAuth flow, an A2A
//     `AUTH_REQUIRED`, a deadline-driven pause); the caller falls
//     through to the direct Coordinator.Resume path.
//   - (false, err) — a gate returned a substantive error (scope
//     mismatch, closed gate, coordinator error). Surface loud.
//
// Map-iteration is deliberately unordered: the routing is content-
// addressed (a token belongs to exactly ONE gate), so order does not
// matter for correctness. The gate-iteration cost is O(N_gates) per
// resume; N_gates is the count of approval-gated tools declared in
// `tools.entries[]`, typically <100 even for large catalogs.
func (a *applier) routeThroughGate(ctx context.Context, ev ControlEvent, token pauseresume.Token) (bool, error) {
	// Map the steering ControlType onto the approval-package decision
	// vocabulary. The mapping is exhaustive over the two ControlType
	// values routeThroughGate is reachable from (advancePause guards
	// the call to ApproveOrReject). An unreachable default fails loud
	// rather than silently treating a RESUME as an APPROVE.
	var decision approval.ApprovalDecision
	switch ev.Type {
	case ControlApprove:
		decision = approval.DecisionApprove
	case ControlReject:
		decision = approval.DecisionReject
	default:
		// Unreachable: advancePause already filtered to APPROVE / REJECT
		// before calling routeThroughGate. Fail loud per CLAUDE.md §5.
		return false, fmt.Errorf("steering: routeThroughGate reached with non-approve/reject control %q (invariant violated)",
			string(ev.Type))
	}

	// The wire-side request carries an optional `reason` string under
	// the canonical payload key. Missing / non-string is tolerated as
	// an empty reason — the gate's payload validator accepts an empty
	// reason and the per-tool events surface it verbatim.
	reason, _ := stringFromPayload(ev.Payload, "reason")

	// Elevate the bridge ctx to admin scope. The gate's
	// `ResolveApproval` enforces `protocolauth.HasScope(ctx, ScopeAdmin) ||
	// HasScope(ctx, ScopeConsoleFleet)` as a defence-in-depth check —
	// the Phase 54 Protocol edge already vetted the caller's scope at
	// inbox-Enqueue time via `CheckScope`. The pre-vetted CallerScope
	// on the drained event attests "this came from a sufficiently-
	// privileged caller"; the bridge re-stamps the ctx with the
	// equivalent protocol scope so the gate's check passes. The
	// elevation is scoped to this single ResolveApproval call (a
	// derived ctx, never propagated back to the caller).
	bridgeCtx := protocolauth.WithScopes(ctx,
		[]protocolauth.Scope{protocolauth.ScopeAdmin, protocolauth.ScopeConsoleFleet})

	for _, gate := range a.gates {
		if gate == nil {
			// A nil gate in the map is a misconfiguration (a builder
			// inserted a placeholder entry without constructing the
			// gate). Skip rather than panic — the catalog builder
			// validates entries at boot, so a nil here is an invariant
			// already trapped upstream. Defence-in-depth.
			continue
		}
		err := gate.ResolveApproval(bridgeCtx, token, decision, reason)
		if err == nil {
			// Owning gate resolved the token. Coordinator.Resume was
			// called inside ResolveApproval; the caller MUST NOT
			// double-resume.
			return true, nil
		}
		if errors.Is(err, approval.ErrApprovalNotFound) {
			// This gate did not own the token; try the next gate.
			continue
		}
		// Substantive error: scope mismatch, gate closed, coordinator
		// error, etc. Surface loud — never silently degrade to the
		// fall-through (which would call Coordinator.Resume a second
		// time and emit a misleading `pause.resumed` event).
		return false, fmt.Errorf("steering: routing %s through gate: %w", ev.Type, err)
	}
	// No gate owned the token — fall through to the direct Coordinator.
	// Resume path. This is the canonical OAuth / A2A AUTH_REQUIRED
	// flow when an APPROVE / REJECT control event targets a pause that
	// was NOT minted by an approval gate.
	return false, nil
}

// wireGateTokenFromPayload extracts the gate's pause-token from the
// wire-side APPROVE / REJECT control payload's `token` key. The
// gate's pause token is distinct from the RunLoop's own outstanding
// pause token: the gate mints its token by calling
// Coordinator.Request from inside RunGuarded, and the wire-side
// control request carries that token verbatim so the steering→gate
// bridge can look up the owning gate.
//
// Returns (token, true) when the payload carries a non-empty string
// under "token"; (empty, false) otherwise. A missing token means the
// wire request targets the RunLoop's OWN outstanding pause (the
// planner's `RequestPause`), not a gate's pause — the canonical
// shape for an APPROVE / REJECT against an OAuth or A2A
// `AUTH_REQUIRED` pause.
func wireGateTokenFromPayload(p map[string]any) (pauseresume.Token, bool) {
	if p == nil {
		return "", false
	}
	v, ok := p["token"]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return pauseresume.Token(s), true
}

// decisionFromControlType maps the steering inbox's resume-shaped
// ControlType (RESUME / APPROVE / REJECT) onto the runtime-level
// `pauseresume.Decision` enum. Any other ControlType is a programmer
// error — applyEvent only dispatches the three resume-shaped types into
// advancePause — and surfaces loud rather than silently picking a
// default.
func decisionFromControlType(t ControlType) (pauseresume.Decision, error) {
	switch t {
	case ControlResume:
		return pauseresume.DecisionResume, nil
	case ControlApprove:
		return pauseresume.DecisionApprove, nil
	case ControlReject:
		return pauseresume.DecisionReject, nil
	default:
		return "", fmt.Errorf("%w: %q is not a resume-shaped control",
			ErrUnknownControlType, string(t))
	}
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
