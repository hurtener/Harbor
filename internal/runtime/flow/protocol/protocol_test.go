package protocol_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	flowprotocol "github.com/hurtener/Harbor/internal/runtime/flow/protocol"
)

// fakeCatalog is a deterministic Catalog for surface-logic tests. It is
// a *_test.go-scoped fixture (CLAUDE.md §13 — test stubs live in test
// files); the production Catalog is RegistryCatalog, exercised by
// catalog_test.go against real drivers.
type fakeCatalog struct {
	flows     []prototypes.Flow
	desc      prototypes.FlowDescription
	runs      []prototypes.FlowRun
	runDesc   prototypes.FlowRunDescription
	metrics   prototypes.FlowMetrics
	notFound  bool
	lastAdmin bool
}

func (f *fakeCatalog) ListFlows(_ context.Context, _ identity.Identity, admin bool) ([]prototypes.Flow, error) {
	f.lastAdmin = admin
	return f.flows, nil
}

func (f *fakeCatalog) DescribeFlow(_ context.Context, _ identity.Identity, _ bool, flowID string) (prototypes.FlowDescription, error) {
	if f.notFound {
		return prototypes.FlowDescription{}, flowprotocol.ErrNotFound
	}
	return f.desc, nil
}

func (f *fakeCatalog) ListRuns(_ context.Context, _ identity.Identity, admin bool, _ string, _ []string) ([]prototypes.FlowRun, error) {
	f.lastAdmin = admin
	if f.notFound {
		return nil, flowprotocol.ErrNotFound
	}
	return f.runs, nil
}

func (f *fakeCatalog) DescribeRun(_ context.Context, _ identity.Identity, _ bool, _ string) (prototypes.FlowRunDescription, error) {
	if f.notFound {
		return prototypes.FlowRunDescription{}, flowprotocol.ErrNotFound
	}
	return f.runDesc, nil
}

func (f *fakeCatalog) FlowMetrics(_ context.Context, _ identity.Identity, _ bool, _ string, _, _ time.Duration) (prototypes.FlowMetrics, error) {
	if f.notFound {
		return prototypes.FlowMetrics{}, flowprotocol.ErrNotFound
	}
	return f.metrics, nil
}

// fakeInvoker is a deterministic Invoker for surface-logic tests.
type fakeInvoker struct {
	resp   prototypes.FlowRunResponse
	err    error
	called bool
}

func (i *fakeInvoker) Invoke(_ context.Context, _ identity.Identity, _ string, _ map[string]any) (prototypes.FlowRunResponse, error) {
	i.called = true
	if i.err != nil {
		return prototypes.FlowRunResponse{}, i.err
	}
	return i.resp, nil
}

func validScope() prototypes.IdentityScope {
	return prototypes.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"}
}

func newSurface(t *testing.T, cat *fakeCatalog, inv *fakeInvoker) *flowprotocol.Surface {
	t.Helper()
	s, err := flowprotocol.NewSurface(cat, inv)
	if err != nil {
		t.Fatalf("NewSurface: %v", err)
	}
	return s
}

func TestNewSurface_NilDependencyFailsLoud(t *testing.T) {
	if _, err := flowprotocol.NewSurface(nil, &fakeInvoker{}); !errors.Is(err, flowprotocol.ErrMisconfigured) {
		t.Fatalf("NewSurface(nil catalog): err = %v, want ErrMisconfigured", err)
	}
	if _, err := flowprotocol.NewSurface(&fakeCatalog{}, nil); !errors.Is(err, flowprotocol.ErrMisconfigured) {
		t.Fatalf("NewSurface(nil invoker): err = %v, want ErrMisconfigured", err)
	}
}

func TestList_IdentityMandatory(t *testing.T) {
	s := newSurface(t, &fakeCatalog{}, &fakeInvoker{})
	_, err := s.List(context.Background(), prototypes.FlowListRequest{
		Identity: prototypes.IdentityScope{Tenant: "t1"}, // incomplete
	}, false)
	if !errors.Is(err, flowprotocol.ErrIdentityRequired) {
		t.Fatalf("List(incomplete identity): err = %v, want ErrIdentityRequired", err)
	}
}

func TestList_CrossTenantWithoutAdminRejected(t *testing.T) {
	s := newSurface(t, &fakeCatalog{}, &fakeInvoker{})
	_, err := s.List(context.Background(), prototypes.FlowListRequest{
		Identity: validScope(),
		Filter:   prototypes.FlowFilter{Tenants: []string{"t1", "t-other"}},
	}, false)
	if !errors.Is(err, flowprotocol.ErrCrossTenantScope) {
		t.Fatalf("List(cross-tenant, no admin): err = %v, want ErrCrossTenantScope", err)
	}
}

func TestList_CrossTenantWithAdminAllowed(t *testing.T) {
	cat := &fakeCatalog{flows: []prototypes.Flow{{ID: "z"}, {ID: "a"}}}
	s := newSurface(t, cat, &fakeInvoker{})
	resp, err := s.List(context.Background(), prototypes.FlowListRequest{
		Identity: validScope(),
		Filter:   prototypes.FlowFilter{Tenants: []string{"t1", "t-other"}},
	}, true)
	if err != nil {
		t.Fatalf("List(cross-tenant, admin): unexpected error: %v", err)
	}
	if !cat.lastAdmin {
		t.Fatal("List: catalog not called with adminScoped=true")
	}
	// Result must be sorted by ID.
	if resp.Flows[0].ID != "a" || resp.Flows[1].ID != "z" {
		t.Fatalf("List: not sorted by ID: %+v", resp.Flows)
	}
}

func TestList_PageSizeOverMaxRejected(t *testing.T) {
	s := newSurface(t, &fakeCatalog{}, &fakeInvoker{})
	_, err := s.List(context.Background(), prototypes.FlowListRequest{
		Identity: validScope(),
		PageSize: prototypes.MaxFlowListPageSize + 1,
	}, false)
	if !errors.Is(err, flowprotocol.ErrInvalidRequest) {
		t.Fatalf("List(oversized page): err = %v, want ErrInvalidRequest", err)
	}
}

func TestList_PaginationAndFilter(t *testing.T) {
	cat := &fakeCatalog{flows: []prototypes.Flow{
		{ID: "alpha", Owner: "team-a", PlannerFamily: "graph"},
		{ID: "beta", Owner: "team-b", PlannerFamily: "workflow"},
		{ID: "gamma", Owner: "team-a", PlannerFamily: "graph"},
	}}
	s := newSurface(t, cat, &fakeInvoker{})
	resp, err := s.List(context.Background(), prototypes.FlowListRequest{
		Identity: validScope(),
		Filter:   prototypes.FlowFilter{PlannerFamilies: []string{"graph"}},
		PageSize: 1,
		Page:     2,
	}, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if resp.TotalRows != 2 {
		t.Fatalf("List TotalRows = %d, want 2 (graph-only)", resp.TotalRows)
	}
	if resp.PageCount != 2 {
		t.Fatalf("List PageCount = %d, want 2", resp.PageCount)
	}
	if len(resp.Flows) != 1 || resp.Flows[0].ID != "gamma" {
		t.Fatalf("List page 2 = %+v, want [gamma]", resp.Flows)
	}
}

func TestDescribe_NotFound(t *testing.T) {
	s := newSurface(t, &fakeCatalog{notFound: true}, &fakeInvoker{})
	_, err := s.Describe(context.Background(), prototypes.FlowDescribeRequest{
		Identity: validScope(), ID: "ghost",
	}, false)
	if !errors.Is(err, flowprotocol.ErrNotFound) {
		t.Fatalf("Describe(ghost): err = %v, want ErrNotFound", err)
	}
}

func TestDescribe_EmptyIDRejected(t *testing.T) {
	s := newSurface(t, &fakeCatalog{}, &fakeInvoker{})
	_, err := s.Describe(context.Background(), prototypes.FlowDescribeRequest{
		Identity: validScope(),
	}, false)
	if !errors.Is(err, flowprotocol.ErrInvalidRequest) {
		t.Fatalf("Describe(empty id): err = %v, want ErrInvalidRequest", err)
	}
}

func TestDescribe_SortsGraphDeterministically(t *testing.T) {
	cat := &fakeCatalog{desc: prototypes.FlowDescription{
		Nodes: []prototypes.FlowNode{{ID: "z"}, {ID: "a"}},
		Edges: []prototypes.FlowEdge{{From: "z", To: "a"}, {From: "a", To: "z"}},
	}}
	s := newSurface(t, cat, &fakeInvoker{})
	resp, err := s.Describe(context.Background(), prototypes.FlowDescribeRequest{
		Identity: validScope(), ID: "f1",
	}, false)
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if resp.Nodes[0].ID != "a" || resp.Nodes[1].ID != "z" {
		t.Fatalf("Describe: nodes not sorted: %+v", resp.Nodes)
	}
	if resp.Edges[0].From != "a" {
		t.Fatalf("Describe: edges not sorted: %+v", resp.Edges)
	}
}

func TestRunsList_EmptyFlowIDRejected(t *testing.T) {
	s := newSurface(t, &fakeCatalog{}, &fakeInvoker{})
	_, err := s.RunsList(context.Background(), prototypes.FlowRunsListRequest{
		Identity: validScope(),
	}, false)
	if !errors.Is(err, flowprotocol.ErrInvalidRequest) {
		t.Fatalf("RunsList(empty flow_id): err = %v, want ErrInvalidRequest", err)
	}
}

func TestRunsList_SortedNewestFirst(t *testing.T) {
	now := time.Now()
	cat := &fakeCatalog{runs: []prototypes.FlowRun{
		{RunID: "old", StartedAt: now.Add(-2 * time.Hour)},
		{RunID: "new", StartedAt: now},
		{RunID: "mid", StartedAt: now.Add(-time.Hour)},
	}}
	s := newSurface(t, cat, &fakeInvoker{})
	resp, err := s.RunsList(context.Background(), prototypes.FlowRunsListRequest{
		Identity: validScope(), FlowID: "f1",
	}, false)
	if err != nil {
		t.Fatalf("RunsList: %v", err)
	}
	if resp.Runs[0].RunID != "new" || resp.Runs[2].RunID != "old" {
		t.Fatalf("RunsList not newest-first: %+v", resp.Runs)
	}
}

func TestRunsList_CrossTenantWithoutAdminRejected(t *testing.T) {
	s := newSurface(t, &fakeCatalog{}, &fakeInvoker{})
	_, err := s.RunsList(context.Background(), prototypes.FlowRunsListRequest{
		Identity: validScope(), FlowID: "f1",
		Tenants: []string{"t-other"},
	}, false)
	if !errors.Is(err, flowprotocol.ErrCrossTenantScope) {
		t.Fatalf("RunsList(cross-tenant, no admin): err = %v, want ErrCrossTenantScope", err)
	}
}

func TestRunsDescribe_EmptyRunIDRejected(t *testing.T) {
	s := newSurface(t, &fakeCatalog{}, &fakeInvoker{})
	_, err := s.RunsDescribe(context.Background(), prototypes.FlowRunDescribeRequest{
		Identity: validScope(),
	}, false)
	if !errors.Is(err, flowprotocol.ErrInvalidRequest) {
		t.Fatalf("RunsDescribe(empty run_id): err = %v, want ErrInvalidRequest", err)
	}
}

func TestRun_RequiresAdminScope(t *testing.T) {
	inv := &fakeInvoker{resp: prototypes.FlowRunResponse{RunID: "r1"}}
	s := newSurface(t, &fakeCatalog{}, inv)
	_, err := s.Run(context.Background(), prototypes.FlowRunRequest{
		Identity: validScope(), FlowID: "f1",
	}, false) // adminScoped=false
	if !errors.Is(err, flowprotocol.ErrRunScopeRequired) {
		t.Fatalf("Run(no admin): err = %v, want ErrRunScopeRequired", err)
	}
	if inv.called {
		t.Fatal("Run: invoker called despite missing scope claim")
	}
}

func TestRun_WithAdminScopeInvokes(t *testing.T) {
	inv := &fakeInvoker{resp: prototypes.FlowRunResponse{RunID: "r1", Status: prototypes.FlowRunRunning}}
	s := newSurface(t, &fakeCatalog{}, inv)
	resp, err := s.Run(context.Background(), prototypes.FlowRunRequest{
		Identity: validScope(), FlowID: "f1", Inputs: map[string]any{"x": 1},
	}, true)
	if err != nil {
		t.Fatalf("Run(admin): %v", err)
	}
	if !inv.called {
		t.Fatal("Run: invoker not called")
	}
	if resp.RunID != "r1" {
		t.Fatalf("Run RunID = %q, want r1", resp.RunID)
	}
}

func TestRun_EmptyFlowIDRejected(t *testing.T) {
	s := newSurface(t, &fakeCatalog{}, &fakeInvoker{})
	_, err := s.Run(context.Background(), prototypes.FlowRunRequest{
		Identity: validScope(),
	}, true)
	if !errors.Is(err, flowprotocol.ErrInvalidRequest) {
		t.Fatalf("Run(empty flow_id): err = %v, want ErrInvalidRequest", err)
	}
}

func TestMetrics_BucketExceedsWindowRejected(t *testing.T) {
	s := newSurface(t, &fakeCatalog{}, &fakeInvoker{})
	_, err := s.Metrics(context.Background(), prototypes.FlowMetricsRequest{
		Identity: validScope(), FlowID: "f1",
		WindowMS: 1000, BucketMS: 2000,
	}, false)
	if !errors.Is(err, flowprotocol.ErrInvalidRequest) {
		t.Fatalf("Metrics(bucket>window): err = %v, want ErrInvalidRequest", err)
	}
}

func TestRunsDescribe_HappyPath(t *testing.T) {
	cat := &fakeCatalog{runDesc: prototypes.FlowRunDescription{
		Run: prototypes.FlowRun{RunID: "r1", FlowID: "f1"},
	}}
	s := newSurface(t, cat, &fakeInvoker{})
	resp, err := s.RunsDescribe(context.Background(), prototypes.FlowRunDescribeRequest{
		Identity: validScope(), RunID: "r1",
	}, false)
	if err != nil {
		t.Fatalf("RunsDescribe: %v", err)
	}
	if resp.Run.RunID != "r1" {
		t.Fatalf("RunsDescribe run = %+v", resp.Run)
	}
}

func TestRunsDescribe_IdentityMandatory(t *testing.T) {
	s := newSurface(t, &fakeCatalog{}, &fakeInvoker{})
	_, err := s.RunsDescribe(context.Background(), prototypes.FlowRunDescribeRequest{
		Identity: prototypes.IdentityScope{User: "u1"}, RunID: "r1",
	}, false)
	if !errors.Is(err, flowprotocol.ErrIdentityRequired) {
		t.Fatalf("RunsDescribe(incomplete identity): err = %v, want ErrIdentityRequired", err)
	}
}

func TestRunsDescribe_NotFound(t *testing.T) {
	s := newSurface(t, &fakeCatalog{notFound: true}, &fakeInvoker{})
	_, err := s.RunsDescribe(context.Background(), prototypes.FlowRunDescribeRequest{
		Identity: validScope(), RunID: "ghost",
	}, false)
	if !errors.Is(err, flowprotocol.ErrNotFound) {
		t.Fatalf("RunsDescribe(ghost): err = %v, want ErrNotFound", err)
	}
}

// TestRun_InvokerRuntimeErrorWrapped proves an unclassified Invoker
// failure surfaces as ErrRuntime — never silently swallowed.
func TestRun_InvokerRuntimeErrorWrapped(t *testing.T) {
	inv := &fakeInvoker{err: errors.New("launcher exploded")}
	s := newSurface(t, &fakeCatalog{}, inv)
	_, err := s.Run(context.Background(), prototypes.FlowRunRequest{
		Identity: validScope(), FlowID: "f1",
	}, true)
	if !errors.Is(err, flowprotocol.ErrRuntime) {
		t.Fatalf("Run(invoker error): err = %v, want ErrRuntime", err)
	}
}

// TestRun_InvokerNotFoundPassthrough proves an Invoker ErrNotFound is
// classified as not-found, not wrapped as ErrRuntime.
func TestRun_InvokerNotFoundPassthrough(t *testing.T) {
	inv := &fakeInvoker{err: flowprotocol.ErrNotFound}
	s := newSurface(t, &fakeCatalog{}, inv)
	_, err := s.Run(context.Background(), prototypes.FlowRunRequest{
		Identity: validScope(), FlowID: "f1",
	}, true)
	if !errors.Is(err, flowprotocol.ErrNotFound) {
		t.Fatalf("Run(invoker not-found): err = %v, want ErrNotFound", err)
	}
}

func TestMetrics_IdentityMandatory(t *testing.T) {
	s := newSurface(t, &fakeCatalog{}, &fakeInvoker{})
	_, err := s.Metrics(context.Background(), prototypes.FlowMetricsRequest{
		Identity: prototypes.IdentityScope{Tenant: "t1", User: "u1"}, FlowID: "f1",
	}, false)
	if !errors.Is(err, flowprotocol.ErrIdentityRequired) {
		t.Fatalf("Metrics(incomplete identity): err = %v, want ErrIdentityRequired", err)
	}
}

func TestMetrics_EmptyFlowIDRejected(t *testing.T) {
	s := newSurface(t, &fakeCatalog{}, &fakeInvoker{})
	_, err := s.Metrics(context.Background(), prototypes.FlowMetricsRequest{
		Identity: validScope(),
	}, false)
	if !errors.Is(err, flowprotocol.ErrInvalidRequest) {
		t.Fatalf("Metrics(empty flow_id): err = %v, want ErrInvalidRequest", err)
	}
}

func TestMetrics_DefaultsApplied(t *testing.T) {
	cat := &fakeCatalog{metrics: prototypes.FlowMetrics{FlowID: "f1"}}
	s := newSurface(t, cat, &fakeInvoker{})
	resp, err := s.Metrics(context.Background(), prototypes.FlowMetricsRequest{
		Identity: validScope(), FlowID: "f1",
	}, false)
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if resp.FlowID != "f1" {
		t.Fatalf("Metrics FlowID = %q, want f1", resp.FlowID)
	}
}
