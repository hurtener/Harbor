package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// ErrSubflowFactoryFailed — the factory passed to CallSubflow returned
// a non-nil error. The factory result is not used; the caller's err
// chain wraps the original.
var ErrSubflowFactoryFailed = errors.New("engine: subflow factory failed")

// SubflowFactory returns a fresh child Engine per call. Caller never
// reuses subflow engines; cheap construction is the contract per
// brief 01 §5 ("a subflow is a freshly-built engine that runs to
// completion for one parent envelope, then Stops").
type SubflowFactory func() (Engine, error)

// CallSubflow constructs a child engine via factory, runs it under
// the parent's RunID, mirrors the parent's ctx cancellation AND the
// parent's Engine.Cancel(parentRunID) into the child, returns the
// first egress envelope, then Stops the child.
//
// **Cancellation scope (Phase 13/14):** two paths cooperate.
//   - ctx propagation: childCtx is derived from ctx via WithCancel,
//     so a parent ctx cancel terminates the child immediately.
//   - Engine.Cancel mirroring: the parent engine fires registered
//     observers when Engine.Cancel(parentRunID) lands. CallSubflow
//     installs an observer that calls child.Cancel(parentRunID), so
//     a steering-side cancel from the parent's Protocol surface
//     reaches the child without requiring the parent's worker ctx
//     to die.
//
// Multi-result subflows compose via concurrency.MapConcurrent over a
// list of factories; CallSubflow itself returns exactly one envelope.
//
// Cleanup ordering on success: drain first egress → deregister cancel
// observer → cancel watcher ctx → child.Stop. On factory error or
// child.Run failure, the child's Stop is still invoked (defer) so no
// goroutines leak.
//
// Identity propagation: the parent envelope (parentEnv) carries the
// quadruple. The child engine sees it via the inbound envelope on its
// inlet; no separate identity copy is needed because Envelope is the
// identity carrier (RFC §6.1).
func (nctx *NodeContext) CallSubflow(ctx context.Context, factory SubflowFactory, parentEnv messages.Envelope) (messages.Envelope, error) {
	if factory == nil {
		return messages.Envelope{}, errors.New("engine: CallSubflow requires a non-nil factory")
	}

	child, err := factory()
	if err != nil {
		return messages.Envelope{}, fmt.Errorf("%w: %w", ErrSubflowFactoryFailed, err)
	}
	if child == nil {
		return messages.Envelope{}, fmt.Errorf("%w: factory returned nil engine", ErrSubflowFactoryFailed)
	}

	childCtx, cancelChild := context.WithCancel(ctx)
	defer cancelChild()

	// Phase 13: mirror parent.Cancel(parentRunID) into the child
	// engine. The observer fires synchronously from the parent's
	// Cancel goroutine; child.Cancel runs against its own locks so
	// the call doesn't deadlock the parent. A timed bounded context
	// keeps a misbehaving child from stalling the parent's Cancel.
	deregister := func() {}
	if nctx != nil && nctx.engine != nil && parentEnv.RunID != "" {
		deregister = nctx.engine.onRunCancelled(parentEnv.RunID, func() {
			cctx, ccancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer ccancel()
			_, _ = child.Cancel(cctx, parentEnv.RunID)
			cancelChild()
		})
	}
	defer deregister()

	if err := child.Run(childCtx); err != nil {
		stopChildBestEffort(child)
		return messages.Envelope{}, fmt.Errorf("engine: subflow Run: %w", err)
	}

	// Best-effort Stop on every exit path. Wrap in a tiny helper so
	// the multiple return points stay readable.
	defer stopChildBestEffort(child)

	if err := child.Emit(childCtx, parentEnv); err != nil {
		return messages.Envelope{}, fmt.Errorf("engine: subflow Emit: %w", err)
	}

	envOut, err := child.Fetch(childCtx)
	if err != nil {
		return messages.Envelope{}, fmt.Errorf("engine: subflow Fetch: %w", err)
	}
	return envOut, nil
}

// stopChildBestEffort calls Stop with a short bounded deadline. If
// the deadline expires the operator can force-kill the process; the
// child's goroutines will GC when the program exits.
func stopChildBestEffort(child Engine) {
	stopCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = child.Stop(stopCtx)
}
