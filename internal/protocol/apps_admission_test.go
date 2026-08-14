package protocol_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// fixedGenerationGate returns one fixed provider/catalog generation.
type fixedGenerationGate struct {
	gen string
	err error
}

func (f *fixedGenerationGate) AuthorizeRender(_ context.Context, _, _ string) (string, error) {
	return f.gen, f.err
}

// newAdmissionSurface builds an AppsSurface with the render-admission
// seams wired. A nil authority / gate leaves the seam unwired.
func newAdmissionSurface(t *testing.T, authority protocol.RenderAdmissionAuthority, gate protocol.RenderAdmissionGate) *protocol.AppsSurface {
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
		RenderAdmissionGate:      gate,
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
	s := newAdmissionSurface(t, newFakeAdmissionAuthority(t), &fixedGenerationGate{gen: "gen-1"})
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
	s := newAdmissionSurface(t, authz, &fixedGenerationGate{gen: "gen-1"})
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
	s := newAdmissionSurface(t, newFakeAdmissionAuthority(t), &fixedGenerationGate{gen: ""})
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
		RenderAdmissionGate:      &fixedGenerationGate{gen: "gen-1"},
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
	// The stub invoker implements the distinct admission-aware seam, so
	// the verified admission rides the admission-aware invocation (never
	// the legacy binding path).
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
	if inv.admittedCalls != 1 {
		t.Errorf("admission-aware invocations = %d, want exactly 1", inv.admittedCalls)
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
	s := newAdmissionSurface(t, authz, &fixedGenerationGate{gen: "gen-1"})
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
// to an ambiguous not-found — and asserts every negative retains the
// counted admission-aware invoker with ZERO executions.
func TestAppsSurface_CallTool_TypedAdmissionOutcomes(t *testing.T) {
	authz := newFakeAdmissionAuthority(t)
	inv := &stubInvoker{}
	s, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource:                 &stubResourceReader{},
		Invoker:                  inv,
		ToolContext:              &stubToolContextReader{},
		AgentResolver:            &stubAppsAgentResolver{},
		AgentReach:               allowAppsAgentReach{},
		RenderAdmissionAuthority: authz,
		RenderAdmissionGate:      &fixedGenerationGate{gen: "gen-1"},
	})
	if err != nil {
		t.Fatalf("NewAppsSurface: %v", err)
	}

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
		{"invalid (tampered well-formed base64url)", mint(func() admission.RenderTuple {
			m := base
			// A structurally broken token: valid base64url wrapping
			// garbage that fails envelope open as unavailable OR claims
			// decode as invalid — either way typed, never not-found.
			return m
		}()) + "tampered", protoerrors.CodeRenderAdmissionUnavailable},
		{"mismatch (foreign resource)", mint(func() admission.RenderTuple {
			m := base
			m.ResourceURI = "ui://other/main.html"
			return m
		}()), protoerrors.CodeRenderAdmissionMismatch},
		{"mismatch (foreign server)", mint(func() admission.RenderTuple {
			m := base
			m.ServerID = "other-srv"
			return m
		}()), protoerrors.CodeRenderAdmissionMismatch},
		{"mismatch (foreign identity)", mint(func() admission.RenderTuple {
			m := base
			m.Identity = identity.Identity{TenantID: "t-9", UserID: "u-9", SessionID: "s-9"}
			return m
		}()), protoerrors.CodeRenderAdmissionMismatch},
		{"mismatch (foreign agent)", mint(func() admission.RenderTuple {
			m := base
			m.AgentID = "other-agent"
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
			if inv.admittedCalls != 0 {
				t.Errorf("admission-aware invocations = %d, want 0 (negative must execute zero callbacks)", inv.admittedCalls)
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
	if inv.admittedCalls != 0 {
		t.Errorf("admission-aware invocations after stale-generation negative = %d, want 0", inv.admittedCalls)
	}
}

// TestAppsSurface_NewRejectsHalfWiredAdmissionSeam pins the construction
// rule: the render-admission authority and the fresh admission gate are a
// WIRED PAIR. Exactly one of the two is a half-wired seam — rejected at
// construction with ErrAppsMisconfigured, never silently degraded to the
// disabled surface.
func TestAppsSurface_NewRejectsHalfWiredAdmissionSeam(t *testing.T) {
	authz := newFakeAdmissionAuthority(t)
	gate := &fixedGenerationGate{gen: "gen-1"}

	if _, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource: &stubResourceReader{}, Invoker: &stubInvoker{}, ToolContext: &stubToolContextReader{},
		AgentResolver:            &stubAppsAgentResolver{},
		RenderAdmissionAuthority: authz, // gate missing
	}); !errors.Is(err, protocol.ErrAppsMisconfigured) {
		t.Errorf("authority-only err = %v, want ErrAppsMisconfigured", err)
	}
	if _, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource: &stubResourceReader{}, Invoker: &stubInvoker{}, ToolContext: &stubToolContextReader{},
		AgentResolver:       &stubAppsAgentResolver{},
		RenderAdmissionGate: gate, // authority missing
	}); !errors.Is(err, protocol.ErrAppsMisconfigured) {
		t.Errorf("gate-only err = %v, want ErrAppsMisconfigured", err)
	}
	// Both absent remains the compatible disabled surface.
	if _, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource: &stubResourceReader{}, Invoker: &stubInvoker{}, ToolContext: &stubToolContextReader{},
		AgentResolver: &stubAppsAgentResolver{},
	}); err != nil {
		t.Errorf("both-absent construction err = %v, want the disabled surface to construct", err)
	}
}

// TestAppsSurface_CallTool_RenderAdmissionRequiresServerAndURI asserts a
// render-admission-backed call requires BOTH the host-derived server
// identity and the exact resource URI before any lookup — an empty server
// must never fall through to ordinary/global resolution.
func TestAppsSurface_CallTool_RenderAdmissionRequiresServerAndURI(t *testing.T) {
	s := newAdmissionSurface(t, newFakeAdmissionAuthority(t), &fixedGenerationGate{gen: "gen-1"})
	_, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: appsID(), Tool: "srv_tool", ResourceURI: "ui://app/main.html",
		RenderAdmission: "opaque-token",
	})
	assertCode(t, err, protoerrors.CodeInvalidRequest)
	_, err = s.Dispatch(verifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: appsID(), ServerID: "srv", Tool: "srv_tool",
		RenderAdmission: "opaque-token",
	})
	assertCode(t, err, protoerrors.CodeInvalidRequest)
}

// TestAppsSurface_CallTool_AdmissionInvokerSeamAbsentFailsLoud asserts a
// render-admission-backed call fails LOUD (CodeRuntimeError) when the
// surface's invoker does NOT implement the distinct admission-aware
// seam — it never falls back to the ordinary or the legacy-binding
// invocation path, and executes zero callbacks.
func TestAppsSurface_CallTool_AdmissionInvokerSeamAbsentFailsLoud(t *testing.T) {
	authz := newFakeAdmissionAuthority(t)
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
	// nonAdmissionInvoker implements ONLY the ordinary + legacy binding
	// seams — deliberately NOT the admission-aware seam.
	inv := &nonAdmissionInvoker{}
	s, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource:                 &stubResourceReader{},
		Invoker:                  inv,
		ToolContext:              &stubToolContextReader{},
		AgentResolver:            &stubAppsAgentResolver{},
		AgentReach:               allowAppsAgentReach{},
		RenderAdmissionAuthority: authz,
		RenderAdmissionGate:      &fixedGenerationGate{gen: "gen-1"},
	})
	if err != nil {
		t.Fatalf("NewAppsSurface: %v", err)
	}
	_, err = s.Dispatch(verifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: appsID(), ServerID: "srv", Tool: "srv_tool",
		ResourceURI: "ui://app/main.html", RenderAdmission: tok.Value,
	})
	var perr *protoerrors.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *protoerrors.Error", err)
	}
	if perr.Code != protoerrors.CodeRuntimeError {
		t.Errorf("code = %q, want %q (seam absent must fail loud, never fall back)", perr.Code, protoerrors.CodeRuntimeError)
	}
	if inv.CallToolCalls != 0 || inv.BindingCalls != 0 {
		t.Errorf("ordinary/legacy invocations = %d/%d, want 0/0 (no fallback)", inv.CallToolCalls, inv.BindingCalls)
	}
}

// nonAdmissionInvoker implements the ordinary + legacy binding seams but
// NOT the distinct admission-aware seam, so the surface can prove a
// render-admission-backed call fails loud instead of falling back. It is
// deliberately NOT an embedding of stubInvoker (embedding would promote
// CallToolAdmitted and defeat the test).
type nonAdmissionInvoker struct {
	res           protocol.MCPAppToolResultRow
	err           error
	gotSrv        string
	gotTool       string
	CallToolCalls int
	BindingCalls  int
}

func (n *nonAdmissionInvoker) CallTool(_ context.Context, serverID, tool string, _ json.RawMessage) (protocol.MCPAppToolResultRow, error) {
	n.CallToolCalls++
	n.gotSrv, n.gotTool = serverID, tool
	if n.err != nil {
		return protocol.MCPAppToolResultRow{}, n.err
	}
	return n.res, nil
}

func (n *nonAdmissionInvoker) CallToolWithBinding(_ context.Context, serverID, _, _, tool string, _ json.RawMessage) (protocol.MCPAppToolResultRow, error) {
	n.BindingCalls++
	n.gotSrv, n.gotTool = serverID, tool
	if n.err != nil {
		return protocol.MCPAppToolResultRow{}, n.err
	}
	return n.res, nil
}

// TestAppsSurface_CallTool_GateRefusalIsScopeMismatch asserts a fresh-gate
// refusal (current render-admission conditions refuse the tuple — erasure,
// exposure, paused/disabled) maps to CodeScopeMismatch at the callback
// with ZERO executions — never a collapse into not-found, never a silent
// fall-through.
func TestAppsSurface_CallTool_GateRefusalIsScopeMismatch(t *testing.T) {
	authz := newFakeAdmissionAuthority(t)
	inv := &stubInvoker{}
	s, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource:                 &stubResourceReader{},
		Invoker:                  inv,
		ToolContext:              &stubToolContextReader{},
		AgentResolver:            &stubAppsAgentResolver{},
		AgentReach:               allowAppsAgentReach{},
		RenderAdmissionAuthority: authz,
		RenderAdmissionGate:      &refusingGate{},
	})
	if err != nil {
		t.Fatalf("NewAppsSurface: %v", err)
	}
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
		Identity: appsID(), ServerID: "srv", Tool: "srv_tool",
		ResourceURI: "ui://app/main.html", RenderAdmission: tok.Value,
	})
	var perr *protoerrors.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *protoerrors.Error", err)
	}
	if perr.Code != protoerrors.CodeScopeMismatch {
		t.Errorf("code = %q, want %q (fresh-gate refusal is a scope-level refusal)", perr.Code, protoerrors.CodeScopeMismatch)
	}
	if inv.admittedCalls != 0 {
		t.Errorf("admission-aware invocations = %d, want 0", inv.admittedCalls)
	}
}

// refusingGate always refuses the render tuple with ErrRenderAdmissionRefused.
type refusingGate struct{}

func (*refusingGate) AuthorizeRender(context.Context, string, string) (string, error) {
	return "", fmt.Errorf("%w: tuple refused by test gate", protocol.ErrRenderAdmissionRefused)
}

// TestAppsSurface_ReadResource_GateRefusalIsUnavailable asserts a
// fresh-gate refusal at MINT answers the closed `unavailable` admission
// object with NO token — the read itself succeeded; the admission could
// not be minted because the CURRENT conditions refuse.
func TestAppsSurface_ReadResource_GateRefusalIsUnavailable(t *testing.T) {
	s := newAdmissionSurface(t, newFakeAdmissionAuthority(t), &refusingGate{})
	resp, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity: appsID(), ServerID: "srv", ResourceURI: "ui://app/main.html",
		RequestRenderAdmission: true,
	})
	if err != nil {
		t.Fatalf("Dispatch(read_resource): %v", err)
	}
	out := resp.(*types.ReadMCPResourceResponse)
	if out.RenderAdmission == nil {
		t.Fatal("gate refusal: expected the explicit unavailable admission object")
	}
	if out.RenderAdmission.Availability != types.RenderAdmissionUnavailable {
		t.Errorf("availability = %q, want %q", out.RenderAdmission.Availability, types.RenderAdmissionUnavailable)
	}
	if out.RenderAdmission.Token != "" {
		t.Error("refused admission must carry no token")
	}
}

// TestAppsSurface_CallTool_ExpiredThroughSurface exercises the EXPIRED
// outcome through the AppsSurface (not only the sealed-authority unit
// package): a token minted under a controllable clock, then verified
// after its TTL passes, fails with CodeRenderAdmissionExpired and ZERO
// executions.
func TestAppsSurface_CallTool_ExpiredThroughSurface(t *testing.T) {
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	sealer, err := authsealer.NewAESGCMSealer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	clock := func() time.Time { return now }
	authz, err := admission.New(sealer, admission.WithClock(clock), admission.WithTTL(15*time.Minute))
	if err != nil {
		t.Fatalf("admission.New: %v", err)
	}
	inv := &stubInvoker{}
	s, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource:                 &stubResourceReader{},
		Invoker:                  inv,
		ToolContext:              &stubToolContextReader{},
		AgentResolver:            &stubAppsAgentResolver{},
		AgentReach:               allowAppsAgentReach{},
		RenderAdmissionAuthority: &fakeAdmissionAuthority{auth: authz},
		RenderAdmissionGate:      &fixedGenerationGate{gen: "gen-1"},
	})
	if err != nil {
		t.Fatalf("NewAppsSurface: %v", err)
	}
	tok, err := authz.Mint(context.Background(), admission.RenderTuple{
		Identity:              identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"},
		AgentID:               appsDefaultAgentID,
		ServerID:              "srv",
		ResourceURI:           "ui://app/main.html",
		DescriptorFingerprint: "gen-1",
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Advance past the TTL AND the bounded clock-skew tolerance, then
	// verify through the surface.
	now = now.Add(30 * time.Minute)
	_, err = s.Dispatch(verifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: appsID(), ServerID: "srv", Tool: "srv_tool",
		ResourceURI: "ui://app/main.html", RenderAdmission: tok.Value,
	})
	var perr *protoerrors.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *protoerrors.Error", err)
	}
	if perr.Code != protoerrors.CodeRenderAdmissionExpired {
		t.Errorf("code = %q, want %q", perr.Code, protoerrors.CodeRenderAdmissionExpired)
	}
	if inv.admittedCalls != 0 {
		t.Errorf("admission-aware invocations = %d, want 0", inv.admittedCalls)
	}
}

// TestAppsSurface_CallTool_MintsExactCallLocalProof proves the
// method-selection-is-not-authority fix: the surface mints the
// unforgeable call-local proof ONLY on the render-admission-backed path
// and ONLY after the sealed admission was opened and the fresh
// verification succeeded, and the proof binds the EXACT verified tuple —
// identity, effective agent, server, and resource URI. The
// admission-aware seam receives a ctx whose proof verifies for that
// tuple and for no other, so the accessor's re-check (which refuses any
// direct no-proof / mismatched-proof call) has something exact to hold
// against.
func TestAppsSurface_CallTool_MintsExactCallLocalProof(t *testing.T) {
	authz := newFakeAdmissionAuthority(t)
	inv := &stubInvoker{}
	s, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource:                 &stubResourceReader{},
		Invoker:                  inv,
		ToolContext:              &stubToolContextReader{},
		AgentResolver:            &stubAppsAgentResolver{},
		AgentReach:               allowAppsAgentReach{},
		RenderAdmissionAuthority: authz,
		RenderAdmissionGate:      &fixedGenerationGate{gen: "gen-1"},
	})
	if err != nil {
		t.Fatalf("NewAppsSurface: %v", err)
	}
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
	if _, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity:        appsID(),
		ServerID:        "srv",
		Tool:            "srv_tool",
		ResourceURI:     "ui://app/main.html",
		RenderAdmission: tok.Value,
	}); err != nil {
		t.Fatalf("Dispatch(call_tool): %v", err)
	}
	if inv.admittedCtx == nil {
		t.Fatal("admission-aware seam did not receive a ctx (proof cannot be checked)")
	}
	exact := identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"}
	if !protocol.CheckRenderAdmissionProof(inv.admittedCtx, exact, appsDefaultAgentID, "srv", "ui://app/main.html") {
		t.Fatal("the seam's ctx does not carry a proof for the exact verified tuple")
	}
	// Every other tuple is refused by the proof — the exact server /
	// resource / identity / agent re-check the accessor performs.
	mismatches := []struct {
		name string
		ok   func() bool
	}{
		{"foreign tenant", func() bool {
			return protocol.CheckRenderAdmissionProof(inv.admittedCtx,
				identity.Identity{TenantID: "t-9", UserID: "u-1", SessionID: "s-1"}, appsDefaultAgentID, "srv", "ui://app/main.html")
		}},
		{"foreign user", func() bool {
			return protocol.CheckRenderAdmissionProof(inv.admittedCtx,
				identity.Identity{TenantID: "t-1", UserID: "u-9", SessionID: "s-1"}, appsDefaultAgentID, "srv", "ui://app/main.html")
		}},
		{"foreign session", func() bool {
			return protocol.CheckRenderAdmissionProof(inv.admittedCtx,
				identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-9"}, appsDefaultAgentID, "srv", "ui://app/main.html")
		}},
		{"foreign agent", func() bool {
			return protocol.CheckRenderAdmissionProof(inv.admittedCtx, exact, "other-agent", "srv", "ui://app/main.html")
		}},
		{"foreign server", func() bool {
			return protocol.CheckRenderAdmissionProof(inv.admittedCtx, exact, appsDefaultAgentID, "other-srv", "ui://app/main.html")
		}},
		{"foreign resource", func() bool {
			return protocol.CheckRenderAdmissionProof(inv.admittedCtx, exact, appsDefaultAgentID, "srv", "ui://other/main.html")
		}},
		{"empty identity", func() bool {
			return protocol.CheckRenderAdmissionProof(inv.admittedCtx, identity.Identity{}, appsDefaultAgentID, "srv", "ui://app/main.html")
		}},
	}
	for _, tc := range mismatches {
		if tc.ok() {
			t.Errorf("%s: a mismatched tuple must not verify against the minted proof", tc.name)
		}
	}
}

// TestAppsSurface_CallTool_OrdinaryPathCarriesNoProof proves the proof
// is minted ONLY on the render-admission-backed path: an ordinary
// (no-admission) call reaches the invoker with NO proof — whichever
// non-admission seam the invoker implements (ordinary or legacy
// binding) — so no other call, and no later direct admission-aware call,
// can borrow one.
func TestAppsSurface_CallTool_OrdinaryPathCarriesNoProof(t *testing.T) {
	inv := &stubInvoker{}
	s := newAppsSurface(t, &stubResourceReader{}, inv)
	if _, err := s.Dispatch(verifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: appsID(), ServerID: "srv", Tool: "srv_tool", ResourceURI: "ui://app/main.html",
	}); err != nil {
		t.Fatalf("Dispatch(call_tool): %v", err)
	}
	exact := identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"}
	gotCtx := inv.callCtx
	if gotCtx == nil {
		gotCtx = inv.bindingCtx
	}
	if gotCtx == nil {
		t.Fatal("no-admission call did not reach any invoker seam")
	}
	if protocol.CheckRenderAdmissionProof(gotCtx, exact, appsDefaultAgentID, "srv", "ui://app/main.html") {
		t.Fatal("an ordinary (no-admission) call must carry NO call-local proof")
	}
}
