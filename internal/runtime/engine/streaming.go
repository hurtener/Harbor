package engine

import (
	"context"
	"errors"
	"sync"

	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// StreamFrame is a chunked payload tied to a parent run. StreamID
// defaults to RunID; sub-streams within a run use a custom StreamID.
// Seq is monotonic per StreamID and is engine-assigned (callers must
// NOT pre-fill — the engine rejects with ErrSeqProvided).
//
// Per-stream order is preserved as long as a single goroutine emits
// per StreamID — the dispatcher's per-run subqueue is FIFO. Two
// goroutines emitting on the same StreamID concurrently will
// interleave (the engine has no per-StreamID lock); operators who
// need cross-goroutine ordering must serialize the emit themselves.
type StreamFrame struct {
	Meta     map[string]any
	StreamID string
	Text     string
	Seq      int
	Done     bool
}

// Sentinel errors specific to streaming. Phase 10 sentinels (e.g.
// ErrEngineStopped, ErrIdentityRequired) cover engine-wide failures;
// these cover stream-shape-specific ones.
var (
	// ErrSeqProvided — caller pre-filled StreamFrame.Seq. The engine
	// owns sequence assignment so callers can't accidentally desync
	// the per-StreamID monotonic invariant.
	ErrSeqProvided = errors.New("engine: caller pre-filled StreamFrame.Seq; engine owns sequencing")
	// ErrStreamClosed — EmitChunk called for a StreamID whose Done
	// frame already drained. The dispatcher deletes the StreamID's
	// Seq counter on Done; subsequent calls fail loud rather than
	// silently re-opening the stream.
	ErrStreamClosed = errors.New("engine: stream closed (Done frame already drained)")
	// ErrEmptyRunID — EmitChunk called from a NodeFunc that was
	// invoked on an envelope without a RunID. Stream backpressure is
	// keyed by RunID; an empty RunID would collapse all anonymous
	// streams onto one bucket.
	ErrEmptyRunID = errors.New("engine: EmitChunk requires a non-empty RunID on the originating envelope")
)

// runCapacity is the per-run streaming bookkeeping. One instance per
// active RunID; created lazily on first EmitChunk for the run. Stored
// on *engine.capacities under capMu.
//
// Concurrency: all field access is guarded by mu. cond.Signal() is
// called under mu so a waiter that re-checks pending >= capacity
// after wakeup observes a consistent counter. cond.Broadcast() in
// Stop ensures every waiter observes stopped=true.
type runCapacity struct {
	cond      *sync.Cond
	seqs      map[string]int
	closed    map[string]bool
	pending   int
	capacity  int
	mu        sync.Mutex
	stopped   bool
	cancelled bool
}

func newRunCapacity(capacity int) *runCapacity {
	rc := &runCapacity{
		capacity: capacity,
		seqs:     make(map[string]int),
		closed:   make(map[string]bool),
	}
	rc.cond = sync.NewCond(&rc.mu)
	return rc
}

// reserve waits until a slot is available, then increments pending +
// assigns and returns the next Seq for streamID. The producer-side
// "stream is closed" flag is set on the Done frame's reserve call so
// any subsequent EmitChunk for the same StreamID fails fast — even
// before the consumer drains the Done frame.
//
// Returns:
//   - ErrEngineStopped if Stop fires while waiting.
//   - ctx.Err() if ctx cancels while waiting.
//   - ErrStreamClosed if streamID was already Done'd by a prior call.
//
// The Seq is assigned BEFORE the channel send so concurrent
// EmitChunks on the same StreamID get strictly increasing Seq values
// in call order (callers serialize themselves; see the StreamFrame
// godoc).
//
// Implementation: a side goroutine watches ctx.Done() and Broadcasts
// on the cond. Per Go's sync.Cond docs, Broadcast may be called
// without holding the cond's lock; this lets the watcher fire
// without contending for rc.mu. The main goroutine wakes from
// cond.Wait, re-acquires rc.mu, and re-checks the loop conditions
// (including ctx.Err()) on each wakeup.
func (rc *runCapacity) reserve(ctx context.Context, streamID string, done bool) (int, error) {
	// Watcher: if ctx cancels while we're waiting, broadcast so the
	// main goroutine wakes and observes ctx.Err(). Stops on `cleanup`.
	cleanup := make(chan struct{})
	defer close(cleanup)
	go func() {
		select {
		case <-ctx.Done():
			rc.cond.Broadcast()
		case <-cleanup:
		}
	}()

	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.closed[streamID] {
		return 0, ErrStreamClosed
	}
	if rc.cancelled {
		return 0, ErrRunCancelled
	}
	for rc.pending >= rc.capacity && !rc.stopped && !rc.cancelled && ctx.Err() == nil && !rc.closed[streamID] {
		rc.cond.Wait()
	}
	if rc.stopped {
		return 0, ErrEngineStopped
	}
	if rc.cancelled {
		return 0, ErrRunCancelled
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if rc.closed[streamID] {
		return 0, ErrStreamClosed
	}
	seq := rc.seqs[streamID] + 1
	rc.seqs[streamID] = seq
	rc.pending++
	if done {
		// Mark the stream closed at emit time — the producer side
		// learns about closure before the consumer drains the Done
		// frame. This prevents a races where a fast producer emits
		// Done then immediately attempts another EmitChunk for the
		// same StreamID.
		rc.closed[streamID] = true
	}
	return seq, nil
}

// release decrements pending and signals one waiter. Called when the
// consumer drains a stream frame (Engine.Fetch / FetchByRun returns).
// Idempotent across the same frame — but the dispatcher only fires
// it once per frame so callers don't need to deduplicate.
//
// `done` is preserved on the closed map (set initially by reserve,
// re-set here for clarity in case the producer side never observed
// the Done emit).
func (rc *runCapacity) release(streamID string, done bool) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.pending > 0 {
		rc.pending--
	}
	if done {
		rc.closed[streamID] = true
		delete(rc.seqs, streamID)
	}
	rc.cond.Signal()
}

// stop signals the tracker that the engine has shut down. All blocked
// reserve calls wake and return ErrEngineStopped. Idempotent.
func (rc *runCapacity) stop() {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.stopped = true
	rc.cond.Broadcast()
}

// cancel signals the tracker that the run has been cancelled. All
// blocked reserve calls wake and return ErrRunCancelled. Idempotent.
// Distinct from stop so callers can tell a run-cancel from an
// engine-stop apart in the failure mode.
func (rc *runCapacity) cancel() {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.cancelled = true
	rc.cond.Broadcast()
}

// EmitChunk emits a stream frame. Blocks when the originating run's
// pending-frame count has reached its RunCapacity (default = the
// engine's DefaultQueueSize, 64). The block is per-run, never per-
// engine — a single run's saturation does not pause other runs (this
// is the deadlock-prevention guarantee from brief 01 §4).
//
// The frame is wrapped in an Envelope whose Payload is the
// StreamFrame and whose identity inherits from the originating
// NodeFunc's incoming envelope (the worker passes that ctx to
// EmitChunk's caller; the NodeContext.lastEnv field carries the
// run's identity).
//
// Done: true marks the terminal frame for the StreamID. After a Done
// frame drains, subsequent EmitChunk for that StreamID returns
// ErrStreamClosed.
//
// Failure modes:
//   - ErrSeqProvided: caller pre-filled frame.Seq.
//   - ErrEmptyRunID: the originating envelope had no RunID.
//   - ErrStreamClosed: the StreamID was previously Done-terminated.
//   - ErrEngineStopped: Stop fired while waiting.
//   - ctx.Err(): the caller's ctx cancelled while waiting.
func (nctx *NodeContext) EmitChunk(ctx context.Context, frame StreamFrame) error {
	if frame.Seq != 0 {
		return ErrSeqProvided
	}
	env := nctx.lastEnv
	runID := env.RunID
	if runID == "" {
		return ErrEmptyRunID
	}
	streamID := frame.StreamID
	if streamID == "" {
		streamID = runID
		frame.StreamID = streamID
	}
	if nctx.engine.stopped.Load() {
		return ErrEngineStopped
	}
	cap := nctx.engine.runCapacityFor(runID, nctx.node)
	rc := nctx.engine.acquireCapacity(runID, cap)
	seq, err := rc.reserve(ctx, streamID, frame.Done)
	if err != nil {
		return err
	}
	frame.Seq = seq

	// Wrap the frame in an Envelope. Identity inherits from the
	// originating envelope so the dispatcher's RunID demux routes the
	// frame to the right per-run subqueue.
	wrap := messages.Envelope{
		Payload:    frame,
		Headers:    env.Headers,
		RunID:      env.RunID,
		SessionID:  env.SessionID,
		Timestamp:  env.Timestamp,
		DeadlineAt: env.DeadlineAt,
		Meta:       map[string]any{streamFrameMetaKey: true},
	}
	if err := nctx.engine.emitFromNode(ctx, nctx.node, wrap, false); err != nil {
		// Roll back the reservation so the producer's slot becomes
		// available again. Without this, a failing emit (e.g. ctx
		// cancelled mid-send) would leak a pending slot.
		rc.release(streamID, frame.Done)
		return err
	}
	return nil
}

// streamFrameMetaKey marks an envelope as carrying a StreamFrame
// payload so the dispatcher's drain path can release the run's
// capacity counter. Internal use only.
const streamFrameMetaKey = "_engine_stream_frame"

// isStreamFrame reports whether env wraps a StreamFrame. The check
// reads the Meta marker (not a type assertion on Payload) because a
// node could legitimately emit a StreamFrame as its regular output
// — only EmitChunk-emitted frames participate in capacity bookkeeping.
func isStreamFrame(env messages.Envelope) bool {
	if env.Meta == nil {
		return false
	}
	v, ok := env.Meta[streamFrameMetaKey]
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// runCapacityFor resolves the per-run cap. Resolution order: explicit
// WithRunCapacity override on the originating Emit, then the node's
// Policy.RunCapacity (if > 0), then the engine's DefaultQueueSize.
func (e *engine) runCapacityFor(runID, nodeName string) int {
	e.capMu.Lock()
	override, hasOverride := e.runCapacityOverrides[runID]
	e.capMu.Unlock()
	if hasOverride && override > 0 {
		return override
	}
	if node, ok := e.nodes[nodeName]; ok && node.Policy.RunCapacity > 0 {
		return node.Policy.RunCapacity
	}
	return e.cfg.queueSize
}

// acquireCapacity returns the per-run capacity tracker, creating it
// on first call. Mutex-guarded so concurrent EmitChunks on the same
// run don't double-create.
func (e *engine) acquireCapacity(runID string, capacity int) *runCapacity {
	e.capMu.Lock()
	defer e.capMu.Unlock()
	rc, ok := e.capacities[runID]
	if ok {
		return rc
	}
	rc = newRunCapacity(capacity)
	if e.stopped.Load() {
		// If we're already shutting down, create the tracker
		// pre-stopped so any reserve call wakes immediately.
		rc.stopped = true
	}
	e.capacities[runID] = rc
	return rc
}

// signalDrainedFrame is invoked by the dispatcher when it drains a
// frame from a per-run subqueue. If the envelope carries a
// StreamFrame, decrement the run's capacity counter and signal a
// waiter. No-op for non-stream envelopes.
func (e *engine) signalDrainedFrame(env messages.Envelope) {
	if !isStreamFrame(env) {
		return
	}
	frame, ok := env.Payload.(StreamFrame)
	if !ok {
		return
	}
	e.capMu.Lock()
	rc, ok := e.capacities[env.RunID]
	e.capMu.Unlock()
	if !ok {
		return
	}
	rc.release(frame.StreamID, frame.Done)
}

// stopAllCapacities releases every blocked EmitChunk waiter with
// ErrEngineStopped. Called by engine.Stop after the workers have
// joined; idempotent.
func (e *engine) stopAllCapacities() {
	e.capMu.Lock()
	trackers := make([]*runCapacity, 0, len(e.capacities))
	for _, rc := range e.capacities {
		trackers = append(trackers, rc)
	}
	e.capMu.Unlock()
	for _, rc := range trackers {
		rc.stop()
	}
}

// recordRunCapacityOverride stashes a WithRunCapacity override under
// the run's RunID. Looked up by the first EmitChunk for that run.
// Concurrent-safe.
func (e *engine) recordRunCapacityOverride(runID string, n int) {
	if runID == "" || n <= 0 {
		return
	}
	e.capMu.Lock()
	defer e.capMu.Unlock()
	if e.runCapacityOverrides == nil {
		e.runCapacityOverrides = make(map[string]int)
	}
	e.runCapacityOverrides[runID] = n
}
