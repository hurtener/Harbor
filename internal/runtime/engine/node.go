package engine

import (
	"context"

	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// NodeFunc is the unit of computation a Node wraps. It receives the
// incoming envelope plus a per-invocation NodeContext (engine handle
// scoped to the running node) and returns the outgoing envelope.
//
// Returning a nil envelope WITH nil error means "no emission on this
// hop" — the worker drops the result and waits for the next ingress
// envelope. Returning a non-nil error logs through the audit-redacted
// logger (Phase 04 wiring) and continues; Phase 11 promotes errors to
// the structured RunError envelope.
type NodeFunc func(ctx context.Context, in messages.Envelope, nctx *NodeContext) (messages.Envelope, error)

// Node wraps a typed async function with reliability policy + cycle
// opt-in. Phase 10 shipped the shape; Phase 11 fills NodePolicy with
// real fields (Validate / Timeout / Retry / Backoff) and the worker
// loop now applies them via the reliability shell.
type Node struct {
	// Name is the unique identifier for the node within an engine.
	// New rejects duplicates with ErrDuplicateNodeName.
	Name string
	// Func is invoked by the worker loop on each incoming envelope.
	// nil Func is rejected at New time.
	Func NodeFunc
	// Policy controls validation, timeout, retry, and backoff for
	// each invocation of Func. Zero value = "no policy" (single
	// invocation, no timeout, no retry — Phase 10's bare worker
	// behavior). See policy.go for fields.
	Policy NodePolicy
	// AllowCycle opts this node out of the cycle detector. Set true
	// for legitimate self-loop or controller-loop graphs (e.g. a
	// planner node that emits to itself for the next reasoning step).
	AllowCycle bool
}

// NodeRef identifies a node by name. Used for per-channel queue
// overrides and EmitTo's explicit-target form.
type NodeRef struct {
	Name string
}

// Ref returns a NodeRef for n.
func (n Node) Ref() NodeRef { return NodeRef{Name: n.Name} }

// NodeContext is the per-invocation handle the worker passes to the
// NodeFunc. Carries the engine reference so the function can Emit /
// EmitNoWait / EmitChunk / Fetch through the same channel mechanic as
// external callers. CallSubflow (Phase 14) hangs off this same type.
//
// lastEnv records the incoming envelope for the current invocation so
// per-invocation operations (EmitChunk's identity propagation, future
// pause-resume hooks) can see the originating run's quadruple without
// requiring callers to thread it through manually. The worker loop
// sets this before invoking Func.
//
// NodeContext is constructed by the worker; callers must not build
// one directly. The struct's internal fields are unexported.
type NodeContext struct {
	engine  *engine
	node    string
	lastEnv messages.Envelope
}

// Emit sends env down the node's outgoing channel(s). Blocks if any
// outgoing channel is full — this is the backpressure path. Phase 12
// will hook capacity waiters here so per-run streaming doesn't
// deadlock against shared bounded queues.
//
// When the node has multiple outgoing edges (fan-out), Emit copies
// env to each edge in adjacency order. A single full channel pauses
// the entire emit until that channel drains.
func (nctx *NodeContext) Emit(ctx context.Context, env messages.Envelope) error {
	return nctx.engine.emitFromNode(ctx, nctx.node, env, false)
}

// EmitNoWait is the non-blocking variant. Returns ErrChannelFull
// immediately if any outgoing channel is saturated. Callers that
// want backpressure should use Emit; callers that want to drop
// rather than wait use EmitNoWait.
//
// `ctx` carries identity (D-001) into the downstream emit path and
// honours the caller's cancellation. The send itself is
// non-blocking; ctx is read for identity propagation + early-exit
// on a cancelled run, NOT to wait for channel capacity.
func (nctx *NodeContext) EmitNoWait(ctx context.Context, env messages.Envelope) error {
	return nctx.engine.emitFromNode(ctx, nctx.node, env, true)
}
