package protocol_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	artifactsinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/flow"
	flowprotocol "github.com/hurtener/Harbor/internal/runtime/flow/protocol"
	"github.com/hurtener/Harbor/internal/runtime/messages"
)

func catPassthrough(_ context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
	return in, nil
}

func catFixtureDef(name string) flow.Definition {
	return flow.Definition{
		Name:  name,
		Entry: "a",
		Exit:  "b",
		Nodes: map[flow.NodeID]flow.NodeSpec{
			"a": {Name: "a", Func: catPassthrough, To: []flow.NodeID{"b"}},
			"b": {Name: "b", Func: catPassthrough, Policy: engine.NodePolicy{MaxRetries: 2, TimeoutMS: 500}},
		},
		Budget:    flow.Budget{Deadline: time.Minute, HopBudget: 12, CostCap: 3.0},
		InSchema:  json.RawMessage(`{}`),
		OutSchema: json.RawMessage(`{}`),
	}
}

func newArtifactStore(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	st, err := artifactsinmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatalf("artifacts inmem New: %v", err)
	}
	return st
}

func newRegistryWithRuns(t *testing.T) *flow.Registry {
	t.Helper()
	r := flow.NewRegistry()
	if err := r.Register(catFixtureDef("flow-pay"), flow.Metadata{
		Owner: "team-payments", Version: "v2", PlannerFamily: "graph",
		Source: "internal/flows/pay.go",
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	now := time.Now()
	// t1 runs.
	for i, st := range []string{"succeeded", "failed", "succeeded"} {
		rec := flow.RunRecord{
			FlowName:  "flow-pay",
			RunID:     "t1-run-" + string(rune('a'+i)),
			Identity:  identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
			Trigger:   "user",
			Status:    st,
			StartedAt: now.Add(-time.Duration(i+1) * time.Hour),
			Duration:  time.Duration(100+i*50) * time.Millisecond,
			CostUSD:   0.25,
			NodeStates: []flow.NodeRunRecord{
				{NodeID: "a", Status: "succeeded", Duration: 40 * time.Millisecond},
				{NodeID: "b", Status: st, Duration: 60 * time.Millisecond},
			},
		}
		if err := r.RecordRun(rec); err != nil {
			t.Fatalf("RecordRun: %v", err)
		}
	}
	// t-other run (cross-tenant).
	if err := r.RecordRun(flow.RunRecord{
		FlowName:  "flow-pay",
		RunID:     "tother-run",
		Identity:  identity.Identity{TenantID: "t-other", UserID: "u9", SessionID: "s9"},
		Trigger:   "user",
		Status:    "succeeded",
		StartedAt: now.Add(-30 * time.Minute),
		Duration:  120 * time.Millisecond,
	}); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}
	return r
}

func TestRegistryCatalog_NilDependencyFailsLoud(t *testing.T) {
	if _, err := flowprotocol.NewRegistryCatalog(nil, newArtifactStore(t), 1024); !errors.Is(err, flowprotocol.ErrMisconfigured) {
		t.Fatalf("NewRegistryCatalog(nil registry): err = %v, want ErrMisconfigured", err)
	}
	if _, err := flowprotocol.NewRegistryCatalog(flow.NewRegistry(), nil, 1024); !errors.Is(err, flowprotocol.ErrMisconfigured) {
		t.Fatalf("NewRegistryCatalog(nil store): err = %v, want ErrMisconfigured", err)
	}
	if _, err := flowprotocol.NewRegistryCatalog(flow.NewRegistry(), newArtifactStore(t), 0); !errors.Is(err, flowprotocol.ErrMisconfigured) {
		t.Fatalf("NewRegistryCatalog(0 threshold): err = %v, want ErrMisconfigured", err)
	}
}

func TestRegistryCatalog_ListFlows_ProjectsGraphAndBudget(t *testing.T) {
	cat, err := flowprotocol.NewRegistryCatalog(newRegistryWithRuns(t), newArtifactStore(t), 1024)
	if err != nil {
		t.Fatalf("NewRegistryCatalog: %v", err)
	}
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	flows, err := cat.ListFlows(context.Background(), id, false)
	if err != nil {
		t.Fatalf("ListFlows: %v", err)
	}
	if len(flows) != 1 {
		t.Fatalf("ListFlows len = %d, want 1", len(flows))
	}
	f := flows[0]
	if f.ID != "flow-pay" || f.Owner != "team-payments" || f.Version != "v2" {
		t.Fatalf("ListFlows row metadata wrong: %+v", f)
	}
	if f.NodeCount != 2 || f.EdgeCount != 1 {
		t.Fatalf("ListFlows graph counts = (%d nodes, %d edges), want (2,1)", f.NodeCount, f.EdgeCount)
	}
	if f.Budget.RequestCap != 12 || f.Budget.CostCapUSD != 3.0 {
		t.Fatalf("ListFlows budget wrong: %+v", f.Budget)
	}
	// Non-admin caller sees only their own tenant's 3 runs.
	if f.Runs24h != 3 {
		t.Fatalf("ListFlows Runs24h = %d, want 3 (own tenant only)", f.Runs24h)
	}
}

func TestRegistryCatalog_ListRuns_TenantScoped(t *testing.T) {
	cat, err := flowprotocol.NewRegistryCatalog(newRegistryWithRuns(t), newArtifactStore(t), 1024)
	if err != nil {
		t.Fatalf("NewRegistryCatalog: %v", err)
	}
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	// Non-admin: own tenant only.
	runs, err := cat.ListRuns(context.Background(), id, false, "flow-pay", nil)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(runs) != 3 {
		t.Fatalf("ListRuns(non-admin) len = %d, want 3", len(runs))
	}
	for _, r := range runs {
		if r.Identity.Tenant != "t1" {
			t.Fatalf("ListRuns(non-admin) leaked tenant %q", r.Identity.Tenant)
		}
	}
	// Admin: fans across tenants.
	allRuns, err := cat.ListRuns(context.Background(), id, true, "flow-pay", nil)
	if err != nil {
		t.Fatalf("ListRuns(admin): %v", err)
	}
	if len(allRuns) != 4 {
		t.Fatalf("ListRuns(admin) len = %d, want 4", len(allRuns))
	}
}

func TestRegistryCatalog_ListRuns_UnknownFlowNotFound(t *testing.T) {
	cat, err := flowprotocol.NewRegistryCatalog(flow.NewRegistry(), newArtifactStore(t), 1024)
	if err != nil {
		t.Fatalf("NewRegistryCatalog: %v", err)
	}
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	_, err = cat.ListRuns(context.Background(), id, false, "ghost", nil)
	if !errors.Is(err, flowprotocol.ErrNotFound) {
		t.Fatalf("ListRuns(ghost): err = %v, want ErrNotFound", err)
	}
}

func TestRegistryCatalog_DescribeRun_HeavyOutputRoutedByRef(t *testing.T) {
	r := flow.NewRegistry()
	if err := r.Register(catFixtureDef("flow-heavy"), flow.Metadata{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	heavy := strings.Repeat("x", 5000) // exceeds the 1024-byte threshold
	if err := r.RecordRun(flow.RunRecord{
		FlowName:  "flow-heavy",
		RunID:     "heavy-run",
		Identity:  identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
		Status:    "succeeded",
		StartedAt: time.Now(),
		Output:    heavy,
	}); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}
	cat, err := flowprotocol.NewRegistryCatalog(r, newArtifactStore(t), 1024)
	if err != nil {
		t.Fatalf("NewRegistryCatalog: %v", err)
	}
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	desc, err := cat.DescribeRun(context.Background(), id, false, "heavy-run")
	if err != nil {
		t.Fatalf("DescribeRun: %v", err)
	}
	if desc.OutputPreview != "" {
		t.Fatal("DescribeRun: heavy output inlined — D-026 violation")
	}
	if desc.OutputRef == nil || desc.OutputRef.ID == "" {
		t.Fatalf("DescribeRun: heavy output not routed by-reference: %+v", desc.OutputRef)
	}
}

func TestRegistryCatalog_DescribeRun_LightOutputInline(t *testing.T) {
	r := flow.NewRegistry()
	if err := r.Register(catFixtureDef("flow-light"), flow.Metadata{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.RecordRun(flow.RunRecord{
		FlowName: "flow-light", RunID: "light-run",
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
		Status:   "succeeded", StartedAt: time.Now(),
		Output: "small result",
	}); err != nil {
		t.Fatalf("RecordRun: %v", err)
	}
	cat, err := flowprotocol.NewRegistryCatalog(r, newArtifactStore(t), 1024)
	if err != nil {
		t.Fatalf("NewRegistryCatalog: %v", err)
	}
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	desc, err := cat.DescribeRun(context.Background(), id, false, "light-run")
	if err != nil {
		t.Fatalf("DescribeRun: %v", err)
	}
	if desc.OutputPreview != "small result" {
		t.Fatalf("DescribeRun OutputPreview = %q, want inline", desc.OutputPreview)
	}
	if desc.OutputRef != nil {
		t.Fatal("DescribeRun: light output routed by-reference unnecessarily")
	}
}

func TestRegistryCatalog_DescribeRun_CrossTenantNonAdminNotFound(t *testing.T) {
	cat, err := flowprotocol.NewRegistryCatalog(newRegistryWithRuns(t), newArtifactStore(t), 1024)
	if err != nil {
		t.Fatalf("NewRegistryCatalog: %v", err)
	}
	// t1 caller asks for the t-other run — non-admin must see not-found
	// (no existence oracle leak).
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	_, err = cat.DescribeRun(context.Background(), id, false, "tother-run")
	if !errors.Is(err, flowprotocol.ErrNotFound) {
		t.Fatalf("DescribeRun(cross-tenant, non-admin): err = %v, want ErrNotFound", err)
	}
	// Admin can describe it.
	if _, err := cat.DescribeRun(context.Background(), id, true, "tother-run"); err != nil {
		t.Fatalf("DescribeRun(cross-tenant, admin): unexpected error: %v", err)
	}
}

func TestRegistryCatalog_FlowMetrics_Buckets(t *testing.T) {
	cat, err := flowprotocol.NewRegistryCatalog(newRegistryWithRuns(t), newArtifactStore(t), 1024)
	if err != nil {
		t.Fatalf("NewRegistryCatalog: %v", err)
	}
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	m, err := cat.FlowMetrics(context.Background(), id, false, "flow-pay", 24*time.Hour, time.Hour)
	if err != nil {
		t.Fatalf("FlowMetrics: %v", err)
	}
	if m.FlowID != "flow-pay" {
		t.Fatalf("FlowMetrics FlowID = %q", m.FlowID)
	}
	if len(m.Buckets) != 24 {
		t.Fatalf("FlowMetrics buckets = %d, want 24", len(m.Buckets))
	}
	var total int64
	for _, b := range m.Buckets {
		total += b.Runs
	}
	if total != 3 {
		t.Fatalf("FlowMetrics total bucketed runs = %d, want 3 (own tenant)", total)
	}
}

func TestRegistryCatalog_DescribeFlow_Source(t *testing.T) {
	cat, err := flowprotocol.NewRegistryCatalog(newRegistryWithRuns(t), newArtifactStore(t), 1024)
	if err != nil {
		t.Fatalf("NewRegistryCatalog: %v", err)
	}
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	desc, err := cat.DescribeFlow(context.Background(), id, false, "flow-pay")
	if err != nil {
		t.Fatalf("DescribeFlow: %v", err)
	}
	if desc.Source != "internal/flows/pay.go" {
		t.Fatalf("DescribeFlow Source = %q", desc.Source)
	}
	if len(desc.Nodes) != 2 || len(desc.Edges) != 1 {
		t.Fatalf("DescribeFlow graph = (%d nodes, %d edges)", len(desc.Nodes), len(desc.Edges))
	}
	// node "b" carries a non-default policy.
	var sawPolicy bool
	for _, n := range desc.Nodes {
		if n.ID == "b" && n.Policy != nil && n.Policy.MaxRetries == 2 {
			sawPolicy = true
		}
	}
	if !sawPolicy {
		t.Fatalf("DescribeFlow: node b policy not projected: %+v", desc.Nodes)
	}
}

// TestRegistryInvoker_RejectsUnknownFlow proves the FuncInvoker rejects
// an unregistered flow before reaching the launcher.
func TestRegistryInvoker_RejectsUnknownFlow(t *testing.T) {
	r := flow.NewRegistry()
	launched := false
	launch := func(_ context.Context, _ identity.Identity, _ string, _ map[string]any) (string, time.Time, error) {
		launched = true
		return "r1", time.Now(), nil
	}
	inv, err := flowprotocol.NewFuncInvoker(launch, r)
	if err != nil {
		t.Fatalf("NewFuncInvoker: %v", err)
	}
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	_, err = inv.Invoke(context.Background(), id, "ghost", nil)
	if !errors.Is(err, flowprotocol.ErrNotFound) {
		t.Fatalf("Invoke(ghost): err = %v, want ErrNotFound", err)
	}
	if launched {
		t.Fatal("Invoke: launcher called for an unregistered flow")
	}
}

func TestRegistryInvoker_LaunchesRegisteredFlow(t *testing.T) {
	r := flow.NewRegistry()
	if err := r.Register(catFixtureDef("flow-go"), flow.Metadata{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	started := time.Now()
	launch := func(_ context.Context, _ identity.Identity, flowID string, _ map[string]any) (string, time.Time, error) {
		return "run-" + flowID, started, nil
	}
	inv, err := flowprotocol.NewFuncInvoker(launch, r)
	if err != nil {
		t.Fatalf("NewFuncInvoker: %v", err)
	}
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	resp, err := inv.Invoke(context.Background(), id, "flow-go", map[string]any{"k": "v"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.RunID != "run-flow-go" || resp.Status != prototypes.FlowRunRunning {
		t.Fatalf("Invoke resp = %+v", resp)
	}
}

func TestRegistryCatalog_ListRuns_AdminTenantFilter(t *testing.T) {
	cat, err := flowprotocol.NewRegistryCatalog(newRegistryWithRuns(t), newArtifactStore(t), 1024)
	if err != nil {
		t.Fatalf("NewRegistryCatalog: %v", err)
	}
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	// Admin caller restricting the fan-in to t-other only.
	runs, err := cat.ListRuns(context.Background(), id, true, "flow-pay", []string{"t-other"})
	if err != nil {
		t.Fatalf("ListRuns(admin, t-other): %v", err)
	}
	if len(runs) != 1 || runs[0].Identity.Tenant != "t-other" {
		t.Fatalf("ListRuns(admin, t-other) = %+v, want 1 t-other run", runs)
	}
}

func TestRegistryCatalog_FlowMetrics_UnknownFlowNotFound(t *testing.T) {
	cat, err := flowprotocol.NewRegistryCatalog(flow.NewRegistry(), newArtifactStore(t), 1024)
	if err != nil {
		t.Fatalf("NewRegistryCatalog: %v", err)
	}
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	_, err = cat.FlowMetrics(context.Background(), id, false, "ghost", time.Hour, time.Minute)
	if !errors.Is(err, flowprotocol.ErrNotFound) {
		t.Fatalf("FlowMetrics(ghost): err = %v, want ErrNotFound", err)
	}
}

func TestRegistryCatalog_DescribeFlow_UnknownFlowNotFound(t *testing.T) {
	cat, err := flowprotocol.NewRegistryCatalog(flow.NewRegistry(), newArtifactStore(t), 1024)
	if err != nil {
		t.Fatalf("NewRegistryCatalog: %v", err)
	}
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	_, err = cat.DescribeFlow(context.Background(), id, false, "ghost")
	if !errors.Is(err, flowprotocol.ErrNotFound) {
		t.Fatalf("DescribeFlow(ghost): err = %v, want ErrNotFound", err)
	}
}

func TestRegistryCatalog_DescribeRun_UnknownRunNotFound(t *testing.T) {
	cat, err := flowprotocol.NewRegistryCatalog(newRegistryWithRuns(t), newArtifactStore(t), 1024)
	if err != nil {
		t.Fatalf("NewRegistryCatalog: %v", err)
	}
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	_, err = cat.DescribeRun(context.Background(), id, false, "ghost-run")
	if !errors.Is(err, flowprotocol.ErrNotFound) {
		t.Fatalf("DescribeRun(ghost): err = %v, want ErrNotFound", err)
	}
}

func TestRegistryInvoker_PropagatesLauncherError(t *testing.T) {
	r := flow.NewRegistry()
	if err := r.Register(catFixtureDef("flow-err"), flow.Metadata{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	launch := func(context.Context, identity.Identity, string, map[string]any) (string, time.Time, error) {
		return "", time.Time{}, errors.New("spawn failed")
	}
	inv, err := flowprotocol.NewFuncInvoker(launch, r)
	if err != nil {
		t.Fatalf("NewFuncInvoker: %v", err)
	}
	id := identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}
	_, err = inv.Invoke(context.Background(), id, "flow-err", nil)
	if err == nil {
		t.Fatal("Invoke: expected launcher error, got nil")
	}
}

func TestNewFuncInvoker_NilDependencyFailsLoud(t *testing.T) {
	if _, err := flowprotocol.NewFuncInvoker(nil, flow.NewRegistry()); !errors.Is(err, flowprotocol.ErrMisconfigured) {
		t.Fatalf("NewFuncInvoker(nil launch): err = %v, want ErrMisconfigured", err)
	}
	launch := func(context.Context, identity.Identity, string, map[string]any) (string, time.Time, error) {
		return "", time.Time{}, nil
	}
	if _, err := flowprotocol.NewFuncInvoker(launch, nil); !errors.Is(err, flowprotocol.ErrMisconfigured) {
		t.Fatalf("NewFuncInvoker(nil registry): err = %v, want ErrMisconfigured", err)
	}
}
