package protocol_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tools"
	toolsprotocol "github.com/hurtener/Harbor/internal/tools/protocol"
)

// validID is a complete identity triple used by every happy-path test.
func validID() prototypes.IdentityScope {
	return prototypes.IdentityScope{Tenant: "t1", User: "u1", Session: "s1"}
}

// newTestCatalog builds an in-memory catalog seeded with three tools
// spanning the transport / side-effect / scope axes the facet filters
// branch on.
func newTestCatalog(t *testing.T) tools.ToolCatalog {
	t.Helper()
	cat := tools.NewCatalog()
	register := func(name string, transport tools.TransportKind, se tools.SideEffect, scopes []string) {
		if err := cat.Register(tools.ToolDescriptor{
			Tool: tools.Tool{
				Name:        name,
				Description: name + " description",
				Transport:   transport,
				SideEffects: se,
				AuthScopes:  scopes,
				Loading:     tools.LoadingAlways,
			},
			Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
				return tools.ToolResult{}, nil
			},
		}); err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
	}
	register("alpha_search", tools.TransportInProcess, tools.SideEffectPure, nil)
	register("beta_http", tools.TransportHTTP, tools.SideEffectWrite, nil)
	// gamma_mcp is an MCP tool — scopeOf derives the "agent" scope facet
	// from the MCP transport. Tools registered for the Console operator
	// lens carry no planner-step AuthScopes: the AuthScope gate is a
	// runtime planner-visibility concern, while the Console catalog is
	// the operator's full-discovery lens (page-tools.md §9).
	register("gamma_mcp", tools.TransportMCP, tools.SideEffectExternal, nil)
	return cat
}

func newService(t *testing.T) *toolsprotocol.Service {
	t.Helper()
	proj, err := toolsprotocol.NewCatalogProjector(newTestCatalog(t))
	if err != nil {
		t.Fatalf("NewCatalogProjector: %v", err)
	}
	svc, err := toolsprotocol.NewService(proj)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestNewService_NilProjector_FailsLoudly(t *testing.T) {
	if _, err := toolsprotocol.NewService(nil); !errors.Is(err, toolsprotocol.ErrMisconfigured) {
		t.Fatalf("NewService(nil) error = %v, want ErrMisconfigured", err)
	}
}

func TestNewCatalogProjector_NilCatalog_FailsLoudly(t *testing.T) {
	if _, err := toolsprotocol.NewCatalogProjector(nil); !errors.Is(err, toolsprotocol.ErrMisconfigured) {
		t.Fatalf("NewCatalogProjector(nil) error = %v, want ErrMisconfigured", err)
	}
}

func TestList_HappyPath_ReturnsCatalogAndAggregates(t *testing.T) {
	svc := newService(t)
	resp, err := svc.List(context.Background(), prototypes.ToolListRequest{Identity: validID()})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Tools) != 3 {
		t.Fatalf("List returned %d tools, want 3", len(resp.Tools))
	}
	if resp.Aggregates.Total != 3 {
		t.Errorf("Aggregates.Total = %d, want 3", resp.Aggregates.Total)
	}
	if resp.Page != 1 || resp.PageSize != prototypes.DefaultToolListPageSize {
		t.Errorf("Page/PageSize = %d/%d, want 1/%d", resp.Page, resp.PageSize, prototypes.DefaultToolListPageSize)
	}
	// Sorted by Name.
	if resp.Tools[0].Name != "alpha_search" || resp.Tools[2].Name != "gamma_mcp" {
		t.Errorf("List not sorted by name: %v", []string{resp.Tools[0].Name, resp.Tools[2].Name})
	}
}

func TestList_MissingIdentity_FailsLoudly(t *testing.T) {
	svc := newService(t)
	_, err := svc.List(context.Background(), prototypes.ToolListRequest{
		Identity: prototypes.IdentityScope{Tenant: "t1"}, // missing user + session
	})
	if !errors.Is(err, toolsprotocol.ErrIdentityRequired) {
		t.Fatalf("List with incomplete identity error = %v, want ErrIdentityRequired", err)
	}
}

func TestList_TransportFacet_FiltersRows(t *testing.T) {
	svc := newService(t)
	resp, err := svc.List(context.Background(), prototypes.ToolListRequest{
		Identity: validID(),
		Filter:   prototypes.ToolFilter{Transports: []prototypes.ToolTransport{prototypes.ToolTransportMCP}},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Tools) != 1 || resp.Tools[0].Name != "gamma_mcp" {
		t.Fatalf("MCP facet returned %d rows, want 1 (gamma_mcp)", len(resp.Tools))
	}
}

func TestList_ScopeFacet_FiltersRows(t *testing.T) {
	svc := newService(t)
	resp, err := svc.List(context.Background(), prototypes.ToolListRequest{
		Identity: validID(),
		Filter:   prototypes.ToolFilter{Scopes: []string{"agent"}},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// gamma_mcp declares AuthScopes → scope "agent".
	if len(resp.Tools) != 1 || resp.Tools[0].Name != "gamma_mcp" {
		t.Fatalf("agent-scope facet returned %d rows, want 1", len(resp.Tools))
	}
}

func TestList_SearchFacet_FiltersRows(t *testing.T) {
	svc := newService(t)
	resp, err := svc.List(context.Background(), prototypes.ToolListRequest{
		Identity: validID(),
		Filter:   prototypes.ToolFilter{Search: "BETA"},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Tools) != 1 || resp.Tools[0].Name != "beta_http" {
		t.Fatalf("search facet returned %d rows, want 1 (beta_http)", len(resp.Tools))
	}
}

func TestList_AllFacets_Combined(t *testing.T) {
	svc := newService(t)
	resp, err := svc.List(context.Background(), prototypes.ToolListRequest{
		Identity: validID(),
		Filter: prototypes.ToolFilter{
			Transports:       []prototypes.ToolTransport{prototypes.ToolTransportMCP},
			Scopes:           []string{"agent"},
			ReliabilityTiers: []string{"external"},
			Search:           "gamma",
		},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Tools) != 1 {
		t.Fatalf("all-facets combination returned %d rows, want 1", len(resp.Tools))
	}
}

func TestList_UnknownFacet_FailsLoudly(t *testing.T) {
	svc := newService(t)
	_, err := svc.List(context.Background(), prototypes.ToolListRequest{
		Identity: validID(),
		Filter:   prototypes.ToolFilter{Transports: []prototypes.ToolTransport{"carrier-pigeon"}},
	})
	if !errors.Is(err, toolsprotocol.ErrInvalidRequest) {
		t.Fatalf("List with unknown facet error = %v, want ErrInvalidRequest", err)
	}
}

func TestList_OversizedPageSize_FailsLoudly(t *testing.T) {
	svc := newService(t)
	_, err := svc.List(context.Background(), prototypes.ToolListRequest{
		Identity: validID(),
		PageSize: prototypes.MaxToolListPageSize + 1,
	})
	if !errors.Is(err, toolsprotocol.ErrInvalidRequest) {
		t.Fatalf("List with oversized page_size error = %v, want ErrInvalidRequest", err)
	}
}

func TestList_Pagination_SecondPage(t *testing.T) {
	svc := newService(t)
	resp, err := svc.List(context.Background(), prototypes.ToolListRequest{
		Identity: validID(),
		Page:     2,
		PageSize: 2,
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if resp.PageCount != 2 || resp.TotalRows != 3 {
		t.Errorf("PageCount/TotalRows = %d/%d, want 2/3", resp.PageCount, resp.TotalRows)
	}
	if len(resp.Tools) != 1 {
		t.Fatalf("page 2 (size 2) returned %d rows, want 1", len(resp.Tools))
	}
}

func TestGet_HappyPath(t *testing.T) {
	svc := newService(t)
	tool, err := svc.Get(context.Background(), prototypes.ToolGetRequest{Identity: validID(), ID: "beta_http"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if tool.Transport != prototypes.ToolTransportHTTP {
		t.Errorf("Get transport = %q, want HTTP", tool.Transport)
	}
}

func TestGet_UnknownTool_NotFound(t *testing.T) {
	svc := newService(t)
	_, err := svc.Get(context.Background(), prototypes.ToolGetRequest{Identity: validID(), ID: "ghost"})
	if !errors.Is(err, toolsprotocol.ErrToolNotFound) {
		t.Fatalf("Get unknown tool error = %v, want ErrToolNotFound", err)
	}
}

func TestGet_EmptyID_FailsLoudly(t *testing.T) {
	svc := newService(t)
	_, err := svc.Get(context.Background(), prototypes.ToolGetRequest{Identity: validID(), ID: "  "})
	if !errors.Is(err, toolsprotocol.ErrInvalidRequest) {
		t.Fatalf("Get empty ID error = %v, want ErrInvalidRequest", err)
	}
}

func TestDescribe_HappyPath_ManifestShape(t *testing.T) {
	svc := newService(t)
	m, err := svc.Describe(context.Background(), prototypes.ToolDescribeRequest{Identity: validID(), ID: "gamma_mcp"})
	if err != nil {
		t.Fatalf("Describe: %v", err)
	}
	if m.Tool.ID != "gamma_mcp" {
		t.Errorf("manifest tool ID = %q, want gamma_mcp", m.Tool.ID)
	}
	if m.SideEffect != string(tools.SideEffectExternal) {
		t.Errorf("manifest SideEffect = %q, want external", m.SideEffect)
	}
	if m.Tool.Scope != "agent" {
		t.Errorf("manifest tool Scope = %q, want agent (MCP transport)", m.Tool.Scope)
	}
	if m.LoadingMode != string(tools.LoadingAlways) {
		t.Errorf("manifest LoadingMode = %q, want always", m.LoadingMode)
	}
}

func TestMetrics_DefaultWindow_HealthyStatus(t *testing.T) {
	svc := newService(t)
	m, err := svc.Metrics(context.Background(), prototypes.ToolMetricsRequest{Identity: validID(), ID: "alpha_search"})
	if err != nil {
		t.Fatalf("Metrics: %v", err)
	}
	if m.Window != prototypes.ToolWindow1h {
		t.Errorf("default window = %q, want 1h", m.Window)
	}
	if m.Status != prototypes.ToolStatusHealthy {
		t.Errorf("status = %q, want Healthy", m.Status)
	}
}

func TestMetrics_UnknownWindow_FailsLoudly(t *testing.T) {
	svc := newService(t)
	_, err := svc.Metrics(context.Background(), prototypes.ToolMetricsRequest{
		Identity: validID(), ID: "alpha_search", Window: "1y",
	})
	if !errors.Is(err, toolsprotocol.ErrInvalidRequest) {
		t.Fatalf("Metrics unknown window error = %v, want ErrInvalidRequest", err)
	}
}

func TestContentStats_HappyPath(t *testing.T) {
	svc := newService(t)
	cs, err := svc.ContentStats(context.Background(), prototypes.ToolContentStatsRequest{
		Identity: validID(), ID: "alpha_search",
	})
	if err != nil {
		t.Fatalf("ContentStats: %v", err)
	}
	if cs.ID != "alpha_search" {
		t.Errorf("content stats ID = %q, want alpha_search", cs.ID)
	}
}

func TestSetApprovalPolicy_WithoutAdminScope_FailsLoudly(t *testing.T) {
	svc := newService(t)
	_, err := svc.SetApprovalPolicy(context.Background(), prototypes.ToolSetApprovalPolicyRequest{
		Identity: validID(), ID: "alpha_search", Policy: prototypes.ToolApprovalGated,
	}, false)
	if !errors.Is(err, toolsprotocol.ErrAdminScopeRequired) {
		t.Fatalf("SetApprovalPolicy without admin error = %v, want ErrAdminScopeRequired", err)
	}
}

func TestSetApprovalPolicy_WithAdminScope_UpdatesAndIsObservable(t *testing.T) {
	svc := newService(t)
	resp, err := svc.SetApprovalPolicy(context.Background(), prototypes.ToolSetApprovalPolicyRequest{
		Identity: validID(), ID: "alpha_search", Policy: prototypes.ToolApprovalGated,
	}, true)
	if err != nil {
		t.Fatalf("SetApprovalPolicy: %v", err)
	}
	if resp.Policy != prototypes.ToolApprovalGated {
		t.Errorf("response policy = %q, want gated", resp.Policy)
	}
	// The override is observable on the next Get — never a silent no-op.
	tool, err := svc.Get(context.Background(), prototypes.ToolGetRequest{Identity: validID(), ID: "alpha_search"})
	if err != nil {
		t.Fatalf("Get after policy update: %v", err)
	}
	if tool.ApprovalPolicy != prototypes.ToolApprovalGated {
		t.Errorf("post-update ApprovalPolicy = %q, want gated", tool.ApprovalPolicy)
	}
}

func TestSetApprovalPolicy_InvalidPolicy_FailsLoudly(t *testing.T) {
	svc := newService(t)
	_, err := svc.SetApprovalPolicy(context.Background(), prototypes.ToolSetApprovalPolicyRequest{
		Identity: validID(), ID: "alpha_search", Policy: "maybe",
	}, true)
	if !errors.Is(err, toolsprotocol.ErrInvalidRequest) {
		t.Fatalf("SetApprovalPolicy invalid policy error = %v, want ErrInvalidRequest", err)
	}
}

func TestRevokeOAuth_WithoutAdminScope_FailsLoudly(t *testing.T) {
	svc := newService(t)
	_, err := svc.RevokeOAuth(context.Background(), prototypes.ToolRevokeOAuthRequest{
		Identity: validID(), ID: "alpha_search",
	}, false)
	if !errors.Is(err, toolsprotocol.ErrAdminScopeRequired) {
		t.Fatalf("RevokeOAuth without admin error = %v, want ErrAdminScopeRequired", err)
	}
}

func TestRevokeOAuth_WithAdminScope_NoAnnotator_FailsLoudly(t *testing.T) {
	svc := newService(t)
	// The default CatalogProjector has no OAuth-aware Annotator, so a
	// revoke fails loud with ErrAdminUnsupported rather than silently
	// reporting a success.
	_, err := svc.RevokeOAuth(context.Background(), prototypes.ToolRevokeOAuthRequest{
		Identity: validID(), ID: "alpha_search",
	}, true)
	if !errors.Is(err, toolsprotocol.ErrAdminUnsupported) {
		t.Fatalf("RevokeOAuth without annotator error = %v, want ErrAdminUnsupported", err)
	}
}

// fakeAnnotator is a deterministic Annotator used to exercise the
// annotated-projection paths + the OAuth-revoke admin backend.
type fakeAnnotator struct {
	revoked int64
}

func (f *fakeAnnotator) OAuthStatus(context.Context, identity.Identity, string) prototypes.ToolOAuthStatus {
	return prototypes.ToolOAuthRequired
}
func (f *fakeAnnotator) ApprovalPolicy(context.Context, identity.Identity, string) prototypes.ToolApprovalPolicy {
	return prototypes.ToolApprovalAuto
}
func (f *fakeAnnotator) LastUsedAt(context.Context, identity.Identity, string) time.Time {
	return time.Now()
}
func (f *fakeAnnotator) Metrics(_ context.Context, _ identity.Identity, _ string, w prototypes.ToolMetricsWindow) prototypes.ToolMetrics {
	return prototypes.ToolMetrics{Window: w, ErrorRate1h: 0.5, Status: prototypes.ToolStatusDegraded}
}
func (f *fakeAnnotator) ContentStats(context.Context, identity.Identity, string) prototypes.ToolContentStats {
	return prototypes.ToolContentStats{
		Histogram:  []prototypes.ToolContentBucket{{MaxBytes: 1024, Count: 7}},
		HeavyCount: 2,
	}
}
func (f *fakeAnnotator) DisplayModes(context.Context, identity.Identity, string) map[string]string {
	return map[string]string{"image/png": "inline"}
}
func (f *fakeAnnotator) RevokeOAuth(context.Context, identity.Identity, string) (int64, error) {
	f.revoked++
	return 3, nil
}

func TestRevokeOAuth_WithOAuthAnnotator_Succeeds(t *testing.T) {
	proj, err := toolsprotocol.NewCatalogProjector(newTestCatalog(t),
		toolsprotocol.WithAnnotator(&fakeAnnotator{}))
	if err != nil {
		t.Fatalf("NewCatalogProjector: %v", err)
	}
	svc, err := toolsprotocol.NewService(proj)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	resp, err := svc.RevokeOAuth(context.Background(), prototypes.ToolRevokeOAuthRequest{
		Identity: validID(), ID: "beta_http",
	}, true)
	if err != nil {
		t.Fatalf("RevokeOAuth: %v", err)
	}
	if resp.RevokedCount != 3 {
		t.Errorf("RevokedCount = %d, want 3", resp.RevokedCount)
	}
}

func TestList_WithAnnotator_AggregatesReflectAnnotations(t *testing.T) {
	proj, err := toolsprotocol.NewCatalogProjector(newTestCatalog(t),
		toolsprotocol.WithAnnotator(&fakeAnnotator{}))
	if err != nil {
		t.Fatalf("NewCatalogProjector: %v", err)
	}
	svc, err := toolsprotocol.NewService(proj)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	resp, err := svc.List(context.Background(), prototypes.ToolListRequest{Identity: validID()})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// fakeAnnotator marks every tool OAuthRequired + non-zero LastUsedAt.
	if resp.Aggregates.AwaitingOAuth != 3 {
		t.Errorf("AwaitingOAuth = %d, want 3", resp.Aggregates.AwaitingOAuth)
	}
	if resp.Aggregates.Active != 3 {
		t.Errorf("Active = %d, want 3", resp.Aggregates.Active)
	}
}
