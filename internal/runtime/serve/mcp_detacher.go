package serve

import (
	"context"
	"errors"
	"log/slog"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// cmd_dev_mcp_detacher.go — the production concrete that drives the DETACH leg
// of run-start reconciliation (the inverse of cmd_dev_mcp_attacher.go). It is
// the §4.4 boundary glue: it imports the concrete MCP driver (allowed in
// cmd/harbor) and satisfies the driver-agnostic
// projection.ConnectionDetacher interface the run-start reconcile depends on.
//
// Detach deregisters a no-longer-declared MCP server's tools from the live
// catalog and the MCP registry and closes its transport gracefully — the
// physical teardown a `agent_config.remove_mcp_connection` revision (or a
// rollback past an add) implies. It runs at a run-start reconcile, never in
// the middle of the run that triggered it; teardown is process-global, so a
// DIFFERENT session's in-flight run calling the detached server fails loudly
// (typed not-found / closed transport — see projection.ReconcileConnections).
//
// The harbortest/devstack helper carries a mirror of this concrete so
// integration tests exercise the same real detach path.

// MCPConnectionDetacher implements projection.ConnectionDetacher against
// the LIVE catalog + registry. Concurrent reuse: the collaborators are set
// once at construction; the type holds no mutable state (the registry +
// catalog own their own synchronisation).
type MCPConnectionDetacher struct {
	catalog  tools.ToolCatalog
	registry *mcpdrv.Registry
	logger   *slog.Logger
}

// Compile-time assertions that the production concrete satisfies BOTH reconcile
// seams the run loop binds it to. The run loop binds the allowance reconciler
// through an UNCHECKED type assertion
// (`d.connectionDetacher.(projection.DiscoveryOriginReconciler)`), which
// degrades silently to a no-op on a signature drift — a rollback past a grant
// would quietly stop revoking the origin live. These pin the shapes at compile
// time so the drift is a build failure instead.
var (
	_ projection.ConnectionDetacher        = (*MCPConnectionDetacher)(nil)
	_ projection.DiscoveryOriginReconciler = (*MCPConnectionDetacher)(nil)
)

// NewMCPConnectionDetacher builds the production detacher. catalog and
// registry are mandatory (the reconcile is a no-op without them).
func NewMCPConnectionDetacher(catalog tools.ToolCatalog, registry *mcpdrv.Registry, logger *slog.Logger) *MCPConnectionDetacher {
	return &MCPConnectionDetacher{catalog: catalog, registry: registry, logger: logger}
}

// BootDeclaredMCPServerNames returns the names of every MCP server declared in
// the boot yaml (`tools.mcp_servers[].name`) — the set the remove verb rejects
// and the run-start reconcile never detaches (boot-declared servers are not
// revisioned state).
func BootDeclaredMCPServerNames(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	out := make([]string, 0, len(cfg.Tools.MCPServers))
	for _, ms := range cfg.Tools.MCPServers {
		if ms.Name != "" {
			out = append(out, ms.Name)
		}
	}
	return out
}

// BootDeclaredOAuthProviderNames returns the names of every OAuth provider
// declared in the boot yaml (`tools.oauth_providers[].name`) — the set the
// set/remove_oauth_provider verbs reject (boot wins; edit yaml + restart).
func BootDeclaredOAuthProviderNames(cfg *config.Config) []string {
	if cfg == nil {
		return nil
	}
	out := make([]string, 0, len(cfg.Tools.OAuthProviders))
	for _, p := range cfg.Tools.OAuthProviders {
		if p.Name != "" {
			out = append(out, p.Name)
		}
	}
	return out
}

// BootDeclaredMCPServerSet returns the boot-declared MCP server names as a set
// for the run-start reconcile's O(1) skip check.
func BootDeclaredMCPServerSet(cfg *config.Config) map[string]struct{} {
	names := BootDeclaredMCPServerNames(cfg)
	if len(names) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return set
}

// AttachedSources returns the reconciling OWNER's runtime-added source ids —
// the owner-scoped reconcile VIEW (Registry.RuntimeAddedSources), NOT the
// process-global Registry.SourceIDs enumeration. Boot-declared servers stay
// untagged and every OTHER owner's runtime-adds carry a different owner tag, so
// neither appears in the view: one owner's run-start reconcile can never detach
// a boot server or another owner's connection. The registry + catalog stay
// process-global and deployment-shared (resolution + dispatch by bare name);
// only the reconcile VIEW is owner-scoped.
func (d *MCPConnectionDetacher) AttachedSources(_ context.Context, owner toolauth.Owner) []string {
	if d.registry == nil {
		return nil
	}
	return d.registry.RuntimeAddedSources(owner)
}

// SetOAuthDiscoveryOrigins re-applies a runtime-added connection's
// OAuth-discovery cross-origin allow-list to the live registry — the run-start
// allowance-reconcile leg's live effect (the rollback path for the
// discovery-allowance write). It delegates to the process-global bare-name
// registry (identity-mandatory for authorization) and passes the reconciling
// owner through, so the replacement lands only on a registration carrying that
// owner tag — the same owner the sources came from (AttachedSources). Satisfies
// projection.DiscoveryOriginReconciler alongside AttachedSources.
func (d *MCPConnectionDetacher) SetOAuthDiscoveryOrigins(ctx context.Context, owner toolauth.Owner, name string, origins []string) (prev []string, err error) {
	if d.registry == nil {
		return nil, nil
	}
	return d.registry.SetOAuthDiscoveryOrigins(ctx, name, owner, origins)
}

// Detach deregisters the source's tools from the catalog (when the catalog
// supports source deregistration — the optional CatalogSourceDeregisterer
// companion) and from the MCP registry, closing its transport gracefully. An
// already-gone source is a no-op (ErrServerNotFound is swallowed — idempotent).
//
// owner is the reconciling (tenant, agent) owner — the SAME owner whose view
// produced the source (AttachedSources). It is passed through to the registry's
// owner-scoped deregister, so the removal lands only on a registration carrying
// that tag: the reconcile leg cannot remove a boot-declared server or another
// owner's connection even if a name reaches it, and the guard sits at the
// registry's own choke point rather than in this caller alone.
func (d *MCPConnectionDetacher) Detach(ctx context.Context, source string, owner toolauth.Owner) error {
	return detachSource(ctx, d.catalog, d.registry, source, owner, d.logger,
		"mcp: detached runtime-added server at run-start reconcile")
}

// detachSource is the ONE teardown implementation both detach callers share:
// the run-start reconcile's detach pass ([MCPConnectionDetacher.Detach]) and
// the add door's compensating detach
// ([MCPConnectionAttacher.DetachConnection], which runs when an attach
// succeeded and its revision write then failed). A second copy would drift on
// the next change to either half of the teardown (CLAUDE.md §13); `reason` is
// the only thing that legitimately differs between them, so it is the only
// thing parameterised.
//
// It deregisters the source's tools from the catalog (when the catalog
// supports source deregistration — the optional CatalogSourceDeregisterer
// companion) and from the MCP registry, closing its transport gracefully. An
// already-gone source is a no-op (ErrServerNotFound is swallowed —
// idempotent), which is what makes it safe to call on a compensation path that
// cannot know how far the attach got.
func detachSource(ctx context.Context, catalog tools.ToolCatalog, registry *mcpdrv.Registry, source string, owner toolauth.Owner, logger *slog.Logger, reason string) error {
	if dc, ok := catalog.(tools.CatalogSourceDeregisterer); ok {
		removed := dc.DeregisterSource(tools.ToolSourceID(source))
		if logger != nil {
			logger.InfoContext(ctx, reason,
				slog.String("source", source), slog.Int("tools_deregistered", removed))
		}
	}
	if registry == nil {
		return nil
	}
	if err := registry.Deregister(ctx, source, owner); err != nil {
		if errors.Is(err, mcpdrv.ErrServerNotFound) {
			return nil
		}
		return err
	}
	return nil
}

func detachSourceExpected(ctx context.Context, catalog tools.ToolCatalog, registry *mcpdrv.Registry, source string, owner toolauth.Owner, fingerprint string, logger *slog.Logger, reason string) error {
	if registry == nil || fingerprint == "" {
		return errors.New("mcp: exact teardown requires registry and descriptor fingerprint")
	}
	dc, ok := catalog.(tools.CatalogSourceDeregisterer)
	if !ok {
		return errors.New("mcp: exact teardown requires catalog source deregistration")
	}
	removed, err := registry.DeregisterExact(ctx, source, owner, fingerprint, func() int {
		return dc.DeregisterSource(tools.ToolSourceID(source))
	})
	if err != nil {
		if errors.Is(err, mcpdrv.ErrServerNotFound) {
			return nil // already detached — idempotent.
		}
		return err
	}
	if logger != nil {
		logger.InfoContext(ctx, reason, slog.String("source", source), slog.Int("tools_deregistered", removed))
	}
	return nil
}
