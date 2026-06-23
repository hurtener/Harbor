package engine

import (
	"context"
	"errors"
	"sync"
	"time"

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
//
// TTL caveat: the per-run Seq bookkeeping lives in the run's capacity
// tracker, which the capacity sweeper reaps after DefaultCapacityTTL of
// inactivity with no pending frames. A run that resumes streaming after
// that window starts a fresh tracker — Seq restarts at 1 and a stream
// previously marked Done is no longer remembered. Streams that complete
// within the TTL (the overwhelming common case) are unaffected.
type StreamFrame struct {
	StreamID string
	Seq      int
	Text     string
	Done     bool
	Meta     map[string]any
}

// Sentinel errors specific to streaming. sentinels (e.g.
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
	mu        sync.Mutex
	cond      *sync.Cond
	pending   int
	capacity  int
	seqs      map[string]int  // StreamID -> next Seq to assign (monotonic)
	closed    map[string]bool // StreamID -> Done frame drained
	stopped   bool            // engine Stop signalled; release waiters with ErrEngineStopped
	cancelled bool            // Cancel(runID) signalled; release waiters with ErrRunCancelled
	// lastActivity is the wall time of the most recent reserve/release.
	// Read under mu by the capacity sweeper to reap trackers idle past
	// the TTL (the run is over). Guarded by mu like every other field.
	lastActivity time.Time
}

func newRunCapacity(capacity int) *runCapacity {
	rc := &runCapacity{
		capacity:     capacity,
		seqs:         make(map[string]int),
		closed:       make(map[string]bool),
		lastActivity: time.Now(),
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
	rc.lastActivity = time.Now()
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
	rc.lastActivity = time.Now()
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
// is the deadlock-prevention guarantee).
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
//   - ErrStreamClosed: the StreamID was previously Done-terminated —
//     subject to the capacity-TTL caveat (see StreamFrame): if the run
//     was idle past DefaultCapacityTTL its tracker is reaped, so a
//     previously-Done StreamID is no longer remembered and Seq restarts.
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
	if hasOverride && override.n > 0 {
		return override.n
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
	// The override (if any) was consumed by runCapacityFor when sizing
	// this tracker; drop it so it doesn't outlive the run. The tracker
	// itself is reaped by the capacity sweeper once idle.
	delete(e.runCapacityOverrides, runID)
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

// CapacityEntryCount returns the number of live per-run streaming
// capacity trackers. It is a lock-safe read over the same capMu-guarded
// map the capacity sweeper bounds — no per-run state is stored on the
// engine, so the count is a pure observation safe to call from any
// goroutine (including an observability gauge callback at scrape time).
func (e *engine) CapacityEntryCount() int {
	e.capMu.Lock()
	defer e.capMu.Unlock()
	return len(e.capacities)
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

// runOverride is a WithRunCapacity value plus the wall time it was
// recorded. setAt lets the capacity sweeper reap an override whose run
// never streamed (so no tracker was ever created to consume it) once it
// is older than the TTL — the streamed-run case is reaped at tracker
// creation in acquireCapacity.
type runOverride struct {
	n     int
	setAt time.Time
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
		e.runCapacityOverrides = make(map[string]runOverride)
	}
	e.runCapacityOverrides[runID] = runOverride{n: n, setAt: time.Now()}
}

// DefaultCapacityTTL is how long a per-run streaming-capacity tracker is
// retained after its last reserve/release before the capacity sweeper
// reaps it. Generous relative to DefaultCancelTTL. Reaping inputs are
// ONLY idle-time and pending count — NOT run liveness: a tracker is
// reaped once it has had no reserve/release for this long AND has no
// pending frames. So a run quiet *under* the TTL keeps its per-stream
// bookkeeping; a run still live but idle past the TTL with nothing
// pending (e.g. parked on a long HITL pause) is reaped and, if it later
// resumes streaming, loses that bookkeeping — see reapIdleCapacities and
// the StreamFrame godoc for the guarantees that lapse.
const DefaultCapacityTTL = 10 * time.Minute

// capacitySweepInterval is how often the capacity sweeper runs.
const capacitySweepInterval = 30 * time.Second

// WithCapacityTTL overrides DefaultCapacityTTL. Non-positive values
// silently fall back to the default.
func WithCapacityTTL(d time.Duration) Option {
	return func(cfg *engineConfig) {
		if d > 0 {
			cfg.capacityTTL = d
		}
	}
}

// runCapacitySweeper periodically reaps idle per-run capacity trackers
// and orphaned overrides. Runs in its own goroutine; joined by Stop via
// the engine wg. Mirrors runCancellationSweeper.
func (e *engine) runCapacitySweeper(ctx context.Context) {
	ticker := time.NewTicker(capacitySweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.reapIdleCapacities(time.Now())
		}
	}
}

// reapIdleCapacities deletes capacity trackers idle longer than the
// capacity TTL with no pending frames (plus their consumed overrides),
// and orphan overrides for runs that never streamed. Called by the
// sweeper goroutine; tests may invoke it directly with a chosen `now`.
//
// Lock order is capMu (engine) then rc.mu (per-tracker). No path takes
// capMu while holding rc.mu (reserve/release touch only rc.mu), so this
// cannot deadlock.
//
// Guarantee relaxation (deliberate, bounded by the TTL): reaping drops
// the tracker's `seqs` and `closed` maps. If a run streams again after
// its tracker is reaped, acquireCapacity rebuilds a fresh tracker, so
// for that run BOTH the per-StreamID monotonic-Seq guarantee (Seq
// restarts at 1) AND the post-Done ErrStreamClosed guarantee (a stream
// previously Done'd is no longer remembered) lapse. This only affects a
// stream idle past the capacity TTL with nothing pending — a stream that
// completes within the TTL is unaffected. See the StreamFrame /
// EmitChunk godoc.
func (e *engine) reapIdleCapacities(now time.Time) {
	ttl := e.cfg.capacityTTL
	if ttl <= 0 {
		ttl = DefaultCapacityTTL
	}
	e.capMu.Lock()
	defer e.capMu.Unlock()
	for runID, rc := range e.capacities {
		rc.mu.Lock()
		reap := rc.pending == 0 && now.Sub(rc.lastActivity) > ttl
		rc.mu.Unlock()
		if reap {
			delete(e.capacities, runID)
			delete(e.runCapacityOverrides, runID)
		}
	}
	for runID, ov := range e.runCapacityOverrides {
		if _, hasTracker := e.capacities[runID]; hasTracker {
			continue
		}
		if now.Sub(ov.setAt) > ttl {
			delete(e.runCapacityOverrides, runID)
		}
	}
}
