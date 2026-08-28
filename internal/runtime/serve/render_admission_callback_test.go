package serve

// render_admission_callback_test.go — the serve-band HA-56 current-state
// callback proof: a paused server and a disabled callback tool execute
// ZERO times through the REAL Registry + AppsAccessor + AppsSurface
// wrapped invocation path, with the PRODUCTION render-admission gate
// (durable erasure / retirement / current exposure / exact server +
// resource / current provider-catalog generation) and the PRODUCTION
// sealed authority wired exactly as serve.Boot composes them.
//
// # What this file adds over the mcpconsole package's admission tests
//
// The mcpconsole suite proves the accessor-level matrix with a focused
// generation-only gate. This file proves the SERVE-BAND composition:
// the production renderAdmissionGate (composed by WireRenderAdmission
// over the real sessions registry + agent-config desired-state + MCP
// registry) riding the real AppsSurface mint + callback-verification
// paths. The tool-specific disable is honestly described: the callback
// NAME is not known at render mint (mint binds only the (server,
// resource) tuple), so a disabled tool refuses at CALLBACK DISPATCH,
// inside the AppsAccessor's current exposure gate — never at mint.
//
// # Never persisted
//
// The render admission is a wire-only capability: it rides ctx (the
// call-local proof) and the request/response bytes, and the surface
// strips it before the invoker. Because the invocation path never sees
// the token, it cannot reach tool input (the tool-context capture), the
// tool result (App metadata), the task record (turn rows), or the event
// stream (session history). The tests assert the token never reaches
// the wrapped descriptor's arguments and that no tool-context row
// exists after a full mint+callback.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/artifacts"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole"
	"github.com/hurtener/Harbor/internal/mcpconsole/admission"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// admissionCallbackID is the callback-test verified triple.
var admissionCallbackID = identity.Identity{TenantID: "cb-t1", UserID: "cb-u1", SessionID: "cb-s1"}

// admissionCallbackAgent is the reach-admitted effective agent.
const admissionCallbackAgent = "cb-agent"

// admissionCallbackServer / resource / tool are the staged MCP App.
const (
	admissionCallbackServer   = "cb-srv"
	admissionCallbackResource = "ui://cb-srv/app.html"
	admissionCallbackTool     = "cb-srv_cb"
)

// fakeAdmClock is the controllable time source the expired-admission
// subtest injects into the real authority (admission.WithClock).
type fakeAdmClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeAdmClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeAdmClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// admissionCbProvider is a deterministic MCP provider whose Discover
// returns the CURRENT descriptor set (an app-only callback + the
// `ui://` resource) and whose ReadResource serves the App document.
type admissionCbProvider struct {
	id    tools.ToolSourceID
	descs []tools.ToolDescriptor
}

func (p *admissionCbProvider) SourceID() tools.ToolSourceID { return p.id }
func (p *admissionCbProvider) Close(context.Context) error  { return nil }
func (p *admissionCbProvider) DisplayModes() []string       { return nil }
func (p *admissionCbProvider) ReadResource(context.Context, string) ([]byte, string, error) {
	return []byte("<html>admission-app</html>"), "text/html", nil
}
func (p *admissionCbProvider) Discover(context.Context) ([]tools.ToolDescriptor, error) {
	return p.descs, nil
}

// admissionCallbackFixture wires the FULL production composition: real
// state / bus / artifacts / sessions / agent-config registry / MCP
// registry / tool-context store, the shared KEK-backed sealer, the
// WireRenderAdmission authority+gate pair, the AppsAccessor, and the
// AppsSurface. The app-only callback's Invoke counts executions and
// captures the exact arguments the wrapped descriptor received.
type admissionCallbackFixture struct {
	surface  *protocol.AppsSurface
	reg      *mcp.Registry
	agentCfg agentcfg.Registry
	st       state.StateStore
	calls    *atomic.Int64
	lastArgs atomic.Value // json.RawMessage
	id       identity.Identity
	agentID  string
}

// stageCbServer publishes one server with the app-only callback set.
// The app-only callback's Invoke counts executions and, when argsCapture
// is non-nil, hands the EXACT arguments the wrapped descriptor received
// to it (the surface strips the admission before the invoker; this is
// how the never-persisted proof observes the invocation input).
func stageCbServer(t *testing.T, reg *mcp.Registry, name string, descs []tools.ToolDescriptor, calls *atomic.Int64, argsCapture func(json.RawMessage)) {
	t.Helper()
	counted := make([]tools.ToolDescriptor, 0, len(descs))
	for _, d := range descs {
		if d.Tool.AppOnly {
			inner := d
			d.Invoke = func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
				calls.Add(1)
				if argsCapture != nil {
					argsCapture(append(json.RawMessage(nil), args...))
				}
				return tools.ToolResult{Value: map[string]any{"ok": inner.Tool.Name, "args": string(args)}}, nil
			}
		}
		counted = append(counted, d)
	}
	swap, err := reg.StageRegistration(mcp.ServerRegistration{
		Provider:     &admissionCbProvider{id: tools.ToolSourceID(name), descs: counted},
		Transport:    "inmemory",
		InitialState: mcp.ServerStateOnline,
	}, counted)
	if err != nil {
		t.Fatalf("StageRegistration(%q): %v", name, err)
	}
	if err := swap.Commit(context.Background()); err != nil {
		t.Fatalf("Commit(%q): %v", name, err)
	}
	if gen, ok := reg.CurrentGeneration(name); !ok || gen == "" {
		t.Fatalf("CurrentGeneration(%q) after commit: ok=%v gen=%q, want a real generation", name, ok, gen)
	}
}

// cbCallbackDescs is the canonical current descriptor set of the staged
// server (the resource + the app-only callback).
func cbCallbackDescs() []tools.ToolDescriptor {
	return []tools.ToolDescriptor{
		{Tool: tools.Tool{Name: admissionCallbackServer + "__resource." + admissionCallbackResource, Source: tools.ToolSourceID(admissionCallbackServer), Transport: tools.TransportMCP}},
		{Tool: tools.Tool{Name: admissionCallbackTool, Source: tools.ToolSourceID(admissionCallbackServer), Transport: tools.TransportMCP, AppOnly: true}},
	}
}

// buildAdmissionCallbackFixture composes the serve-band production
// path. authOpts forward to the sealed authority (a bounded TTL /
// controllable clock for the expired subtest); nil keeps the default.
func buildAdmissionCallbackFixture(t *testing.T, authOpts ...admission.Option) *admissionCallbackFixture {
	t.Helper()
	ctx := context.Background()
	red := auditpatterns.New()
	id := admissionCallbackID
	agentID := admissionCallbackAgent

	st, err := state.Open(ctx, config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(ctx) })
	bus := mkDriverTestBus(t, red)
	t.Cleanup(func() { _ = bus.Close(ctx) })
	arts, err := artifacts.Open(ctx, config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = arts.Close(ctx) })

	// The sessions registry the gate's durable erasure probe reads.
	sessReg, err := sessions.New(st, config.SessionsConfig{
		IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: time.Hour,
	}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = sessReg.CloseRegistry(ctx) })

	// The REAL agent-config desired-state registry the gate AND the
	// AppsAccessor's current exposure gate read from.
	agentCfg, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() { _ = agentCfg.Close(ctx) })
	retReg, ok := agentCfg.(agentcfg.RetirementRegistry)
	if !ok {
		t.Fatalf("agentcfg registry does not implement the retirement/read seam required by the production gate")
	}
	q := identity.Quadruple{Identity: id}
	if _, err := agentCfg.SetRevision(ctx, q, agentID, agentcfg.ConfigScopeAgent,
		agentcfg.ConfigPayload{ToolExposure: &agentcfg.ToolExposure{}}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("SetRevision: %v", err)
	}

	// The MCP registry with the app-only callback + `ui://` resource.
	calls := new(atomic.Int64)
	f := &admissionCallbackFixture{reg: mcp.NewRegistry(), agentCfg: agentCfg, st: st, calls: calls, id: id, agentID: agentID}
	stageCbServer(t, f.reg, admissionCallbackServer, cbCallbackDescs(), calls, func(args json.RawMessage) {
		f.lastArgs.Store(args)
	})

	// The ONE shared KEK-backed sealer (the shared-authority path the
	// OAuth store / signed admissions / HA-61 tokens also derive from).
	t.Setenv(admKEKEnv, admDummyKEKHex)
	sealer, err := toolauth.NewSealerFromEnv(admKEKEnv)
	if err != nil {
		t.Fatalf("NewSealerFromEnv: %v", err)
	}

	// The PRODUCTION authority + gate pair (WireRenderAdmission) — the
	// exact composition serve.Boot + the devstack kit wire.
	authority, gate, err := WireRenderAdmission(RenderAdmissionAuthorityDeps{
		Enabled:          true,
		Sessions:         sessReg,
		AgentConfig:      retReg,
		SessionOverlay:   nil,
		Registry:         f.reg,
		Sealer:           sealer,
		AdmissionOptions: authOpts,
	})
	if err != nil {
		t.Fatalf("WireRenderAdmission: %v", err)
	}

	// The real tool-context store + AppsAccessor + AppsSurface.
	toolCtx, err := mcpconsole.NewToolContextStore(mcpconsole.ToolContextDeps{
		State: st, Store: arts, Bus: bus,
	})
	if err != nil {
		t.Fatalf("NewToolContextStore: %v", err)
	}
	accessor, err := mcpconsole.NewAppsAccessor(mcpconsole.AppsDeps{
		Registry:    f.reg,
		Catalog:     tools.NewCatalog(),
		Store:       arts,
		Bus:         bus,
		ToolContext: toolCtx,
		AgentConfig: agentCfg,
		AgentID:     agentID,
		Threshold:   1024,
	})
	if err != nil {
		t.Fatalf("NewAppsAccessor: %v", err)
	}
	resolver := NewAgentResolverAdapter(agentCfg, agentID)
	reach := auth.NewAgentReachAuthorizer()
	surface, err := protocol.NewAppsSurface(protocol.AppsDeps{
		Resource:                 accessor,
		Invoker:                  accessor,
		ToolContext:              accessor,
		AgentResolver:            resolver,
		AgentReach:               reach,
		RenderAdmissionAuthority: authority,
		RenderAdmissionGate:      gate,
	})
	if err != nil {
		t.Fatalf("NewAppsSurface: %v", err)
	}
	f.surface = surface
	return f
}

// reqCtx seats the verified identity + signed agent reach the surface's
// per-request checks require (the exact ctx the Protocol transport
// produces after JWT validation).
func (f *admissionCallbackFixture) reqCtx(t *testing.T) context.Context {
	t.Helper()
	return admissionCallbackReqCtx(t, f.id, f.agentID)
}

// admissionCallbackReqCtx seats the verified identity and signed agent reach
// for one caller. It is used by the user-isolation callback test to drive the
// same AppsSurface with two users while sharing one compiled accessor.
func admissionCallbackReqCtx(t *testing.T, id identity.Identity, agentID string) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatalf("seat verified identity: %v", err)
	}
	return auth.WithAgentReach(ctx, []string{agentID})
}

// scope is the wire identity scope matching the verified triple.
func (f *admissionCallbackFixture) scope() types.IdentityScope {
	return types.IdentityScope{Tenant: f.id.TenantID, User: f.id.UserID, Session: f.id.SessionID}
}

func admissionCallbackScope(id identity.Identity) types.IdentityScope {
	return types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
}

// mint requests the opt-in render admission through the REAL surface
// path and returns the response object.
func (f *admissionCallbackFixture) mint(t *testing.T) *types.ReadMCPResourceResponse {
	t.Helper()
	resp, err := f.surface.Dispatch(f.reqCtx(t), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity:               f.scope(),
		AgentID:                f.agentID,
		ServerID:               admissionCallbackServer,
		ResourceURI:            admissionCallbackResource,
		RequestRenderAdmission: true,
	})
	if err != nil {
		t.Fatalf("Dispatch(read_resource): %v", err)
	}
	return resp.(*types.ReadMCPResourceResponse)
}

// callback issues a render-admission-backed app-tool-call through the
// REAL surface path. Returns the response or the typed Protocol error.
func (f *admissionCallbackFixture) callback(t *testing.T, token string) (*types.MCPAppCallToolResponse, *protoerrors.Error) {
	t.Helper()
	resp, err := f.surface.Dispatch(f.reqCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity:        f.scope(),
		ServerID:        admissionCallbackServer,
		Tool:            admissionCallbackTool,
		ResourceURI:     admissionCallbackResource,
		RenderAdmission: token,
		Arguments:       json.RawMessage(`{"q":1}`),
	})
	if err != nil {
		var perr *protoerrors.Error
		if !errors.As(err, &perr) {
			t.Fatalf("Dispatch(call_tool) non-Protocol error: %v", err)
		}
		return nil, perr
	}
	return resp.(*types.MCPAppCallToolResponse), nil
}

func (f *admissionCallbackFixture) mintAs(t *testing.T, id identity.Identity) *types.ReadMCPResourceResponse {
	t.Helper()
	resp, err := f.surface.Dispatch(admissionCallbackReqCtx(t, id, f.agentID), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity:               admissionCallbackScope(id),
		AgentID:                f.agentID,
		ServerID:               admissionCallbackServer,
		ResourceURI:            admissionCallbackResource,
		RequestRenderAdmission: true,
	})
	if err != nil {
		t.Fatalf("Dispatch(read_resource as %s): %v", id.UserID, err)
	}
	return resp.(*types.ReadMCPResourceResponse)
}

func (f *admissionCallbackFixture) callbackAs(t *testing.T, id identity.Identity, token string) (*types.MCPAppCallToolResponse, *protoerrors.Error) {
	t.Helper()
	resp, err := f.surface.Dispatch(admissionCallbackReqCtx(t, id, f.agentID), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity:        admissionCallbackScope(id),
		AgentID:         f.agentID,
		ServerID:        admissionCallbackServer,
		Tool:            admissionCallbackTool,
		ResourceURI:     admissionCallbackResource,
		RenderAdmission: token,
		Arguments:       json.RawMessage(`{"q":1}`),
	})
	if err != nil {
		var perr *protoerrors.Error
		if !errors.As(err, &perr) {
			t.Fatalf("Dispatch(call_tool as %s) non-Protocol error: %v", id.UserID, err)
		}
		return nil, perr
	}
	return resp.(*types.MCPAppCallToolResponse), nil
}

func (f *admissionCallbackFixture) setUserExposure(t *testing.T, id identity.Identity, paused, disabled []string) {
	t.Helper()
	q := identity.Quadruple{Identity: id}
	if _, err := f.agentCfg.SetRevision(context.Background(), q, f.agentID, agentcfg.ConfigScopeUser,
		agentcfg.ConfigPayload{ToolExposure: &agentcfg.ToolExposure{
			PausedServers: append([]string(nil), paused...),
			DisabledTools: append([]string(nil), disabled...),
		}}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("SetRevision(user exposure %s): %v", id.UserID, err)
	}
}

// setExposure rewrites the agent's CURRENT active revision exposure —
// the durable desired-state both the production gate and the
// AppsAccessor's callback-dispatch gate read.
func (f *admissionCallbackFixture) setExposure(t *testing.T, paused, disabled []string) {
	t.Helper()
	q := identity.Quadruple{Identity: f.id}
	if _, err := f.agentCfg.SetRevision(context.Background(), q, f.agentID, agentcfg.ConfigScopeAgent,
		agentcfg.ConfigPayload{ToolExposure: &agentcfg.ToolExposure{
			PausedServers: append([]string(nil), paused...),
			DisabledTools: append([]string(nil), disabled...),
		}}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("SetRevision(exposure): %v", err)
	}
}

// TestRenderAdmissionCallback_PausedServer_ZeroExecutions proves a
// paused server executes ZERO callbacks through the real wrapped path:
// a mint AFTER the pause answers the closed `unavailable` object (the
// production gate refuses at mint), and a callback carrying a token
// minted BEFORE the pause is refused at callback verification (the gate
// re-runs the CURRENT conditions) — both with zero invocations.
func TestRenderAdmissionCallback_PausedServer_ZeroExecutions(t *testing.T) {
	f := buildAdmissionCallbackFixture(t)

	// Mint while the server is exposed → a bounded admission.
	out := f.mint(t)
	if out.RenderAdmission == nil || out.RenderAdmission.Token == "" {
		t.Fatalf("pre-pause mint returned no admission: %+v", out.RenderAdmission)
	}
	token := out.RenderAdmission.Token

	// Pause the server (CURRENT desired state).
	f.setExposure(t, []string{admissionCallbackServer}, nil)

	// The callback with the pre-pause token is refused at the fresh
	// gate's callback-verification re-check → ZERO invocations.
	_, perr := f.callback(t, token)
	if perr == nil {
		t.Fatal("paused-server callback succeeded, want a typed refusal")
	}
	if perr.Code != protoerrors.CodeScopeMismatch {
		t.Fatalf("paused-server callback code = %q, want %q", perr.Code, protoerrors.CodeScopeMismatch)
	}
	if got := f.calls.Load(); got != 0 {
		t.Fatalf("paused-server wrapped invocations = %d, want 0", got)
	}

	// A mint while paused answers the closed unavailable object — no
	// token, no authority.
	out2 := f.mint(t)
	if out2.RenderAdmission == nil || out2.RenderAdmission.Availability != types.RenderAdmissionUnavailable {
		t.Fatalf("paused mint = %+v, want the closed unavailable admission", out2.RenderAdmission)
	}
	if out2.RenderAdmission.Token != "" {
		t.Fatal("paused mint produced a token")
	}
}

// TestRenderAdmissionCallback_DisabledCallbackTool_ZeroExecutions proves
// a disabled callback tool executes ZERO times. The tool name is not
// known at render mint — the mint binds only the (server, resource)
// tuple, so a disabled TOOL does not refuse the mint (server pause is
// the mint-time axis); the tool-specific disable is checked at CALLBACK
// DISPATCH, inside the AppsAccessor's current exposure gate, before any
// invocation. This is the honest description of the asymmetry.
func TestRenderAdmissionCallback_DisabledCallbackTool_ZeroExecutions(t *testing.T) {
	f := buildAdmissionCallbackFixture(t)

	// Disable the callback tool BEFORE the mint.
	f.setExposure(t, nil, []string{admissionCallbackTool})

	// The mint still succeeds: the callback name is not addressable at
	// render mint (only the server axis is binding there). The disabled
	// tool set remains part of the exposure union the callback-time
	// dispatch gate re-checks.
	out := f.mint(t)
	if out.RenderAdmission == nil || out.RenderAdmission.Token == "" {
		t.Fatalf("disabled-tool mint returned no admission: %+v (the tool name is not known at render mint; server pause is the mint-time axis)", out.RenderAdmission)
	}

	// The callback is refused at DISPATCH (the tool-specific disable is
	// checked by name there) → ZERO invocations.
	_, perr := f.callback(t, out.RenderAdmission.Token)
	if perr == nil {
		t.Fatal("disabled-tool callback succeeded, want a typed refusal")
	}
	if perr.Code != protoerrors.CodeScopeMismatch {
		t.Fatalf("disabled-tool callback code = %q, want %q", perr.Code, protoerrors.CodeScopeMismatch)
	}
	if got := f.calls.Load(); got != 0 {
		t.Fatalf("disabled-tool wrapped invocations = %d, want 0", got)
	}
}

// TestRenderAdmissionCallback_UserExposure_Isolated proves the admitted
// callback path re-checks the acting user's durable ConfigScopeUser exposure
// at invocation time. User A's disable and pause are both refused after the
// token was minted, while User B can invoke the same agent/server through the
// same surface and accessor.
func TestRenderAdmissionCallback_UserExposure_Isolated(t *testing.T) {
	f := buildAdmissionCallbackFixture(t)
	userA := f.id
	userB := identity.Identity{TenantID: f.id.TenantID, UserID: "cb-u2", SessionID: "cb-s2"}

	// Give user B an explicit agent revision so both users exercise the same
	// real agent resolver path; their USER revisions remain independent.
	if _, err := f.agentCfg.SetRevision(context.Background(), identity.Quadruple{Identity: userB}, f.agentID,
		agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{ToolExposure: &agentcfg.ToolExposure{}}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("SetRevision(user B agent): %v", err)
	}

	// Mint before either personal change. The render-admission gate sees the
	// shared admin exposure, while the callback gate will read each user's
	// current revision later.
	tokenA := f.mintAs(t, userA).RenderAdmission.Token
	if tokenA == "" {
		t.Fatal("user A mint returned no render admission token")
	}
	f.setUserExposure(t, userA, nil, []string{admissionCallbackTool})
	if _, perr := f.callbackAs(t, userA, tokenA); perr == nil || perr.Code != protoerrors.CodeScopeMismatch {
		t.Fatalf("user A personally-disabled callback error = %v, want scope mismatch", perr)
	}
	if got := f.calls.Load(); got != 0 {
		t.Fatalf("user A personally-disabled callback invocations = %d, want 0", got)
	}

	// User B has no personal disable and remains able to use the same
	// shared server/agent. This is the cross-user isolation assertion.
	tokenB := f.mintAs(t, userB).RenderAdmission.Token
	if tokenB == "" {
		t.Fatal("user B mint returned no render admission token")
	}
	if resp, perr := f.callbackAs(t, userB, tokenB); perr != nil || resp == nil {
		t.Fatalf("user B unaffected callback: response=%v error=%v", resp, perr)
	}
	if got := f.calls.Load(); got != 1 {
		t.Fatalf("user B unaffected callback invocations = %d, want 1", got)
	}

	// Replace only user A's personal revision with a server pause and prove
	// the same callback-time gate refuses it too. User B remains unaffected.
	tokenA2 := f.mintAs(t, userA).RenderAdmission.Token
	if tokenA2 == "" {
		t.Fatal("user A second mint returned no render admission token")
	}
	f.setUserExposure(t, userA, []string{admissionCallbackServer}, nil)
	if _, perr := f.callbackAs(t, userA, tokenA2); perr == nil || perr.Code != protoerrors.CodeScopeMismatch {
		t.Fatalf("user A personally-paused callback error = %v, want scope mismatch", perr)
	}
	if got := f.calls.Load(); got != 1 {
		t.Fatalf("user A personally-paused callback changed invocation count to %d, want 1", got)
	}

	// A fresh user-B token/call still succeeds after both user-A changes.
	tokenB2 := f.mintAs(t, userB).RenderAdmission.Token
	if tokenB2 == "" {
		t.Fatal("user B second mint returned no render admission token")
	}
	if resp, perr := f.callbackAs(t, userB, tokenB2); perr != nil || resp == nil {
		t.Fatalf("user B second unaffected callback: response=%v error=%v", resp, perr)
	}
	if got := f.calls.Load(); got != 2 {
		t.Fatalf("user B second callback invocations = %d, want 2", got)
	}
}

// TestRenderAdmissionCallback_ReplacementAndDetach_ZeroExecutions proves
// a provider/catalog generation change after mint executes ZERO
// callbacks: a REPLACEMENT (a different canonical descriptor set → a new
// generation) fails the admission's exact-generation binding, and a
// DETACH (no current generation) fails the fresh gate closed. Only a
// re-mint against the CURRENT generation re-authorizes.
func TestRenderAdmissionCallback_ReplacementAndDetach_ZeroExecutions(t *testing.T) {
	f := buildAdmissionCallbackFixture(t)

	// Mint against generation G1.
	out := f.mint(t)
	if out.RenderAdmission == nil || out.RenderAdmission.Token == "" {
		t.Fatalf("mint returned no admission: %+v", out.RenderAdmission)
	}
	tokenG1 := out.RenderAdmission.Token
	gen1, _ := f.reg.CurrentGeneration(admissionCallbackServer)

	// Replacement: stage the same server with an EXTRA descriptor —
	// the canonical current descriptor set changes, so the generation
	// moves and the G1 admission no longer binds.
	replacement := append(cbCallbackDescs(), tools.ToolDescriptor{
		Tool: tools.Tool{Name: admissionCallbackServer + "_extra", Source: tools.ToolSourceID(admissionCallbackServer), Transport: tools.TransportMCP},
	})
	stageCbServer(t, f.reg, admissionCallbackServer, replacement, f.calls, func(args json.RawMessage) {
		f.lastArgs.Store(args)
	})
	gen2, ok := f.reg.CurrentGeneration(admissionCallbackServer)
	if !ok || gen2 == gen1 || gen2 == "" {
		t.Fatalf("replacement did not move the generation (gen1=%q gen2=%q ok=%v)", gen1, gen2, ok)
	}

	// The G1 token against the G2 generation → typed mismatch, ZERO.
	_, perr := f.callback(t, tokenG1)
	if perr == nil {
		t.Fatal("stale-generation callback succeeded, want a typed refusal")
	}
	if perr.Code != protoerrors.CodeRenderAdmissionMismatch {
		t.Fatalf("stale-generation code = %q, want %q", perr.Code, protoerrors.CodeRenderAdmissionMismatch)
	}
	if got := f.calls.Load(); got != 0 {
		t.Fatalf("stale-generation wrapped invocations = %d, want 0", got)
	}

	// Re-mint against the CURRENT generation → the callback succeeds
	// exactly once (the fresh admission re-authorizes).
	out2 := f.mint(t)
	if out2.RenderAdmission == nil || out2.RenderAdmission.Token == "" {
		t.Fatalf("re-mint returned no admission: %+v", out2.RenderAdmission)
	}
	if _, perr := f.callback(t, out2.RenderAdmission.Token); perr != nil {
		t.Fatalf("fresh-generation callback refused: %v", perr)
	}
	if got := f.calls.Load(); got != 1 {
		t.Fatalf("fresh-generation wrapped invocations = %d, want exactly 1", got)
	}

	// DETACH: no current generation → the fresh gate fails closed at
	// callback verification (and the next read cannot mint — the
	// detached server's App is not readable at all), ZERO more.
	if err := f.reg.Deregister(f.reqCtx(t), admissionCallbackServer, toolauth.Owner{}); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	if _, ok := f.reg.CurrentGeneration(admissionCallbackServer); ok {
		t.Fatal("Deregister left a current generation")
	}
	if _, perr := f.callback(t, out2.RenderAdmission.Token); perr == nil {
		t.Fatal("detached-server callback succeeded, want a typed refusal")
	} else if perr.Code != protoerrors.CodeScopeMismatch {
		t.Fatalf("detached-server callback code = %q, want %q", perr.Code, protoerrors.CodeScopeMismatch)
	}
	if got := f.calls.Load(); got != 1 {
		t.Fatalf("post-detach wrapped invocations = %d, want still 1 (zero more)", got)
	}
	// The post-detach opt-in READ fails as not-found (the resource is
	// gone before any admission can be considered) — the honest surface
	// posture: a detached server's App cannot be read, so no admission
	// object exists, and no token is minted.
	if _, err := f.surface.Dispatch(f.reqCtx(t), methods.MethodMCPReadResource, &types.ReadMCPResourceRequest{
		Identity:               f.scope(),
		AgentID:                f.agentID,
		ServerID:               admissionCallbackServer,
		ResourceURI:            admissionCallbackResource,
		RequestRenderAdmission: true,
	}); err == nil {
		t.Fatal("post-detach opt-in read succeeded, want not-found")
	} else {
		var perr *protoerrors.Error
		if !errors.As(err, &perr) || perr.Code != protoerrors.CodeNotFound {
			t.Fatalf("post-detach opt-in read error = %v, want CodeNotFound", err)
		}
	}
}

// TestRenderAdmissionCallback_MismatchTamperedExpired_ZeroExecutions
// proves a mismatched, tampered, or expired admission executes ZERO
// callbacks through the real wrapped path — each with its exact typed
// Protocol code, never a collapse into not-found.
func TestRenderAdmissionCallback_MismatchTamperedExpired_ZeroExecutions(t *testing.T) {
	t.Run("resource_mismatch", func(t *testing.T) {
		f := buildAdmissionCallbackFixture(t)
		out := f.mint(t)
		if out.RenderAdmission == nil || out.RenderAdmission.Token == "" {
			t.Fatalf("mint returned no admission: %+v", out.RenderAdmission)
		}
		// The SAME server + token, but a DIFFERENT ui:// resource —
		// the sealed tuple no longer matches the current render tuple.
		_, perr := f.dispatchCallback(t, out.RenderAdmission.Token, admissionCallbackServer, "ui://cb-srv/other.html")
		if perr == nil {
			t.Fatal("resource-mismatch callback succeeded, want a typed refusal")
		}
		if perr.Code != protoerrors.CodeRenderAdmissionMismatch {
			t.Fatalf("resource-mismatch code = %q, want %q", perr.Code, protoerrors.CodeRenderAdmissionMismatch)
		}
		if got := f.calls.Load(); got != 0 {
			t.Fatalf("resource-mismatch wrapped invocations = %d, want 0", got)
		}
	})

	t.Run("tampered", func(t *testing.T) {
		f := buildAdmissionCallbackFixture(t)
		out := f.mint(t)
		if out.RenderAdmission == nil || out.RenderAdmission.Token == "" {
			t.Fatalf("mint returned no admission: %+v", out.RenderAdmission)
		}
		_, perr := f.callback(t, out.RenderAdmission.Token+"tampered")
		if perr == nil {
			t.Fatal("tampered callback succeeded, want a typed refusal")
		}
		if perr.Code != protoerrors.CodeRenderAdmissionUnavailable {
			t.Fatalf("tampered code = %q, want %q", perr.Code, protoerrors.CodeRenderAdmissionUnavailable)
		}
		if got := f.calls.Load(); got != 0 {
			t.Fatalf("tampered wrapped invocations = %d, want 0", got)
		}
	})

	t.Run("expired", func(t *testing.T) {
		// Claims carry second-granular instants, and expiry applies the
		// bounded clock skew — so the advance must cross TTL + skew (the
		// same shape the mcpconsole expired-through-surface test uses).
		clock := &fakeAdmClock{now: time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)}
		f := buildAdmissionCallbackFixture(t, admission.WithClock(clock.Now))
		out := f.mint(t)
		if out.RenderAdmission == nil || out.RenderAdmission.Token == "" {
			t.Fatalf("mint returned no admission: %+v", out.RenderAdmission)
		}
		// Advance the authority clock past TTL + the bounded clock skew.
		clock.Advance(30 * time.Minute)
		_, perr := f.callback(t, out.RenderAdmission.Token)
		if perr == nil {
			t.Fatal("expired callback succeeded, want a typed refusal")
		}
		if perr.Code != protoerrors.CodeRenderAdmissionExpired {
			t.Fatalf("expired code = %q, want %q", perr.Code, protoerrors.CodeRenderAdmissionExpired)
		}
		if got := f.calls.Load(); got != 0 {
			t.Fatalf("expired wrapped invocations = %d, want 0", got)
		}
	})
}

// dispatchCallback is the callback helper with an explicit server /
// resource override (the resource-mismatch subtest).
func (f *admissionCallbackFixture) dispatchCallback(t *testing.T, token, serverID, resourceURI string) (*types.MCPAppCallToolResponse, *protoerrors.Error) {
	t.Helper()
	resp, err := f.surface.Dispatch(f.reqCtx(t), methods.MethodMCPAppsCallTool, &types.MCPAppCallToolRequest{
		Identity:        f.scope(),
		ServerID:        serverID,
		Tool:            admissionCallbackTool,
		ResourceURI:     resourceURI,
		RenderAdmission: token,
		Arguments:       json.RawMessage(`{"q":1}`),
	})
	if err != nil {
		var perr *protoerrors.Error
		if !errors.As(err, &perr) {
			t.Fatalf("Dispatch(call_tool) non-Protocol error: %v", err)
		}
		return nil, perr
	}
	return resp.(*types.MCPAppCallToolResponse), nil
}

// TestRenderAdmissionCallback_NeverPersisted proves the admission is a
// wire-only capability: after a full mint+callback through the real
// surface, the token reaches NEITHER the wrapped descriptor's arguments
// (the surface strips it before the invoker) NOR any persisted tool
// context row — so it cannot reach turn rows, session history, tool
// context, or App metadata downstream.
func TestRenderAdmissionCallback_NeverPersisted(t *testing.T) {
	f := buildAdmissionCallbackFixture(t)

	// A full happy-path mint + callback.
	out := f.mint(t)
	if out.RenderAdmission == nil || out.RenderAdmission.Token == "" {
		t.Fatalf("mint returned no admission: %+v", out.RenderAdmission)
	}
	token := out.RenderAdmission.Token
	resp, perr := f.callback(t, token)
	if perr != nil {
		t.Fatalf("callback refused: %v", perr)
	}
	if resp.Tool != admissionCallbackTool {
		t.Fatalf("echoed tool = %q, want %q", resp.Tool, admissionCallbackTool)
	}
	if got := f.calls.Load(); got != 1 {
		t.Fatalf("wrapped invocations = %d, want exactly 1", got)
	}

	// The wrapped descriptor's captured arguments are EXACTLY the
	// caller's arguments — the admission token is never part of them.
	// (The capture rides the invocation input; the response echoes the
	// same input, which is the honest observable.)
	if got := string(f.lastArgs.Load().(json.RawMessage)); got != `{"q":1}` {
		t.Fatalf("wrapped descriptor received args %s, want exactly the caller's {\"q\":1} (the admission must never reach the invocation)", got)
	}
	if strings.Contains(string(resp.Content), token) {
		t.Fatal("the render-admission token reached the tool invocation result — it must stay wire-only")
	}
	// The response's App ref (App metadata) is derived from the tool
	// RESULT value's `_meta.ui` slot — never the admission. The callback
	// result declares no App, so none is projected.
	if resp.App != nil {
		t.Fatalf("callback unexpectedly produced an App ref %+v (App metadata derives from the tool result, never the admission)", resp.App)
	}

	// No tool-context row exists for the call: the admission path never
	// persisted a captured context carrying the token (the invocation
	// received only the caller's arguments).
	q := identity.Quadruple{Identity: f.id}
	rows, err := f.st.ListKindForIdentityBounded(context.Background(), q, "mcp.apps.tool_context", 10)
	if err != nil {
		t.Fatalf("ListKindForIdentityBounded(tool_context): %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("tool-context rows after mint+callback = %d, want 0 (no capture on the admission path)", len(rows))
	}

	// The session's durable state carries no admission anywhere: scan
	// the identity-scoped record kinds the run/session surface could
	// touch and assert the token bytes appear in none of them.
	for _, prefix := range []string{"session.", "task.", "mcp.", "planner.", "run."} {
		recs, lErr := f.st.ListKindForIdentityBounded(context.Background(), q, prefix, 50)
		if lErr != nil {
			t.Fatalf("ListKindForIdentityBounded(%q): %v", prefix, lErr)
		}
		for _, rec := range recs {
			if strings.Contains(string(rec.Bytes), token) {
				t.Fatalf("render-admission token persisted in state kind %q (record %s) — the admission must never be persisted in turn rows, session history, tool context, or App metadata", prefix, rec.Kind)
			}
		}
	}
}
