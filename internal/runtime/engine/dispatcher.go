package engine

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// dispatcher is the always-on egress demux. A single goroutine reads
// from the engine's outlet channel and routes each envelope into a
// per-RunID subqueue (`map[runID]chan messages.Envelope`). Fetch
// (any-run) returns whichever subqueue has the next envelope;
// FetchByRun(runID) reads from a specific subqueue keyed by RunID.
//
// Why always-on: the predecessor's "two egress modes" (pre-dispatcher
// direct fetch vs post-dispatcher per-run demux) is bolted-on and
// surfaces sharp edges (e.g. the "Fetch with no RunID becomes
// unsupported once any RunID-Fetch happens" rule). RFC §6.1 settles
// the call: dispatcher on by default, always — see brief 01 §5.
//
// Concurrency model:
//   - One reader (the dispatcher goroutine) on outlet.
//   - Many writers on outlet (the worker goroutines for outlet nodes).
//   - Many readers on the per-run subqueues via Fetch / FetchByRun;
//     access to the subqueue map is mutex-guarded. A per-run
//     atomic.Bool enforces the single-fetcher-per-run contract.
//   - Subqueues are created lazily on first envelope for a RunID and
//     closed on Cancel(runID) or engine Stop.
type dispatcher struct {
	outlet chan messages.Envelope
	// anyRun receives envelopes that any Fetch (no run filter) can
	// consume. Phase 10's Fetch reads exclusively from anyRun;
	// FetchByRun(runID) reads from runQueues[runID]. Both modes coexist
	// because the dispatcher writes to BOTH targets: the per-run
	// subqueue AND anyRun (the latter so FetchAny doesn't have to scan
	// every subqueue).
	anyRun chan messages.Envelope

	mu        sync.Mutex
	runQueues map[string]chan messages.Envelope
	// fetcherActive[runID] tracks whether a FetchByRun is currently in
	// flight for that run. Used to enforce the
	// "concurrent FetchByRun forbidden" contract (brief 01 §5). The
	// flag is set on entry and cleared on exit; only one goroutine
	// holds it at a time per run.
	fetcherActive map[string]*atomic.Bool
	// subscribed records runIDs that have had at least one FetchByRun
	// call. Once a run is subscribed, the dispatcher's per-run
	// subqueue write becomes BLOCKING (backpressure-bound delivery).
	// Until then, the write is non-blocking with drop-on-full so a
	// Fetch-only consumer doesn't backpressure-cascade the dispatcher
	// (the Phase 12 cross-run no-deadlock guarantee). Set on first
	// FetchByRun for a run; never cleared (subscription is permanent
	// for the run's lifetime).
	subscribed map[string]struct{}
	// cancelled tracks runIDs whose subqueues have been closed by
	// Cancel(runID). FetchByRun checks this before opening a new
	// subscription so a race between Cancel and FetchByRun returns
	// ErrRunCancelled deterministically.
	cancelled map[string]struct{}
	// subqueueSize is the bounded capacity of each per-run subqueue
	// AND of anyRun. Defaults to the engine's queueSize so the
	// backpressure shape is symmetric across the demux.
	subqueueSize int

	// onFetched is invoked after the consumer has drained an envelope
	// via fetchAny or fetchByRun. Used by Phase 12 to release the
	// run's streaming capacity counter so EmitChunk waiters can wake.
	// nil = no-op (Phase 10 behavior). The callback runs synchronously
	// on the calling goroutine; keep it short.
	//
	// Why fetch-time and not route-time: the producer must block
	// until the CONSUMER catches up, not until the dispatcher has
	// queued the frame. Releasing on dispatcher-route would let a
	// fast dispatcher unblock the producer ahead of an unresponsive
	// consumer, defeating the backpressure semantic the test
	// `TestEmitChunk_BlocksAtCapacity_ReleasedOnDrain` pins.
	onFetched func(messages.Envelope)

	done chan struct{}
	wg   sync.WaitGroup
}

// newDispatcher allocates a dispatcher reading from outlet. The
// caller passes in subqueueSize (typically engine.cfg.queueSize) and
// an optional onFetched callback fired after each successful Fetch.
// The dispatcher does NOT start its goroutine here — call start(ctx)
// once the engine's internal context is ready.
func newDispatcher(outlet chan messages.Envelope, subqueueSize int, onFetched func(messages.Envelope)) *dispatcher {
	return &dispatcher{
		outlet:        outlet,
		anyRun:        make(chan messages.Envelope, subqueueSize),
		runQueues:     make(map[string]chan messages.Envelope),
		fetcherActive: make(map[string]*atomic.Bool),
		subscribed:    make(map[string]struct{}),
		cancelled:     make(map[string]struct{}),
		subqueueSize:  subqueueSize,
		onFetched:     onFetched,
		done:          make(chan struct{}),
	}
}

// start launches the dispatcher goroutine. Returns immediately.
//
// The goroutine ranges over outlet; for each envelope, it allocates
// (lazily) the per-RunID subqueue, then sends the envelope to BOTH
// the subqueue and anyRun. Both writes block until the consumer
// drains; this preserves backpressure all the way back to the worker.
//
// Shutdown: Stop closes outlet (after joining workers); the
// dispatcher's range loop exits, then it closes anyRun + every
// per-run subqueue so blocked Fetch callers observe channel close
// (returning ErrEngineStopped).
func (d *dispatcher) start(ctx context.Context) {
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer d.closeSubqueues()
		for {
			select {
			case <-ctx.Done():
				return
			case <-d.done:
				return
			case env, ok := <-d.outlet:
				if !ok {
					return
				}
				d.routeEnvelope(ctx, env)
			}
		}
	}()
}

// routeEnvelope sends env to anyRun (the consumer-facing FIFO) AND to
// its per-RunID subqueue. The subqueue write mode depends on whether
// any FetchByRun call has subscribed to the run:
//
//   - SUBSCRIBED: blocking write. A stalled FetchByRun consumer
//     backpressures the dispatcher, which backpressures the worker
//     via the outlet channel — symmetric with the anyRun path. This
//     matches Phase 13's "no half-measure" delivery contract for
//     FetchByRun consumers (brief 01 §5).
//   - UNSUBSCRIBED: non-blocking write with drop on full. A
//     Fetch-only consumer (any-run) doesn't drain the per-run
//     subqueue; without this fall-through, the subqueue would fill
//     and cascade-block every worker emitting on that run (the
//     Phase 12 cross-run no-deadlock failure). The anyRun write is
//     still bounded-blocking so the run's frames remain visible to
//     Fetch.
//
// A run that's been Cancel'd skips the subqueue write entirely (the
// subqueue is closed). The anyRun write still occurs — callers using
// any-run Fetch see the trailing envelope before subsequent envelopes
// from un-cancelled runs arrive.
//
// Honors ctx cancellation (engine shutdown) on both writes.
func (d *dispatcher) routeEnvelope(ctx context.Context, env messages.Envelope) {
	subq, mode := d.subqueueForSend(env.RunID)
	switch mode {
	case sendBlocking:
		select {
		case <-ctx.Done():
			return
		case <-d.done:
			return
		case subq <- env:
		}
	case sendNonBlocking:
		select {
		case subq <- env:
		default:
			// Drop: no FetchByRun consumer subscribed; the frame is
			// still delivered via anyRun below.
		}
	case sendSkip:
		// Run is cancelled; no subqueue write.
	}
	// anyRun is the consumer-facing FIFO; this send must succeed for
	// the frame to be visible. Block on full anyRun (backpressure
	// flows back to the worker).
	select {
	case <-ctx.Done():
		return
	case <-d.done:
		return
	case d.anyRun <- env:
	}
}

// sendMode classifies how routeEnvelope writes to a per-run subqueue.
type sendMode int

const (
	sendSkip sendMode = iota
	sendBlocking
	sendNonBlocking
)

// subqueueForSend returns the per-RunID subqueue + the write mode.
// Returns (nil, sendSkip) when the run has been Cancel'd. Otherwise,
// blocking when a FetchByRun consumer has subscribed (the subqueue is
// the contract for that run's delivery), non-blocking with drop on
// full when no consumer has subscribed (preserving Phase 12's
// cross-run no-deadlock guarantee).
//
// Lazy-creates the subqueue on first access either way: the buffer
// exists in case a future FetchByRun arrives and wants to drain
// already-routed envelopes (best-effort up to subqueueSize).
func (d *dispatcher) subqueueForSend(runID string) (chan messages.Envelope, sendMode) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, isCancelled := d.cancelled[runID]; isCancelled {
		return nil, sendSkip
	}
	q, ok := d.runQueues[runID]
	if !ok {
		q = make(chan messages.Envelope, d.subqueueSize)
		d.runQueues[runID] = q
	}
	if _, sub := d.subscribed[runID]; sub {
		return q, sendBlocking
	}
	return q, sendNonBlocking
}

// fetchAny reads from the any-run channel. Returns ctx.Err() when
// ctx cancels; returns ErrEngineStopped when the channel closes
// (engine shutdown). On a successful read, fires onFetched (Phase
// 12's streaming capacity-release hook).
func (d *dispatcher) fetchAny(ctx context.Context) (messages.Envelope, error) {
	select {
	case <-ctx.Done():
		return messages.Envelope{}, ctx.Err()
	case env, ok := <-d.anyRun:
		if !ok {
			return messages.Envelope{}, ErrEngineStopped
		}
		if d.onFetched != nil {
			d.onFetched(env)
		}
		return env, nil
	}
}

// fetchByRun reads from the per-run subqueue. Enforces the
// single-fetcher-per-run contract: concurrent fetchers return
// ErrConcurrentFetchByRun. Returns ErrRunCancelled when the subqueue
// has been closed by Cancel(runID); returns ErrEngineStopped when the
// engine has shut down.
func (d *dispatcher) fetchByRun(ctx context.Context, runID string) (messages.Envelope, error) {
	flag := d.acquireFetcher(runID)
	if !flag.CompareAndSwap(false, true) {
		return messages.Envelope{}, ErrConcurrentFetchByRun
	}
	defer flag.Store(false)

	q, cancelled := d.subqueueForFetch(runID)
	if cancelled {
		return messages.Envelope{}, ErrRunCancelled
	}

	select {
	case <-ctx.Done():
		return messages.Envelope{}, ctx.Err()
	case env, ok := <-q:
		if !ok {
			// Subqueue closed: either Cancel(runID) fired or engine
			// Stop fired. Distinguish via the cancelled map.
			d.mu.Lock()
			_, wasCancelled := d.cancelled[runID]
			d.mu.Unlock()
			if wasCancelled {
				return messages.Envelope{}, ErrRunCancelled
			}
			return messages.Envelope{}, ErrEngineStopped
		}
		if d.onFetched != nil {
			d.onFetched(env)
		}
		return env, nil
	}
}

// subqueueForFetch returns (queue, false) when a real subqueue exists
// for runID, or (nil, true) when Cancel(runID) has closed the queue.
// Creates the subqueue lazily so a FetchByRun call that arrives
// before any Emit observes a real (empty) queue and blocks until the
// first envelope arrives.
//
// Side effect: marks the run as subscribed so future routeEnvelope
// writes for this run become blocking (reliable delivery). Once
// marked, the subscription is permanent for the run's lifetime —
// every subsequent envelope is backpressure-bound.
func (d *dispatcher) subqueueForFetch(runID string) (chan messages.Envelope, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, isCancelled := d.cancelled[runID]; isCancelled {
		return nil, true
	}
	q, ok := d.runQueues[runID]
	if !ok {
		q = make(chan messages.Envelope, d.subqueueSize)
		d.runQueues[runID] = q
	}
	d.subscribed[runID] = struct{}{}
	return q, false
}

// acquireFetcher returns the per-run fetcher flag, creating it on
// first access. Mutex-guarded; the returned *atomic.Bool is owned by
// the caller for the duration of the FetchByRun call.
func (d *dispatcher) acquireFetcher(runID string) *atomic.Bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	flag, ok := d.fetcherActive[runID]
	if !ok {
		flag = &atomic.Bool{}
		d.fetcherActive[runID] = flag
	}
	return flag
}

// cancelRun closes the per-run subqueue and marks the run as
// cancelled. Subsequent FetchByRun calls return ErrRunCancelled
// immediately. In-flight FetchByRun calls observe the channel close
// and return ErrRunCancelled.
//
// Returns the count of envelopes drained from the subqueue at close
// time. Each drained envelope contributes to the Cancel "was the run
// active" determination.
func (d *dispatcher) cancelRun(runID string) int {
	d.mu.Lock()
	if _, already := d.cancelled[runID]; already {
		d.mu.Unlock()
		return 0
	}
	d.cancelled[runID] = struct{}{}
	q, hasQueue := d.runQueues[runID]
	if hasQueue {
		// Remove from runQueues so any future routeEnvelope (if it
		// races with the cancel) takes the cancelled-skip branch
		// rather than re-creating the subqueue.
		delete(d.runQueues, runID)
	}
	d.mu.Unlock()

	if !hasQueue {
		return 0
	}
	// Drain pending envelopes BEFORE closing so an in-flight
	// FetchByRun that's already past the cancel-check observes a
	// closed channel rather than a partial drain.
	drained := 0
drainLoop:
	for {
		select {
		case _, ok := <-q:
			if !ok {
				break drainLoop
			}
			drained++
		default:
			break drainLoop
		}
	}
	close(q)
	return drained
}

// stop signals the dispatcher to exit and waits for its goroutine
// to join. Idempotent.
func (d *dispatcher) stop() {
	select {
	case <-d.done:
		// already stopped
	default:
		close(d.done)
	}
	d.wg.Wait()
}

// closeSubqueues closes anyRun + every per-run subqueue (skipping
// runs already closed by cancelRun). Called from the dispatcher
// goroutine on exit.
func (d *dispatcher) closeSubqueues() {
	d.mu.Lock()
	defer d.mu.Unlock()
	close(d.anyRun)
	for runID, q := range d.runQueues {
		// Skip queues that cancelRun already closed (defensive: the
		// runQueues map should already be cleared for those, but the
		// double-close is real if a race lands here).
		if _, isCancelled := d.cancelled[runID]; isCancelled {
			continue
		}
		close(q)
	}
}
