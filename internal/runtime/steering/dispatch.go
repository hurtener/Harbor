package steering

import (
	"context"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
)

// execOutcome carries the ToolExecutor's three-value return across the
// per-step dispatch goroutine boundary. Internal — never
// exported; the run loop unpacks it right after the join.
type execOutcome struct {
	observation    any
	llmObservation any
	err            error
}

// dispatchDecision executes the planner's non-Finish, non-RequestPause
// decision via the ToolExecutor on a per-step goroutine and, while the
// execution is in flight, keeps draining the run's steering inbox —
// routing ONLY approval-bridge-eligible APPROVE / REJECT controls (the
// gate bridge) mid-step. This is the deadlock fix: an
// approval-gated tool parks inside `gate.RunGuarded` until
// `ResolveApproval` fires, and previously the ONLY production caller
// of `ResolveApproval` was the step-boundary drain — which the
// synchronous ExecuteDecision call was itself blocking. A
// planner-dispatched gated tool therefore hung the run until ctx
// cancellation. Dispatching on a per-step goroutine and bridging
// APPROVE / REJECT mid-step closes the cycle.
//
// Semantics (the smallest delta over the earlier synchronous path):
//
//   - ONLY bridge-eligible APPROVE / REJECT controls (a gate-owned
//     wire `token` — see applier.routeApprovalControl) are consumed
//     mid-step. A consumed control gets the same control.received /
//     control.applied lifecycle emits + control-history record the
//     boundary path produces, and is NOT re-applied at the next
//     boundary (consumed is consumed).
//   - Other drained controls (PAUSE / RESUME / soft CANCEL / REDIRECT /
//     INJECT_CONTEXT / USER_MESSAGE / PRIORITIZE, and any APPROVE /
//     REJECT no gate owns) keeps its step-boundary semantics: it is
//     returned in `deferred` and the run loop merges it ahead of the
//     next boundary's fresh drain (FIFO preserved), where it gets the
//     full applyEvent treatment — exactly when it would have been
//     applied under the synchronous dispatch (the step was in flight;
//     the boundary is the first point it could ever act).
//   - Hard CANCEL already cancelled the run context at inbox admission.
//     The execution's child stepCtx interrupts an in-flight gated decision;
//     queued execution checks it before dispatch. Terminal bookkeeping
//     records the accepted cancellation after the executor joins.
//   - The per-step goroutine is ALWAYS joined before return — on the
//     happy path, on run-ctx cancellation, and on a bridge error (where
//     stepCtx is cancelled first so a parked RunGuarded waiter
//     unblocks). No goroutine outlives the step (§11
//     goroutine-leak gate). All dispatch state lives on this
//     goroutine's stack — nothing lands on the RunLoop struct.
//
// Returns the executor's outcome, the deferred (still-unapplied)
// control events, and a non-nil error ONLY when a mid-step bridge
// apply failed substantively (the same fail-loud posture as a
// step-boundary apply failure — the run loop returns it verbatim).
func (rl *RunLoop) dispatchDecision(
	ctx context.Context,
	q identity.Quadruple,
	inbox *Inbox,
	generation uint64,
	exec ToolExecutor,
	rc planner.RunContext,
	decision planner.Decision,
) (execOutcome, []ControlEvent, error) {
	// stepCtx scopes the in-flight execution to THIS step: it inherits
	// the run ctx (identity values + run-level cancellation propagate)
	// and is additionally cancelled when the mid-step drain must abort
	// the execution (a bridge error, a retired inbox).
	stepCtx, cancelStep := context.WithCancel(ctx)
	defer cancelStep()
	stepCtx, fenceErr := inbox.fenceInvocation(stepCtx, generation)
	if fenceErr != nil {
		return execOutcome{err: fenceErr}, nil, nil
	}

	// done is 1-buffered so the executor goroutine's send never blocks
	// — the goroutine always runs to completion and every return path
	// below receives from it exactly once (the join).
	done := make(chan execOutcome, 1)
	go func() {
		// Stop may win while the durable intent is being written or while
		// this goroutine waits to run. Do not enter a queued executor then.
		if err := stepCtx.Err(); err != nil {
			done <- execOutcome{err: err}
			return
		}
		if !inbox.admitDecision(generation, false) {
			done <- execOutcome{err: errDecisionSuperseded}
			return
		}
		obs, llmObs, err := exec.ExecuteDecision(stepCtx, rc, decision)
		done <- execOutcome{observation: obs, llmObservation: llmObs, err: err}
	}()

	var deferred []ControlEvent
	for {
		// Race the in-flight execution against the next inbox wake.
		// waitRes is 1-buffered and received on every path, so the
		// waiter goroutine never outlives the step.
		waitCtx, cancelWait := context.WithCancel(ctx)
		waitRes := make(chan error, 1)
		go func() { waitRes <- inbox.WaitForEvent(waitCtx) }()

		select {
		case out := <-done:
			cancelWait()
			<-waitRes // join the waiter goroutine
			return out, deferred, nil
		case werr := <-waitRes:
			cancelWait()
			if werr != nil {
				// The run ctx was cancelled (stepCtx is a child, so
				// the executor is unwinding too) or the inbox was
				// retired out-of-band. Abort the in-flight execution,
				// join, and hand the outcome back — the next step
				// boundary surfaces the underlying condition
				// (ctx.Err() / Drain's ErrInboxNotFound) exactly as
				// the earlier synchronous dispatch did.
				cancelStep()
				out := <-done
				return out, deferred, nil
			}
		}

		drained, derr := inbox.Drain()
		if derr != nil {
			// Retired between the wake and the Drain — same posture
			// as the WaitForEvent error path above.
			cancelStep()
			out := <-done
			return out, deferred, nil
		}
		for i, ev := range drained {
			routed, rerr := rl.applier.routeApprovalControl(ctx, ev)
			if rerr != nil {
				// A substantive gate error is loud — the same posture
				// as a step-boundary apply failure. Record the same
				// lifecycle + history footprint, abort the in-flight
				// execution (the parked RunGuarded waiter unblocks via
				// stepCtx), JOIN, and surface the error. The remaining
				// drained events ride back in `deferred` so nothing is
				// silently dropped on the error path.
				rl.emitLifecycle(ctx, q, ev.Type, EventTypeControlReceived, "")
				rl.history.record(q.SessionID, AppliedControl{
					Type:      ev.Type,
					RunID:     q.RunID,
					AppliedAt: rl.clock.Now(),
					Err:       rerr,
				})
				rl.emitLifecycle(ctx, q, ev.Type, EventTypeControlApplied, classifyApplyErr(rerr))
				cancelStep()
				out := <-done // join and retain any returned evidence
				deferred = append(deferred, drained[i+1:]...)
				return out, deferred, rerr
			}
			if !routed {
				// Not bridge-eligible mid-step. Defer verbatim — the
				// next step boundary gives it the full applyEvent
				// treatment, INCLUDING its control.received /
				// control.applied lifecycle emits and history record
				// (emitted once, at apply time — never duplicated).
				deferred = append(deferred, ev)
				continue
			}
			// Consumed mid-step: an owning gate resolved the token and
			// unblocked its RunGuarded waiter. Record the same
			// lifecycle + history footprint the boundary path records.
			rl.emitLifecycle(ctx, q, ev.Type, EventTypeControlReceived, "")
			rl.history.record(q.SessionID, AppliedControl{
				Type:      ev.Type,
				RunID:     q.RunID,
				AppliedAt: rl.clock.Now(),
			})
			rl.emitLifecycle(ctx, q, ev.Type, EventTypeControlApplied, "")
		}
	}
}
