package serve

// render_admission_test.go — the serve-band HA-56 render-admission
// composition: the ONE shared restart-stable KEK-backed sealer
// (ResolveSharedKEKSealer) and the production renderAdmissionGate
// (WireRenderAdmission) the Protocol AppsSurface runs before every mint
// and callback verification. The gate's fail-closed refusals — missing
// identity, absent/empty generation, non-ui resource, durable erasure,
// retirement, paused server — are pinned against the REAL collaborators
// (sessions registry, MCP registry) where they matter.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/artifacts"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/skills"
	localdb "github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// renderAdmID is the gate test's verified triple.
var renderAdmID = identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}

// renderAdmAgent is the gate test's effective agent.
const renderAdmAgent = "agent-x"

// renderAdmServer is the registered MCP server the gate checks.
const renderAdmServer = "apps-server"

// admKEKEnv is the test-dummy env name naming the shared KEK (the
// operator's `tools.oauth_token_kek_env` slot — a documented dummy, never
// a real secret, per CLAUDE.md §7 rule 2).
const admKEKEnv = "HARBOR_RENDER_ADM_TEST_KEK"

// admDummyKEKHex is a documented-dummy 32-byte hex KEK.
const admDummyKEKHex = "0101010101010101010101010101010101010101010101010101010101010101"

// fakeAdmissionAgentCfg is the narrow desired-state reader the gate
// consumes: current exposure (Active) + the retirement gate. It records
// the agent ids each read was queried under so tests can prove the gate
// reads the request's stamped effective agent (never a boot default).
type fakeAdmissionAgentCfg struct {
	revision  agentcfg.Revision
	hasActive bool
	retired   bool
	activeErr error
	retireErr error

	// queried is the last agent id each seam was asked about.
	activeQueriedAgent   string
	retirementQueriedFor string
}

func (f *fakeAdmissionAgentCfg) Active(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope) (agentcfg.Revision, bool, error) {
	f.activeQueriedAgent = agentID
	return f.revision, f.hasActive, f.activeErr
}

func (f *fakeAdmissionAgentCfg) RetirementStatus(ctx context.Context, id identity.Quadruple, agentID string) (agentcfg.RetirementStatus, bool, error) {
	f.retirementQueriedFor = agentID
	if f.retired {
		return agentcfg.RetirementStatus{Completed: true}, true, nil
	}
	return agentcfg.RetirementStatus{}, false, f.retireErr
}

var _ renderAdmissionAgentConfig = (*fakeAdmissionAgentCfg)(nil)

// admissionStubProvider is the minimal serverProvider the MCP registry
// registration needs (the gate never touches the provider — only the
// registry's recorded generation).
type admissionStubProvider struct{ id string }

func (p admissionStubProvider) SourceID() tools.ToolSourceID { return tools.ToolSourceID(p.id) }
func (p admissionStubProvider) Discover(context.Context) ([]tools.ToolDescriptor, error) {
	return []tools.ToolDescriptor{{Tool: tools.Tool{Name: p.id + "-tool"}}}, nil
}
func (p admissionStubProvider) DisplayModes() []string { return nil }
func (p admissionStubProvider) ReadResource(context.Context, string) ([]byte, string, error) {
	return nil, "", errors.New("unused in gate tests")
}
func (p admissionStubProvider) Close(context.Context) error { return nil }

// buildAdmissionGateFixture wires the REAL collaborators the gate
// checks: an MCP registry with one discovered server (a real
// generation), a sessions registry + erasure cascade (the durable
// erasure probe), and a fake agent-config reader. The agentCfg pointer
// is SHARED between the gate and the fixture so tests can mutate the
// desired-state reader and observe which agent id the gate queried.
type admissionGateFixture struct {
	gate     *renderAdmissionGate
	registry *mcp.Registry
	sessions *sessions.Registry
	eraser   *sessions.CascadeEraser
	agentCfg *fakeAdmissionAgentCfg
	overlay  sessionoverlay.Store
}

func buildAdmissionGateFixture(t *testing.T) admissionGateFixture {
	t.Helper()
	ctx := context.Background()
	red := auditpatterns.New()

	// The MCP registry with a real current provider/catalog generation.
	reg := mcp.NewRegistry()
	swap, err := reg.StageRegistration(mcp.ServerRegistration{
		Provider:     admissionStubProvider{id: renderAdmServer},
		Transport:    "http+sse",
		URLOrCommand: "https://mcp.example.com/apps",
		InitialState: mcp.ServerStateOnline,
	}, []tools.ToolDescriptor{{Tool: tools.Tool{Name: renderAdmServer + "-tool"}}})
	if err != nil {
		t.Fatalf("StageRegistration: %v", err)
	}
	if err := swap.Commit(ctx); err != nil {
		t.Fatalf("Commit registration: %v", err)
	}
	if gen, ok := reg.CurrentGeneration(renderAdmServer); !ok || gen == "" {
		t.Fatalf("CurrentGeneration after registration: ok=%v gen=%q, want a real generation", ok, gen)
	}

	// The sessions registry + full erasure cascade (the durable erasure
	// probe the gate fails closed on).
	st, err := state.Open(ctx, config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(ctx) })
	bus := mkDriverTestBus(t, red)
	t.Cleanup(func() { _ = bus.Close(ctx) })
	mem, err := memory.Open(ctx, memory.ConfigSnapshot{
		Driver: "inmem", Strategy: memory.StrategyTruncation, BudgetTokens: 1000,
	}, memory.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = mem.Close(ctx) })
	arts, err := artifacts.Open(ctx, config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = arts.Close(ctx) })
	skStore, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("skills localdb.New: %v", err)
	}
	t.Cleanup(func() { _ = skStore.Close(ctx) })
	sessReg, err := sessions.New(st, config.SessionsConfig{
		IdleTTL: 24 * time.Hour, HardCap: 720 * time.Hour, SweepInterval: time.Hour,
	}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	t.Cleanup(func() { _ = sessReg.CloseRegistry(ctx) })
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: sessReg, State: st, Memory: mem, Artifacts: arts, Skills: skStore,
		Bus: bus, Redactor: red,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	agentCfg := &fakeAdmissionAgentCfg{
		revision: agentcfg.Revision{Payload: agentcfg.ConfigPayload{
			ToolExposure: &agentcfg.ToolExposure{},
		}},
		hasActive: true,
	}
	gate := &renderAdmissionGate{
		sessions:       sessReg,
		agentCfg:       agentCfg,
		sessionOverlay: nil,
		registry:       reg,
	}
	return admissionGateFixture{gate: gate, registry: reg, sessions: sessReg, eraser: eraser, agentCfg: agentCfg}
}

// admReqCtx wraps ctx with the verified identity the gate requires.
func admReqCtx(ctx context.Context, id identity.Identity) context.Context {
	c, err := identity.With(ctx, id)
	if err != nil {
		panic(err)
	}
	return c
}

// admAgentCtx wraps ctx with the verified identity AND the request's
// reach-admitted effective agent stamp the Protocol surface seats after
// normal identity / signed-reach / lifecycle resolution — the exact ctx
// shape the gate consumes (it reads tools.EffectiveAgentConfigFrom on
// every call and fails closed when the stamp is absent).
func admAgentCtx(ctx context.Context, id identity.Identity, agentID string) context.Context {
	return tools.WithEffectiveAgentConfig(admReqCtx(ctx, id), agentID)
}

func admErrIsRefused(err error) bool {
	return errors.Is(err, protocol.ErrRenderAdmissionRefused)
}

// TestResolveSharedKEKSealer_Matrix pins the ONE-sealer resolution:
// the broker's already-constructed sealer wins when present; an
// EXPLICITLY configured env resolves the shared sealer regardless of
// the HA-56 flag (the consumer-independent contract — HA-61 import
// keeps its sealer when render admission is disabled and no broker is
// present); a configured-but-unresolvable env fails loud even when
// disabled; the ENABLED surface resolves exactly one sealer from the
// shared env and fails loud on an empty/unset/invalid env even with no
// broker declared; and a disabled surface with NO env configured
// resolves (nil, nil) — no consumer needs a sealer.
func TestResolveSharedKEKSealer_Matrix(t *testing.T) {
	t.Setenv(admKEKEnv, admDummyKEKHex)
	cfg := &config.Config{Tools: config.ToolsConfig{OAuthTokenKEKEnv: admKEKEnv}}

	// (a) Broker sealer wins — no env needed.
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	builder, err := toolauth.NewProviderBuilder(context.Background(), config.ToolsConfig{
		OAuthTokenKEKEnv: admKEKEnv,
		OAuthCredentialBrokers: []config.ToolOAuthCredentialBrokerConfig{{
			Name: "broker-1", TokenURL: "https://broker.example.com/exchange",
			AllowedDownstreamHosts: []string{"https://mcp.example.com"},
		}},
	}, toolauth.BuildDeps{State: st})
	if err != nil {
		t.Fatalf("NewProviderBuilder: %v", err)
	}
	if builder.AdmissionSealer() == nil {
		t.Fatal("fixture: broker builder must hold a sealer")
	}
	sealer, err := ResolveSharedKEKSealer(cfg, builder)
	if err != nil {
		t.Fatalf("ResolveSharedKEKSealer (broker): %v", err)
	}
	if sealer == nil {
		t.Fatal("ResolveSharedKEKSealer (broker) returned nil")
	}

	// (b) Disabled surface + no broker + explicitly configured VALID env
	//     → the shared sealer resolves (the P1 consumer-independent
	//     regression: HA-61 import keeps its sealer even when render
	//     admission is disabled and no OAuth broker is present).
	t.Setenv(admKEKEnv, admDummyKEKHex)
	sealer, err = ResolveSharedKEKSealer(cfg, nil)
	if err != nil {
		t.Fatalf("ResolveSharedKEKSealer (disabled, configured valid env): %v", err)
	}
	if sealer == nil {
		t.Fatal("ResolveSharedKEKSealer (disabled, configured valid env) returned nil — an explicitly configured shared KEK must resolve for HA-61 import regardless of the HA-56 flag")
	}

	// (c) Disabled surface + no broker + configured but UNSET env → LOUD
	//     failure naming the env: the operator explicitly declared the
	//     key slot, so a broken one is a boot error — never a silent
	//     (nil, nil) that 501s HA-61 import.
	t.Setenv(admKEKEnv, "")
	if _, err := ResolveSharedKEKSealer(cfg, nil); err == nil {
		t.Fatal("ResolveSharedKEKSealer (disabled, configured unset env) must fail loud")
	} else if !strings.Contains(err.Error(), "tools.oauth_token_kek_env") {
		t.Fatalf("disabled-configured-unset sealer error %q does not name tools.oauth_token_kek_env", err)
	}

	// (d) Enabled surface + valid env → exactly one sealer.
	cfg.Tools.MCPAppRenderAdmission.Enabled = true
	t.Setenv(admKEKEnv, admDummyKEKHex)
	sealer, err = ResolveSharedKEKSealer(cfg, nil)
	if err != nil {
		t.Fatalf("ResolveSharedKEKSealer (enabled, valid env): %v", err)
	}
	if sealer == nil {
		t.Fatal("ResolveSharedKEKSealer (enabled, valid env) returned nil")
	}

	// (e) Enabled surface + unset env → LOUD readiness failure naming the
	//     surface.
	t.Setenv(admKEKEnv, "")
	if _, err := ResolveSharedKEKSealer(cfg, nil); err == nil {
		t.Fatal("ResolveSharedKEKSealer (enabled, unset env) must fail loud")
	} else if !strings.Contains(err.Error(), "tools.mcp_app_render_admission.enabled") {
		t.Fatalf("enabled-surface sealer error %q does not name the surface", err)
	}

	// (f) Enabled surface + invalid env → LOUD readiness failure naming
	//     the surface.
	t.Setenv(admKEKEnv, "not-hex!!")
	if _, err := ResolveSharedKEKSealer(cfg, nil); err == nil {
		t.Fatal("ResolveSharedKEKSealer (enabled, invalid env) must fail loud")
	} else if !strings.Contains(err.Error(), "tools.mcp_app_render_admission.enabled") {
		t.Fatalf("enabled-surface invalid-KEK error %q does not name the surface", err)
	}

	// (g) Disabled surface + NO env configured → (nil, nil): no consumer
	//     needs a sealer, and the env is never touched.
	cfg.Tools.MCPAppRenderAdmission.Enabled = false
	cfg.Tools.OAuthTokenKEKEnv = ""
	t.Setenv(admKEKEnv, "")
	sealer, err = ResolveSharedKEKSealer(cfg, nil)
	if err != nil || sealer != nil {
		t.Fatalf("ResolveSharedKEKSealer (disabled, no env) = (%v, %v), want (nil, nil)", sealer, err)
	}

	// (h) Enabled surface + NO env configured → LOUD readiness failure
	//     naming the surface (an enabled surface never falls back to the
	//     disabled surface).
	cfg.Tools.MCPAppRenderAdmission.Enabled = true
	if _, err := ResolveSharedKEKSealer(cfg, nil); err == nil {
		t.Fatal("ResolveSharedKEKSealer (enabled, no env) must fail loud")
	} else if !strings.Contains(err.Error(), "tools.mcp_app_render_admission.enabled") {
		t.Fatalf("enabled-no-env sealer error %q does not name the surface", err)
	}
}

// TestWireRenderAdmission_EnabledGatePins the explicit-opt-in contract:
// the pair is wired ONLY when Enabled is true — a nil sealer keeps the
// surface disabled (nil, nil, nil) — while an ENABLED surface with no
// shared sealer, no registry, no sessions registry, or no agent-config
// reader fails construction LOUD (the readiness failure posture, and
// the P1 "the enabled gate may never skip an authorization check"
// rule), never a silent fallback to the disabled surface.
func TestWireRenderAdmission_EnabledGatePins(t *testing.T) {
	t.Setenv(admKEKEnv, admDummyKEKHex)
	sealer, err := toolauth.NewSealerFromEnv(admKEKEnv)
	if err != nil {
		t.Fatalf("NewSealerFromEnv: %v", err)
	}

	// Disabled (the zero value) → (nil, nil, nil) regardless of sealer
	// availability — sealer availability is NOT feature enablement.
	if a, g, err := WireRenderAdmission(RenderAdmissionAuthorityDeps{}); a != nil || g != nil || err != nil {
		t.Fatalf("WireRenderAdmission(disabled) = (%v, %v, %v), want (nil, nil, nil)", a, g, err)
	}
	if a, g, err := WireRenderAdmission(RenderAdmissionAuthorityDeps{Sealer: sealer}); a != nil || g != nil || err != nil {
		t.Fatalf("WireRenderAdmission(disabled, sealer) = (%v, %v, %v), want (nil, nil, nil) — a broker sealer alone must never wire the surface", a, g, err)
	}

	// Enabled + missing shared sealer → LOUD readiness failure (an
	// enabled surface with an unresolvable tools.oauth_token_kek_env).
	if a, g, err := WireRenderAdmission(RenderAdmissionAuthorityDeps{Enabled: true}); a != nil || g != nil || err == nil {
		t.Fatalf("WireRenderAdmission(enabled, nil sealer) = (%v, %v, %v), want a loud error", a, g, err)
	} else if !strings.Contains(err.Error(), "tools.oauth_token_kek_env") {
		t.Fatalf("enabled-nil-sealer error %q does not name tools.oauth_token_kek_env", err)
	}

	// Enabled + sealer but no registry → loud error (the surface
	// rejects a half-wired pair).
	if a, g, err := WireRenderAdmission(RenderAdmissionAuthorityDeps{Enabled: true, Sealer: sealer}); a != nil || g != nil || err == nil {
		t.Fatalf("WireRenderAdmission(enabled, sealer, no registry) = (%v, %v, %v), want loud error", a, g, err)
	}

	// Enabled + sealer + registry but no sessions registry → loud error:
	// the enabled gate may never skip the durable erasure check.
	reg := mcp.NewRegistry()
	if a, g, err := WireRenderAdmission(RenderAdmissionAuthorityDeps{
		Enabled: true, Sealer: sealer, Registry: reg,
	}); a != nil || g != nil || err == nil {
		t.Fatalf("WireRenderAdmission(enabled, sealer, registry, no sessions) = (%v, %v, %v), want loud error (the erasure check may never be skipped)", a, g, err)
	} else if !strings.Contains(err.Error(), "sessions registry") {
		t.Fatalf("enabled-no-sessions error %q does not name the sessions registry", err)
	}

	// Enabled + sealer + registry + sessions but no agent-config reader →
	// loud error: the enabled gate may never skip the
	// retirement/current-exposure checks.
	f := buildAdmissionGateFixture(t)
	if a, g, err := WireRenderAdmission(RenderAdmissionAuthorityDeps{
		Enabled: true, Sealer: sealer, Registry: reg, Sessions: f.sessions,
	}); a != nil || g != nil || err == nil {
		t.Fatalf("WireRenderAdmission(enabled, sealer, registry, sessions, no agentcfg) = (%v, %v, %v), want loud error (the retirement/current-exposure checks may never be skipped)", a, g, err)
	} else if !strings.Contains(err.Error(), "agent-config reader") {
		t.Fatalf("enabled-no-agentcfg error %q does not name the agent-config reader", err)
	}

	// Enabled + full deps → the pair is wired together.
	if a, g, err := WireRenderAdmission(RenderAdmissionAuthorityDeps{
		Enabled: true, Sealer: sealer, Registry: reg,
		Sessions: f.sessions, AgentConfig: f.agentCfg,
	}); err != nil {
		t.Fatalf("WireRenderAdmission(enabled, full deps): %v", err)
	} else if a == nil || g == nil {
		t.Fatalf("WireRenderAdmission(enabled, full deps) = (%v, %v), want both wired", a, g)
	}
}

// TestRenderAdmissionGate_RefusalMatrix pins the fail-closed refusals
// against the REAL collaborators.
func TestRenderAdmissionGate_RefusalMatrix(t *testing.T) {
	f := buildAdmissionGateFixture(t)
	ctx := context.Background()
	uiResource := "ui://" + renderAdmServer + "/app"
	agentCtx := admAgentCtx(ctx, renderAdmID, renderAdmAgent)

	// (a) Missing identity → typed refusal before any check.
	if _, err := f.gate.AuthorizeRender(ctx, renderAdmServer, uiResource); !admErrIsRefused(err) {
		t.Fatalf("no-identity AuthorizeRender err = %v, want ErrRenderAdmissionRefused", err)
	}

	// (b) Unknown server (no generation) → typed refusal.
	if _, err := f.gate.AuthorizeRender(agentCtx, "unknown-server", uiResource); !admErrIsRefused(err) {
		t.Fatalf("unknown-server AuthorizeRender err = %v, want ErrRenderAdmissionRefused", err)
	}

	// (c) Non-ui resource → typed refusal.
	if _, err := f.gate.AuthorizeRender(agentCtx, renderAdmServer, "https://example.com/not-app"); !admErrIsRefused(err) {
		t.Fatalf("non-ui AuthorizeRender err = %v, want ErrRenderAdmissionRefused", err)
	}

	// (d) Erased session → typed refusal (the durable erasure probe).
	if _, err := f.sessions.Open(ctx, renderAdmID.SessionID, renderAdmID); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := f.eraser.Erase(ctx, renderAdmID); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if _, err := f.gate.AuthorizeRender(agentCtx, renderAdmServer, uiResource); !admErrIsRefused(err) {
		t.Fatalf("erased-session AuthorizeRender err = %v, want ErrRenderAdmissionRefused", err)
	}

	// (e) Retired agent → typed refusal.
	f2 := buildAdmissionGateFixture(t)
	f2.agentCfg.retired = true
	f2.gate.agentCfg = f2.agentCfg
	if _, err := f2.gate.AuthorizeRender(admAgentCtx(ctx, renderAdmID, renderAdmAgent), renderAdmServer, uiResource); !admErrIsRefused(err) {
		t.Fatalf("retired-agent AuthorizeRender err = %v, want ErrRenderAdmissionRefused", err)
	}

	// (f) Paused server → typed refusal.
	f3 := buildAdmissionGateFixture(t)
	f3.agentCfg.revision.Payload.ToolExposure = &agentcfg.ToolExposure{PausedServers: []string{renderAdmServer}}
	f3.gate.agentCfg = f3.agentCfg
	if _, err := f3.gate.AuthorizeRender(admAgentCtx(ctx, renderAdmID, renderAdmAgent), renderAdmServer, uiResource); !admErrIsRefused(err) {
		t.Fatalf("paused-server AuthorizeRender err = %v, want ErrRenderAdmissionRefused", err)
	}

	// (g) Happy path: verified identity + reach-admitted effective
	//     agent + current exposure + real generation + ui:// resource →
	//     the exact current generation is bound (never an empty one).
	f4 := buildAdmissionGateFixture(t)
	gen, err := f4.gate.AuthorizeRender(admAgentCtx(ctx, renderAdmID, renderAdmAgent), renderAdmServer, uiResource)
	if err != nil {
		t.Fatalf("happy-path AuthorizeRender: %v", err)
	}
	if gen == "" {
		t.Fatal("happy-path AuthorizeRender returned an empty generation (admissions never bind an empty generation)")
	}
	if current, _ := f4.registry.CurrentGeneration(renderAdmServer); gen != current {
		t.Fatalf("bound generation %q != registry current %q", gen, current)
	}
}

// TestRenderAdmissionGate_EffectiveAgent_FailClosedAndUsed pins
// correction #3: the gate reads the request's reach-admitted effective
// agent from ctx on EVERY call. An absent stamp fails closed (no
// invented boot/default agent), and the stamped agent is the agent the
// retirement + current-exposure + overlay reads are performed under.
func TestRenderAdmissionGate_EffectiveAgent_FailClosedAndUsed(t *testing.T) {
	f := buildAdmissionGateFixture(t)
	ctx := context.Background()
	uiResource := "ui://" + renderAdmServer + "/app"

	// (a) Missing provenance: verified identity WITHOUT the effective-
	//     agent stamp → typed refusal naming the missing stamp. The
	//     gate must never fall back to a boot/default agent.
	if _, err := f.gate.AuthorizeRender(admReqCtx(ctx, renderAdmID), renderAdmServer, uiResource); !admErrIsRefused(err) {
		t.Fatalf("missing-provenance AuthorizeRender err = %v, want ErrRenderAdmissionRefused", err)
	} else if !strings.Contains(err.Error(), "effective agent missing") {
		t.Fatalf("missing-provenance error %q does not name the missing effective agent", err)
	}

	// (b) Named agent: the stamped agent is the agent the gate reads
	//     current retirement/exposure for — never a boot default.
	f2 := buildAdmissionGateFixture(t)
	if _, err := f2.gate.AuthorizeRender(admAgentCtx(ctx, renderAdmID, renderAdmAgent), renderAdmServer, uiResource); err != nil {
		t.Fatalf("named-agent AuthorizeRender: %v", err)
	}
	if f2.agentCfg.retirementQueriedFor != renderAdmAgent {
		t.Fatalf("retirement gate queried agent %q, want the stamped effective agent %q", f2.agentCfg.retirementQueriedFor, renderAdmAgent)
	}
	if f2.agentCfg.activeQueriedAgent != renderAdmAgent {
		t.Fatalf("active-exposure read queried agent %q, want the stamped effective agent %q", f2.agentCfg.activeQueriedAgent, renderAdmAgent)
	}

	// (c) Wrong-agent exposure: a DIFFERENT stamped agent whose
	//     current exposure pauses the server is refused — the check is
	//     against the stamped agent's exposure, not a boot default's.
	f3 := buildAdmissionGateFixture(t)
	f3.agentCfg.revision.Payload.ToolExposure = &agentcfg.ToolExposure{PausedServers: []string{renderAdmServer}}
	f3.gate.agentCfg = f3.agentCfg
	if _, err := f3.gate.AuthorizeRender(admAgentCtx(ctx, renderAdmID, "other-agent"), renderAdmServer, uiResource); !admErrIsRefused(err) {
		t.Fatalf("other-agent-paused AuthorizeRender err = %v, want ErrRenderAdmissionRefused (the stamped agent's exposure is what is checked)", err)
	}
	if f3.agentCfg.activeQueriedAgent != "other-agent" {
		t.Fatalf("other-agent exposure read queried agent %q, want the stamped %q", f3.agentCfg.activeQueriedAgent, "other-agent")
	}

	// (d) Retired stamped agent → typed refusal naming the stamped id.
	f4 := buildAdmissionGateFixture(t)
	f4.agentCfg.retired = true
	f4.gate.agentCfg = f4.agentCfg
	if _, err := f4.gate.AuthorizeRender(admAgentCtx(ctx, renderAdmID, "retired-agent"), renderAdmServer, uiResource); !admErrIsRefused(err) {
		t.Fatalf("retired-stamped-agent AuthorizeRender err = %v, want ErrRenderAdmissionRefused", err)
	} else if !strings.Contains(err.Error(), "retired-agent") {
		t.Fatalf("retired-stamped-agent error %q does not name the stamped agent", err)
	}
}
