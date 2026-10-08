package protocol_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole/admission"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tools/appoperation"
)

func TestAppsSurface_AppOperationRequiresCurrentAdmission(t *testing.T) {
	authority := newFakeAdmissionAuthority(t)
	inv := &stubInvoker{}
	gate := &fixedGenerationGate{gen: "gen-1"}
	s, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource: &stubResourceReader{}, Invoker: inv, ToolContext: &stubToolContextReader{},
		AgentResolver: &stubAppsAgentResolver{}, AgentReach: allowAppsAgentReach{},
		RenderAdmissionAuthority: authority, RenderAdmissionGate: gate,
	})
	if err != nil {
		t.Fatal(err)
	}
	token, err := authority.auth.Mint(context.Background(), admission.RenderTuple{
		Identity: identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"},
		AgentID:  appsDefaultAgentID, ServerID: "srv", ResourceURI: "ui://app", DescriptorFingerprint: "gen-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	req := types.MCPAppCallToolRequest{Identity: appsID(), ServerID: "srv", Tool: "srv_save", ResourceURI: "ui://app",
		RenderAdmission: token.Value, AppOperation: strings.Repeat("a", 64), Arguments: json.RawMessage(`{"value":9007199254740993}`)}
	if _, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPAppsCallTool, &req); err != nil {
		t.Fatal(err)
	}
	b, ok := appoperation.From(inv.admittedCtx)
	if !ok || b.Tool != "srv_save" || b.Generation != "gen-1" || b.AgentID != appsDefaultAgentID {
		t.Fatal("verified coordinates not carried")
	}
	if inv.admittedCalls != 1 || inv.callCtx != nil || inv.bindingCtx != nil {
		t.Fatal("wrong invocation path")
	}
	for _, change := range []func(*types.MCPAppCallToolRequest){
		func(r *types.MCPAppCallToolRequest) { r.RenderAdmission = "" },
		func(r *types.MCPAppCallToolRequest) { r.Binding = "legacy" },
		func(r *types.MCPAppCallToolRequest) { r.RenderAdmission = "tampered" },
		func(r *types.MCPAppCallToolRequest) { r.ResourceURI = "ui://other" },
		func(r *types.MCPAppCallToolRequest) { r.ServerID = "other" },
		func(r *types.MCPAppCallToolRequest) { r.AgentID = "other" },
		func(r *types.MCPAppCallToolRequest) { r.Identity.User = "other" },
		func(r *types.MCPAppCallToolRequest) { r.Arguments = json.RawMessage(`{"a":1,"a":2}`) },
		func(r *types.MCPAppCallToolRequest) { r.AppOperation = "invalid" },
	} {
		bad := req
		change(&bad)
		if _, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPAppsCallTool, &bad); err == nil {
			t.Error("invalid operation admitted")
		}
	}
	gate.gen = "gen-2"
	if _, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPAppsCallTool, &req); err == nil {
		t.Error("stale generation admitted")
	}
	if inv.admittedCalls != 1 {
		t.Fatal("refused request invoked callback")
	}
}

func TestAppsSurface_AppOperationReadReturnsSealedGeneration(t *testing.T) {
	for _, tc := range []struct {
		name, selector, generation, want string
		optIn                            bool
	}{
		{"bound", strings.Repeat("a", 64), "catalog-exact", "catalog-exact", true},
		{"ordinary", "", "catalog-exact", "", true},
		{"unavailable", strings.Repeat("a", 64), "", "", true},
		{"no_admission", strings.Repeat("a", 64), "catalog-exact", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			authority := newFakeAdmissionAuthority(t)
			s, err := protocol.NewAppsSurface(protocol.AppsDeps{
				Resource: &stubResourceReader{}, Invoker: &stubInvoker{}, ToolContext: &stubToolContextReader{},
				AgentResolver: &stubAppsAgentResolver{}, AgentReach: allowAppsAgentReach{},
				RenderAdmissionAuthority: authority, RenderAdmissionGate: &fixedGenerationGate{gen: tc.generation},
			})
			if err != nil {
				t.Fatal(err)
			}
			raw, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
				Identity: appsID(), ServerID: "srv", ResourceURI: "ui://app", AppOperation: tc.selector, RequestRenderAdmission: tc.optIn,
			})
			if err != nil {
				t.Fatal(err)
			}
			got := raw.(*types.ReadMCPResourceResponse)
			if got.AppOperationGeneration != tc.want {
				t.Fatalf("generation=%q, want%q", got.AppOperationGeneration, tc.want)
			}
			if tc.want != "" {
				if got.RenderAdmission == nil {
					t.Fatal("missing admission")
				}
				_, err = authority.auth.Verify(context.Background(), admission.RenderTuple{
					Identity: identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"},
					AgentID:  appsDefaultAgentID, ServerID: "srv", ResourceURI: "ui://app", DescriptorFingerprint: got.AppOperationGeneration,
				}, got.RenderAdmission.Token)
				if err != nil {
					t.Fatalf("returned generation differs from sealed generation: %v", err)
				}
			}
		})
	}
}
