#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 92f smoke — agent-config add-connection: the
# `agent_config.add_mcp_connection` admin Protocol method (dial + initialize
# handshake + OAuth-via-pause/resume + stdio allowlist gate), the connection
# descriptor payload section, the explicit attach lifecycle states + events,
# the pause/resume reuse, the stdio fail-closed gate, the typed Console module
# entry, and the generated-docs rows.
#
# Conventions (AGENTS.md §4.2): 404/405/501 -> SKIP; OK >= 1 once shipped;
# use scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. The add_mcp_connection method constant is single-sourced in methods.go.
assert_grep_present 'MethodAgentConfigAddMCPConnection Method = "agent_config.add_mcp_connection"' \
    internal/protocol/methods/methods.go \
    'phase 92f: add_mcp_connection method constant present'

# 2. The connection-descriptor payload section is on the config envelope.
assert_grep_present 'type MCPConnectionDescriptor struct' \
    internal/agentcfg/agentcfg.go \
    'phase 92f: MCPConnectionDescriptor type present'
assert_grep_present 'Connections +\*ConnectionsSection' \
    internal/agentcfg/agentcfg.go \
    'phase 92f: ConfigPayload.Connections section present'

# 3. The add-connection service drives the real attach + the explicit lifecycle.
assert_grep_present 'func \(s \*Service\) AddMCPConnection' \
    internal/runtime/agentcfg/protocol/addconnection.go \
    'phase 92f: AddMCPConnection service method present'
assert_grep_present 'ConnectionStateAuthRequired ConnectionState = "auth_required"' \
    internal/runtime/agentcfg/protocol/addconnection.go \
    'phase 92f: explicit auth_required lifecycle state present'

# 4. The attach lifecycle events are declared.
assert_grep_present 'EventTypeMCPConnectionAdded' \
    internal/agentcfg/events.go \
    'phase 92f: mcp.connection.added event declared'
assert_grep_present 'EventTypeMCPConnectionFailed' \
    internal/agentcfg/events.go \
    'phase 92f: mcp.connection.failed event declared'

# 5. The OAuth path reuses the unified pause/resume primitive (no new dance).
assert_grep_present 's.coordinator.Request' \
    internal/runtime/agentcfg/protocol/addconnection.go \
    'phase 92f: auth-required attach parks on the pause/resume Coordinator'

# 6. The stdio gate is fail-closed (allowlist; argv-form; never sh -c).
assert_grep_present 'ErrStdioNotAllowed' \
    internal/runtime/agentcfg/protocol/addconnection.go \
    'phase 92f: stdio allowlist gate error present'
assert_grep_present 'stdio_allowlist' \
    internal/config/config.go \
    'phase 92f: tools.mcp_add_connection.stdio_allowlist config field present'

# 7. The driver-agnostic attach seam + the concrete that drives mcpdrv.Attach.
assert_grep_present 'type ConnectionAttacher interface' \
    internal/runtime/agentcfg/protocol/addconnection.go \
    'phase 92f: ConnectionAttacher seam present (the section 4.4 boundary)'
assert_grep_present 'func NewMCPConnectionAttacher' \
    internal/runtime/serve/mcp_attacher.go \
    'phase 92f: production attach concrete present (promoted to the serve band)'

# 8. The typed Console method + wire interface exist.
assert_grep_present 'addMcpConnection' \
    web/console/src/lib/protocol/client.ts \
    'phase 92f: typed Console addMcpConnection method present'
assert_grep_present 'export interface AgentConfigAddMCPConnectionRequest' \
    web/console/src/lib/protocol/agentconfig.ts \
    'phase 92f: typed Console add-connection request interface present'

# 9. The generated Protocol docs carry the method + the events.
assert_grep_present 'agent_config.add_mcp_connection' \
    docs/site/protocol/methods.md \
    'phase 92f: generated methods.md carries add_mcp_connection'
assert_grep_present 'mcp.connection.added' \
    docs/site/protocol/events.md \
    'phase 92f: generated events.md carries mcp.connection.added'

# 10. Live (preflight dev server): the add_mcp_connection route is admin-gated.
# An unauthenticated POST must NOT be 200 — 401 / 403 / 501 are all healthy.
# A 404 means the surface is not mounted (SKIP).
ROUTE="$(api_url /v1/agent_config/add_mcp_connection)"
if skip_if_404 "${ROUTE}" 'phase 92f: agent_config.add_mcp_connection route mounted'; then
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H 'Content-Type: application/json' -d '{}' "${ROUTE}" || true)
    case "${code}" in
        401|403) ok "phase 92f: agent_config.add_mcp_connection is identity/admin-gated (${code})" ;;
        501)     ok "phase 92f: agent_config.add_mcp_connection route present but unwired (501)" ;;
        200)     fail "phase 92f: agent_config.add_mcp_connection answered 200 unauthenticated — admin gate missing" ;;
        *)       fail "phase 92f: agent_config.add_mcp_connection unexpected status ${code}" ;;
    esac
fi

smoke_summary
