package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// ErrRunCancelled — operation observed a cancelled run. Returned by
// Cancel-aware code paths (worker loop between iterations, reliability
// shell between retries, EmitChunk capacity-waiter, FetchByRun, Emit
// for runs whose cancellation flag is still within the TTL).
//
// Wraps via fmt.Errorf("...: %w", ErrRunCancelled, ...) so callers can
// errors.Is against this sentinel AND read the structured RunError on
// the same chain.
var ErrRunCancelled = errors.New("engine: run cancelled")

// ErrConcurrentFetchByRun — two goroutines called FetchByRun for the
// same RunID at the same time. Per brief 01 §5 ("no half-measure"),
// the dispatcher's per-run subqueue has a single consumer; concurrent
// fetchers fight for ordering and the API forbids the contention
// rather than serializing under the hood.
var ErrConcurrentFetchByRun = errors.New("engine: only one FetchByRun in flight per run")

// cancelObserver is a registered "fire on parent.Cancel(runID)"
// callback. CallSubflow installs one per child engine; Cancel iterates
// observers AFTER setting the per-run flag and fires them outside
// cancelMu so a slow callback can't stall the cancel path.
//
// The observer carries an opaque token (the *cancelObserver itself)
// so the registrant can deregister exactly its entry on the success
// path without affecting siblings. ID-keyed deregistration would race
// with concurrent registrations.
type cancelObserver struct {
	fn func()
}

// runCancellation is the per-run cancellation record. One instance per
// cancelled RunID; created on the first Cancel call. Stored on
// *engine.cancellations under cancelMu.
//
// Lifecycle: created on Cancel; the cancelled flag flips immediately;
// cancelledAt records wall time so the TTL sweeper can prune stale
// entries. droppedCount is incremented by the queued-envelope drain
// path (step 2 of the 4-step cancellation).
//
// Why atomic.Bool for `cancelled`: the worker loop polls it every
// iteration without contending for cancelMu. Map lookup happens once
// per envelope; the cached *runCancellation pointer carries the bool.
type runCancellation struct {
	cancelled    atomic.Bool
	cancelledAt  time.Time
	droppedCount atomic.Int64
}

// DefaultCancelTTL is how long the engine remembers a run's
// cancellation flag after Cancel. Default is 60s — generous enough for
// an operator who pre-computes a RunID and calls Cancel before Emit
// (covering legitimate "abort an inflight client" flows).
const DefaultCancelTTL = 60 * time.Second

// cancelSweepInterval is how often the TTL sweeper runs. Internal
// only; not exposed because the sweep is lifecycle-bookkeeping, not a
// tunable.
const cancelSweepInterval = 10 * time.Second

// WithCancelTTL overrides DefaultCancelTTL. Must be > 0; non-positive
// values silently fall back to the default. The TTL applies to the
// cancellation flag: an Emit landing within the TTL of a Cancel for
// the same RunID is rejected with ErrRunCancelled.
func WithCancelTTL(d time.Duration) Option {
	return func(cfg *engineConfig) {
		if d > 0 {
			cfg.cancelTTL = d
		}
	}
}

// Cancel idempotently cancels the run with the given RunID. Returns
// (true, nil) if the run was active — at least one of: pending
// envelopes in any channel, in-flight worker invocation, non-empty
// per-run egress subqueue, blocked EmitChunk capacity waiter. Returns
// (false, nil) when the run had no observable presence (e.g. already
// completed, never started, second Cancel call).
//
// The four-step propagation per brief 01 §4:
//
//  1. Set the per-run cancellation flag (atomic; observable
//     immediately by every worker / shell loop / capacity waiter).
//  2. Drain queued envelopes for the run from every channel
//     (non-blocking; counted via droppedCount).
//  3. Cancel in-flight worker invocations: workers observe the flag
//     between iterations AND between retries (Phase 11's shell
//     polls ctx.Err and the per-run flag at every loop boundary).
//     Workers return *RunError(CodeRunCancelled).
//  4. Release the run's capacity waiter (Phase 12) so any blocked
//     EmitChunk returns ErrRunCancelled, and drain its egress
//     subqueue so an in-flight FetchByRun returns ErrRunCancelled.
//
// Cancellation TTL: the flag persists for cancelTTL (default 60s) so
// an Emit landing just after Cancel is rejected with ErrRunCancelled.
// A periodic sweeper prunes flags older than the TTL.
//
// Bus emit: best-effort runtime.run_cancelled with the RunCancelledPayload
// shape. A bus error does NOT block Cancel from returning.
func (e *engine) Cancel(ctx context.Context, runID string) (bool, error) {
	if e.stopped.Load() {
		return false, ErrEngineStopped
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if runID == "" {
		return false, errors.New("engine: Cancel requires a non-empty runID")
	}

	// Step 1: flag (idempotent).
	e.cancelMu.Lock()
	rc, existed := e.cancellations[runID]
	if !existed {
		rc = &runCancellation{cancelledAt: time.Now()}
		e.cancellations[runID] = rc
	}
	alreadyCancelled := !rc.cancelled.CompareAndSwap(false, true)
	if alreadyCancelled {
		// Refresh the cancelledAt so the second Cancel resets the
		// TTL window. Useful when an operator retries Cancel after
		// a network blip.
		rc.cancelledAt = time.Now()
	}
	e.cancelMu.Unlock()

	if alreadyCancelled {
		// Idempotent: second Cancel for an already-cancelled run
		// returns false (the run wasn't active in this call's frame
		// of reference).
		return false, nil
	}

	wasActive := false

	// Step 2: drain queued envelopes for this run from every channel.
	// Channels are bounded; we receive non-blockingly until a non-match
	// or the channel is empty. Matches are dropped (counted in
	// droppedCount).
	dropped := e.drainQueuedForRun(runID)
	if dropped > 0 {
		rc.droppedCount.Store(int64(dropped))
		wasActive = true
	}

	// Step 3: in-flight workers observe the flag on their next loop
	// iteration (no synchronous join here — the worker's ctx is the
	// engine's, so we cannot cancel it without affecting other runs).
	// The flag-poll path is the worker's responsibility; see
	// runIsCancelled and the wiring in workerLoop / shell.go.
	//
	// Detect "any worker is mid-invocation for this run" via the
	// activeRuns map (incremented before invoke, decremented after).
	if e.runHasActiveWorkers(runID) {
		wasActive = true
	}

	// Step 4: release Phase 12 capacity waiters + drain the per-run
	// subqueue.
	if e.releaseRunCapacity(runID) {
		wasActive = true
	}
	if e.dispatcher != nil {
		drained := e.dispatcher.cancelRun(runID)
		if drained > 0 {
			wasActive = true
		}
	}

	// Phase 14 follow-up: fire registered cancel observers (subflows
	// mirror parent.Cancel into their child engines). Snapshot under
	// the lock; fire outside so a slow child.Cancel can't stall us.
	for _, obs := range e.snapshotCancelObservers(runID) {
		func() {
			defer func() { _ = recover() }() //nolint:errcheck // swallow observer panic; recovered value not needed
			obs.fn()
		}()
		wasActive = true
	}

	// Best-effort bus emit. Match Phase 08's pattern: a bus error must
	// not block the lifecycle method.
	e.publishRunCancelled(ctx, runID, rc)

	return wasActive, nil
}

// onRunCancelled registers fn to fire when Cancel(runID) is called.
// The returned deregister function removes the observer; callers
// invoke it on the happy-path exit so a completed run doesn't leave a
// dangling callback. Multiple observers per runID are supported.
func (e *engine) onRunCancelled(runID string, fn func()) (deregister func()) {
	if runID == "" || fn == nil {
		return func() {}
	}
	obs := &cancelObserver{fn: fn}
	e.cancelMu.Lock()
	if e.cancelObservers == nil {
		e.cancelObservers = make(map[string][]*cancelObserver)
	}
	e.cancelObservers[runID] = append(e.cancelObservers[runID], obs)
	// If Cancel already fired for this run, fire the new observer
	// immediately so the registrant can't miss the signal due to a
	// race with a recent Cancel call.
	rc, alreadyCancelled := e.cancellations[runID], false
	if rc != nil {
		alreadyCancelled = rc.cancelled.Load()
	}
	e.cancelMu.Unlock()
	if alreadyCancelled {
		func() {
			defer func() { _ = recover() }() //nolint:errcheck // swallow observer panic; recovered value not needed
			obs.fn()
		}()
	}
	return func() {
		e.cancelMu.Lock()
		obs := obs
		list := e.cancelObservers[runID]
		for i, o := range list {
			if o == obs {
				e.cancelObservers[runID] = append(list[:i], list[i+1:]...)
				if len(e.cancelObservers[runID]) == 0 {
					delete(e.cancelObservers, runID)
				}
				break
			}
		}
		e.cancelMu.Unlock()
	}
}

// snapshotCancelObservers returns a copy of the observers registered
// for runID, then deletes the entry so a re-Cancel doesn't double-fire
// the same callbacks.
func (e *engine) snapshotCancelObservers(runID string) []*cancelObserver {
	e.cancelMu.Lock()
	defer e.cancelMu.Unlock()
	obs := e.cancelObservers[runID]
	if len(obs) == 0 {
		return nil
	}
	out := make([]*cancelObserver, len(obs))
	copy(out, obs)
	delete(e.cancelObservers, runID)
	return out
}

// drainQueuedForRun walks every per-adjacency channel + the inlet
// channels + the outlet channel and non-blockingly removes envelopes
// whose RunID matches. Returns the count of dropped envelopes.
//
// We hold no engine lock during the drain — the channels are
// goroutine-safe for receive, and the writers (workers) observe the
// cancel flag on their next iteration so they stop adding new ones.
// A small race window exists where a worker writes one more envelope
// concurrently with the drain; that envelope is observed by the next
// receiver (worker / dispatcher / Fetch), which itself checks the
// cancel flag and discards.
func (e *engine) drainQueuedForRun(runID string) int {
	var dropped int
	tryDrain := func(ch chan messages.Envelope) {
		for {
			select {
			case env, ok := <-ch:
				if !ok {
					return
				}
				if env.RunID == runID {
					dropped++
					continue
				}
				// Wrong run — re-enqueue. We hold no consumer; the
				// re-send is non-blocking and may fail if the channel
				// just filled, in which case we drop the foreign
				// envelope (the worker producing it would also drop it
				// shortly because the channel's now full).
				select {
				case ch <- env:
				default:
				}
				return
			default:
				return
			}
		}
	}
	for _, byTo := range e.channels {
		for _, ch := range byTo {
			tryDrain(ch)
		}
	}
	for _, ch := range e.inletChans {
		tryDrain(ch)
	}
	tryDrain(e.outletChan)
	return dropped
}

// runIsCancelled reports whether runID has an active cancellation
// flag. Hot path: the worker loop polls this every iteration. Returns
// the cached *runCancellation so callers can read droppedCount or
// extend the cached pointer-poll if needed.
func (e *engine) runIsCancelled(runID string) (*runCancellation, bool) {
	if runID == "" {
		return nil, false
	}
	e.cancelMu.Lock()
	rc, ok := e.cancellations[runID]
	e.cancelMu.Unlock()
	if !ok {
		return nil, false
	}
	return rc, rc.cancelled.Load()
}

// runHasActiveWorkers reports whether any worker is currently
// mid-invocation for runID. Used by Cancel's "was the run active"
// determination. Internally synchronized via activeRunsMu.
func (e *engine) runHasActiveWorkers(runID string) bool {
	e.activeRunsMu.Lock()
	defer e.activeRunsMu.Unlock()
	count := e.activeRuns[runID]
	return count > 0
}

// markRunActive / markRunDone bracket the worker's per-invocation
// presence on a run. The pair lets Cancel report (true, nil) when a
// worker is mid-invocation; lets Stop wait for graceful join; lets
// FetchByRun's subqueue cleanup know when no worker can still emit
// a stream frame for the run.
func (e *engine) markRunActive(runID string) {
	if runID == "" {
		return
	}
	e.activeRunsMu.Lock()
	if e.activeRuns == nil {
		e.activeRuns = make(map[string]int)
	}
	e.activeRuns[runID]++
	e.activeRunsMu.Unlock()
}

func (e *engine) markRunDone(runID string) {
	if runID == "" {
		return
	}
	e.activeRunsMu.Lock()
	if c := e.activeRuns[runID]; c > 1 {
		e.activeRuns[runID] = c - 1
	} else {
		delete(e.activeRuns, runID)
	}
	e.activeRunsMu.Unlock()
}

// releaseRunCapacity flips the run's capacity tracker into a "this run
// is cancelled" state. Blocked EmitChunk callers wake and return
// ErrRunCancelled. Returns true if a tracker existed.
func (e *engine) releaseRunCapacity(runID string) bool {
	e.capMu.Lock()
	rc, ok := e.capacities[runID]
	e.capMu.Unlock()
	if !ok {
		return false
	}
	rc.cancel()
	return true
}

// publishRunCancelled is the best-effort runtime.run_cancelled bus
// emit path. Matches Phase 08's "a bus error doesn't block lifecycle"
// contract via the configured RunErrorHandler-style hook.
//
// Phase 13 introduces the RunCancelledHandler seam so the engine
// package stays a leaf (no telemetry import). Production wiring
// installs the handler in cmd/harbor; tests can install a recording
// callback.
func (e *engine) publishRunCancelled(ctx context.Context, runID string, rc *runCancellation) {
	if e.cfg.runCancelledHandler == nil {
		return
	}
	defer func() { _ = recover() }() //nolint:errcheck // bus errors must not block Cancel; recovered value not needed
	e.cfg.runCancelledHandler(ctx, RunCancelledNotice{
		RunID:                runID,
		CancelledAt:          rc.cancelledAt,
		DroppedEnvelopeCount: rc.droppedCount.Load(),
	})
}

// runCancellationSweeper runs in its own goroutine, periodically
// pruning cancellation entries older than cancelTTL. Joined by Stop
// via the engine's wg.
func (e *engine) runCancellationSweeper(ctx context.Context) {
	ticker := time.NewTicker(cancelSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.sweepExpiredCancellations(time.Now())
		}
	}
}

// sweepExpiredCancellations removes entries whose cancelledAt is older
// than cancelTTL. Called by the sweeper goroutine; tests may invoke
// directly to avoid waiting on the ticker.
func (e *engine) sweepExpiredCancellations(now time.Time) {
	ttl := e.cfg.cancelTTL
	if ttl <= 0 {
		ttl = DefaultCancelTTL
	}
	e.cancelMu.Lock()
	defer e.cancelMu.Unlock()
	for runID, rc := range e.cancellations {
		if now.Sub(rc.cancelledAt) > ttl {
			delete(e.cancellations, runID)
		}
	}
}
