package types

import (
	"sort"
	"time"
)

// TopologyProjection is the canonical Protocol wire shape of an engine's
// node graph (Phase 74 / D-114). It is the projection a Protocol client
// — the Live Runtime topology canvas (Phase 73b), the Playground trace
// toggle (Phase 73n), a third-party Console — renders the engine's
// static graph + live per-edge queue depth from. It is NOT a re-export
// of any internal Runtime type: the Runtime constructs a
// TopologyProjection from its private adjacency list + per-edge channel
// state at request / emit time, and the consumer never sees those
// internals (CLAUDE.md §8; RFC §5.1 — a Protocol type that mapped 1:1
// onto an internal Go struct would be the reject-on-sight smell).
//
// The projection is engine-scoped, not run-scoped: it carries the
// static graph plus the live channel depth, both byte-stable across
// runs of the same engine. Per-run overlays (node status, latency,
// selection state) are NOT here — the consumer composes those from the
// existing event taxonomy (`tool.invoked`, `task.spawned`,
// `pause.requested`, ...). See D-114.
//
// TopologyProjection is the wire shape on BOTH Phase 74 surfaces: the
// `topology.snapshot` Protocol method (request → reply) returns one, and
// the `topology.changed` canonical event carries one in its payload.
type TopologyProjection struct {
	// EngineID identifies the engine the projection describes. In V1
	// (one engine per Runtime process) it is the Runtime's engine id;
	// the field exists so a post-V1 multi-engine Runtime can scope a
	// projection without a wire-shape break.
	EngineID string `json:"engine_id"`
	// OccurredAt is the wall-clock instant the projection was built.
	// On the `topology.changed` event it is the construction / edge-
	// change time; on a `topology.snapshot` reply it is the snapshot
	// time.
	OccurredAt time.Time `json:"occurred_at"`
	// Nodes is the engine's node set, sorted lexicographically by Name
	// for byte-stability across snapshots of the same engine.
	Nodes []TopologyNode `json:"nodes"`
	// Edges is the engine's directed edge set, sorted lexicographically
	// by (From, To) for byte-stability.
	Edges []TopologyEdge `json:"edges"`
}

// TopologyNodeKind tags a node's role in the graph. The V1 set is
// closed — inlet (no parent), node (intermediate), outlet (no child).
type TopologyNodeKind string

// The V1 node-kind constants. A node with no parent is an inlet; a node
// with no child is an outlet; every other node is a plain node.
const (
	// NodeKindInlet tags a node with no incoming edge — an entry point
	// where an external Emit lands.
	NodeKindInlet TopologyNodeKind = "inlet"
	// NodeKindNode tags an intermediate node — it has both a parent and
	// a child.
	NodeKindNode TopologyNodeKind = "node"
	// NodeKindOutlet tags a node with no outgoing edge — an exit point
	// that writes to the engine's egress.
	NodeKindOutlet TopologyNodeKind = "outlet"
)

// TopologyNode is one vertex of the projected graph — a name plus a
// role tag. Status / latency / selection are deliberately absent: they
// are per-run overlays the consumer composes from the event stream.
type TopologyNode struct {
	// Name is the node's unique identifier within the engine.
	Name string `json:"name"`
	// Kind is the node's role tag — inlet / node / outlet.
	Kind TopologyNodeKind `json:"kind"`
}

// TopologyEdge is one directed edge of the projected graph plus its
// live bounded-channel state. QueueDepth / QueueCapacity are the
// liveness fields the Live Runtime canvas renders as a fill bar.
type TopologyEdge struct {
	// From is the upstream node's Name.
	From string `json:"from"`
	// To is the downstream node's Name.
	To string `json:"to"`
	// QueueDepth is the number of envelopes currently buffered on the
	// (From → To) channel — len(channel) at projection time.
	QueueDepth int `json:"queue_depth"`
	// QueueCapacity is the bounded buffer size of the (From → To)
	// channel — cap(channel). A consumer renders QueueDepth /
	// QueueCapacity as a saturation indicator.
	QueueCapacity int `json:"queue_capacity"`
}

// SortDeterministic sorts the projection's Nodes (by Name) and Edges
// (by From then To) in place so two projections of the same engine
// marshal to byte-identical JSON. The Runtime calls this before
// returning a projection on either Phase 74 surface; callers
// constructing a projection by hand (tests) call it to get the
// canonical ordering.
func (p *TopologyProjection) SortDeterministic() {
	sort.Slice(p.Nodes, func(i, j int) bool {
		return p.Nodes[i].Name < p.Nodes[j].Name
	})
	sort.Slice(p.Edges, func(i, j int) bool {
		if p.Edges[i].From != p.Edges[j].From {
			return p.Edges[i].From < p.Edges[j].From
		}
		return p.Edges[i].To < p.Edges[j].To
	})
}

// TopologySnapshotRequest is the wire request for the `topology.snapshot`
// Protocol method. It carries the flat identity scope the runtime-side
// surface translates into the runtime's identity triple at the edge —
// identity is mandatory (RFC §5.5). The request targets the Runtime's
// engine; a cross-tenant snapshot (Identity.Tenant ≠ the engine's
// tenant) requires the verified `auth.ScopeAdmin` claim (D-079).
type TopologySnapshotRequest struct {
	// Identity is the mandatory caller identity scope. An incomplete
	// triple fails the request closed with CodeIdentityRequired.
	Identity IdentityScope `json:"identity"`
}
