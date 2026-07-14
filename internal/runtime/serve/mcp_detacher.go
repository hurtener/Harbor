package serve

import (
	"context"
	"errors"
	"log/slog"

	"github.com/hurtener/Harbor/internal/config"
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

// Detach deregisters the source's tools from the catalog (when the catalog
// supports source deregistration — the optional CatalogSourceDeregisterer
// companion) and from the MCP registry, closing its transport gracefully. An
// already-gone source is a no-op (ErrServerNotFound is swallowed — idempotent).
func (d *MCPConnectionDetacher) Detach(ctx context.Context, source string) error {
	if dc, ok := d.catalog.(tools.CatalogSourceDeregisterer); ok {
		removed := dc.DeregisterSource(tools.ToolSourceID(source))
		if d.logger != nil {
			d.logger.InfoContext(ctx, "mcp: detached runtime-added server at run-start reconcile",
				slog.String("source", source), slog.Int("tools_deregistered", removed))
		}
	}
	if d.registry == nil {
		return nil
	}
	if err := d.registry.Deregister(ctx, source); err != nil {
		if errors.Is(err, mcpdrv.ErrServerNotFound) {
			return nil // already detached — idempotent.
		}
		return err
	}
	return nil
}
