package protocol_test

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// fakeTopologyAccessor is a deterministic TopologyAccessor for the
// Phase 74 ControlSurface tests. It is NOT a mock at a subsystem seam
// (CLAUDE.md §17.3) — the real engine.Engine is exercised end-to-end by
// test/integration/phase74_topology_test.go; here we only need a
// deterministic projection + a tenant to drive the surface's dispatch
// + admin-gate logic.
type fakeTopologyAccessor struct {
	tenant string
	proj   types.TopologyProjection
	err    error
}

func (f *fakeTopologyAccessor) Topology(_ context.Context) (types.TopologyProjection, error) {
	if f.err != nil {
		return types.TopologyProjection{}, f.err
	}
	return f.proj, nil
}

func (f *fakeTopologyAccessor) TenantID() string { return f.tenant }

func sampleProjection() types.TopologyProjection {
	p := types.TopologyProjection{
		EngineID:   "engine-test",
		OccurredAt: time.Unix(1000, 0).UTC(),
		Nodes: []types.TopologyNode{
			{Name: "in", Kind: types.NodeKindInlet},
			{Name: "out", Kind: types.NodeKindOutlet},
		},
		Edges: []types.TopologyEdge{
			{From: "in", To: "out", QueueDepth: 0, QueueCapacity: 64},
		},
	}
	p.SortDeterministic()
	return p
}

// newTopologySurface builds a ControlSurface with a wired topology
// accessor + an injected scope checker. adminScopes is the set of
// scopes the checker reports true for.
func newTopologySurface(t *testing.T, accessor protocol.TopologyAccessor, adminScopes ...auth.Scope) *protocol.ControlSurface {
	t.Helper()
	fx := newSurfaceFixture(t)
	scopeSet := map[auth.Scope]struct{}{}
	for _, s := range adminScopes {
		scopeSet[s] = struct{}{}
	}
	checker := func(_ context.Context, s auth.Scope) bool {
		_, ok := scopeSet[s]
		return ok
	}
	surface, err := protocol.NewControlSurface(fx.tasks, fx.steering,
		protocol.WithTopologyAccessor(accessor),
		protocol.WithScopeChecker(checker),
		protocol.WithEventBus(fx.bus),
	)
	if err != nil {
		t.Fatalf("NewControlSurface(topology): %v", err)
	}
	return surface
}

// TestDispatch_Topology_HappyPath — a same-tenant snapshot returns the
// engine's projection.
func TestDispatch_Topology_HappyPath(t *testing.T) {
	accessor := &fakeTopologyAccessor{tenant: "tenant-1", proj: sampleProjection()}
	surface := newTopologySurface(t, accessor)

	resp, err := surface.Dispatch(context.Background(), methods.MethodTopologySnapshot,
		&types.TopologySnapshotRequest{
			Identity: types.IdentityScope{Tenant: "tenant-1", User: "u1", Session: "s1"},
		})
	if err != nil {
		t.Fatalf("Dispatch(topology.snapshot) error = %v, want nil", err)
	}
	proj, ok := resp.(*types.TopologyProjection)
	if !ok {
		t.Fatalf("response type = %T, want *types.TopologyProjection", resp)
	}
	if proj.EngineID != "engine-test" {
		t.Errorf("EngineID = %q, want engine-test", proj.EngineID)
	}
	if len(proj.Nodes) != 2 || len(proj.Edges) != 1 {
		t.Errorf("projection shape = %d nodes / %d edges, want 2 / 1", len(proj.Nodes), len(proj.Edges))
	}
}

// TestDispatch_Topology_NilAccessor_UnknownMethod — a Runtime that
// hosts no engine rejects topology.snapshot with CodeUnknownMethod.
func TestDispatch_Topology_NilAccessor_UnknownMethod(t *testing.T) {
	fx := newSurfaceFixture(t) // surface built WITHOUT WithTopologyAccessor
	_, err := fx.surface.Dispatch(context.Background(), methods.MethodTopologySnapshot,
		&types.TopologySnapshotRequest{
			Identity: types.IdentityScope{Tenant: "t", User: "u", Session: "s"},
		})
	if got := codeOf(t, err); got != protoerrors.CodeUnknownMethod {
		t.Fatalf("Dispatch(topology.snapshot, nil accessor) code = %q, want %q", got, protoerrors.CodeUnknownMethod)
	}
}

// TestDispatch_Topology_WrongRequestType_FailsClosed — a wrong wire
// type fails closed with CodeInvalidRequest.
func TestDispatch_Topology_WrongRequestType_FailsClosed(t *testing.T) {
	accessor := &fakeTopologyAccessor{tenant: "tenant-1", proj: sampleProjection()}
	surface := newTopologySurface(t, accessor)

	_, err := surface.Dispatch(context.Background(), methods.MethodTopologySnapshot,
		&types.StartRequest{Identity: types.IdentityScope{Tenant: "t", User: "u", Session: "s"}})
	if got := codeOf(t, err); got != protoerrors.CodeInvalidRequest {
		t.Fatalf("Dispatch(topology.snapshot, wrong type) code = %q, want %q", got, protoerrors.CodeInvalidRequest)
	}
	_, err = surface.Dispatch(context.Background(), methods.MethodTopologySnapshot, nil)
	if got := codeOf(t, err); got != protoerrors.CodeInvalidRequest {
		t.Fatalf("Dispatch(topology.snapshot, nil) code = %q, want %q", got, protoerrors.CodeInvalidRequest)
	}
}

// TestDispatch_Topology_IncompleteIdentity_FailsClosed — a request with
// an incomplete identity triple fails closed with CodeIdentityRequired.
func TestDispatch_Topology_IncompleteIdentity_FailsClosed(t *testing.T) {
	accessor := &fakeTopologyAccessor{tenant: "tenant-1", proj: sampleProjection()}
	surface := newTopologySurface(t, accessor)

	for _, scope := range []types.IdentityScope{
		{},
		{Tenant: "tenant-1"},
		{Tenant: "tenant-1", User: "u1"},
		{User: "u1", Session: "s1"},
	} {
		_, err := surface.Dispatch(context.Background(), methods.MethodTopologySnapshot,
			&types.TopologySnapshotRequest{Identity: scope})
		if got := codeOf(t, err); got != protoerrors.CodeIdentityRequired {
			t.Errorf("Dispatch(topology.snapshot, scope=%+v) code = %q, want %q", scope, got, protoerrors.CodeIdentityRequired)
		}
	}
}

// TestDispatch_Topology_CrossTenantNonAdmin_FailsClosed — a caller
// whose tenant differs from the engine's tenant, without the admin
// scope, is rejected with CodeAuthRejected (D-079).
func TestDispatch_Topology_CrossTenantNonAdmin_FailsClosed(t *testing.T) {
	accessor := &fakeTopologyAccessor{tenant: "engine-tenant", proj: sampleProjection()}
	surface := newTopologySurface(t, accessor) // no admin scope

	_, err := surface.Dispatch(context.Background(), methods.MethodTopologySnapshot,
		&types.TopologySnapshotRequest{
			Identity: types.IdentityScope{Tenant: "other-tenant", User: "u1", Session: "s1"},
		})
	if got := codeOf(t, err); got != protoerrors.CodeAuthRejected {
		t.Fatalf("Dispatch(topology.snapshot, cross-tenant, no admin) code = %q, want %q", got, protoerrors.CodeAuthRejected)
	}
}

// TestDispatch_Topology_CrossTenantAdmin_Succeeds — the same cross-
// tenant call succeeds when the caller holds the verified admin scope,
// AND emits an audit.admin_scope_used event on the bus (RFC §6.13).
func TestDispatch_Topology_CrossTenantAdmin_Succeeds(t *testing.T) {
	accessor := &fakeTopologyAccessor{tenant: "engine-tenant", proj: sampleProjection()}
	fx := newSurfaceFixture(t)
	checker := func(_ context.Context, s auth.Scope) bool { return s == auth.ScopeAdmin }
	surface, err := protocol.NewControlSurface(fx.tasks, fx.steering,
		protocol.WithTopologyAccessor(accessor),
		protocol.WithScopeChecker(checker),
		protocol.WithEventBus(fx.bus),
	)
	if err != nil {
		t.Fatalf("NewControlSurface: %v", err)
	}

	// Subscribe to audit.admin_scope_used for the caller's identity.
	sub, err := fx.bus.Subscribe(context.Background(), events.Filter{
		Tenant: "other-tenant", User: "u1", Session: "s1",
		Types: []events.EventType{events.EventTypeAdminScopeUsed},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	resp, err := surface.Dispatch(context.Background(), methods.MethodTopologySnapshot,
		&types.TopologySnapshotRequest{
			Identity: types.IdentityScope{Tenant: "other-tenant", User: "u1", Session: "s1"},
		})
	if err != nil {
		t.Fatalf("Dispatch(topology.snapshot, cross-tenant, admin) error = %v, want nil", err)
	}
	if _, ok := resp.(*types.TopologyProjection); !ok {
		t.Fatalf("response type = %T, want *types.TopologyProjection", resp)
	}

	select {
	case ev := <-sub.Events():
		if ev.Type != events.EventTypeAdminScopeUsed {
			t.Fatalf("event type = %q, want audit.admin_scope_used", ev.Type)
		}
		if ev.Identity.TenantID != "other-tenant" {
			t.Errorf("audit event tenant = %q, want other-tenant (the admin caller)", ev.Identity.TenantID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no audit.admin_scope_used event within 2s of a cross-tenant admin topology read")
	}
}
