package devstack

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// devstack_mcp_attacher.go — the production runtime MCP-attach concrete
// mirrored from cmd/harbor's devMCPConnectionAttacher
// (cmd/harbor/cmd_dev_mcp_attacher.go). It drives the
// real MCP attach lifecycle (dial → initialize → discover → register) for the
// admin-driven runtime add of a NEW MCP connection
// (`agent_config.add_mcp_connection`) against the LIVE catalog + registry +
// bus, reusing the boot-time mcpdrv.Attach helper. It satisfies the
// driver-agnostic agentcfgprotocol.ConnectionAttacher interface.
//
// Exported so an integration test can construct the SAME real attach concrete
// the wave-end / page surface drives (§17.4 — real drivers on the seam). The
// test kit is the §13-sanctioned home for the concrete MCP-driver import in a
// test-scoped harness.

// MCPConnectionAttacher is the test-kit concrete that drives the real MCP
// attach lifecycle for a runtime add. See the file doc.
type MCPConnectionAttacher struct {
	catalog         tools.ToolCatalog
	registry        *mcpdrv.Registry
	bus             events.EventBus
	logger          *slog.Logger
	defaultIdentity identity.Identity
	// oauthProviders is the declared OAuth-provider registry a runtime-added
	// connection's `oauth_provider` binding resolves against (mcpdrv.Attach
	// does the resolution + fail-loud). Twin of cmd/harbor's attacher. Set
	// once at construction; nil is valid when no provider is declared.
	oauthProviders map[string]toolauth.OAuthProvider

	mu      sync.Mutex
	closers []func(context.Context) error
}

// NewMCPConnectionAttacher builds the test-kit attacher. catalog, registry,
// and bus are mandatory (mcpdrv.Attach validates them too). A nil logger
// silences the per-server attach log line. oauthProviders may be nil when no
// OAuth provider is declared; a runtime add that binds an `oauth_provider`
// name resolves against it (unknown name → loud attach failure).
func NewMCPConnectionAttacher(catalog tools.ToolCatalog, registry *mcpdrv.Registry, bus events.EventBus, logger *slog.Logger, defaultIdentity identity.Identity, oauthProviders map[string]toolauth.OAuthProvider) *MCPConnectionAttacher {
	return &MCPConnectionAttacher{
		catalog:         catalog,
		registry:        registry,
		bus:             bus,
		logger:          logger,
		defaultIdentity: defaultIdentity,
		oauthProviders:  oauthProviders,
	}
}

// Attach drives the real MCP attach for one runtime-added connection. The
// operator-supplied auth Headers flow to the live transport ONLY (the
// agent-config service never persists them). An auth-required condition is
// surfaced by wrapping agentcfgprotocol.ErrAuthRequired so the service parks
// on the unified pause/resume primitive.
func (a *MCPConnectionAttacher) Attach(ctx context.Context, req agentcfgprotocol.AttachRequest) error {
	ms := config.MCPServerConfig{
		Name:            req.Name,
		TransportMode:   transportModeForAdd(req.Transport),
		URL:             req.URL,
		Command:         append([]string(nil), req.Command...),
		Headers:         req.Headers, // SECRET — used for the live transport, never persisted
		OAuthProvider:   req.OAuthProvider,
		MetaAnnotations: req.MetaAnnotations,
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	var local []func(context.Context) error
	err := mcpdrv.Attach(ctx, ms, mcpdrv.AttachDeps{
		Catalog:         a.catalog,
		Registry:        a.registry,
		Bus:             a.bus,
		Logger:          a.logger,
		DefaultIdentity: a.defaultIdentity,
		Closers:         &local,
		OAuthProviders:  a.oauthProviders,
	})
	a.closers = append(a.closers, local...)
	if err != nil {
		if looksLikeAuthRequired(err) {
			return fmt.Errorf("%w: %w", agentcfgprotocol.ErrAuthRequired, err)
		}
		return err
	}
	return nil
}

// Close drains every runtime-added server's transport in reverse order.
func (a *MCPConnectionAttacher) Close(ctx context.Context) error {
	a.mu.Lock()
	closers := a.closers
	a.closers = nil
	a.mu.Unlock()
	var firstErr error
	for i := len(closers) - 1; i >= 0; i-- {
		if err := closers[i](ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// transportModeForAdd maps the control-plane transport onto the MCP driver's
// transport-mode string (mirror of cmd/harbor).
func transportModeForAdd(t agentcfg.MCPTransport) string {
	switch t {
	case agentcfg.MCPTransportStdio:
		return string(mcpdrv.TransportStdio)
	default:
		return string(mcpdrv.TransportAuto)
	}
}

// looksLikeAuthRequired is the best-effort auth-required heuristic (mirror of
// cmd/harbor — replaced by errors.Is when the driver gains a typed sentinel).
func looksLikeAuthRequired(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{"401", "unauthorized", "www-authenticate", "oauth", "authorization required"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// resolveDevIdentity resolves the dev identity triple from AssembleOpts,
// substituting the package defaults — mirrors the dev-token block.
func resolveDevIdentity(opts AssembleOpts) identity.Identity {
	tenant := opts.Identity.Tenant
	if tenant == "" {
		tenant = DefaultDevTenant
	}
	user := opts.Identity.User
	if user == "" {
		user = DefaultDevUser
	}
	session := opts.Identity.Session
	if session == "" {
		session = DefaultDevSession
	}
	return identity.Identity{TenantID: tenant, UserID: user, SessionID: session}
}
