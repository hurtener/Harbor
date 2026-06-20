#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 92d smoke — agent-config MCP pause/resume + per-tool policy: the
# `agent_config.set_tool_exposure` admin Protocol method, the ToolExposure
# payload section, the run-start projection + app-call gate symbols, the
# `mcp.connection.paused` / `.resumed` events, the typed Console module
# entries, and the generated-docs rows.
#
# Conventions (AGENTS.md §4.2): 404/405/501 -> SKIP; OK >= 1 once shipped;
# use scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. The set_tool_exposure method constant is single-sourced in methods.go.
assert_grep_present 'MethodAgentConfigSetToolExposure Method = "agent_config.set_tool_exposure"' \
    internal/protocol/methods/methods.go \
    'phase 92d: set_tool_exposure method constant present'

# 2. The ToolExposure payload section (exclusion-based) is on the envelope.
assert_grep_present 'type ToolExposure struct' \
    internal/agentcfg/agentcfg.go \
    'phase 92d: ToolExposure type present'
assert_grep_present 'PausedServers \[\]string' \
    internal/agentcfg/agentcfg.go \
    'phase 92d: ToolExposure.PausedServers field present'

# 3. The tool-exposure service method records a revision + emits events.
assert_grep_present 'func \(s \*Service\) SetToolExposure' \
    internal/runtime/agentcfg/protocol/mcppolicy.go \
    'phase 92d: SetToolExposure service method present'
assert_grep_present 'EventTypeMCPConnectionPaused' \
    internal/agentcfg/events.go \
    'phase 92d: mcp.connection.paused event declared'
assert_grep_present 'EventTypeMCPConnectionResumed' \
    internal/agentcfg/events.go \
    'phase 92d: mcp.connection.resumed event declared'

# 4. The run-start projection excludes paused servers / disabled tools.
assert_grep_present 'func ActivePlannerCatalogView' \
    internal/runtime/agentcfg/projection/projection.go \
    'phase 92d: tool-exposure run-start projection present'

# 5. The app->host current-state gate (the asymmetry, D-234).
assert_grep_present 'func \(a \*AppsAccessor\) gateToolExposure' \
    internal/mcpconsole/apps.go \
    'phase 92d: app-call exposure gate present'
assert_grep_present 'ErrAppToolExposureDenied' \
    internal/mcpconsole/apps.go \
    'phase 92d: typed app-call authorization error present'

# 6. The typed Console method + wire interface exist.
assert_grep_present 'setToolExposure' \
    web/console/src/lib/protocol/client.ts \
    'phase 92d: typed Console setToolExposure method present'
assert_grep_present 'export interface AgentConfigToolExposure' \
    web/console/src/lib/protocol/agentconfig.ts \
    'phase 92d: typed Console ToolExposure interface present'

# 7. The generated Protocol docs carry the method + the events.
assert_grep_present 'agent_config.set_tool_exposure' \
    docs/site/protocol/methods.md \
    'phase 92d: generated methods.md carries set_tool_exposure'
assert_grep_present 'mcp.connection.paused' \
    docs/site/protocol/events.md \
    'phase 92d: generated events.md carries mcp.connection.paused'

# 8. Live (preflight dev server): the set_tool_exposure route is admin-gated.
# An unauthenticated POST must NOT be 200 — 401 / 403 / 501 are all healthy.
# A 404 means the surface is not mounted (SKIP).
ROUTE="$(api_url /v1/agent_config/set_tool_exposure)"
if skip_if_404 "${ROUTE}" 'phase 92d: agent_config.set_tool_exposure route mounted'; then
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H 'Content-Type: application/json' -d '{}' "${ROUTE}" || true)
    case "${code}" in
        401|403) ok "phase 92d: agent_config.set_tool_exposure is identity/admin-gated (${code})" ;;
        501)     ok "phase 92d: agent_config.set_tool_exposure route present but unwired (501)" ;;
        200)     fail "phase 92d: agent_config.set_tool_exposure answered 200 unauthenticated — admin gate missing" ;;
        *)       fail "phase 92d: agent_config.set_tool_exposure unexpected status ${code}" ;;
    esac
fi

smoke_summary
