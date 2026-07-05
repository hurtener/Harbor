package devstack

import (
	"context"
	"errors"
	"log/slog"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	"github.com/hurtener/Harbor/internal/tools"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// devstack_mcp_detacher.go — the DETACH-leg concrete mirrored from
// cmd/harbor's devMCPConnectionDetacher (cmd/harbor/cmd_dev_mcp_detacher.go).
// It deregisters a no-longer-declared MCP server's tools from the LIVE catalog
// + registry and closes its transport, satisfying the driver-agnostic
// projection.ConnectionDetacher interface the run-start reconcile depends on.
//
// Exported so an integration test can construct the SAME real detach concrete
// the run loop drives (§17.4 — real drivers on the seam). The test kit is the
// §13-sanctioned home for the concrete MCP-driver import in a test-scoped
// harness.

// MCPConnectionDetacher is the test-kit concrete that drives the real MCP
// detach at run-start reconcile. See the file doc.
type MCPConnectionDetacher struct {
	catalog  tools.ToolCatalog
	registry *mcpdrv.Registry
	logger   *slog.Logger
}

// NewMCPConnectionDetacher builds the test-kit detacher. catalog and registry
// are mandatory (the reconcile is a no-op without them); a nil logger silences
// the per-detach log line.
func NewMCPConnectionDetacher(catalog tools.ToolCatalog, registry *mcpdrv.Registry, logger *slog.Logger) *MCPConnectionDetacher {
	return &MCPConnectionDetacher{catalog: catalog, registry: registry, logger: logger}
}

// devstackConnectionDetacher builds the run-start reconcile detacher for the
// stack, or nil when the catalog band / MCP registry is absent (no reconcile).
func devstackConnectionDetacher(stack *DevStack) projection.ConnectionDetacher {
	if stack == nil || stack.Catalog == nil || stack.MCPRegistry == nil {
		return nil
	}
	return NewMCPConnectionDetacher(stack.Catalog, stack.MCPRegistry, nil)
}

// devstackBootDeclaredMCPNames returns the boot-declared (yaml) MCP server
// names — the set the remove verb rejects.
func devstackBootDeclaredMCPNames(cfg *config.Config) []string {
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

// devstackBootDeclaredMCPSet returns the boot-declared MCP server names as a
// set for the reconcile's O(1) skip check.
func devstackBootDeclaredMCPSet(cfg *config.Config) map[string]struct{} {
	names := devstackBootDeclaredMCPNames(cfg)
	if len(names) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(names))
	for _, n := range names {
		set[n] = struct{}{}
	}
	return set
}

// AttachedSources returns the source ids currently live in the MCP registry.
// NOTE: Registry.SourceIDs is a PROCESS-GLOBAL enumeration — correct for the
// single-agent dev wiring, but the future multi-agent attach leg must scope
// the attached set to the reconciling agent (see the
// projection.ConnectionDetacher interface doc).
func (d *MCPConnectionDetacher) AttachedSources(_ context.Context) []string {
	if d.registry == nil {
		return nil
	}
	return d.registry.SourceIDs()
}

// Detach deregisters the source's tools from the catalog (when it supports the
// optional CatalogSourceDeregisterer companion) and from the MCP registry,
// closing its transport gracefully. An already-gone source is a no-op
// (idempotent).
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
