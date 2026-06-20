package main

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/tools"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// cmd_dev_mcp_attacher.go — the production concrete that drives the real MCP
// attach lifecycle for the admin-driven runtime add
// (`agent_config.add_mcp_connection`). It is the §4.4 boundary glue: it
// imports the concrete MCP driver (allowed in cmd/harbor) and satisfies the
// driver-agnostic agentcfgprotocol.ConnectionAttacher interface the
// agent-config service depends on. The driver remains unaware of the registry
// and the agent-config service — this glue orchestrates.
//
// The harbortest/devstack helper carries a mirror of this concrete so
// integration tests exercise the same real attach path.

// devMCPConnectionAttacher implements agentcfgprotocol.ConnectionAttacher by
// reusing the boot-time mcpdrv.Attach lifecycle (dial → initialize →
// discover → register) against the LIVE catalog + registry + bus. It owns a
// mutex-guarded closer chain so a runtime-added server's transport drains on
// stack teardown.
//
// Concurrent reuse: the collaborators (catalog / registry / bus / logger /
// defaultIdentity) are set once at construction; the only mutable state is
// the closers slice, guarded by mu (documented internally-synchronised).
type devMCPConnectionAttacher struct {
	catalog         tools.ToolCatalog
	registry        *mcpdrv.Registry
	bus             events.EventBus
	logger          *slog.Logger
	defaultIdentity identity.Identity

	mu      sync.Mutex
	closers []func(context.Context) error
}

// newDevMCPConnectionAttacher builds the production attacher. catalog,
// registry, and bus are mandatory (mcpdrv.Attach validates them too).
func newDevMCPConnectionAttacher(catalog tools.ToolCatalog, registry *mcpdrv.Registry, bus events.EventBus, logger *slog.Logger, defaultIdentity identity.Identity) *devMCPConnectionAttacher {
	return &devMCPConnectionAttacher{
		catalog:         catalog,
		registry:        registry,
		bus:             bus,
		logger:          logger,
		defaultIdentity: defaultIdentity,
	}
}

// Attach drives the real MCP attach for one runtime-added connection. The
// operator-supplied auth Headers flow to the live transport ONLY (the
// agent-config service never persists them). A partial-attach failure is
// drained by mcpdrv.Attach's own closer-chain handling; this method merges
// the per-add closers into its master chain under the lock so teardown drains
// the subprocess. An auth-required condition is surfaced by wrapping
// agentcfgprotocol.ErrAuthRequired so the service parks on the unified
// pause/resume primitive.
func (a *devMCPConnectionAttacher) Attach(ctx context.Context, req agentcfgprotocol.AttachRequest) error {
	ms := config.MCPServerConfig{
		Name:          req.Name,
		TransportMode: transportModeForAdd(req.Transport),
		URL:           req.URL,
		Command:       append([]string(nil), req.Command...),
		Headers:       req.Headers, // SECRET — used for the live transport, never persisted
	}

	// Serialise adds: the per-add closer slice is merged into the master
	// chain under the lock, and serialising the whole attach keeps two
	// concurrent adds of the same name from racing in the catalog / registry.
	// Adds are infrequent admin actions, so the coarse lock is acceptable.
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
	})
	// Merge whatever closers Attach appended (a successful Connect appends the
	// provider's Close even if a later step failed — drain it on teardown).
	a.closers = append(a.closers, local...)
	if err != nil {
		if looksLikeAuthRequired(err) {
			return fmt.Errorf("%w: %w", agentcfgprotocol.ErrAuthRequired, err)
		}
		return err
	}
	return nil
}

// Close drains every runtime-added server's transport in reverse order. Wired
// into the boot closer chain so a runtime-added subprocess does not outlive
// the stack.
func (a *devMCPConnectionAttacher) Close(ctx context.Context) error {
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

// mcpAddStdioAllowlist projects the operator's stdio allowlist out of config
// for the runtime add-connection gate. A nil block yields a nil allowlist
// (fail-closed — every stdio add is rejected).
func mcpAddStdioAllowlist(cfg *config.Config) []string {
	if cfg == nil || cfg.Tools.MCPAddConnection == nil {
		return nil
	}
	return append([]string(nil), cfg.Tools.MCPAddConnection.StdioAllowlist...)
}

// transportModeForAdd maps the control-plane transport onto the MCP driver's
// transport-mode string. http maps to "auto" (the driver inspects the URL and
// negotiates streamable-HTTP first, then SSE); stdio maps to "stdio".
func transportModeForAdd(t agentcfg.MCPTransport) string {
	switch t {
	case agentcfg.MCPTransportStdio:
		return string(mcpdrv.TransportStdio)
	default:
		return string(mcpdrv.TransportAuto)
	}
}

// looksLikeAuthRequired reports whether an attach error indicates the MCP
// server requires authorization, so the service routes it onto the unified
// pause/resume primitive. The MCP driver does not yet surface a TYPED auth
// error, so this is a documented best-effort string heuristic over the
// transport's HTTP-status / OAuth markers; it is conservative (a false
// negative surfaces as a loud `failed`, never a silent drop, and a false
// positive only parks an operator-cancellable connection — no security
// bypass either way). When the driver gains a typed auth sentinel this
// heuristic is replaced by errors.Is — tracked as a follow-up alongside the
// MCP-SDK typed-error work.
func looksLikeAuthRequired(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	// The numeric 401 is matched on a word boundary so it does not trip on
	// an unrelated number (a latency, a byte count, a port). The remaining
	// markers are auth-specific phrases.
	if status401Pattern.MatchString(msg) {
		return true
	}
	for _, marker := range []string{"unauthorized", "www-authenticate", "oauth", "invalid_token", "authentication required", "authorization required"} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

// status401Pattern matches an HTTP 401 status code on a word boundary so a
// bare "401" inside an unrelated number does not false-positive.
var status401Pattern = regexp.MustCompile(`\b401\b`)
