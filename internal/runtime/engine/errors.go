package engine

import "errors"

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrCycleDetected — engine.New detected an unintended cycle in
	// the adjacency graph. The wrapped message includes the cycle
	// path (e.g. "A -> B -> C -> A"). Per-node AllowCycle: true opts
	// the node out of the detector for legitimate self-loops or
	// controller-loop graphs.
	ErrCycleDetected = errors.New("engine: cycle detected without AllowCycle")
	// ErrIdentityRequired — Emit was called with an Envelope whose
	// identity triple (TenantID, UserID, SessionID) is incomplete.
	// Wraps identity.ErrIdentityIncomplete so callers can errors.Is
	// against either sentinel.
	ErrIdentityRequired = errors.New("engine: Emit requires non-empty identity triple")
	// ErrChannelFull — EmitNoWait found the outgoing channel
	// saturated. Use Emit for blocking semantics with backpressure.
	ErrChannelFull = errors.New("engine: channel full (use Emit for blocking semantics)")
	// ErrEngineStopped — operation attempted on an engine whose Stop
	// has been called (or whose internal context was cancelled). The
	// engine never resumes after Stop; callers must construct a new
	// engine.
	ErrEngineStopped = errors.New("engine: stopped")
	// ErrInvalidQueueSize — WithQueueSize / WithChannelOverride
	// received n <= 0. Returned from New.
	ErrInvalidQueueSize = errors.New("engine: queue size must be > 0")
	// ErrNodeNotFound — a NodeRef referenced a name that doesn't
	// exist in the engine's adjacency set.
	ErrNodeNotFound = errors.New("engine: node not found")
	// ErrNotImplemented — a method that lands in a later phase was
	// called. Reserved for forward stubs; no current method returns
	// this (Phase 10's Cancel + FetchByRun stubs were filled in Phase
	// 13). Callers that errors.Is on this can detect "this surface
	// isn't ready yet" without crashing.
	ErrNotImplemented = errors.New("engine: not implemented in this phase")
	// ErrDuplicateNodeName — two adjacencies referenced different
	// nodes with the same Name. The engine's worker map is keyed by
	// Name; duplicates are a build mis-configuration.
	ErrDuplicateNodeName = errors.New("engine: duplicate node name")
	// ErrDeadlineExceeded — the worker observed an Envelope whose
	// DeadlineAt has passed before it could invoke the node. Phase 11
	// will promote this to a structured RunError; Phase 10 returns
	// the typed sentinel directly.
	ErrDeadlineExceeded = errors.New("engine: envelope deadline exceeded")
	// ErrEmptyAdjacencies — New was called with an empty adjacency
	// list. Engines must have at least one node to do useful work.
	ErrEmptyAdjacencies = errors.New("engine: adjacencies must be non-empty")
)
