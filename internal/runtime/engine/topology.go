package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// Topology builds a canonical types.TopologyProjection of this engine —
// the static node graph plus the live per-edge bounded-channel depth.
// It is the runtime-side accessor the Phase 74 `topology.snapshot`
// Protocol method calls; the Protocol surface returns the projection to
// the caller verbatim (Phase 74 / D-114).
//
// Identity is mandatory (RFC §5.5, CLAUDE.md §6 rule 9): a ctx whose
// identity triple is incomplete fails closed with ErrIdentityRequired —
// there is no identity-downgrading knob. The projection itself carries
// no tenant id (it is engine-scoped, not tenant-scoped), but the
// accessor still demands a complete identity so the read is auditable
// and consistent with every other identity-mandatory runtime surface.
//
// The returned projection is deterministic: Nodes are sorted by Name,
// Edges by (From, To). Two calls against the same engine with the same
// channel state return byte-identical projections — the property the
// `topology.snapshot` ↔ `topology.changed` byte-stability contract
// rests on.
//
// Topology is a pure read: it never mutates the engine. It is safe for
// N concurrent callers against one shared engine under -race (D-025) —
// topology_concurrent_test.go pins N≥128. The per-edge queue-depth read
// (len / cap of the bounded channel) holds no engine lock: len and cap
// on a channel are atomic in Go and the channel map itself is written
// once at New and never mutated after, so the read needs no
// synchronisation.
func (e *engine) Topology(ctx context.Context) (types.TopologyProjection, error) {
	if err := identity.Validate(identityFromCtx(ctx)); err != nil {
		return types.TopologyProjection{}, fmt.Errorf("%w: %w", ErrIdentityRequired, err)
	}
	return buildProjection(e.engineID, e.nodes, e.adjs, e.channels, time.Now()), nil
}

// identityFromCtx extracts the identity triple from ctx. A ctx with no
// identity returns a zero Identity, which identity.Validate rejects —
// the fail-closed path for an unscoped Topology call.
func identityFromCtx(ctx context.Context) identity.Identity {
	q, ok := identity.From(ctx)
	if !ok {
		return identity.Identity{}
	}
	return q
}

// buildProjection is the pure-function projection builder. It reads the
// engine's node index + adjacency list + per-edge channel map and emits
// a deterministic types.TopologyProjection. It has no engine-state
// dependency beyond its arguments and no package-level mutable state —
// safe to call concurrently (D-025).
//
// Node kind is derived from the graph shape: a node with no incoming
// edge is an inlet, a node with no outgoing edge is an outlet, every
// other node is a plain node. A single-node graph (its own inlet AND
// outlet) is tagged inlet — the inlet role wins, matching the engine's
// own inlet-first lexicographic Emit default.
//
// Per-edge queue depth / capacity come from the bounded channel for
// that (from → to) edge. An edge with no allocated channel (should not
// happen for a constructed engine) reports depth 0 / capacity 0 rather
// than panicking — fail visible, not fatal.
func buildProjection(
	engineID string,
	nodes map[string]Node,
	adjs []Adjacency,
	channels map[string]map[string]chan messages.Envelope,
	at time.Time,
) types.TopologyProjection {
	hasParent := make(map[string]bool, len(nodes))
	hasChild := make(map[string]bool, len(nodes))
	for _, adj := range adjs {
		if len(adj.To) > 0 {
			hasChild[adj.From.Name] = true
		}
		for _, to := range adj.To {
			hasParent[to.Name] = true
		}
	}

	projNodes := make([]types.TopologyNode, 0, len(nodes))
	for name := range nodes {
		kind := types.NodeKindNode
		switch {
		case !hasParent[name]:
			kind = types.NodeKindInlet
		case !hasChild[name]:
			kind = types.NodeKindOutlet
		}
		projNodes = append(projNodes, types.TopologyNode{Name: name, Kind: kind})
	}

	projEdges := make([]types.TopologyEdge, 0)
	for _, adj := range adjs {
		for _, to := range adj.To {
			depth, capacity := 0, 0
			if byTo, ok := channels[adj.From.Name]; ok {
				if ch, ok := byTo[to.Name]; ok {
					depth = len(ch)
					capacity = cap(ch)
				}
			}
			projEdges = append(projEdges, types.TopologyEdge{
				From:          adj.From.Name,
				To:            to.Name,
				QueueDepth:    depth,
				QueueCapacity: capacity,
			})
		}
	}

	p := types.TopologyProjection{
		EngineID:   engineID,
		OccurredAt: at,
		Nodes:      projNodes,
		Edges:      projEdges,
	}
	p.SortDeterministic()
	return p
}

// publishTopologyChanged emits one EventTypeTopologyChanged event onto
// the configured bus carrying the engine's current projection. It is
// called once from New (construction-time emit) when WithEventBus
// supplied a bus. A nil bus is a no-op — the Phase 02 engine-test
// surface that never wires a bus sees zero behavioural change.
//
// Best-effort: a bus Publish error is returned to the caller (New)
// which surfaces it loud rather than swallowing it — a topology event
// the bus rejected is a wiring bug the operator must see (CLAUDE.md §5
// fail-loudly). The emit uses a fresh background ctx carrying the
// engine's own synthetic system identity: the projection is engine-
// scoped, not run-scoped, and construction happens outside any request
// ctx.
func (e *engine) publishTopologyChanged(ctx context.Context) error {
	if e.cfg.eventBus == nil {
		return nil
	}
	proj := buildProjection(e.engineID, e.nodes, e.adjs, e.channels, time.Now())
	q, ok := identity.From(ctx)
	if !ok {
		return fmt.Errorf("%w: topology.changed emit requires an identity-scoped ctx", ErrIdentityRequired)
	}
	ev := events.Event{
		Type:     events.EventTypeTopologyChanged,
		Identity: identity.Quadruple{Identity: q},
		Payload: events.TopologyChangedPayload{
			Projection: proj,
		},
	}
	if err := e.cfg.eventBus.Publish(ctx, ev); err != nil {
		return fmt.Errorf("engine: publish topology.changed: %w", err)
	}
	return nil
}

// newEngineID mints a fresh engine identifier. It is 16 random bytes
// hex-encoded — collision-free for any realistic process and stable
// for the engine's lifetime (set once at New, never mutated). The id
// appears on every TopologyProjection so a multi-engine post-V1
// Runtime can scope a projection without a wire-shape break.
func newEngineID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is catastrophic and effectively never
		// happens; fall back to a time-derived id rather than panicking
		// in a constructor path.
		return fmt.Sprintf("engine-%d", time.Now().UnixNano())
	}
	return "engine-" + hex.EncodeToString(b[:])
}
