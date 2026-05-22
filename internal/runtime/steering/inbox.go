package steering

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// ControlEvent is the canonical steering record (RFC §6.3, brief 02
// §2 `ControlEvent`). It is what the Protocol edge constructs from an
// inbound control request, validates, scope-checks, and enqueues on
// the run's Inbox. Phase 53 drains these between planner steps and
// projects the result onto RunContext.Control.
type ControlEvent struct {
	EnqueuedAt   time.Time
	Payload      map[string]any
	Identity     identity.Quadruple
	Type         ControlType
	CallerScope  Scope
	CallerTenant string
	EventID      string
}

// Inbox is the per-run steering inbox. The Runtime owns it; planners
// never touch it (RFC §6.3 — "the Runtime owns the inbox"; planners
// observe RunContext.Control only). It is a FIFO enqueue + drain
// surface, identity-scoped to exactly one run quadruple.
//
// An Inbox is concurrent-safe: N Protocol-edge goroutines may
// Enqueue while the run loop Drains, all against one Inbox — the
// queue is mutex-guarded (D-025). Per-run state never leaks across
// runs because each Inbox holds only its own run's events and is
// keyed by its own quadruple.
//
// Construct an Inbox via Registry.Open; do not construct one
// directly.
type Inbox struct {
	clock    Clock
	notify   chan struct{}
	identity identity.Quadruple
	queue    []ControlEvent
	mu       sync.Mutex
	closed   bool
}

// Identity returns the run quadruple this Inbox is scoped to.
func (in *Inbox) Identity() identity.Quadruple { return in.identity }

// Enqueue validates, scope-checks, and appends a ControlEvent to the
// inbox queue. It fails closed (no silent drop — CLAUDE.md §5):
//
//   - ErrIdentityRequired — ev.Identity is not the Inbox's own
//     quadruple (a control for another run must never land here), or
//     the quadruple is incomplete.
//   - ErrUnknownControlType — ev.Type is not a canonical type.
//   - ErrScopeMismatch / ErrInvalidScope — the caller scope is below
//     the type's RFC §6.3 minimum, or a cross-tenant non-admin
//     submission.
//   - ErrPayloadInvalid / ErrUnsupportedPayloadValue — the payload
//     violated an RFC §6.3 bound or carried an unencodable leaf.
//   - ErrInboxNotFound — the inbox has been retired (Close called).
//
// On success the event's EnqueuedAt is stamped from the Inbox's
// Clock and the event is appended. A caller-supplied non-zero
// EnqueuedAt is rejected with ErrPayloadInvalid — the Inbox owns the
// timeline.
func (in *Inbox) Enqueue(ev ControlEvent) error {
	// Identity must be the inbox's own run quadruple. This is the
	// per-run isolation gate: an event for run A enqueued on run B's
	// inbox would be cross-run bleed.
	if err := validateQuadruple(ev.Identity); err != nil {
		return err
	}
	if ev.Identity != in.identity {
		return fmt.Errorf("%w: event identity %+v does not match inbox identity %+v",
			ErrIdentityRequired, ev.Identity, in.identity)
	}
	if !ev.EnqueuedAt.IsZero() {
		return fmt.Errorf("%w: caller pre-filled EnqueuedAt; the inbox owns the timeline", ErrPayloadInvalid)
	}
	if !IsValidControlType(ev.Type) {
		return fmt.Errorf("%w: %q", ErrUnknownControlType, string(ev.Type))
	}
	if err := CheckScope(ev.Type, ev.CallerScope, ev.CallerTenant, ev.Identity); err != nil {
		return err
	}
	if err := ValidatePayload(ev.Payload); err != nil {
		return err
	}

	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return fmt.Errorf("%w: %+v", ErrInboxNotFound, in.identity)
	}
	ev.EnqueuedAt = in.clock.Now()
	in.queue = append(in.queue, ev)

	// Coalesced wake: a non-blocking send on the 1-buffered notify
	// channel. A waiter (Phase 53's RunLoop, blocked in WaitForEvent
	// while a pause is outstanding) wakes and Drains everything; a full
	// buffer means a wake is already pending — N enqueues coalesce to
	// one wake, which is correct because the waiter always drains the
	// whole queue. The send is done UNDER the lock: close() also runs
	// under the lock and sets in.closed first, so a send here can never
	// race with (or land after) the close — in.closed is false here by
	// the guard above, and close() cannot interleave while we hold mu.
	if in.notify != nil {
		select {
		case in.notify <- struct{}{}:
		default:
		}
	}
	return nil
}

// WaitForEvent blocks until the inbox has at least one queued event, the
// inbox is retired, or ctx is cancelled. It is the surface Phase 53's
// RunLoop uses to wait — without busy-spinning — for a steering control
// to arrive while a run is paused (a pause is outstanding and the
// planner must not be re-entered until a RESUME / APPROVE / REJECT lands).
//
// It returns:
//
//   - nil — an event is queued; the caller should Drain.
//   - ctx.Err() — the wait was cancelled.
//   - ErrInboxNotFound — the inbox was retired while waiting.
//
// WaitForEvent does NOT itself Drain — the caller calls Drain after a
// nil return. This keeps Drain the single atomic-remove surface.
func (in *Inbox) WaitForEvent(ctx context.Context) error {
	for {
		in.mu.Lock()
		if in.closed {
			in.mu.Unlock()
			return fmt.Errorf("%w: %+v", ErrInboxNotFound, in.identity)
		}
		if len(in.queue) > 0 {
			in.mu.Unlock()
			return nil
		}
		notify := in.notify
		in.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case _, ok := <-notify:
			if !ok {
				// notify closed by close() — the inbox was retired.
				return fmt.Errorf("%w: %+v", ErrInboxNotFound, in.identity)
			}
			// Woken — loop back, re-check under the lock (the queue may
			// have been drained by another goroutine between the wake
			// and the re-check; that is fine, we just wait again).
		}
	}
}

// Drain atomically removes and returns every queued ControlEvent in
// FIFO order, leaving the inbox empty. This is the surface Phase 53's
// run loop calls between planner steps. Drain on an empty inbox
// returns an empty (non-nil) slice. Drain on a retired inbox returns
// ErrInboxNotFound.
//
// The returned slice is owned by the caller — the Inbox keeps no
// reference to it.
func (in *Inbox) Drain() ([]ControlEvent, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return nil, fmt.Errorf("%w: %+v", ErrInboxNotFound, in.identity)
	}
	drained := in.queue
	in.queue = nil
	if drained == nil {
		return []ControlEvent{}, nil
	}
	return drained, nil
}

// Len returns the number of currently-queued events. Primarily for
// tests and observability; Phase 53's run loop uses Drain.
func (in *Inbox) Len() int {
	in.mu.Lock()
	defer in.mu.Unlock()
	return len(in.queue)
}

// close retires the inbox: any queued-but-undrained events are
// dropped (the run is ending; there is nothing left to apply them
// to) and further Enqueue / Drain calls fail with ErrInboxNotFound.
// close is idempotent. Called by Registry.Retire — not exported,
// because inbox lifecycle is the Registry's responsibility.
func (in *Inbox) close() {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		// Idempotent: a second close must not double-close notify.
		return
	}
	in.closed = true
	in.queue = nil
	if in.notify != nil {
		// Closing notify unblocks any WaitForEvent waiter with
		// ErrInboxNotFound — a retired inbox's waiter must not hang.
		close(in.notify)
	}
}

// validateQuadruple fails closed on an incomplete run quadruple. The
// run component is mandatory — the inbox is per-run — in addition to
// the (tenant, user, session) triple.
func validateQuadruple(q identity.Quadruple) error {
	if err := identity.Validate(q.Identity); err != nil {
		return fmt.Errorf("%w: %w", ErrIdentityRequired, err)
	}
	if q.RunID == "" {
		return fmt.Errorf("%w: run_id empty", ErrIdentityRequired)
	}
	return nil
}
