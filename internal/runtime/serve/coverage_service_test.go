package serve

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

func TestBoot_PostgresProjectionServicesComposeAndClose(t *testing.T) {
	dsn := os.Getenv("HARBOR_PG_DSN")
	if dsn == "" {
		t.Skip("HARBOR_PG_DSN not set; CI and the service-backed coverage lane provide PostgreSQL")
	}
	cfg, err := loadTestCfg(t, writeTestCfg(t))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Sessions.Turns = config.TurnsConfig{Driver: "postgres", DSN: dsn, Retention: 32}
	cfg.Observability.Rollups = config.RollupsConfig{Driver: "postgres", DSN: dsn}
	cfg.Sessions.RetainedContextTurns = 2

	opts := baseOptions(t)
	opts.Config = cfg
	h, err := Boot(context.Background(), opts)
	if err != nil {
		t.Fatalf("Boot with PostgreSQL projections: %v", err)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	h.Close(closeCtx)
}

func TestBoot_CoordinatorBoundGrantRequiresCredentialAuthority(t *testing.T) {
	opts := baseOptions(t)
	opts.ExternalGrant = llm.ExternalGrantConfig{
		Mode: llm.ExternalGrantRequired, RouteMode: llm.ExternalGrantRouteCoordinatorBound,
	}
	h, err := Boot(context.Background(), opts)
	if h != nil || err == nil || !errors.Is(err, llm.ErrInvalidConfig) && !containsAll(err.Error(), "coordinator_bound", "credential") {
		t.Fatalf("handle=%v err=%v, want missing coordinator credential refusal", h, err)
	}
}

func TestMCPPreparedPublication_ProofFailureRollsBackAndSuccessIsOwnerScoped(t *testing.T) {
	fixture := reattachFixtureServer(t)
	cat := tools.NewCatalog()
	registry := mcpdrv.NewRegistry()
	bus := mkDriverTestBus(t, auditpatterns.New())
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	attacher := NewMCPConnectionAttacher(cat, registry, bus, logger,
		resolveMCPAttachIdentity(identity.Identity{}), nil, nil, nil)
	t.Cleanup(func() { _ = attacher.Close(context.Background()) })
	owner := toolauth.Owner{Tenant: "tenant-a", Agent: "agent-a"}
	request := func(name string) agentcfgprotocol.AttachRequest {
		return agentcfgprotocol.AttachRequest{
			Identity: identity.Identity{TenantID: owner.Tenant, UserID: "user-a", SessionID: "session-a"},
			AgentID:  owner.Agent, Owner: owner, Name: name, Transport: agentcfg.MCPTransportHTTP, URL: fixture.URL,
			RequestTimeoutMS: 5_000, ConnectTimeoutMS: 5_000,
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	identityCtx, err := identity.With(ctx, request("scope").Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	prepared, err := attacher.PrepareConnection(ctx, request("proof-fails"))
	if err != nil {
		t.Fatalf("PrepareConnection(proof failure): %v", err)
	}
	authorityPrepared, ok := prepared.(agentcfgprotocol.AuthorityBoundPreparedConnection)
	if !ok {
		t.Fatal("prepared connection does not implement authority-bound publication")
	}
	wantProofErr := errors.New("revision compare-and-swap failed")
	if err := authorityPrepared.ActivateIf(ctx, func(context.Context) error { return wantProofErr }); !errors.Is(err, wantProofErr) {
		t.Fatalf("ActivateIf proof failure = %v, want %v", err, wantProofErr)
	}
	if _, ok := registry.OwnerOf(registry.PhysicalSourceForOwner("proof-fails", owner)); ok {
		t.Fatal("proof failure published a registry source")
	}
	if err := prepared.Close(ctx); err != nil {
		t.Fatalf("Close after proof failure: %v", err)
	}

	prepared, err = attacher.PrepareConnection(ctx, request("proof-passes"))
	if err != nil {
		t.Fatalf("PrepareConnection(success): %v", err)
	}
	authorityPrepared, ok = prepared.(agentcfgprotocol.AuthorityBoundPreparedConnection)
	if !ok {
		t.Fatal("prepared connection does not implement authority-bound publication")
	}
	if err := authorityPrepared.ActivateIf(ctx, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("ActivateIf success: %v", err)
	}
	physical := registry.PhysicalSourceForOwner("proof-passes", owner)
	if got, ok := registry.OwnerOf(physical); !ok || got != owner {
		t.Fatalf("published owner = %+v, %v; want %+v", got, ok, owner)
	}
	previous, err := attacher.SetOAuthDiscoveryOrigins(identityCtx, owner.Tenant, owner.Agent, "proof-passes", []string{"https://auth.example"})
	if err != nil || len(previous) != 0 {
		t.Fatalf("SetOAuthDiscoveryOrigins previous=%v err=%v", previous, err)
	}

	detacher := NewMCPConnectionDetacher(cat, registry, logger)
	if got, ok := detacher.OwnerOfSource(tools.ToolSourceID(physical)); !ok || got != owner {
		t.Fatalf("OwnerOfSource = %+v, %v; want %+v", got, ok, owner)
	}
	if got, ok := detacher.LogicalNameOfSource(tools.ToolSourceID(physical)); !ok || got != "proof-passes" {
		t.Fatalf("LogicalNameOfSource = %q, %v", got, ok)
	}
	_, fingerprint, ok := registry.RegistrationIdentityForOwner("proof-passes", owner)
	if !ok || fingerprint == "" {
		t.Fatal("published connection has no descriptor fingerprint")
	}
	fence, err := attacher.BeginExactConnectionTeardown(owner.Tenant, owner.Agent, "proof-passes", fingerprint)
	if err != nil {
		t.Fatalf("BeginExactConnectionTeardown: %v", err)
	}
	if err := fence.Cancel(ctx); err != nil {
		t.Fatalf("cancel exact teardown fence: %v", err)
	}
	if err := attacher.DetachExactConnection(ctx, owner.Tenant, owner.Agent, "proof-passes", fingerprint); err != nil {
		t.Fatalf("DetachExactConnection: %v", err)
	}
}

// TestMCPPublicationHelpers_RejectIncompleteAuthority pins the small but
// security-critical wrapper checks around live discovery updates and exact
// teardown. An incomplete owner or fingerprint must never widen into a
// process-global mutation, while an absent exact target remains idempotent.
func TestMCPPublicationHelpers_RejectIncompleteAuthority(t *testing.T) {
	ctx := context.Background()
	owner := toolauth.Owner{Tenant: "tenant-a", Agent: "agent-a", User: "user-a"}

	noRegistry := &MCPConnectionAttacher{}
	if _, err := noRegistry.SetOAuthDiscoveryOrigins(ctx, owner.Tenant, owner.Agent, "missing", nil); err == nil {
		t.Fatal("discovery update without registry succeeded")
	}
	registry := mcpdrv.NewRegistry()
	attacher := &MCPConnectionAttacher{registry: registry, catalog: tools.NewCatalog()}
	if _, err := attacher.PrepareConnection(ctx, agentcfgprotocol.AttachRequest{
		Identity: identity.Identity{TenantID: owner.Tenant, UserID: owner.User, SessionID: "session-a"},
		AgentID:  owner.Agent, Owner: toolauth.Owner{Tenant: owner.Tenant, Agent: "other-agent"}, Name: "owner-mismatch",
	}); !errors.Is(err, ErrRuntimeAddOwnerMissing) {
		t.Fatalf("mismatched attach owner = %v, want ErrRuntimeAddOwnerMissing", err)
	}
	if _, err := attacher.SetOAuthDiscoveryOrigins(ctx, "", owner.Agent, "missing", nil); !errors.Is(err, ErrRuntimeAddOwnerMissing) {
		t.Fatalf("ownerless discovery update = %v, want ErrRuntimeAddOwnerMissing", err)
	}
	if err := attacher.DetachConnection(ctx, owner.Tenant, "", "missing"); !errors.Is(err, ErrRuntimeAddOwnerMissing) {
		t.Fatalf("ownerless compensating detach = %v, want ErrRuntimeAddOwnerMissing", err)
	}
	if err := attacher.DetachConnection(ctx, owner.Tenant, owner.Agent, "missing"); err != nil {
		t.Fatalf("absent compensating detach must be idempotent: %v", err)
	}
	if err := attacher.DetachExactConnection(ctx, owner.Tenant, owner.Agent, "missing", ""); !errors.Is(err, ErrRuntimeAddOwnerMissing) {
		t.Fatalf("fingerprint-less exact detach = %v, want ErrRuntimeAddOwnerMissing", err)
	}
	if err := attacher.DetachExactConnectionForOwner(ctx, toolauth.Owner{Tenant: owner.Tenant, Agent: owner.Agent}, "missing", "fingerprint"); !errors.Is(err, ErrRuntimeAddOwnerMissing) {
		t.Fatalf("userless exact detach = %v, want ErrRuntimeAddOwnerMissing", err)
	}
	if _, err := noRegistry.BeginExactConnectionTeardown(owner.Tenant, owner.Agent, "missing", "fingerprint"); err == nil {
		t.Fatal("exact teardown fence without registry succeeded")
	}
	if _, err := noRegistry.BeginExactConnectionTeardownForOwner(owner, "missing", "fingerprint"); err == nil {
		t.Fatal("user-scoped teardown fence without registry succeeded")
	}
	fence, err := attacher.BeginExactConnectionTeardownForOwner(owner, "missing", "fingerprint")
	if err != nil {
		t.Fatalf("reserve user-scoped exact teardown: %v", err)
	}
	if err := fence.Cancel(ctx); err != nil {
		t.Fatalf("cancel user-scoped exact teardown: %v", err)
	}

	if _, err := attacher.SetOAuthDiscoveryOrigins(mustIdentityContext(t, ctx, identity.Identity{TenantID: owner.Tenant, UserID: owner.User, SessionID: "session-a"}), owner.Tenant, owner.Agent, "missing", nil); !errors.Is(err, agentcfgprotocol.ErrDiscoveryTargetNotLive) {
		t.Fatalf("discovery update for absent source = %v, want ErrDiscoveryTargetNotLive", err)
	}
	if err := attacher.DetachExactConnection(ctx, owner.Tenant, owner.Agent, "missing", "fingerprint"); err != nil {
		t.Fatalf("absent exact detach must be idempotent: %v", err)
	}
	if err := attacher.DetachExactConnectionForOwner(ctx, owner, "missing", "fingerprint"); err != nil {
		t.Fatalf("absent user exact detach must be idempotent: %v", err)
	}

	var nilDetacher *MCPConnectionDetacher
	if _, ok := nilDetacher.OwnerOfSource("missing"); ok {
		t.Fatal("nil detacher returned an owner")
	}
	if _, ok := nilDetacher.LogicalNameOfSource("missing"); ok {
		t.Fatal("nil detacher returned a logical name")
	}
	detacher := NewMCPConnectionDetacher(nil, nil, nil)
	if got := detacher.AttachedSources(ctx, owner); got != nil {
		t.Fatalf("nil-registry attached sources = %v, want nil", got)
	}
	if prev, err := detacher.SetOAuthDiscoveryOrigins(ctx, owner, "missing", nil); err != nil || prev != nil {
		t.Fatalf("nil-registry discovery update = (%v, %v), want (nil, nil)", prev, err)
	}
	if err := detacher.Detach(ctx, "missing", owner); err != nil {
		t.Fatalf("nil-registry detach = %v, want idempotent nil", err)
	}
	detacher = NewMCPConnectionDetacher(tools.NewCatalog(), registry, nil)
	identityCtx := mustIdentityContext(t, ctx, identity.Identity{TenantID: owner.Tenant, UserID: owner.User, SessionID: "session-a"})
	if _, err := detacher.SetOAuthDiscoveryOrigins(identityCtx, owner, "missing", nil); !errors.Is(err, mcpdrv.ErrServerNotFound) {
		t.Fatalf("detacher discovery update for absent source = %v, want driver not-found", err)
	}
	if err := detachSourceExpected(ctx, tools.NewCatalog(), nil, "missing", owner, "fingerprint", nil, "test"); err == nil {
		t.Fatal("exact teardown without registry succeeded")
	}
	if err := detachSourceExpected(ctx, nil, registry, "missing", owner, "fingerprint", nil, "test"); err == nil {
		t.Fatal("exact teardown without catalog deregistration succeeded")
	}
}

func mustIdentityContext(t *testing.T, ctx context.Context, id identity.Identity) context.Context {
	t.Helper()
	identityCtx, err := identity.With(ctx, id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return identityCtx
}

func containsAll(s string, values ...string) bool {
	for _, value := range values {
		if !strings.Contains(s, value) {
			return false
		}
	}
	return true
}

func TestOAuthProviderInstaller_RealBuilderOwnerAndCollisionBoundaries(t *testing.T) {
	const authEnv = "HARBOR_SERVE_COVERAGE_BROKER_AUTH"
	const kekEnv = "HARBOR_SERVE_COVERAGE_BROKER_KEK"
	t.Setenv(authEnv, "dummy-local-broker-token")
	t.Setenv(kekEnv, "0303030303030303030303030303030303030303030303030303030303030303")
	redactor := auditpatterns.New()
	bus := mkDriverTestBus(t, redactor)
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	builder, err := toolauth.NewProviderBuilder(context.Background(), config.ToolsConfig{
		OAuthTokenKEKEnv: kekEnv,
		OAuthCredentialBrokers: []config.ToolOAuthCredentialBrokerConfig{{
			Name: "broker", TokenURL: "https://token.example", CredentialURL: "https://credential.example",
			AuthTokenEnv: authEnv, ScopeCeiling: []string{"read"}, AllowedDownstreamHosts: []string{"api.example"},
		}},
	}, toolauth.BuildDeps{State: store, Bus: bus, Redactor: redactor, Coordinator: pauseresume.New(pauseresume.WithBus(bus))})
	if err != nil {
		t.Fatalf("NewProviderBuilder: %v", err)
	}
	providerSet := toolauth.NewProviderSet(nil)
	installer := NewOAuthProviderInstaller(builder, providerSet, false, nil)
	desc := agentcfg.OAuthProviderDescriptor{Name: "provider", CredentialBroker: "broker", Scopes: []string{"read"}}
	if err := installer.InstallProvider(context.Background(), "tenant-a", "agent-a", desc); err != nil {
		t.Fatalf("InstallProvider: %v", err)
	}
	if got := installer.InstalledFor(context.Background(), toolauth.Owner{Tenant: "tenant-a", Agent: "agent-a"}); len(got) != 1 || got[0] != "provider" {
		t.Fatalf("InstalledFor = %v", got)
	}
	if err := installer.InstallProvider(context.Background(), "tenant-b", "agent-b", desc); !errors.Is(err, agentcfgprotocol.ErrInvalidProvider) {
		t.Fatalf("cross-owner collision = %v", err)
	}
	prepared, err := installer.PrepareProvider(context.Background(), "tenant-a", "agent-a",
		agentcfg.OAuthProviderDescriptor{Name: "prepared", CredentialBroker: "broker", Scopes: []string{"read"}})
	if err != nil {
		t.Fatalf("PrepareProvider: %v", err)
	}
	if prepared.Binding() == nil {
		t.Fatal("prepared provider binding is nil")
	}
	if err := prepared.Close(context.Background()); err != nil {
		t.Fatalf("prepared Close: %v", err)
	}
	if _, err := installer.PrepareSignedCapabilityProvider(context.Background(), "missing",
		toolauth.SignedCapabilityExchangeBinding{TenantID: "tenant-a", AgentID: "agent-a", ProviderName: "signed", Audience: "audience-a", Resource: "https://api.example"}, nil); !errors.Is(err, agentcfgprotocol.ErrProviderBrokerUnknown) {
		t.Fatalf("signed unknown broker = %v", err)
	}
	if err := installer.UninstallProvider(context.Background(), "tenant-a", "agent-a", "provider"); err != nil {
		t.Fatalf("UninstallProvider: %v", err)
	}
}
