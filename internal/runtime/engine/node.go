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

// Node wraps a typed async function with policy + cycle opt-in. Phase
// 10 ships the shape; the Policy field is reserved for Phase 11's
// reliability shell (timeout / retry / validate) and is unused at this
// layer.
type Node struct {
	// Name is the unique identifier for the node within an engine.
	// New rejects duplicates with ErrDuplicateNodeName.
	Name string
	// Func is invoked by the worker loop on each incoming envelope.
	// nil Func is rejected at New time.
	Func NodeFunc
	// Policy is reserved for Phase 11 (NodePolicy). Phase 10's worker
	// does NOT consult this field; the type is here so adjacencies
	// remain stable across the wave-4 chain.
	Policy NodePolicy
	// AllowCycle opts this node out of the cycle detector. Set true
	// for legitimate self-loop or controller-loop graphs (e.g. a
	// planner node that emits to itself for the next reasoning step).
	AllowCycle bool
}

// NodePolicy is reserved for Phase 11. Phase 10 ships the type so
// adjacencies don't need to refactor when the reliability shell
// lands; the worker never reads any field here.
type NodePolicy struct {
	// Reserved for Phase 11.
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
// EmitNoWait / Fetch through the same channel mechanic as external
// callers. EmitChunk (Phase 12) and CallSubflow (Phase 14) will hang
// off this same type.
//
// NodeContext is constructed by the worker; callers must not build
// one directly. The struct's internal fields are unexported.
type NodeContext struct {
	engine *engine
	node   string // node Name; identifies which adjacency to emit on
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
func (nctx *NodeContext) EmitNoWait(env messages.Envelope) error {
	return nctx.engine.emitFromNode(context.Background(), nctx.node, env, true)
}
