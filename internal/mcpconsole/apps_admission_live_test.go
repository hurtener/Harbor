package mcpconsole_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/mcpconsole/admission"
	"github.com/hurtener/Harbor/internal/protocol"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tools"
	authsealer "github.com/hurtener/Harbor/internal/tools/auth"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// admissionWireDigestHex matches a SHA-256 hex digest — the generation
// fingerprint's exact shape — so the wire-facing regression can prove a
// refusal Message carries no generation digest no matter which value
// leaked.
var admissionWireDigestHex = regexp.MustCompile(`[0-9a-f]{64}`)

// --- HA-56 real Registry+AppsAccessor+AppsSurface admission path -------

// admissionAppProvider is a deterministic mcp provider whose Discover
// returns the CURRENT descriptor set, including an app-only callback
// whose Invoke counts executions. It deliberately does NOT implement the
// registry's appBindingProvider seam (ValidateAppBinding), so the legacy
// live-binding path can never validate an admission — a successful
// render-admission-backed invocation proves the admission-aware seam
// never required it.
type admissionAppProvider struct {
	id    tools.ToolSourceID
	descs []tools.ToolDescriptor
}

func (p *admissionAppProvider) SourceID() tools.ToolSourceID { return p.id }
func (p *admissionAppProvider) Close(context.Context) error  { return nil }
func (p *admissionAppProvider) DisplayModes() []string       { return nil }
func (p *admissionAppProvider) ReadResource(context.Context, string) ([]byte, string, error) {
	return []byte("<html>app</html>"), "text/html", nil
}
func (p *admissionAppProvider) Discover(context.Context) ([]tools.ToolDescriptor, error) {
	return p.descs, nil
}

// admissionAppDesc builds an app-only callback descriptor with a counted
// Invoke — the "same wrapped descriptor" the admission seam must invoke.
func admissionAppDesc(name string, source tools.ToolSourceID, calls *atomic.Int64) tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Tool: tools.Tool{Name: name, Source: source, Transport: tools.TransportMCP, AppOnly: true},
		Invoke: func(context.Context, json.RawMessage) (tools.ToolResult, error) {
			calls.Add(1)
			return tools.ToolResult{Value: map[string]any{"ok": name}}, nil
		},
	}
}

// admissionResourceDesc builds the `ui://` resource descriptor the mint
// path reads through the accessor.
func admissionResourceDesc(name string, source tools.ToolSourceID) tools.ToolDescriptor {
	return tools.ToolDescriptor{
		Tool: tools.Tool{Name: name, Source: source, Transport: tools.TransportMCP},
	}
}

// stageAdmissionServer publishes one server through the exact registry
// publication path (StageRegistration + Publish with the descriptor set),
// mirroring what the attach path does — the App dispatch catalog AND the
// deterministic current provider/catalog generation are derived from the
// same staged snapshot.
func stageAdmissionServer(t *testing.T, reg *mcp.Registry, providerID string, descs []tools.ToolDescriptor) {
	t.Helper()
	swap, err := reg.StageRegistration(mcp.ServerRegistration{
		Provider:     &admissionAppProvider{id: tools.ToolSourceID(providerID), descs: descs},
		Transport:    "inmemory",
		InitialState: mcp.ServerStateOnline,
	}, descs)
	if err != nil {
		t.Fatalf("StageRegistration(%q): %v", providerID, err)
	}
	if _, err := swap.Publish(context.Background(), nil); err != nil {
		t.Fatalf("Publish(%q): %v", providerID, err)
	}
}

// realAdmissionGate is the focused real-path fresh admission gate: it
// reads the REAL deterministic current provider/catalog generation from
// the registry (via the AppsAccessor) and refuses when no current
// generation is established (detach / never-discovered → fail closed).
// It is the G3 seam test-double; G4 composes the full erasure / exposure
// checks around this same generation source.
type realAdmissionGate struct {
	acc *mcpconsole.AppsAccessor
}

func (g *realAdmissionGate) AuthorizeRender(_ context.Context, serverID, _ string) (string, error) {
	gen, ok := g.acc.CurrentGeneration(serverID)
	if !ok {
		return "", fmt.Errorf("%w: server %q has no established current provider/catalog generation",
			protocol.ErrRenderAdmissionRefused, serverID)
	}
	return gen, nil
}

// admissionAgentResolver resolves the effective agent without tenant
// state, matching the protocol-test posture.
type admissionAgentResolver struct{}

func (*admissionAgentResolver) EffectiveAgentID(requested string) (string, error) {
	if requested == "" {
		return appsAdmissionAgentID, nil
	}
	return requested, nil
}
func (*admissionAgentResolver) ResolveAgent(_ context.Context, _ identity.Identity, agentID string) (bool, error) {
	return agentID != "", nil
}

type allowAdmissionReach struct{}

func (allowAdmissionReach) AuthorizeAgentReach(context.Context, string) error { return nil }

const appsAdmissionAgentID = "admission-agent"

// admissionAuthoritySeam adapts the real sealed authority to the surface
// seam.
type admissionAuthoritySeam struct {
	auth *admission.Authority
}

func (a *admissionAuthoritySeam) Mint(ctx context.Context, rt admission.RenderTuple) (admission.Token, error) {
	return a.auth.Mint(ctx, rt)
}
func (a *admissionAuthoritySeam) Verify(ctx context.Context, expected admission.RenderTuple, token string) (admission.Claims, error) {
	return a.auth.Verify(ctx, expected, token)
}

// admissionID is the identity scope matching the verified identity
// `admissionVerifiedCtx` seats.
func admissionID() types.IdentityScope {
	return types.IdentityScope{Tenant: "t-1", User: "u-1", Session: "s-1"}
}

// admissionVerifiedCtx seats a verified identity triple (body-scope
// reconciliation requires it).
func admissionVerifiedCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), identity.Identity{
		TenantID: "t-1", UserID: "u-1", SessionID: "s-1",
	})
	if err != nil {
		t.Fatalf("seat verified identity: %v", err)
	}
	return ctx
}

// newAdmissionAuthority builds the real sealed authority over a
// deterministic AES-GCM sealer.
func newAdmissionAuthority(t *testing.T, opts ...admission.Option) *admission.Authority {
	t.Helper()
	sealer, err := authsealer.NewAESGCMSealer([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("NewAESGCMSealer: %v", err)
	}
	a, err := admission.New(sealer, opts...)
	if err != nil {
		t.Fatalf("admission.New: %v", err)
	}
	return a
}

// buildAdmissionSurface wires the REAL Registry (already staged) +
// AppsAccessor + AppsSurface with the admission seams. It returns the
// surface, the counted Invoke counter (for zero-execution assertions),
// and the registry (for generation reads / detach / replacement).
func buildAdmissionSurface(t *testing.T, reg *mcp.Registry, providerID string, descs []tools.ToolDescriptor, authz *admission.Authority, gate protocol.RenderAdmissionGate) (*protocol.AppsSurface, *atomic.Int64, *mcp.Registry, *mcpconsole.AppsAccessor) {
	t.Helper()
	// (Re)stage the server with a counted Invoke on every app-only
	// callback, so admission-path executions are observable.
	calls := new(atomic.Int64)
	counted := make([]tools.ToolDescriptor, 0, len(descs))
	for _, d := range descs {
		if d.Tool.AppOnly {
			d.Invoke = func(context.Context, json.RawMessage) (tools.ToolResult, error) {
				calls.Add(1)
				return tools.ToolResult{Value: map[string]any{"ok": d.Tool.Name}}, nil
			}
		}
		counted = append(counted, d)
	}
	stageAdmissionServer(t, reg, providerID, counted)

	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:    reg,
		Catalog:     tools.NewCatalog(),
		Store:       newAppsStore(t),
		Bus:         newAppsBus(t),
		ToolContext: newAppsToolCtx(t),
		Threshold:   1024,
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	if gate == nil {
		gate = &realAdmissionGate{acc: acc}
	}
	if authz == nil {
		authz = newAdmissionAuthority(t)
	}
	s, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource:                 acc,
		Invoker:                  acc,
		ToolContext:              acc,
		AgentResolver:            &admissionAgentResolver{},
		AgentReach:               allowAdmissionReach{},
		RenderAdmissionAuthority: &admissionAuthoritySeam{auth: authz},
		RenderAdmissionGate:      gate,
	})
	if err != nil {
		t.Fatalf("NewAppsSurface: %v", err)
	}
	return s, calls, reg, acc
}

// TestAdmissionPath_RealRegistry_MintsViaSurfaceAndInvokesExactlyOnce is
// the focused REAL-path success test the review demanded: sealed
// admission → same-server app-only ResolveAppTool → wrapped invocation
// EXACTLY ONCE, with NO legacy ValidateAppBinding requirement (the
// provider does not implement appBindingProvider, so the legacy path
// could never validate it).
func TestAdmissionPath_RealRegistry_MintsViaSurfaceAndInvokesExactlyOnce(t *testing.T) {
	reg := mcp.NewRegistry()
	descs := []tools.ToolDescriptor{
		admissionResourceDesc("srv-a__resource.ui://srv-a/app.html", "srv-a"),
		admissionAppDesc("srv-a_cb", "srv-a", new(atomic.Int64)),
	}
	s, calls, _, _ := buildAdmissionSurface(t, reg, "srv-a", descs, nil, nil)

	// Mint through the REAL surface path: read_resource with the opt-in
	// flag binds the current registry generation and seals the tuple.
	resp, err := s.Dispatch(admissionVerifiedCtx(t), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity:               admissionID(),
		ServerID:               "srv-a",
		ResourceURI:            "ui://srv-a/app.html",
		RequestRenderAdmission: true,
	})
	if err != nil {
		t.Fatalf("Dispatch(read_resource): %v", err)
	}
	out := resp.(*types.ReadMCPResourceResponse)
	if out.RenderAdmission == nil || out.RenderAdmission.Token == "" {
		t.Fatalf("opt-in read returned no bounded admission: %+v", out.RenderAdmission)
	}
	if out.RenderAdmission.Availability != types.RenderAdmissionAvailable {
		t.Fatalf("availability = %q, want available", out.RenderAdmission.Availability)
	}
	if out.RenderAdmission.IssuedAt == "" || out.RenderAdmission.ExpiresAt == "" {
		t.Errorf("bounded token metadata missing: %+v", out.RenderAdmission)
	}

	// Invoke through the REAL surface path: the verified admission rides
	// the distinct admission-aware seam → same-server ResolveAppTool →
	// the SAME wrapped descriptor, exactly once.
	resp2, err := s.Dispatch(admissionVerifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity:        admissionID(),
		ServerID:        "srv-a",
		Tool:            "srv-a_cb",
		ResourceURI:     "ui://srv-a/app.html",
		RenderAdmission: out.RenderAdmission.Token,
	})
	if err != nil {
		t.Fatalf("Dispatch(call_tool): %v", err)
	}
	ct := resp2.(*types.MCPAppCallToolResponse)
	if ct.Tool != "srv-a_cb" {
		t.Errorf("echoed tool = %q, want srv-a_cb", ct.Tool)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("wrapped descriptor invocations = %d, want exactly 1", got)
	}
}

// TestAdmissionPath_CrossServer_ZeroExecutions proves a token minted for
// one server can never invoke a callback on another server: the sealed
// tuple mismatch refuses before the seam, executing zero callbacks.
func TestAdmissionPath_CrossServer_ZeroExecutions(t *testing.T) {
	reg := mcp.NewRegistry()
	descsA := []tools.ToolDescriptor{
		admissionResourceDesc("srv-a__resource.ui://srv-a/app.html", "srv-a"),
		admissionAppDesc("srv-a_cb", "srv-a", new(atomic.Int64)),
	}
	descsB := []tools.ToolDescriptor{
		admissionResourceDesc("srv-b__resource.ui://srv-b/app.html", "srv-b"),
		admissionAppDesc("srv-b_cb", "srv-b", new(atomic.Int64)),
	}
	s, calls, reg, acc := buildAdmissionSurface(t, reg, "srv-a", descsA, nil, nil)
	stageAdmissionServer(t, reg, "srv-b", descsB)
	authz := newAdmissionAuthority(t)

	genA, _ := reg.CurrentGeneration("srv-a")
	tokA, err := authz.Mint(context.Background(), admission.RenderTuple{
		Identity:              identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"},
		AgentID:               appsAdmissionAgentID,
		ServerID:              "srv-a",
		ResourceURI:           "ui://srv-a/app.html",
		DescriptorFingerprint: genA,
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Call naming server srv-b: the sealed tuple binds srv-a, so the
	// verify refuses (mismatch) before the seam — zero executions.
	_, err = s.Dispatch(admissionVerifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: admissionID(), ServerID: "srv-b", Tool: "srv-a_cb",
		ResourceURI: "ui://srv-a/app.html", RenderAdmission: tokA.Value,
	})
	var perr *protoerrors.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *protoerrors.Error", err)
	}
	if perr.Code != protoerrors.CodeRenderAdmissionMismatch {
		t.Errorf("code = %q, want %q", perr.Code, protoerrors.CodeRenderAdmissionMismatch)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("wrapped invocations = %d, want 0 (cross-server must execute zero callbacks)", got)
	}
	_ = acc
}

// TestAdmissionPath_GenerationMismatchAfterRefresh_ZeroExecutions proves
// a STALE generation after a successful discovery change executes zero
// callbacks: the token was minted against generation G1; the refresh
// changes the descriptor set → the gate returns G2 → the sealed tuple
// mismatch refuses before the seam.
func TestAdmissionPath_GenerationMismatchAfterRefresh_ZeroExecutions(t *testing.T) {
	reg := mcp.NewRegistry()
	descs := []tools.ToolDescriptor{
		admissionResourceDesc("srv-a__resource.ui://srv-a/app.html", "srv-a"),
		admissionAppDesc("srv-a_cb", "srv-a", new(atomic.Int64)),
	}
	s, calls, reg, acc := buildAdmissionSurface(t, reg, "srv-a", descs, nil, nil)
	_ = acc
	authz := newAdmissionAuthority(t)

	gen1, _ := reg.CurrentGeneration("srv-a")
	tok, err := authz.Mint(context.Background(), admission.RenderTuple{
		Identity:              identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"},
		AgentID:               appsAdmissionAgentID,
		ServerID:              "srv-a",
		ResourceURI:           "ui://srv-a/app.html",
		DescriptorFingerprint: gen1,
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// A successful discovery change: the provider now serves a DIFFERENT
	// callback set, so the deterministic generation changes.
	swap, err := reg.StageRegistration(mcp.ServerRegistration{
		Provider: &admissionAppProvider{id: "srv-a", descs: []tools.ToolDescriptor{
			admissionResourceDesc("srv-a__resource.ui://srv-a/app.html", "srv-a"),
			admissionAppDesc("srv-a_cb-v2", "srv-a", new(atomic.Int64)),
		}},
		Transport: "inmemory", InitialState: mcp.ServerStateOnline,
	}, []tools.ToolDescriptor{
		admissionResourceDesc("srv-a__resource.ui://srv-a/app.html", "srv-a"),
		admissionAppDesc("srv-a_cb-v2", "srv-a", new(atomic.Int64)),
	})
	if err != nil {
		t.Fatalf("StageRegistration(v2): %v", err)
	}
	if _, err := swap.Publish(admissionVerifiedCtx(t), nil); err != nil {
		t.Fatalf("Publish(v2): %v", err)
	}
	gen2, ok := reg.CurrentGeneration("srv-a")
	if !ok || gen2 == gen1 {
		t.Fatalf("replacement did not change the deterministic generation (gen1=%q gen2=%q ok=%v)", gen1, gen2, ok)
	}
	// The OLD token now mismatches the CURRENT generation → typed
	// mismatch, zero executions.
	_, err = s.Dispatch(admissionVerifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: admissionID(), ServerID: "srv-a", Tool: "srv-a_cb",
		ResourceURI: "ui://srv-a/app.html", RenderAdmission: tok.Value,
	})
	var perr *protoerrors.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *protoerrors.Error", err)
	}
	if perr.Code != protoerrors.CodeRenderAdmissionMismatch {
		t.Errorf("code = %q, want %q", perr.Code, protoerrors.CodeRenderAdmissionMismatch)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("wrapped invocations = %d, want 0 (stale generation must execute zero callbacks)", got)
	}
}

// TestAdmissionPath_Detach_FailsClosedZeroExecutions proves detach
// (Deregister) makes the current generation unknown → the fresh gate
// refuses → the callback fails with the typed refusal and executes zero
// callbacks.
func TestAdmissionPath_Detach_FailsClosedZeroExecutions(t *testing.T) {
	reg := mcp.NewRegistry()
	descs := []tools.ToolDescriptor{
		admissionResourceDesc("srv-a__resource.ui://srv-a/app.html", "srv-a"),
		admissionAppDesc("srv-a_cb", "srv-a", new(atomic.Int64)),
	}
	s, calls, reg, acc := buildAdmissionSurface(t, reg, "srv-a", descs, nil, nil)
	_ = acc
	authz := newAdmissionAuthority(t)

	gen, _ := reg.CurrentGeneration("srv-a")
	tok, err := authz.Mint(context.Background(), admission.RenderTuple{
		Identity:              identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"},
		AgentID:               appsAdmissionAgentID,
		ServerID:              "srv-a",
		ResourceURI:           "ui://srv-a/app.html",
		DescriptorFingerprint: gen,
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Detach the server (zero owner matches the boot-declared entry).
	if err := reg.Deregister(admissionVerifiedCtx(t), "srv-a", authsealer.Owner{}); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	_, err = s.Dispatch(admissionVerifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: admissionID(), ServerID: "srv-a", Tool: "srv-a_cb",
		ResourceURI: "ui://srv-a/app.html", RenderAdmission: tok.Value,
	})
	var perr *protoerrors.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *protoerrors.Error", err)
	}
	// The gate cannot establish a current generation → scope-level
	// refusal (fail closed), never a not-found.
	if perr.Code != protoerrors.CodeScopeMismatch {
		t.Errorf("code = %q, want %q (detach → fresh-gate refusal)", perr.Code, protoerrors.CodeScopeMismatch)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("wrapped invocations = %d, want 0 (detach must execute zero callbacks)", got)
	}
}

// TestAdmissionPath_ExpiredThroughSurface_ZeroExecutions exercises the
// EXPIRED outcome through the real AppsSurface (not only the sealed-
// authority unit package): a token minted under a controllable clock,
// verified after its TTL + skew passes, executes zero callbacks.
func TestAdmissionPath_ExpiredThroughSurface_ZeroExecutions(t *testing.T) {
	reg := mcp.NewRegistry()
	descs := []tools.ToolDescriptor{
		admissionResourceDesc("srv-a__resource.ui://srv-a/app.html", "srv-a"),
		admissionAppDesc("srv-a_cb", "srv-a", new(atomic.Int64)),
	}
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	authz := newAdmissionAuthority(t, admission.WithClock(clock), admission.WithTTL(15*time.Minute))
	s, calls, reg, _ := buildAdmissionSurface(t, reg, "srv-a", descs, authz, nil)

	gen, _ := reg.CurrentGeneration("srv-a")
	tok, err := authz.Mint(context.Background(), admission.RenderTuple{
		Identity:              identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"},
		AgentID:               appsAdmissionAgentID,
		ServerID:              "srv-a",
		ResourceURI:           "ui://srv-a/app.html",
		DescriptorFingerprint: gen,
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Advance past TTL + the bounded clock skew.
	now = now.Add(30 * time.Minute)
	_, err = s.Dispatch(admissionVerifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: admissionID(), ServerID: "srv-a", Tool: "srv-a_cb",
		ResourceURI: "ui://srv-a/app.html", RenderAdmission: tok.Value,
	})
	var perr *protoerrors.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *protoerrors.Error", err)
	}
	if perr.Code != protoerrors.CodeRenderAdmissionExpired {
		t.Errorf("code = %q, want %q", perr.Code, protoerrors.CodeRenderAdmissionExpired)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("wrapped invocations = %d, want 0 (expired must execute zero callbacks)", got)
	}
}

// TestAdmissionPath_TamperedThroughSurface_ZeroExecutions exercises the
// unavailable (tamper) outcome through the real AppsSurface.
func TestAdmissionPath_TamperedThroughSurface_ZeroExecutions(t *testing.T) {
	reg := mcp.NewRegistry()
	descs := []tools.ToolDescriptor{
		admissionResourceDesc("srv-a__resource.ui://srv-a/app.html", "srv-a"),
		admissionAppDesc("srv-a_cb", "srv-a", new(atomic.Int64)),
	}
	s, calls, _, _ := buildAdmissionSurface(t, reg, "srv-a", descs, nil, nil)
	_, err := s.Dispatch(admissionVerifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: admissionID(), ServerID: "srv-a", Tool: "srv-a_cb",
		ResourceURI: "ui://srv-a/app.html", RenderAdmission: "!!!not-base64url!!!",
	})
	var perr *protoerrors.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *protoerrors.Error", err)
	}
	if perr.Code != protoerrors.CodeRenderAdmissionUnavailable {
		t.Errorf("code = %q, want %q", perr.Code, protoerrors.CodeRenderAdmissionUnavailable)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("wrapped invocations = %d, want 0 (tamper must execute zero callbacks)", got)
	}
}

// TestAdmissionPath_PausedServer_ZeroExecutions proves the current
// paused/disabled exposure gate re-runs INSIDE the admission-aware seam
// (the same wrapped invocation tail as the ordinary path): a server that
// is CURRENTLY paused refuses before any side effect, executing zero
// callbacks, even with a fully valid sealed admission.
func TestAdmissionPath_PausedServer_ZeroExecutions(t *testing.T) {
	reg := mcp.NewRegistry()
	descs := []tools.ToolDescriptor{
		admissionResourceDesc("srv-a__resource.ui://srv-a/app.html", "srv-a"),
		admissionAppDesc("srv-a_cb", "srv-a", new(atomic.Int64)),
	}
	// Build the accessor with a real agent-config registry that PAUSES
	// the server, then build the surface over it.
	reg2 := gateRegistry(t)
	if _, err := reg2.SetRevision(admissionVerifiedCtx(t), gateID(), appsAdmissionAgentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		ToolExposure: &agentcfg.ToolExposure{PausedServers: []string{"srv-a"}},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("set paused: %v", err)
	}
	calls := new(atomic.Int64)
	counted := make([]tools.ToolDescriptor, 0, len(descs))
	for _, d := range descs {
		if d.Tool.AppOnly {
			d.Invoke = func(context.Context, json.RawMessage) (tools.ToolResult, error) {
				calls.Add(1)
				return tools.ToolResult{Value: map[string]any{"ok": d.Tool.Name}}, nil
			}
		}
		counted = append(counted, d)
	}
	stageAdmissionServer(t, reg, "srv-a", counted)
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:    reg,
		Catalog:     tools.NewCatalog(),
		Store:       newAppsStore(t),
		Bus:         newAppsBus(t),
		ToolContext: newAppsToolCtx(t),
		AgentConfig: reg2,
		AgentID:     appsAdmissionAgentID,
		Threshold:   1024,
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	authz := newAdmissionAuthority(t)
	s, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource:                 acc,
		Invoker:                  acc,
		ToolContext:              acc,
		AgentResolver:            &admissionAgentResolver{},
		AgentReach:               allowAdmissionReach{},
		RenderAdmissionAuthority: &admissionAuthoritySeam{auth: authz},
		RenderAdmissionGate:      &realAdmissionGate{acc: acc},
	})
	if err != nil {
		t.Fatalf("NewAppsSurface: %v", err)
	}
	gen, _ := reg.CurrentGeneration("srv-a")
	tok, err := authz.Mint(context.Background(), admission.RenderTuple{
		Identity:              identity.Identity{TenantID: "t-1", UserID: "u-1", SessionID: "s-1"},
		AgentID:               appsAdmissionAgentID,
		ServerID:              "srv-a",
		ResourceURI:           "ui://srv-a/app.html",
		DescriptorFingerprint: gen,
	})
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// The admission is fully valid; the exposure gate refuses BEFORE the
	// wrapped descriptor runs, and the surface classifies the refusal as
	// CodeScopeMismatch (the typed exposure-denial verdict).
	_, err = s.Dispatch(admissionVerifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: admissionID(), ServerID: "srv-a", Tool: "srv-a_cb",
		ResourceURI: "ui://srv-a/app.html", RenderAdmission: tok.Value,
	})
	var perr *protoerrors.Error
	if !errors.As(err, &perr) {
		t.Fatalf("paused-server call err = %v, want *protoerrors.Error", err)
	}
	if perr.Code != protoerrors.CodeScopeMismatch {
		t.Errorf("paused-server code = %q, want %q", perr.Code, protoerrors.CodeScopeMismatch)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("wrapped invocations = %d, want 0 (paused server must execute zero callbacks)", got)
	}
}

// TestAdmissionPath_UnwiredFreshGate_FailsLoudZeroExecutions proves the
// compatible disabled surface: with the admission authority AND gate both
// absent, the opt-in mint fails loud (CodeRuntimeError) and a
// render-admission-backed call fails loud — ordinary reads and the legacy
// binding path are untouched, and zero callbacks execute.
func TestAdmissionPath_UnwiredFreshGate_FailsLoudZeroExecutions(t *testing.T) {
	reg := mcp.NewRegistry()
	descs := []tools.ToolDescriptor{
		admissionResourceDesc("srv-a__resource.ui://srv-a/app.html", "srv-a"),
		admissionAppDesc("srv-a_cb", "srv-a", new(atomic.Int64)),
	}
	calls := new(atomic.Int64)
	counted := make([]tools.ToolDescriptor, 0, len(descs))
	for _, d := range descs {
		if d.Tool.AppOnly {
			d.Invoke = func(context.Context, json.RawMessage) (tools.ToolResult, error) {
				calls.Add(1)
				return tools.ToolResult{Value: map[string]any{"ok": d.Tool.Name}}, nil
			}
		}
		counted = append(counted, d)
	}
	stageAdmissionServer(t, reg, "srv-a", counted)
	acc, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:    reg,
		Catalog:     tools.NewCatalog(),
		Store:       newAppsStore(t),
		Bus:         newAppsBus(t),
		ToolContext: newAppsToolCtx(t),
		Threshold:   1024,
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	// NO authority, NO gate — the compatible disabled surface.
	s, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource: acc, Invoker: acc, ToolContext: acc,
		AgentResolver: &admissionAgentResolver{},
		AgentReach:    allowAdmissionReach{},
	})
	if err != nil {
		t.Fatalf("NewAppsSurface: %v", err)
	}
	// Opt-in mint fails loud.
	_, err = s.Dispatch(admissionVerifiedCtx(t), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity: admissionID(), ServerID: "srv-a", ResourceURI: "ui://srv-a/app.html",
		RequestRenderAdmission: true,
	})
	var perr *protoerrors.Error
	if !errors.As(err, &perr) {
		t.Fatalf("opt-in mint err = %v, want *protoerrors.Error", err)
	}
	if perr.Code != protoerrors.CodeRuntimeError {
		t.Errorf("opt-in mint code = %q, want %q (unwired seam fails loud)", perr.Code, protoerrors.CodeRuntimeError)
	}
	// Render-admission-backed call fails loud, zero executions.
	_, err = s.Dispatch(admissionVerifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: admissionID(), ServerID: "srv-a", Tool: "srv-a_cb",
		ResourceURI: "ui://srv-a/app.html", RenderAdmission: "opaque-token",
	})
	if !errors.As(err, &perr) {
		t.Fatalf("call err = %v, want *protoerrors.Error", err)
	}
	if perr.Code != protoerrors.CodeRuntimeError {
		t.Errorf("call code = %q, want %q", perr.Code, protoerrors.CodeRuntimeError)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("wrapped invocations = %d, want 0", got)
	}
	// Ordinary reads still work.
	if _, err := s.Dispatch(admissionVerifiedCtx(t), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity: admissionID(), ServerID: "srv-a", ResourceURI: "ui://srv-a/app.html",
	}); err != nil {
		t.Fatalf("ordinary read broke on the disabled surface: %v", err)
	}
}

// --- method-selection-is-not-authority (P1): the call-local proof --------

// directAdmissionCtx seats the exact context a direct internal caller
// would hold: a fully verified identity triple AND a reach-admitted
// effective agent — the context the old code accepted. The call-local
// proof is deliberately NOT minted, because only the Protocol surface can
// mint it.
func directAdmissionCtx(t *testing.T) context.Context {
	t.Helper()
	return tools.WithEffectiveAgentConfig(admissionVerifiedCtx(t), appsAdmissionAgentID)
}

// TestAdmissionPath_DirectCall_NoProof_RefusedZeroExecutions proves the
// P1 fix: the exported admission-aware method is NOT an authority. An
// internal caller invoking AppsAccessor.CallToolAdmitted directly with a
// fully verified identity / effective-agent context but NO call-local
// proof is refused BEFORE any resolution or invocation — the named
// app-only callback resolves on the server, yet executes zero callbacks.
func TestAdmissionPath_DirectCall_NoProof_RefusedZeroExecutions(t *testing.T) {
	reg := mcp.NewRegistry()
	descs := []tools.ToolDescriptor{
		admissionResourceDesc("srv-a__resource.ui://srv-a/app.html", "srv-a"),
		admissionAppDesc("srv-a_cb", "srv-a", new(atomic.Int64)),
	}
	s, calls, _, acc := buildAdmissionSurface(t, reg, "srv-a", descs, nil, nil)
	_ = s

	_, err := acc.CallToolAdmitted(directAdmissionCtx(t), "srv-a", "ui://srv-a/app.html", "srv-a_cb", json.RawMessage(`{}`))
	if !errors.Is(err, protocol.ErrAccessorScopeDenied) {
		t.Fatalf("direct no-proof call err = %v, want a wrapped %v", err, protocol.ErrAccessorScopeDenied)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("wrapped invocations = %d, want 0 (no proof must execute zero callbacks)", got)
	}
}

// TestAdmissionPath_DirectCall_NoProof_RefusalPrecedesResolveAppTool
// proves the refusal happens BEFORE ResolveAppTool: a call naming an
// ABSENT server answers the proof refusal (scope denied), never the
// not-found a ResolveAppTool miss would produce — resolution was never
// reached, so the server's existence is neither consulted nor revealed.
func TestAdmissionPath_DirectCall_NoProof_RefusalPrecedesResolveAppTool(t *testing.T) {
	reg := mcp.NewRegistry()
	descs := []tools.ToolDescriptor{
		admissionResourceDesc("srv-a__resource.ui://srv-a/app.html", "srv-a"),
		admissionAppDesc("srv-a_cb", "srv-a", new(atomic.Int64)),
	}
	s, calls, _, acc := buildAdmissionSurface(t, reg, "srv-a", descs, nil, nil)
	_ = s

	_, err := acc.CallToolAdmitted(directAdmissionCtx(t), "absent-srv", "ui://absent-srv/app.html", "absent_cb", json.RawMessage(`{}`))
	if !errors.Is(err, protocol.ErrAccessorScopeDenied) {
		t.Fatalf("absent-server direct call err = %v, want the proof refusal (%v) — proves the refusal precedes ResolveAppTool",
			err, protocol.ErrAccessorScopeDenied)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("wrapped invocations = %d, want 0", got)
	}
}

// TestAdmissionPath_DirectCall_NoIdentity_FailsClosed proves the accessor
// also fails closed on a missing identity (nothing to bind the proof
// against), with zero executions — before any resolution.
func TestAdmissionPath_DirectCall_NoIdentity_FailsClosed(t *testing.T) {
	reg := mcp.NewRegistry()
	descs := []tools.ToolDescriptor{
		admissionResourceDesc("srv-a__resource.ui://srv-a/app.html", "srv-a"),
		admissionAppDesc("srv-a_cb", "srv-a", new(atomic.Int64)),
	}
	s, calls, _, acc := buildAdmissionSurface(t, reg, "srv-a", descs, nil, nil)
	_ = s

	ctx := tools.WithEffectiveAgentConfig(context.Background(), appsAdmissionAgentID)
	_, err := acc.CallToolAdmitted(ctx, "srv-a", "ui://srv-a/app.html", "srv-a_cb", json.RawMessage(`{}`))
	if !errors.Is(err, mcp.ErrIdentityMissing) {
		t.Fatalf("identity-less direct call err = %v, want a wrapped %v", err, mcp.ErrIdentityMissing)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("wrapped invocations = %d, want 0", got)
	}
}

// --- TOCTOU correction (P1): generation is bound into the proof --------

// racingAdmissionInvoker wraps the REAL AppsAccessor and runs a hook
// immediately before delegating an admission-verified call — the
// deterministic synchronization point that forces a catalog generation
// change AFTER the surface's sealed verification / proof mint and BEFORE
// the accessor's atomic compare+resolve. Embedding the accessor promotes
// every other seam, so the surface sees a fully functional invoker.
type racingAdmissionInvoker struct {
	*mcpconsole.AppsAccessor
	beforeAdmitted func()
}

func (r *racingAdmissionInvoker) CallToolAdmitted(ctx context.Context, serverID, resourceURI, tool string, args json.RawMessage) (protocol.MCPAppToolResultRow, error) {
	if r.beforeAdmitted != nil {
		r.beforeAdmitted()
	}
	return r.AppsAccessor.CallToolAdmitted(ctx, serverID, resourceURI, tool, args)
}

// buildRacingAdmissionSurface wires the REAL registry + accessor behind a
// racing invoker whose hook fires between the surface's sealed
// verification / proof mint and the accessor's atomic compare+resolve. It
// stages the counted descriptor set and returns the racing surface, the
// v1 execution counter, the registry, and the accessor. The mint surface
// and the racing surface share ONE authority instance, so a token minted
// through either verifies through the other. No sleeps — the hook is the
// synchronization point.
func buildRacingAdmissionSurface(t *testing.T, reg *mcp.Registry, providerID string, descs []tools.ToolDescriptor, hook func()) (*protocol.AppsSurface, *atomic.Int64, *mcp.Registry, *mcpconsole.AppsAccessor) {
	t.Helper()
	authz := newAdmissionAuthority(t)
	_, calls, reg, acc := buildAdmissionSurface(t, reg, providerID, descs, authz, nil)
	race := &racingAdmissionInvoker{AppsAccessor: acc, beforeAdmitted: hook}
	s, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource:                 acc,
		Invoker:                  race,
		ToolContext:              acc,
		AgentResolver:            &admissionAgentResolver{},
		AgentReach:               allowAdmissionReach{},
		RenderAdmissionAuthority: &admissionAuthoritySeam{auth: authz},
		RenderAdmissionGate:      &realAdmissionGate{acc: acc},
	})
	if err != nil {
		t.Fatalf("NewAppsSurface (racing): %v", err)
	}
	return s, calls, reg, acc
}

// TestAdmissionPath_UnchangedGeneration_SucceedsExactlyOnce drives the
// FULL corrected path (surface verify → proof mint → accessor
// current-generation re-read → atomic compare+resolve) through the racing
// invoker with its hook a NO-OP: the generation is unchanged between
// verification and resolution, so the call succeeds and the wrapped
// descriptor executes EXACTLY ONCE.
func TestAdmissionPath_UnchangedGeneration_SucceedsExactlyOnce(t *testing.T) {
	reg := mcp.NewRegistry()
	descs := []tools.ToolDescriptor{
		admissionResourceDesc("srv-a__resource.ui://srv-a/app.html", "srv-a"),
		admissionAppDesc("srv-a_cb", "srv-a", new(atomic.Int64)),
	}
	s, calls, _, _ := buildRacingAdmissionSurface(t, reg, "srv-a", descs, nil)

	// Mint through the real surface path (binds the current generation).
	resp, err := s.Dispatch(admissionVerifiedCtx(t), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity: admissionID(), ServerID: "srv-a", ResourceURI: "ui://srv-a/app.html",
		RequestRenderAdmission: true,
	})
	if err != nil {
		t.Fatalf("Dispatch(read_resource): %v", err)
	}
	out := resp.(*types.ReadMCPResourceResponse)
	if out.RenderAdmission == nil || out.RenderAdmission.Token == "" {
		t.Fatalf("opt-in read returned no bounded admission: %+v", out.RenderAdmission)
	}
	resp2, err := s.Dispatch(admissionVerifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: admissionID(), ServerID: "srv-a", Tool: "srv-a_cb",
		ResourceURI: "ui://srv-a/app.html", RenderAdmission: out.RenderAdmission.Token,
	})
	if err != nil {
		t.Fatalf("Dispatch(call_tool): %v", err)
	}
	ct := resp2.(*types.MCPAppCallToolResponse)
	if ct.Tool != "srv-a_cb" {
		t.Errorf("echoed tool = %q, want srv-a_cb", ct.Tool)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("wrapped descriptor invocations = %d, want exactly 1", got)
	}
}

// TestAdmissionPath_GenerationChangeAfterProofMint_ZeroExecutions is the
// deterministic TOCTOU regression: the surface verifies the sealed
// admission and mints the call-local proof for generation G1; the racing
// fake then forces a catalog generation change (a replacement: G1 → G2)
// BEFORE the accessor's atomic compare+resolve. The accessor re-reads the
// current generation (G2), the proof binds G1 → the call is refused
// scope-level, and BOTH the old row's descriptor and the new row's
// descriptor execute ZERO callbacks. No sleeps — the fake's hook is the
// synchronization point.
func TestAdmissionPath_GenerationChangeAfterProofMint_ZeroExecutions(t *testing.T) {
	reg := mcp.NewRegistry()
	descs := []tools.ToolDescriptor{
		admissionResourceDesc("srv-a__resource.ui://srv-a/app.html", "srv-a"),
		admissionAppDesc("srv-a_cb", "srv-a", new(atomic.Int64)),
	}
	v2Calls := new(atomic.Int64)
	s, calls, reg, _ := buildRacingAdmissionSurface(t, reg, "srv-a", descs, func() {
		// Force the catalog generation change AFTER the surface's sealed
		// verification / proof mint, BEFORE the accessor's atomic
		// compare+resolve.
		stageAdmissionServer(t, reg, "srv-a", []tools.ToolDescriptor{
			admissionResourceDesc("srv-a__resource.ui://srv-a/app.html", "srv-a"),
			admissionAppDesc("srv-a_cb-v2", "srv-a", v2Calls),
		})
	})

	gen1, _ := reg.CurrentGeneration("srv-a")
	// Mint through the real surface path (binds G1).
	resp, err := s.Dispatch(admissionVerifiedCtx(t), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity: admissionID(), ServerID: "srv-a", ResourceURI: "ui://srv-a/app.html",
		RequestRenderAdmission: true,
	})
	if err != nil {
		t.Fatalf("Dispatch(read_resource): %v", err)
	}
	out := resp.(*types.ReadMCPResourceResponse)
	if out.RenderAdmission == nil || out.RenderAdmission.Token == "" {
		t.Fatalf("opt-in read returned no bounded admission: %+v", out.RenderAdmission)
	}
	// The call races the replacement: the hook fires between the surface's
	// verify/proof-mint and the accessor's atomic compare+resolve.
	_, err = s.Dispatch(admissionVerifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: admissionID(), ServerID: "srv-a", Tool: "srv-a_cb",
		ResourceURI: "ui://srv-a/app.html", RenderAdmission: out.RenderAdmission.Token,
	})
	var perr *protoerrors.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *protoerrors.Error", err)
	}
	if perr.Code != protoerrors.CodeScopeMismatch {
		t.Errorf("code = %q, want %q (stale admission raced a generation change → scope refusal)",
			perr.Code, protoerrors.CodeScopeMismatch)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("v1 wrapped invocations = %d, want 0 (the old row must never execute)", got)
	}
	if got := v2Calls.Load(); got != 0 {
		t.Errorf("v2 wrapped invocations = %d, want 0 (the new row must never execute)", got)
	}
	// The hook DID move the generation — the refusal is a generation
	// mismatch, not a no-op call.
	gen2, ok := reg.CurrentGeneration("srv-a")
	if !ok || gen2 == gen1 {
		t.Fatalf("replacement did not move the generation (gen1=%q gen2=%q ok=%v)", gen1, gen2, ok)
	}
}

// TestAdmissionPath_DetachAfterProofMint_ZeroExecutions proves the detach
// leg of the same race: the server is deregistered AFTER the surface
// verified the sealed admission / minted the proof and BEFORE the
// accessor's resolve. The accessor's current-generation read fails closed
// (absent server → no current generation) → a typed scope refusal, zero
// callbacks — non-oracular, never a not-found, never a fallback to legacy
// binding or ordinary resolution.
func TestAdmissionPath_DetachAfterProofMint_ZeroExecutions(t *testing.T) {
	reg := mcp.NewRegistry()
	descs := []tools.ToolDescriptor{
		admissionResourceDesc("srv-a__resource.ui://srv-a/app.html", "srv-a"),
		admissionAppDesc("srv-a_cb", "srv-a", new(atomic.Int64)),
	}
	s, calls, reg, _ := buildRacingAdmissionSurface(t, reg, "srv-a", descs, func() {
		if err := reg.Deregister(admissionVerifiedCtx(t), "srv-a", authsealer.Owner{}); err != nil {
			t.Errorf("Deregister: %v", err)
		}
	})

	resp, err := s.Dispatch(admissionVerifiedCtx(t), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity: admissionID(), ServerID: "srv-a", ResourceURI: "ui://srv-a/app.html",
		RequestRenderAdmission: true,
	})
	if err != nil {
		t.Fatalf("Dispatch(read_resource): %v", err)
	}
	out := resp.(*types.ReadMCPResourceResponse)
	if out.RenderAdmission == nil || out.RenderAdmission.Token == "" {
		t.Fatalf("opt-in read returned no bounded admission: %+v", out.RenderAdmission)
	}
	// The call races the detach: the hook deregisters between the surface's
	// verify/proof-mint and the accessor's current-generation re-read.
	_, err = s.Dispatch(admissionVerifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: admissionID(), ServerID: "srv-a", Tool: "srv-a_cb",
		ResourceURI: "ui://srv-a/app.html", RenderAdmission: out.RenderAdmission.Token,
	})
	var perr *protoerrors.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *protoerrors.Error", err)
	}
	if perr.Code != protoerrors.CodeScopeMismatch {
		t.Errorf("code = %q, want %q (detach raced the call → scope refusal)",
			perr.Code, protoerrors.CodeScopeMismatch)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("wrapped invocations = %d, want 0 (detach must execute zero callbacks)", got)
	}
}

// TestAdmissionPath_GenerationMismatch_WireErrorHidesDigests is the P2
// wire-facing regression: after a catalog generation change races the
// call (the surface verifies the sealed admission and mints the call-local
// proof for generation G1; the server is then replaced with G2 before the
// accessor's atomic compare+resolve), the typed scope refusal
// (CodeScopeMismatch — the admission is stale) still propagates and zero
// callbacks execute, while the wire Message carries NO generation digest —
// neither the stale one the proof bound (G1) nor the current one the
// accessor re-read (G2). The accessor wraps the refusal verbatim into the
// wire message, so a digest in its text would leak catalog state to
// whoever probes the refusal.
func TestAdmissionPath_GenerationMismatch_WireErrorHidesDigests(t *testing.T) {
	reg := mcp.NewRegistry()
	descs := []tools.ToolDescriptor{
		admissionResourceDesc("srv-a__resource.ui://srv-a/app.html", "srv-a"),
		admissionAppDesc("srv-a_cb", "srv-a", new(atomic.Int64)),
	}
	v2Calls := new(atomic.Int64)
	s, calls, reg, _ := buildRacingAdmissionSurface(t, reg, "srv-a", descs, func() {
		// Force the catalog generation change AFTER the surface's sealed
		// verification / proof mint, BEFORE the accessor's atomic
		// compare+resolve — the exact window whose refusal message must
		// not disclose a digest.
		stageAdmissionServer(t, reg, "srv-a", []tools.ToolDescriptor{
			admissionResourceDesc("srv-a__resource.ui://srv-a/app.html", "srv-a"),
			admissionAppDesc("srv-a_cb-v2", "srv-a", v2Calls),
		})
	})

	gen1, _ := reg.CurrentGeneration("srv-a")
	// Mint through the real surface path (binds G1).
	resp, err := s.Dispatch(admissionVerifiedCtx(t), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity: admissionID(), ServerID: "srv-a", ResourceURI: "ui://srv-a/app.html",
		RequestRenderAdmission: true,
	})
	if err != nil {
		t.Fatalf("Dispatch(read_resource): %v", err)
	}
	out := resp.(*types.ReadMCPResourceResponse)
	if out.RenderAdmission == nil || out.RenderAdmission.Token == "" {
		t.Fatalf("opt-in read returned no bounded admission: %+v", out.RenderAdmission)
	}
	// The call races the replacement: the hook fires between the surface's
	// verify/proof-mint and the accessor's atomic compare+resolve.
	_, err = s.Dispatch(admissionVerifiedCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity: admissionID(), ServerID: "srv-a", Tool: "srv-a_cb",
		ResourceURI: "ui://srv-a/app.html", RenderAdmission: out.RenderAdmission.Token,
	})
	var perr *protoerrors.Error
	if !errors.As(err, &perr) {
		t.Fatalf("err = %v, want *protoerrors.Error", err)
	}
	// The typed mismatch still propagates at the wire: a scope refusal.
	if perr.Code != protoerrors.CodeScopeMismatch {
		t.Errorf("code = %q, want %q (stale admission raced a generation change → scope refusal)",
			perr.Code, protoerrors.CodeScopeMismatch)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("v1 wrapped invocations = %d, want 0 (the old row must never execute)", got)
	}
	if got := v2Calls.Load(); got != 0 {
		t.Errorf("v2 wrapped invocations = %d, want 0 (the new row must never execute)", got)
	}
	gen2, ok := reg.CurrentGeneration("srv-a")
	if !ok || gen2 == gen1 {
		t.Fatalf("replacement did not move the generation (gen1=%q gen2=%q ok=%v)", gen1, gen2, ok)
	}
	// The wire-facing Message carries NO digest: neither generation value,
	// and no 64-hex SHA-256 digest run anywhere in it.
	if msg := perr.Message; strings.Contains(msg, gen1) || strings.Contains(msg, gen2) {
		t.Errorf("wire message discloses a generation digest: %q", msg)
	} else if admissionWireDigestHex.MatchString(msg) {
		t.Errorf("wire message carries a hex digest: %q", msg)
	}
}
