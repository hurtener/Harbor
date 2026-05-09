package engine

import (
	"context"
	"sync"

	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// dispatcher is the always-on egress demux. A single goroutine reads
// from the engine's outlet channel and routes each envelope into a
// per-RunID subqueue (`map[runID]chan messages.Envelope`). Fetch
// (any-run) returns whichever subqueue has the next envelope; Phase
// 13's FetchByRun(runID) will read from a specific subqueue keyed
// by RunID.
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
//     access to the subqueue map is mutex-guarded.
//   - Subqueues are created lazily on first envelope for a RunID.
type dispatcher struct {
	outlet chan messages.Envelope
	// anyRun receives envelopes that any Fetch (no run filter) can
	// consume. Phase 10's Fetch reads exclusively from anyRun; Phase
	// 13's FetchByRun(runID) will read from runQueues[runID]. Both
	// modes coexist because the dispatcher writes to BOTH targets:
	// the per-run subqueue AND anyRun (the latter so FetchAny doesn't
	// have to scan every subqueue).
	anyRun chan messages.Envelope

	mu        sync.Mutex
	runQueues map[string]chan messages.Envelope
	// subqueueSize is the bounded capacity of each per-run subqueue
	// AND of anyRun. Defaults to the engine's queueSize so the
	// backpressure shape is symmetric across the demux.
	subqueueSize int

	done chan struct{}
	wg   sync.WaitGroup
}

// newDispatcher allocates a dispatcher reading from outlet. The
// caller passes in subqueueSize (typically engine.cfg.queueSize). The
// dispatcher does NOT start its goroutine here — call start(ctx) once
// the engine's internal context is ready.
func newDispatcher(outlet chan messages.Envelope, subqueueSize int) *dispatcher {
	return &dispatcher{
		outlet:       outlet,
		anyRun:       make(chan messages.Envelope, subqueueSize),
		runQueues:    make(map[string]chan messages.Envelope),
		subqueueSize: subqueueSize,
		done:         make(chan struct{}),
	}
}

// start launches the dispatcher goroutine. Returns immediately.
//
// The goroutine ranges over outlet; for each envelope, it allocates
// (lazily) the per-RunID subqueue, then sends the envelope to BOTH
// the subqueue and anyRun. A non-blocking send is used for both
// targets: if either is full, the dispatcher blocks until the
// consumer drains. This preserves backpressure all the way back to
// the worker that produced the envelope.
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

// routeEnvelope sends env to its per-RunID subqueue AND to anyRun.
// Blocks if either target is full — backpressure flows back to the
// emitting worker. Honors ctx cancellation (engine shutdown).
func (d *dispatcher) routeEnvelope(ctx context.Context, env messages.Envelope) {
	subq := d.subqueueFor(env.RunID)
	// Send to per-run subqueue first. A blocked send here implies
	// no Fetcher is draining the run; backpressure is the right
	// answer.
	select {
	case <-ctx.Done():
		return
	case <-d.done:
		return
	case subq <- env:
	}
	// Then to anyRun so FetchAny can see it.
	select {
	case <-ctx.Done():
		return
	case <-d.done:
		return
	case d.anyRun <- env:
	}
}

// subqueueFor returns the per-RunID subqueue, creating it on first
// access. Mutex-guarded so concurrent routes don't double-create.
func (d *dispatcher) subqueueFor(runID string) chan messages.Envelope {
	d.mu.Lock()
	defer d.mu.Unlock()
	q, ok := d.runQueues[runID]
	if !ok {
		q = make(chan messages.Envelope, d.subqueueSize)
		d.runQueues[runID] = q
	}
	return q
}

// fetchAny reads from the any-run channel. Returns ctx.Err() when
// ctx cancels; returns ErrEngineStopped when the channel closes
// (engine shutdown).
func (d *dispatcher) fetchAny(ctx context.Context) (messages.Envelope, error) {
	select {
	case <-ctx.Done():
		return messages.Envelope{}, ctx.Err()
	case env, ok := <-d.anyRun:
		if !ok {
			return messages.Envelope{}, ErrEngineStopped
		}
		return env, nil
	}
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

// closeSubqueues closes anyRun + every per-run subqueue so blocked
// Fetch callers observe channel close. Called from the dispatcher
// goroutine on exit.
func (d *dispatcher) closeSubqueues() {
	d.mu.Lock()
	defer d.mu.Unlock()
	close(d.anyRun)
	for _, q := range d.runQueues {
		close(q)
	}
}
