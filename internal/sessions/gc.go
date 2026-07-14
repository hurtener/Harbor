package sessions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// startSweeper kicks off the background GC goroutine. The goroutine
// runs at gcPolicy.SweepInterval until done is closed; CloseRegistry
// closes done and joins via wg.
//
// The goroutine ticks on a real-time time.Ticker (not the injected
// Clock) so production sweeping is wall-clock driven; tests that need
// deterministic GC drive GC explicitly via the public GC method.
//
// The parent ctx (`sweeperCtx`) is bound to `r.done` so an in-flight
// `r.GC(...)` call is cancelled as soon as `CloseRegistry` runs —
// without that link, a long sweep blocks `r.wg.Wait()` for up to a
// full SweepInterval after teardown begins. Each tick derives a
// per-iteration timeout from sweeperCtx so a stuck StateStore probe
// cannot stall the sweeper, AND a registry shutdown cancels the
// in-flight probe promptly.
func (r *Registry) startSweeper() {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		sweeperCtx, sweeperCancel := context.WithCancel(context.Background())
		defer sweeperCancel()
		go func() {
			<-r.done
			sweeperCancel()
		}()
		ticker := time.NewTicker(r.gcPolicy.SweepInterval)
		defer ticker.Stop()
		for {
			select {
			case <-sweeperCtx.Done():
				return
			case <-ticker.C:
				// Best-effort sweep; errors are surfaced via events
				// (gc_reaped is emitted per-reaped session) but a
				// transient probe error must not crash the sweeper.
				ctx, cancel := context.WithTimeout(sweeperCtx, r.gcPolicy.SweepInterval)
				_, _ = r.GC(ctx, r.gcPolicy) //nolint:errcheck // best-effort sweep; transient probe error must not crash the sweeper (see doc above)
				cancel()
			}
		}
	}()
}

// GC performs a single sweep pass. For each currently-open session:
//
//   - if RunningProbe returns true: skip (per RFC §6.9 "GC never reaps
//     a session with a RUNNING task").
//   - else if LastSeen + IdleTTL < clock.Now(): close with reason
//     "gc:idle".
//   - else if max(OpenedAt, LastReopenedAt) + HardCap < clock.Now(): close
//     with reason "gc:hard_cap" (the hard cap wins over recent Touch, but a
//     reopen restarts the cap clock so a resumed conversation survives).
//
// Returns the number of sessions reaped and the first probe / close
// error encountered (the sweep continues past errors so a single bad
// session doesn't block the rest).
func (r *Registry) GC(ctx context.Context, policy GCPolicy) (int, error) {
	if r.closed.Load() {
		return 0, ErrRegistryClosed
	}
	policy = policy.withDefaults()
	now := r.clock.Now()

	// Snapshot the open-sessions list under the lock so the sweep
	// iterates over a stable slice without holding the registry lock
	// across StateStore I/O.
	r.mu.Lock()
	pending := make([]identity.Quadruple, 0, len(r.openSessions))
	for _, q := range r.openSessions {
		pending = append(pending, q)
	}
	r.mu.Unlock()

	reaped := 0
	var firstErr error
	for _, q := range pending {
		// Short-circuit if the registry is shutting down.
		if r.closed.Load() {
			break
		}
		// Probe first — if the session has a RUNNING task we never
		// reap, regardless of TTL / hard-cap.
		running := false
		if policy.RunningProbe != nil {
			r2, err := policy.RunningProbe(ctx, q)
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("sessions: GC probe %q: %w", q.SessionID, err)
				}
				continue
			}
			running = r2
		}
		if running {
			continue
		}

		// The load→check→mutate→save runs through mutateSession (one
		// r.mu critical section, shared with Touch / Close / SetTitle)
		// so the reap can never lose a concurrent writer's update — a
		// SetTitle racing this save previously had a window to
		// re-persist Closed=false after the reap, resurrecting the
		// session as a GC-invisible zombie (the lost-update family the
		// serialized read-modify-write path closes). Loading the latest
		// record inside the section also honors a concurrent Touch.
		var reason string
		if _, err := r.mutateSession(ctx, q.Identity, func(s *Session) error {
			if s.Closed {
				// Already closed (e.g. via Close path); just drop the
				// in-memory tracking entry (the mutator runs under r.mu).
				delete(r.openSessions, q.SessionID)
				return errSkipSave
			}
			// The hard cap is measured from max(OpenedAt, LastReopenedAt), NOT
			// OpenedAt alone: a conversation opened long ago
			// and reopened today restarts its hard-cap clock from the reopen, so
			// "resume an old conversation forever" holds — it is not re-reaped on
			// the very next sweep. A never-reopened session has a zero
			// LastReopenedAt, so hardCapAnchor == OpenedAt and the behaviour is
			// unchanged. OpenedAt itself is never refreshed (erasure lifecycle
			// discriminator).
			hardCapAnchor := s.OpenedAt
			if s.LastReopenedAt.After(hardCapAnchor) {
				hardCapAnchor = s.LastReopenedAt
			}
			switch {
			case !hardCapAnchor.IsZero() && now.Sub(hardCapAnchor) > policy.HardCap:
				reason = "gc:hard_cap"
			case !s.LastSeen.IsZero() && now.Sub(s.LastSeen) > policy.IdleTTL:
				reason = "gc:idle"
			default:
				return errSkipSave
			}
			s.Closed = true
			s.ClosedAt = now
			s.ClosedReason = reason
			return nil
		}); err != nil {
			if errors.Is(err, ErrSessionNotFound) {
				// Record disappeared (e.g. via the erasure path).
				// Drop from openSessions and move on.
				r.mu.Lock()
				delete(r.openSessions, q.SessionID)
				r.mu.Unlock()
				continue
			}
			if firstErr == nil {
				firstErr = fmt.Errorf("sessions: GC reap %q: %w", q.SessionID, err)
			}
			continue
		}
		if reason == "" {
			// Skipped: already closed, or neither TTL nor hard-cap hit.
			continue
		}
		r.mu.Lock()
		delete(r.openSessions, q.SessionID)
		r.mu.Unlock()
		reaped++

		// Emit gc_reaped (best-effort; bus errors don't fail the sweep).
		_ = r.bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort emit; bus errors don't fail the sweep (see comment above)
			Type:     EventTypeSessionGCReaped,
			Identity: q,
			Payload: SessionGCReapedPayload{
				SessionID: q.SessionID,
				ReapedAt:  now.UnixNano(),
				Reason:    reason,
			},
		})
	}

	return reaped, firstErr
}
