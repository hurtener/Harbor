// Package engine is Harbor's typed, async, queue-backed graph
// executor — the runtime kernel every other phase sits on. Phase 10
// shipped the Engine interface, the worker loop (one goroutine per
// node), bounded per-adjacency channels (default 64), the always-on
// egress dispatcher (RunID demux), cycle detection at construction,
// and Run / Stop / Emit / EmitTo / Fetch.
//
// Phase 11 layered the reliability shell on top (NodePolicy,
// RunError); Phase 12 added streaming (StreamFrame, EmitChunk) +
// per-run capacity backpressure; Phase 13 lands Cancel(runID) +
// FetchByRun (replacing Phase 10's stubs) plus engine-Cancel
// mirroring into Phase 14's Subflow; Phase 14 adds routers,
// concurrency utilities, and Subflow. None of those phases change
// this surface — they extend it.
//
// Concurrent reuse contract (D-025): a compiled *engine is reusable
// across goroutines after Run starts. Per-run state lives in the
// dispatcher's subqueues + the worker stacks; never on the engine
// struct itself. The N=100 reuse test pins this — see
// concurrent_test.go.
package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// Engine is the runtime container — the typed, async, queue-backed
// graph executor. One concrete implementation in V1 (the in-memory
// engine); future remote-engine drivers (post-V1) plug behind the
// same interface via the §4.4 seam pattern.
//
// Lifecycle: New constructs the engine + validates the adjacency set
// (cycle detection, duplicate-name check, queue-size validation);
// Run starts one goroutine per node + the dispatcher; Stop joins
// them all under a deadline. Emit lands at the engine's inlet
// channel(s); Fetch drains the dispatcher's any-run subqueue.
type Engine interface {
	Emit(ctx context.Context, env messages.Envelope, opts ...EmitOption) error
	EmitTo(ctx context.Context, env messages.Envelope, target NodeRef) error
	Fetch(ctx context.Context, opts ...FetchOption) (messages.Envelope, error)
	FetchByRun(ctx context.Context, runID string) (messages.Envelope, error)
	Cancel(ctx context.Context, runID string) (bool, error)
	Run(ctx context.Context) error
	Stop(ctx context.Context) error
}

// engine is the in-memory Engine implementation. All exported
// methods are concurrent-safe.
type engine struct {
	cfg       engineConfig
	nodes     map[string]Node
	adjs      []Adjacency
	inlets    []string // sorted node names with no parent
	outlets   []string // sorted node names with no child

	// channels[from][to] is the bounded buffer between two adjacent
	// nodes. inletChans[name] is the ingress channel for an inlet
	// (where Emit lands). The engine writes to outlet from the
	// outlet nodes' workers.
	channels    map[string]map[string]chan messages.Envelope
	inletChans  map[string]chan messages.Envelope
	outletChan  chan messages.Envelope // dispatcher reads here

	dispatcher *dispatcher
	logger     *slog.Logger // optional; nil-safe via slog.New(slog.DiscardHandler) when unset

	// Lifecycle state. Run sets ctx; Stop cancels it. cancelFn is
	// the engine's internal context cancel function. running guards
	// double-Run.
	mu       sync.Mutex
	ctx      context.Context
	cancelFn context.CancelFunc
	running  bool
	stopped  atomic.Bool
	wg       sync.WaitGroup // worker join group

	// Phase 12: per-run streaming capacity bookkeeping.
	//
	// capMu guards capacities + runCapacityOverrides. Trackers are
	// created lazily on first EmitChunk for a run; overrides are
	// recorded on Engine.Emit when WithRunCapacity is passed. Both
	// maps grow over time (one entry per run); a run's entry is NOT
	// reaped on completion in V1 (Phase 13's Cancel and a future
	// run-end signal will manage cleanup).
	capMu                sync.Mutex
	capacities           map[string]*runCapacity
	runCapacityOverrides map[string]int

	// Phase 13: per-run cancellation bookkeeping. cancelMu guards
	// cancellations + cancelObservers; activeRunsMu guards
	// activeRuns. Both maps grow over time bounded by the cancellation
	// TTL sweeper.
	cancelMu        sync.Mutex
	cancellations   map[string]*runCancellation
	cancelObservers map[string][]*cancelObserver
	activeRunsMu    sync.Mutex
	activeRuns      map[string]int
}

// New constructs an Engine from a list of adjacencies + options.
// Cycle detection runs at construction; per-node AllowCycle opts
// out for legitimate self-loops.
//
// Validation errors: ErrEmptyAdjacencies, ErrDuplicateNodeName,
// ErrCycleDetected, ErrInvalidQueueSize, ErrNodeNotFound (when a
// channel override targets an unknown node).
func New(adjacencies []Adjacency, opts ...Option) (Engine, error) {
	if len(adjacencies) == 0 {
		return nil, ErrEmptyAdjacencies
	}

	cfg := engineConfig{
		queueSize: DefaultQueueSize,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.queueSize <= 0 {
		return nil, fmt.Errorf("%w: %d", ErrInvalidQueueSize, cfg.queueSize)
	}
	for k, v := range cfg.channelOverrides {
		if v <= 0 {
			return nil, fmt.Errorf("%w: %s -> %s = %d", ErrInvalidQueueSize, k.from, k.to, v)
		}
	}

	nodes, err := buildNodeIndex(adjacencies)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		if n.Func == nil && !isOutletOnly(n.Name, adjacencies, nodes) {
			return nil, fmt.Errorf("engine: node %q has nil Func and is not outlet-only", n.Name)
		}
	}
	// Validate channel overrides reference real nodes.
	for k := range cfg.channelOverrides {
		if _, ok := nodes[k.from]; !ok {
			return nil, fmt.Errorf("%w: WithChannelOverride from=%q", ErrNodeNotFound, k.from)
		}
		if _, ok := nodes[k.to]; !ok {
			return nil, fmt.Errorf("%w: WithChannelOverride to=%q", ErrNodeNotFound, k.to)
		}
	}

	if err := detectCycle(adjacencies, nodes); err != nil {
		return nil, err
	}

	e := &engine{
		cfg:           cfg,
		nodes:         nodes,
		adjs:          adjacencies,
		inlets:        inletNodes(adjacencies, nodes),
		outlets:       outletNodes(adjacencies, nodes),
		channels:      make(map[string]map[string]chan messages.Envelope),
		inletChans:    make(map[string]chan messages.Envelope),
		outletChan:    make(chan messages.Envelope, cfg.queueSize),
		logger:        slog.Default(),
		capacities:      make(map[string]*runCapacity),
		cancellations:   make(map[string]*runCancellation),
		cancelObservers: make(map[string][]*cancelObserver),
		activeRuns:      make(map[string]int),
	}
	if len(e.inlets) == 0 {
		return nil, fmt.Errorf("engine: graph has no inlet (every node has a parent — adjust AllowCycle or graph shape)")
	}
	if len(e.outlets) == 0 {
		return nil, fmt.Errorf("engine: graph has no outlet (every node has a child — adjust graph shape)")
	}

	// Allocate per-adjacency channels. Each (from -> to) edge gets
	// a bounded buffer sized by the channel override, falling back
	// to the engine-wide queueSize.
	for _, adj := range adjacencies {
		if e.channels[adj.From.Name] == nil {
			e.channels[adj.From.Name] = make(map[string]chan messages.Envelope)
		}
		for _, to := range adj.To {
			size := cfg.queueSize
			if override, ok := cfg.channelOverrides[channelKey{from: adj.From.Name, to: to.Name}]; ok {
				size = override
			}
			e.channels[adj.From.Name][to.Name] = make(chan messages.Envelope, size)
		}
	}
	// Allocate inlet channels (one per inlet node).
	for _, name := range e.inlets {
		e.inletChans[name] = make(chan messages.Envelope, cfg.queueSize)
	}

	e.dispatcher = newDispatcher(e.outletChan, cfg.queueSize, e.signalDrainedFrame)
	return e, nil
}

// isOutletOnly reports whether a node appears only as a To target
// (never as From with children). Such nodes are effectively
// pass-throughs that emit to the synthetic Outlet — they still need
// a Func.
func isOutletOnly(name string, adjs []Adjacency, _ map[string]Node) bool {
	for _, adj := range adjs {
		if adj.From.Name == name && len(adj.To) > 0 {
			return false
		}
	}
	return true
}

// Run starts the engine: one worker goroutine per node + the
// dispatcher goroutine. Returns nil immediately after the goroutines
// are launched; the engine runs until Stop is called or ctx cancels.
//
// Calling Run twice on the same engine returns an error — the
// engine's lifecycle is one-shot. Construct a new engine to restart.
func (e *engine) Run(ctx context.Context) error {
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return errors.New("engine: Run already called")
	}
	if e.stopped.Load() {
		e.mu.Unlock()
		return ErrEngineStopped
	}
	e.running = true
	internalCtx, cancel := context.WithCancel(ctx)
	e.ctx = internalCtx
	e.cancelFn = cancel
	e.mu.Unlock()

	e.dispatcher.start(internalCtx)

	// Phase 13: cancellation TTL sweeper. Joined via wg on Stop.
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		e.runCancellationSweeper(internalCtx)
	}()

	for _, n := range e.nodes {
		// Outlet nodes (no children) write to outletChan; intermediate
		// nodes write to their per-adjacency outgoing channels. The
		// worker reads from incoming channels.
		node := n
		e.wg.Add(1)
		go func() {
			defer e.wg.Done()
			e.workerLoop(internalCtx, node)
		}()
	}
	return nil
}

// Stop cancels the engine's internal context and joins workers +
// dispatcher within ctx's deadline. Returns ctx.Err() if the
// deadline expires before joins complete (operator can force-kill
// the process at that point).
//
// Stop is idempotent: a second call returns nil. After Stop, every
// engine method except Stop returns ErrEngineStopped.
//
// Shutdown sequence:
//
//  1. Cancel the engine's internal context. Workers observe
//     ctx.Done() in their readAny / deliverEnvelope select cases and
//     return naturally; per-adjacency channels are NOT closed here
//     (closing them while workers may still be mid-send would race;
//     they are simply unreferenced after worker exit and GC'd).
//  2. Wait for all worker goroutines to join via wg.Wait.
//  3. Close outletChan so the dispatcher exits its range loop.
//  4. Stop the dispatcher (joins its goroutine).
func (e *engine) Stop(ctx context.Context) error {
	if !e.stopped.CompareAndSwap(false, true) {
		return nil
	}
	e.mu.Lock()
	cancel := e.cancelFn
	e.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	// Phase 12: release every blocked EmitChunk waiter with
	// ErrEngineStopped BEFORE waiting for workers. A worker whose
	// NodeFunc is mid-EmitChunk waits on a per-run sync.Cond; until
	// we broadcast on those conds, the worker would hang and wg.Wait
	// would deadlock against the ctx deadline.
	e.stopAllCapacities()

	// Wait for workers; workers exit when the engine's internal ctx
	// is cancelled (which we just did).
	doneWorkers := make(chan struct{})
	go func() {
		e.wg.Wait()
		// All worker goroutines have returned; safe to close
		// outletChan so the dispatcher exits its range loop.
		close(e.outletChan)
		close(doneWorkers)
	}()

	select {
	case <-doneWorkers:
	case <-ctx.Done():
		return ctx.Err()
	}

	// Now wait for the dispatcher.
	doneDispatcher := make(chan struct{})
	go func() {
		e.dispatcher.stop()
		close(doneDispatcher)
	}()
	select {
	case <-doneDispatcher:
	case <-ctx.Done():
		return ctx.Err()
	}
	return nil
}

// Emit lands env at the engine's inlet(s). Identity-mandatory: the
// envelope's triple (TenantID, UserID, SessionID) must validate per
// identity.Validate; empty RunID is acceptable in Phase 10 (Phase
// 13 will tighten when FetchByRun arrives).
//
// When the graph has multiple inlets, Emit lands env at the first
// inlet (lexicographic order). Use EmitTo(env, target) for explicit
// inlet selection.
//
// Blocks if the inlet channel is full — backpressure flows back to
// the caller. Honors ctx cancellation.
func (e *engine) Emit(ctx context.Context, env messages.Envelope, opts ...EmitOption) error {
	if e.stopped.Load() {
		return ErrEngineStopped
	}
	if err := e.validateIdentity(env); err != nil {
		return err
	}
	// Phase 13: reject Emit for runs that have been Cancel'd within
	// the cancellation TTL. Closes the "Cancel beats Emit" race window.
	if _, cancelled := e.runIsCancelled(env.RunID); cancelled {
		return ErrRunCancelled
	}
	if env.Timestamp.IsZero() {
		env.Timestamp = time.Now()
	}
	o := emitOptions{}
	for _, opt := range opts {
		opt(&o)
	}
	if o.runCapacity > 0 {
		e.recordRunCapacityOverride(env.RunID, o.runCapacity)
	}
	target := e.inlets[0]
	return e.sendBlocking(ctx, e.inletChans[target], env)
}

// EmitTo lands env at a specific node's inlet. The target must be
// an inlet (no parent) — EmitTo to an internal node would skip
// validation Phase 11 will add and isn't supported in V1.
//
// Use case: graphs with multiple typed inlets where the caller
// knows which inlet the envelope belongs to.
func (e *engine) EmitTo(ctx context.Context, env messages.Envelope, target NodeRef) error {
	if e.stopped.Load() {
		return ErrEngineStopped
	}
	if err := e.validateIdentity(env); err != nil {
		return err
	}
	ch, ok := e.inletChans[target.Name]
	if !ok {
		return fmt.Errorf("%w: %q is not an inlet", ErrNodeNotFound, target.Name)
	}
	if env.Timestamp.IsZero() {
		env.Timestamp = time.Now()
	}
	return e.sendBlocking(ctx, ch, env)
}

// Fetch reads from the dispatcher's any-run subqueue. Blocks until
// an envelope arrives or ctx cancels. Returns ErrEngineStopped when
// the channel closes (engine shutdown).
func (e *engine) Fetch(ctx context.Context, _ ...FetchOption) (messages.Envelope, error) {
	if e.stopped.Load() {
		return messages.Envelope{}, ErrEngineStopped
	}
	return e.dispatcher.fetchAny(ctx)
}

// validateIdentity enforces the identity-mandatory contract on the
// inbound envelope. Empty RunID is acceptable in Phase 10; the rest
// of the triple must be non-empty per identity.Validate.
func (e *engine) validateIdentity(env messages.Envelope) error {
	q := env.Identity()
	if err := identity.Validate(q.Identity); err != nil {
		return fmt.Errorf("%w: %w", ErrIdentityRequired, err)
	}
	return nil
}

// sendBlocking writes env to ch with backpressure (blocking on full
// channel). Honors ctx cancel and engine stop.
//
// The pre-check on ctx.Err() is intentional: Go's select picks
// uniformly from ready cases, so a pre-cancelled ctx with a non-full
// channel could pick the send and return nil. The pre-check
// guarantees a cancelled ctx loses to no other case.
func (e *engine) sendBlocking(ctx context.Context, ch chan messages.Envelope, env messages.Envelope) error {
	if ch == nil {
		return ErrNodeNotFound
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	defer func() { _ = recover() }() // ch may close mid-send during Stop
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-e.ctxOrNew().Done():
		return ErrEngineStopped
	case ch <- env:
		return nil
	}
}

// emitFromNode is the worker-side emit path used by NodeContext.
// Writes to all outgoing channels of `node`. When wait is false,
// returns ErrChannelFull immediately on a saturated channel; when
// true, blocks per channel until the consumer drains.
//
// Outlet nodes write to outletChan instead of per-adjacency
// channels.
func (e *engine) emitFromNode(ctx context.Context, node string, env messages.Envelope, nonblocking bool) error {
	if e.stopped.Load() {
		return ErrEngineStopped
	}
	if env.Timestamp.IsZero() {
		env.Timestamp = time.Now()
	}
	outgoing, hasOutgoing := e.channels[node]
	if !hasOutgoing || len(outgoing) == 0 {
		// Outlet node: write to outletChan.
		return e.deliverEnvelope(ctx, e.outletChan, env, nonblocking)
	}
	for _, ch := range outgoing {
		if err := e.deliverEnvelope(ctx, ch, env, nonblocking); err != nil {
			return err
		}
	}
	return nil
}

// deliverEnvelope writes env to ch, optionally blocking. Honors ctx
// cancel and engine stop.
//
// The pre-check on ctx.Err() / engine-stopped is intentional: Go's
// select picks uniformly from ready cases, so a pre-cancelled ctx
// with a non-full channel could otherwise pick the send and return
// nil. The pre-check guarantees a cancelled ctx loses to the send.
func (e *engine) deliverEnvelope(ctx context.Context, ch chan messages.Envelope, env messages.Envelope, nonblocking bool) error {
	defer func() { _ = recover() }() // channel may close during Stop
	if e.stopped.Load() {
		return ErrEngineStopped
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if nonblocking {
		select {
		case ch <- env:
			return nil
		default:
			return ErrChannelFull
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-e.ctxOrNew().Done():
		return ErrEngineStopped
	case ch <- env:
		return nil
	}
}

// ctxOrNew returns the engine's internal context if Run has been
// called, else a never-cancelling background context. Lets pre-Run
// Emit calls block on their target channel without falsely returning
// ErrEngineStopped.
func (e *engine) ctxOrNew() context.Context {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ctx != nil {
		return e.ctx
	}
	return context.Background()
}

// workerLoop is the per-node goroutine. Reads from the node's
// incoming channels (or the inlet channel when the node is an
// inlet), invokes Func, writes to outgoing channels (or outletChan
// for outlet nodes).
//
// Worker error path: a non-nil error from Func is logged via the
// engine's slog.Logger. Phase 11 will replace this with a structured
// RunError emitted to the bus + (optionally) the egress.
func (e *engine) workerLoop(ctx context.Context, node Node) {
	incomingChans := e.incomingChannelsFor(node.Name)
	if len(incomingChans) == 0 {
		// Node has no incoming channels (orphan); workers exit
		// immediately. Should not happen — graph validation
		// catches it — but defensive coding never hurts.
		return
	}
	for {
		env, ok, fatal := e.readAny(ctx, incomingChans)
		if fatal != nil || !ok {
			return
		}
		// Phase 13: per-run cancellation observed BEFORE any
		// processing. A cancelled run's pending envelopes are dropped
		// silently here (Cancel's drainQueuedForRun handled the
		// already-queued ones; this catches envelopes that landed in
		// the channel mid-drain or just after the flag flipped).
		if _, cancelled := e.runIsCancelled(env.RunID); cancelled {
			continue
		}
		// Deadline check before invocation (per brief 01 §4 worker
		// loop). Phase 11 promotes ErrDeadlineExceeded into a
		// RunError; Phase 10 just logs and continues.
		if env.DeadlineAt != nil && time.Now().After(*env.DeadlineAt) {
			e.logWorkerError(env, ErrDeadlineExceeded)
			continue
		}
		if node.Func == nil {
			// Outlet-only node with no Func: pass-through.
			_ = e.emitFromNode(ctx, node.Name, env, false)
			continue
		}
		nctx := &NodeContext{engine: e, node: node.Name, lastEnv: env}
		// Track this worker as active on the run so Cancel can report
		// "the run was active" while the worker is mid-invocation.
		e.markRunActive(env.RunID)
		// Phase 11: invoke under the reliability shell. NodePolicy
		// drives validate / timeout / retry / backoff. Zero-value
		// policy = bare invocation (Phase 10 behavior).
		// Phase 13: pass the per-run cancel-flag pointer so the shell
		// can observe cancellation between retries.
		rcCancel, _ := e.runIsCancelled(env.RunID)
		out, err := runWithReliability(ctx, env, node.Func, node.Policy, nctx, node.Name, nil, rcCancel)
		e.markRunDone(env.RunID)
		if err != nil {
			e.logWorkerError(env, err)
			if e.cfg.errorEmitToEgress {
				e.emitErrorEnvelope(ctx, env, err)
			}
			continue
		}
		if out.Timestamp.IsZero() {
			// Preserve the running envelope's identity if the node
			// produced a zero-valued envelope.
			out.Timestamp = time.Now()
		}
		// Identity propagation: if the node returned an envelope
		// without a SessionID, inherit from the input. Same for
		// Headers.{TenantID, UserID}. RunID is preserved as-is.
		out = inheritIdentity(env, out)
		if err := e.emitFromNode(ctx, node.Name, out, false); err != nil {
			e.logWorkerError(env, err)
		}
	}
}

// inheritIdentity carries the inbound envelope's identity onto the
// outbound envelope when the node didn't fill those fields. Saves
// every NodeFunc from having to remember to copy 4 strings.
func inheritIdentity(in, out messages.Envelope) messages.Envelope {
	if out.Headers.TenantID == "" {
		out.Headers.TenantID = in.Headers.TenantID
	}
	if out.Headers.UserID == "" {
		out.Headers.UserID = in.Headers.UserID
	}
	if out.SessionID == "" {
		out.SessionID = in.SessionID
	}
	if out.RunID == "" {
		out.RunID = in.RunID
	}
	return out
}

// safeInvoke wraps the user-supplied NodeFunc with a panic recover
// so a buggy node can't crash the worker. The recovered panic is
// returned as an error.
func safeInvoke(ctx context.Context, fn NodeFunc, env messages.Envelope, nctx *NodeContext) (out messages.Envelope, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("engine: node panicked: %v", r)
		}
	}()
	return fn(ctx, env, nctx)
}

// readAny blocks on incoming, ctx, and the engine's internal stop
// signal. Returns (env, true, nil) on a successful read; (zero,
// false, nil) when the channel closes (engine shutdown);
// (zero, false, err) on ctx cancel.
func (e *engine) readAny(ctx context.Context, channels []chan messages.Envelope) (messages.Envelope, bool, error) {
	if len(channels) == 1 {
		// Hot path: single incoming channel.
		select {
		case <-ctx.Done():
			return messages.Envelope{}, false, ctx.Err()
		case env, ok := <-channels[0]:
			return env, ok, nil
		}
	}
	// Multi-channel select via reflect-free polling.
	for {
		select {
		case <-ctx.Done():
			return messages.Envelope{}, false, ctx.Err()
		default:
		}
		for _, ch := range channels {
			select {
			case env, ok := <-ch:
				if !ok {
					// Channel closed; remove from candidate set on
					// next poll. Cheap: returning false makes the
					// outer loop re-evaluate.
					return messages.Envelope{}, false, nil
				}
				return env, true, nil
			default:
			}
		}
		// Yield once before the next poll cycle. Tiny latency, no
		// busy-spin.
		select {
		case <-ctx.Done():
			return messages.Envelope{}, false, ctx.Err()
		case <-time.After(time.Microsecond):
		}
	}
}

// incomingChannelsFor returns the channels the node should read
// from. An inlet reads from inletChans[name]; everyone else reads
// from channels[parent][name] for every parent.
func (e *engine) incomingChannelsFor(name string) []chan messages.Envelope {
	if ch, ok := e.inletChans[name]; ok {
		return []chan messages.Envelope{ch}
	}
	var chans []chan messages.Envelope
	for parent, byTo := range e.channels {
		_ = parent
		if ch, ok := byTo[name]; ok {
			chans = append(chans, ch)
		}
	}
	return chans
}

// logWorkerError logs a worker-loop error via the engine's slog
// logger AND fires the configured RunErrorHandler (Phase 11). The
// slog path keeps internal failures visible to operators even when
// no handler is installed; the handler is the seam production wiring
// uses to route the structured RunError into the telemetry.Logger →
// eventbus adapter → runtime.error bus event chain.
//
// The handler call is best-effort: a panic is recovered and logged.
// Bus-emit failures must not crash the worker.
func (e *engine) logWorkerError(env messages.Envelope, err error) {
	if e.logger != nil {
		q := env.Identity()
		var (
			code     string
			nodeName string
		)
		if re, ok := asRunError(err); ok {
			code = string(re.Code)
			nodeName = re.NodeName
		}
		e.logger.Error("engine: worker error",
			slog.String("error", err.Error()),
			slog.String("code", code),
			slog.String("node", nodeName),
			slog.String("tenant_id", q.TenantID),
			slog.String("user_id", q.UserID),
			slog.String("session_id", q.SessionID),
			slog.String("run_id", q.RunID),
		)
	}
	// Phase 11: if a RunErrorHandler is wired and we have a typed
	// RunError, fire it. The handler is the seam telemetry.Logger
	// connects through to the wave-2 eventbus adapter.
	if e.cfg.runErrorHandler != nil {
		if re, ok := asRunError(err); ok {
			func() {
				defer func() {
					if r := recover(); r != nil && e.logger != nil {
						e.logger.Error("engine: RunErrorHandler panicked",
							slog.Any("panic", r))
					}
				}()
				// Build a ctx that carries the failing envelope's
				// quadruple so the BusEmitter (which reads identity
				// from ctx) sees a complete triple.
				ctx := identityCtxFor(env)
				e.cfg.runErrorHandler(ctx, re)
			}()
		}
	}
}

// emitErrorEnvelope writes a special error-shaped envelope onto the
// engine's outlet so callers using WithErrorEmissionToEgress can
// Fetch it. Payload carries the *RunError directly; callers
// type-assert.
//
// Best-effort: a saturated outlet drops the error envelope rather
// than blocking the worker. The error has already gone to the
// logger + handler (the bus-side path), so the egress emission is
// strictly an additive surface for callers who want it.
func (e *engine) emitErrorEnvelope(_ context.Context, env messages.Envelope, err error) {
	re, ok := asRunError(err)
	if !ok {
		return
	}
	out := messages.Envelope{
		Headers:    env.Headers,
		RunID:      env.RunID,
		SessionID:  env.SessionID,
		Timestamp:  time.Now(),
		DeadlineAt: env.DeadlineAt,
		Payload:    re,
	}
	defer func() { _ = recover() }() // outletChan may be closed during Stop
	select {
	case e.outletChan <- out:
	default:
		// Egress saturated; the bus + logger paths still saw the
		// error. Dropping here is documented as the egress contract.
	}
}

// identityCtxFor returns a context.Context carrying the envelope's
// identity quadruple via Phase 01 helpers. Used to ensure the
// RunErrorHandler's BusEmitter sees a complete triple regardless of
// what the worker's ctx carried.
func identityCtxFor(env messages.Envelope) context.Context {
	q := env.Identity()
	ctx, err := identity.With(context.Background(), q.Identity)
	if err != nil {
		return context.Background()
	}
	if q.RunID != "" {
		ctx, err = identity.WithRun(ctx, q.Identity, q.RunID)
		if err != nil {
			return context.Background()
		}
	}
	return ctx
}

// SetLogger lets callers (or tests) install a custom slog.Logger.
// Internally synchronized; safe to call before Run. Replaces the
// default slog.Default() handler.
func (e *engine) SetLogger(l *slog.Logger) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.logger = l
}

// Compile-time assertion: *engine satisfies Engine.
var _ Engine = (*engine)(nil)
