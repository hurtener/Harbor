package protocol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentsprotocol "github.com/hurtener/Harbor/internal/runtime/registry/protocol"
)

// fakeProjector is an in-test Projector — it is a deterministic fixture
// for the Service unit tests, NOT a production default (it lives in a
// _test.go file per CLAUDE.md §13). The real production projector is
// RegistryProjector, exercised in registry_projector_test.go against a
// real Agent Registry.
type fakeProjector struct {
	agents   []prototypes.Agent
	getResp  prototypes.AgentGetResponse
	tools    []prototypes.AgentToolBinding
	mem      prototypes.AgentMemoryBinding
	gov      prototypes.AgentGovernance
	skills   []prototypes.AgentSkillBinding
	perms    prototypes.AgentPermissions
	metrics  prototypes.AgentMetrics
	notFound bool
	listErr  error
}

func (f *fakeProjector) ListAgents(_ context.Context, _ identity.Identity) ([]prototypes.Agent, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.agents, nil
}

func (f *fakeProjector) GetAgent(_ context.Context, _ identity.Identity, _ string) (prototypes.AgentGetResponse, error) {
	if f.notFound {
		return prototypes.AgentGetResponse{}, agentsprotocol.ErrAgentNotFound
	}
	return f.getResp, nil
}

func (f *fakeProjector) AgentTools(_ context.Context, _ identity.Identity, _ string) ([]prototypes.AgentToolBinding, error) {
	if f.notFound {
		return nil, agentsprotocol.ErrAgentNotFound
	}
	return f.tools, nil
}

func (f *fakeProjector) AgentMemory(_ context.Context, _ identity.Identity, _ string) (prototypes.AgentMemoryBinding, error) {
	if f.notFound {
		return prototypes.AgentMemoryBinding{}, agentsprotocol.ErrAgentNotFound
	}
	return f.mem, nil
}

func (f *fakeProjector) AgentGovernance(_ context.Context, _ identity.Identity, _ string) (prototypes.AgentGovernance, error) {
	if f.notFound {
		return prototypes.AgentGovernance{}, agentsprotocol.ErrAgentNotFound
	}
	return f.gov, nil
}

func (f *fakeProjector) AgentSkills(_ context.Context, _ identity.Identity, _ string) ([]prototypes.AgentSkillBinding, error) {
	if f.notFound {
		return nil, agentsprotocol.ErrAgentNotFound
	}
	return f.skills, nil
}

func (f *fakeProjector) AgentPermissions(_ context.Context, _ identity.Identity, _ string) (prototypes.AgentPermissions, error) {
	if f.notFound {
		return prototypes.AgentPermissions{}, agentsprotocol.ErrAgentNotFound
	}
	return f.perms, nil
}

func (f *fakeProjector) Metrics(_ context.Context, _ identity.Identity) (prototypes.AgentMetrics, error) {
	return f.metrics, nil
}

// validScope is a complete identity triple — the golden-path input.
var validScope = prototypes.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"}

func TestNewService_NilProjector_FailsLoud(t *testing.T) {
	_, err := agentsprotocol.NewService(nil)
	if !errors.Is(err, agentsprotocol.ErrMisconfigured) {
		t.Fatalf("NewService(nil) err = %v, want ErrMisconfigured", err)
	}
}

func TestService_List_IdentityMandatory(t *testing.T) {
	svc, err := agentsprotocol.NewService(&fakeProjector{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	cases := []struct {
		name  string
		scope prototypes.IdentityScope
	}{
		{"missing tenant", prototypes.IdentityScope{User: "u1", Session: "s1"}},
		{"missing user", prototypes.IdentityScope{Tenant: "t1", Session: "s1"}},
		{"missing session", prototypes.IdentityScope{Tenant: "t1", User: "u1"}},
		{"all missing", prototypes.IdentityScope{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.List(context.Background(), prototypes.AgentListRequest{Identity: tc.scope})
			if !errors.Is(err, agentsprotocol.ErrIdentityRequired) {
				t.Fatalf("List(%s) err = %v, want ErrIdentityRequired", tc.name, err)
			}
		})
	}
}

func TestService_List_PaginatesAndAggregates(t *testing.T) {
	fp := &fakeProjector{agents: []prototypes.Agent{
		{ID: "a3", Name: "gamma", Status: prototypes.AgentStatusActive},
		{ID: "a1", Name: "alpha", Status: prototypes.AgentStatusActive},
		{ID: "a2", Name: "beta", Status: prototypes.AgentStatusPaused},
	}}
	svc, _ := agentsprotocol.NewService(fp)
	resp, err := svc.List(context.Background(), prototypes.AgentListRequest{
		Identity: validScope, Page: 1, PageSize: 2,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if resp.TotalRows != 3 {
		t.Fatalf("TotalRows = %d, want 3", resp.TotalRows)
	}
	if resp.PageCount != 2 {
		t.Fatalf("PageCount = %d, want 2", resp.PageCount)
	}
	if len(resp.Agents) != 2 || resp.Agents[0].ID != "a1" || resp.Agents[1].ID != "a2" {
		t.Fatalf("page-1 rows = %+v, want sorted [a1,a2]", resp.Agents)
	}
	if resp.Aggregates.Total != 3 || resp.Aggregates.Active != 2 || resp.Aggregates.Paused != 1 {
		t.Fatalf("aggregates = %+v", resp.Aggregates)
	}
}

func TestService_List_FacetFilter(t *testing.T) {
	fp := &fakeProjector{agents: []prototypes.Agent{
		{ID: "a1", Name: "support bot", Status: prototypes.AgentStatusActive, PlannerType: "react"},
		{ID: "a2", Name: "batch worker", Status: prototypes.AgentStatusPaused, PlannerType: "deterministic"},
	}}
	svc, _ := agentsprotocol.NewService(fp)

	resp, err := svc.List(context.Background(), prototypes.AgentListRequest{
		Identity: validScope,
		Filter:   prototypes.AgentFilter{Status: []prototypes.AgentStatus{prototypes.AgentStatusActive}},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Agents) != 1 || resp.Agents[0].ID != "a1" {
		t.Fatalf("status facet = %+v, want [a1]", resp.Agents)
	}

	resp, err = svc.List(context.Background(), prototypes.AgentListRequest{
		Identity: validScope,
		Filter:   prototypes.AgentFilter{Search: "BATCH"},
	})
	if err != nil {
		t.Fatalf("List(search): %v", err)
	}
	if len(resp.Agents) != 1 || resp.Agents[0].ID != "a2" {
		t.Fatalf("search facet = %+v, want [a2]", resp.Agents)
	}
}

func TestService_List_PageSizeOutOfRange(t *testing.T) {
	svc, _ := agentsprotocol.NewService(&fakeProjector{})
	_, err := svc.List(context.Background(), prototypes.AgentListRequest{
		Identity: validScope, PageSize: prototypes.MaxAgentListPageSize + 1,
	})
	if !errors.Is(err, agentsprotocol.ErrInvalidRequest) {
		t.Fatalf("List(oversize) err = %v, want ErrInvalidRequest", err)
	}
}

func TestService_Get_NotFound(t *testing.T) {
	svc, _ := agentsprotocol.NewService(&fakeProjector{notFound: true})
	_, err := svc.Get(context.Background(), prototypes.AgentGetRequest{Identity: validScope, ID: "ghost"})
	if !errors.Is(err, agentsprotocol.ErrAgentNotFound) {
		t.Fatalf("Get(ghost) err = %v, want ErrAgentNotFound", err)
	}
}

func TestService_Get_EmptyID(t *testing.T) {
	svc, _ := agentsprotocol.NewService(&fakeProjector{})
	_, err := svc.Get(context.Background(), prototypes.AgentGetRequest{Identity: validScope, ID: "  "})
	if !errors.Is(err, agentsprotocol.ErrInvalidRequest) {
		t.Fatalf("Get('') err = %v, want ErrInvalidRequest", err)
	}
}

func TestService_Get_ProjectionShape(t *testing.T) {
	want := prototypes.AgentGetResponse{
		Agent:  prototypes.Agent{ID: "a1", Name: "support", VersionHash: "vh1", Incarnation: 2},
		Config: prototypes.AgentConfig{PlannerType: "react", Model: "gpt", MaxSteps: 12},
	}
	svc, _ := agentsprotocol.NewService(&fakeProjector{getResp: want})
	resp, err := svc.Get(context.Background(), prototypes.AgentGetRequest{Identity: validScope, ID: "a1"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.Agent.ID != "a1" || resp.Agent.VersionHash != "vh1" || resp.Config.MaxSteps != 12 {
		t.Fatalf("projection = %+v", resp)
	}
}

func TestService_Tools_IdentityAndShape(t *testing.T) {
	fp := &fakeProjector{tools: []prototypes.AgentToolBinding{
		{ToolID: "search", Transport: "MCP", AuthStatus: "oauth_agent_bound", BindingScope: "agent"},
	}}
	svc, _ := agentsprotocol.NewService(fp)

	if _, err := svc.Tools(context.Background(), prototypes.AgentToolsRequest{
		Identity: prototypes.IdentityScope{Tenant: "t1"}, ID: "a1",
	}); !errors.Is(err, agentsprotocol.ErrIdentityRequired) {
		t.Fatalf("Tools(no-ident) err = %v, want ErrIdentityRequired", err)
	}

	resp, err := svc.Tools(context.Background(), prototypes.AgentToolsRequest{Identity: validScope, ID: "a1"})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if resp.AgentID != "a1" || len(resp.Bindings) != 1 || resp.Bindings[0].BindingScope != "agent" {
		t.Fatalf("tools resp = %+v", resp)
	}
}

func TestService_Memory_Governance_Skills_Permissions(t *testing.T) {
	fp := &fakeProjector{
		mem:    prototypes.AgentMemoryBinding{StrategyID: "rolling_summary", Scope: "session", TTLSeconds: 3600},
		gov:    prototypes.AgentGovernance{Ceilings: []prototypes.AgentCostCeiling{{Tier: "default", LimitUSD: 10, SpendUSD: 2}}},
		skills: []prototypes.AgentSkillBinding{{SkillID: "sk1", Name: "summarise", Generated: true}},
		perms:  prototypes.AgentPermissions{Model: "implicit", Description: "all users"},
	}
	svc, _ := agentsprotocol.NewService(fp)

	mr, err := svc.Memory(context.Background(), prototypes.AgentMemoryRequest{Identity: validScope, ID: "a1"})
	if err != nil || mr.Binding.StrategyID != "rolling_summary" || mr.Binding.Scope != "session" {
		t.Fatalf("Memory = %+v err=%v", mr, err)
	}
	gr, err := svc.Governance(context.Background(), prototypes.AgentGovernanceRequest{Identity: validScope, ID: "a1"})
	if err != nil || len(gr.Governance.Ceilings) != 1 || gr.Governance.Ceilings[0].SpendUSD != 2 {
		t.Fatalf("Governance = %+v err=%v", gr, err)
	}
	sr, err := svc.Skills(context.Background(), prototypes.AgentSkillsRequest{Identity: validScope, ID: "a1"})
	if err != nil || len(sr.Skills) != 1 || !sr.Skills[0].Generated {
		t.Fatalf("Skills = %+v err=%v", sr, err)
	}
	pr, err := svc.Permissions(context.Background(), prototypes.AgentPermissionsRequest{Identity: validScope, ID: "a1"})
	if err != nil || pr.Permissions.Model != "implicit" {
		t.Fatalf("Permissions = %+v err=%v", pr, err)
	}
}

func TestService_Metrics_Rollup(t *testing.T) {
	fp := &fakeProjector{metrics: prototypes.AgentMetrics{
		ActiveAgents: 4, RunningTasks: 2, TotalCostUSD: 12.5, TotalTokens: 9000,
	}}
	svc, _ := agentsprotocol.NewService(fp)
	if _, err := svc.Metrics(context.Background(), prototypes.AgentMetricsRequest{}); !errors.Is(err, agentsprotocol.ErrIdentityRequired) {
		t.Fatalf("Metrics(no-ident) err = %v, want ErrIdentityRequired", err)
	}
	resp, err := svc.Metrics(context.Background(), prototypes.AgentMetricsRequest{Identity: validScope})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if resp.Metrics.ActiveAgents != 4 || resp.Metrics.TotalCostUSD != 12.5 {
		t.Fatalf("metrics = %+v", resp.Metrics)
	}
}

func TestService_List_ProjectorError_Propagates(t *testing.T) {
	svc, _ := agentsprotocol.NewService(&fakeProjector{listErr: errors.New("store down")})
	_, err := svc.List(context.Background(), prototypes.AgentListRequest{Identity: validScope})
	if err == nil || errors.Is(err, agentsprotocol.ErrIdentityRequired) {
		t.Fatalf("List propagation err = %v, want a wrapped non-identity error", err)
	}
}
