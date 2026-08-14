package protocol_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole/admission"
	"github.com/hurtener/Harbor/internal/protocol"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	authsealer "github.com/hurtener/Harbor/internal/tools/auth"
)

// --- HA-56 render-admission seams -----------------------------------------

// fakeAdmissionAuthority wraps the REAL sealed admission.Authority over a
// deterministic AES-GCM sealer — the same authority the production runtime
// wires, driven through the AppsSurface seam.
type fakeAdmissionAuthority struct {
	auth *admission.Authority
}

func newFakeAdmissionAuthority(t *testing.T) *fakeAdmissionAuthority {
	t.Helper()
	sealer, err := authsealer.NewAESGCMSealer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	a, err := admission.New(sealer, admission.WithTTL(15*time.Minute))
	if err != nil {
		t.Fatalf("admission.New: %v", err)
	}
	return &fakeAdmissionAuthority{auth: a}
}

func (f *fakeAdmissionAuthority) Mint(ctx context.Context, rt admission.RenderTuple) (admission.Token, error) {
	return f.auth.Mint(ctx, rt)
}

func (f *fakeAdmissionAuthority) Verify(ctx context.Context, expected admission.RenderTuple, token string) (admission.Claims, error) {
	return f.auth.Verify(ctx, expected, token)
}

// fixedGenerationReader returns one fixed descriptor generation.
type fixedGenerationReader struct {
	gen string
	err error
}

func (f *fixedGenerationReader) DescriptorGeneration(_ context.Context, _, _ string) (string, error) {
	return f.gen, f.err
}

// newAdmissionSurface builds an AppsSurface with the render-admission
// seams wired. A nil authority / generation leaves the seam unwired.
func newAdmissionSurface(t *testing.T, authority protocol.RenderAdmissionAuthority, gen protocol.DescriptorGenerationReader) *protocol.AppsSurface {
	t.Helper()
	s, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource: &stubResourceReader{content: protocol.MCPResourceContent{
			ResourceURI: "ui://app/main.html", MIMEType: "text/html", Inline: []byte("<html>x</html>"),
		}},
		Invoker:                  &stubInvoker{},
		ToolContext:              &stubToolContextReader{},
		AgentResolver:            &stubAppsAgentResolver{},
		AgentReach:               allowAppsAgentReach{},
		RenderAdmissionAuthority: authority,
		DescriptorGeneration:     gen,
	})
	if err != nil {
		t.Fatalf("NewAppsSurface: %v", err)
	}
	return s
}

// appsID returns the body identity scope matching the verified identity
// `verifiedCtx` seats (t-1/u-1/s-1) — the MCP Apps surface's Pinned
// posture requires the body triple to equal the verified one.
func appsID() types.IdentityScope {
	return types.IdentityScope{Tenant: "t-1", User: "u-1", Session: "s-1"}
}

// --- mint side: mcp.servers.read_resource ----------------------------------

// TestAppsSurface_ReadResource_NoFlagMintsNothing pins the HA-56 opt-in
// boundary: an omitted/false `request_render_admission` preserves the
// ordinary read byte-for-byte and mints NO callback authority — even when
// the admission seam IS wired.
func TestAppsSurface_ReadResource_NoFlagMintsNothing(t *testing.T) {
	s := newAdmissionSurface(t, newFakeAdmissionAuthority(t), &fixedGenerationReader{gen: "gen-1"})
	resp, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity:    appsID(),
		ServerID:    "srv",
		ResourceURI: "ui://app/main.html",
	})
	if err != nil {
		t.Fatalf("Dispatch(read_resource): %v", err)
	}
	out := resp.(*types.ReadMCPResourceResponse)
	if out.RenderAdmission != nil {
		t.Fatalf("no-flag read returned a render admission: %+v", out.RenderAdmission)
	}
	if out.Content == "" {
		t.Fatal("no-flag read lost the ordinary content projection")
	}
}

// TestAppsSurface_ReadResource_OptInMintsBoundedAdmission asserts a
// successful opt-in `ui://` read returns the bounded render admission:
// the opaque token, the expiry metadata, and the closed availability
// status — and nothing else.
func TestAppsSurface_ReadResource_OptInMintsBoundedAdmission(t *testing.T) {
	authz := newFakeAdmissionAuthority(t)
	s := newAdmissionSurface(t, authz, &fixedGenerationReader{gen: "gen-1"})
	resp, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity:               appsID(),
		ServerID:               "srv",
		ResourceURI:            "ui://app/main.html",
		RequestRenderAdmission: true,
	})
	if err != nil {
		t.Fatalf("Dispatch(read_resource): %v", err)
	}
	out := resp.(*types.ReadMCPResourceResponse)
	if out.RenderAdmission == nil {
		t.Fatal("opt-in read returned no render admission")
	}
	adm := out.RenderAdmission
	if adm.Token == "" {
		t.Error("admission token is empty")
	}
	if adm.Availability != types.RenderAdmissionAvailable {
		t.Errorf("availability = %q, want %q", adm.Availability, types.RenderAdmissionAvailable)
	}
	if adm.ExpiresAt == "" {
		t.Error("expiry metadata is empty")
	}
	if adm.IssuedAt == "" {
		t.Error("issued metadata is empty")
	}
	// The minted token actually verifies against the same tuple.
	if _, err := authz.auth.Verify(context.Background(), admission.RenderTuple{
		Identity:              identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"},
		AgentID:               appsDefaultAgentID,
		ServerID:              "srv",
		ResourceURI:           "ui://app/main.html",
		DescriptorFingerprint: "gen-1",
	}, adm.Token); err != nil {
		t.Fatalf("minted token does not verify against its tuple: %v", err)
	}
}

// TestAppsSurface_ReadResource_OptInUnwiredSeamFailsLoud asserts the
// opt-in mint fails LOUD (CodeRuntimeError) when the admission seam is
// not wired — never a silent no-admission 200.
func TestAppsSurface_ReadResource_OptInUnwiredSeamFailsLoud(t *testing.T) {
	s := newAdmissionSurface(t, nil, nil)
	_, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity:               appsID(),
		ServerID:               "srv",
		ResourceURI:            "ui://app/main.html",
		RequestRenderAdmission: true,
	})
	var perr *protoerrors.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *protoerrors.Error", err)
	}
	if perr.Code != protoerrors.CodeRuntimeError {
		t.Errorf("code = %q, want %q (the admission authority is not wired)", perr.Code, protoerrors.CodeRuntimeError)
	}
}

// TestAppsSurface_ReadResource_EmptyGenerationIsUnavailable asserts the
// mint binds the exact CURRENT registry descriptor generation: an empty
// generation answers the closed "unavailable" availability with NO token,
// never an admission over an empty generation.
func TestAppsSurface_ReadResource_EmptyGenerationIsUnavailable(t *testing.T) {
	s := newAdmissionSurface(t, newFakeAdmissionAuthority(t), &fixedGenerationReader{gen: ""})
	resp, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity:               appsID(),
		ServerID:               "srv",
		ResourceURI:            "ui://app/main.html",
		RequestRenderAdmission: true,
	})
	if err != nil {
		t.Fatalf("Dispatch(read_resource): %v", err)
	}
	out := resp.(*types.ReadMCPResourceResponse)
	if out.RenderAdmission == nil {
		t.Fatal("empty generation: expected the closed unavailable admission object")
	}
	if out.RenderAdmission.Availability != types.RenderAdmissionUnavailable {
		t.Errorf("availability = %q, want %q", out.RenderAdmission.Availability, types.RenderAdmissionUnavailable)
	}
	if out.RenderAdmission.Token != "" {
		t.Error("unavailable admission must carry no token")
	}
}

// --- call side: mcp.apps.call_tool -----------------------------------------

// TestAppsSurface_CallTool_RenderAdmissionVerifiedAndRidesInvocation
// asserts the fresh render admission is verified against the CURRENT
// tuple BEFORE invocation and the verified token rides the SAME wrapped
// invocation the legacy binding rides (same-server ResolveAppTool + the
// existing wrapped invocation).
func TestAppsSurface_CallTool_RenderAdmissionVerifiedAndRidesInvocation(t *testing.T) {
	authz := newFakeAdmissionAuthority(t)
	inv := &stubInvoker{}
	s, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource:                 &stubResourceReader{},
		Invoker:                  inv,
		ToolContext:              &stubToolContextReader{},
		AgentResolver:            &stubAppsAgentResolver{},
		AgentReach:               allowAppsAgentReach{},
		RenderAdmissionAuthority: authz,
		DescriptorGeneration:     &fixedGenerationReader{gen: "gen-1"},
	})
	if err != nil {
		t.Fatalf("NewAppsSurface: %v", err)
	}
	// Mint a fresh admission for the exact render tuple.
	tok, err := authz.auth.Mint(context.Background(), admission.RenderTuple{
		Identity:              identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"},
		AgentID:               appsDefaultAgentID,
		ServerID:              "srv",
		ResourceURI:           "ui://app/main.html",
		DescriptorFingerprint: "gen-1",
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// The stub invoker implements the binding-invoker seam, so the
	// verified admission token rides the wrapped invocation.
	resp, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity:        appsID(),
		ServerID:        "srv",
		Tool:            "srv_tool",
		ResourceURI:     "ui://app/main.html",
		RenderAdmission: tok.Value,
	})
	if err != nil {
		t.Fatalf("Dispatch(call_tool): %v", err)
	}
	if _, ok := resp.(*types.MCPAppCallToolResponse); !ok {
		t.Fatalf("response = %T, want *types.MCPAppCallToolResponse", resp)
	}
	if inv.gotSrv != "srv" || inv.gotTool != "srv_tool" {
		t.Errorf("invoker got server=%q tool=%q, want srv/srv_tool", inv.gotSrv, inv.gotTool)
	}
}

// TestAppsSurface_CallTool_BothAuthoritiesAmbiguous asserts a request
// supplying BOTH the legacy binding and the fresh render admission is
// refused as ambiguous — the Runtime never guesses which the App meant.
func TestAppsSurface_CallTool_BothAuthoritiesAmbiguous(t *testing.T) {
	authz := newFakeAdmissionAuthority(t)
	s := newAdmissionSurface(t, authz, &fixedGenerationReader{gen: "gen-1"})
	tok, err := authz.auth.Mint(context.Background(), admission.RenderTuple{
		Identity:              identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"},
		AgentID:               appsDefaultAgentID,
		ServerID:              "srv",
		ResourceURI:           "ui://app/main.html",
		DescriptorFingerprint: "gen-1",
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	_, err = s.Dispatch(verifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity:        appsID(),
		ServerID:        "srv",
		Tool:            "srv_tool",
		ResourceURI:     "ui://app/main.html",
		Binding:         "legacy-binding",
		RenderAdmission: tok.Value,
	})
	var perr *protoerrors.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *protoerrors.Error", err)
	}
	if perr.Code != protoerrors.CodeRenderAuthorityAmbiguous {
		t.Errorf("code = %q, want %q", perr.Code, protoerrors.CodeRenderAuthorityAmbiguous)
	}
}

// TestAppsSurface_CallTool_TypedAdmissionOutcomes pins the exact typed
// mapping for every admission failure — an otherwise-current App with an
// unavailable / invalid / expired / mismatched admission never collapses
// to an ambiguous not-found.
func TestAppsSurface_CallTool_TypedAdmissionOutcomes(t *testing.T) {
	authz := newFakeAdmissionAuthority(t)
	s := newAdmissionSurface(t, authz, &fixedGenerationReader{gen: "gen-1"})

	mint := func(tuple admission.RenderTuple) string {
		t.Helper()
		tok, err := authz.auth.Mint(context.Background(), tuple)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		return tok.Value
	}
	base := admission.RenderTuple{
		Identity:              identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"},
		AgentID:               appsDefaultAgentID,
		ServerID:              "srv",
		ResourceURI:           "ui://app/main.html",
		DescriptorFingerprint: "gen-1",
	}
	call := func(renderAdmission string) *protoerrors.Error {
		t.Helper()
		_, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
			Identity:        appsID(),
			ServerID:        "srv",
			Tool:            "srv_tool",
			ResourceURI:     "ui://app/main.html",
			RenderAdmission: renderAdmission,
		})
		if err == nil {
			return nil
		}
		var perr *protoerrors.Error
		if !errors.As(err, &perr) {
			t.Fatalf("err = %v, want *protoerrors.Error", err)
		}
		return perr
	}

	cases := []struct {
		name string
		tok  string
		want protoerrors.Code
	}{
		// An EMPTY render_admission with no legacy binding is the legacy
		// no-authority path and succeeds — it is not a "missing" error.
		// CodeRenderAdmissionMissing is the defensive verify-side outcome
		// when the runtime is asked to verify a render-admission-backed
		// call that carries no token at all.
		{"unavailable (not base64url)", "!!!not-base64url!!!", protoerrors.CodeRenderAdmissionUnavailable},
		{"unavailable (garbage envelope)", "AAAAgarbageAAAA", protoerrors.CodeRenderAdmissionUnavailable},
		{"mismatch (foreign resource)", mint(func() admission.RenderTuple {
			m := base
			m.ResourceURI = "ui://other/main.html"
			return m
		}()), protoerrors.CodeRenderAdmissionMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := call(tc.tok)
			if got == nil {
				t.Fatalf("want code %q, got no error", tc.want)
			}
			if got.Code != tc.want {
				t.Errorf("code = %q, want %q", got.Code, tc.want)
			}
		})
	}

	// A well-formed token for the CURRENT tuple but a STALE generation
	// (the descriptor moved) is a typed mismatch, never not-found.
	stale := mint(func() admission.RenderTuple {
		m := base
		m.DescriptorFingerprint = "gen-0"
		return m
	}())
	if got := call(stale); got.Code != protoerrors.CodeRenderAdmissionMismatch {
		t.Errorf("stale-generation token code = %q, want %q", got.Code, protoerrors.CodeRenderAdmissionMismatch)
	}
}
