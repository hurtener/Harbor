#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 109a smoke — runtime + Protocol surface for MCP Apps.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# This phase ships:
#   - `mcp.servers.read_resource` — fetch a ui:// resource's HTML under the
#     identity triple, honouring the D-026 heavy-content safety net.
#   - the tool-result app-ref projection (ui:// resourceUri + DisplayMode + trust).
#   - the app-tool-call proxy that re-enters the existing tool-safety path.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Phase 109a assertions. Uncomment / flesh out as the surface lands.
#
# Exercise the new read_resource method (SKIPs until the method is wired):
#
#   protocol_call 'mcp.servers.read_resource' \
#     '{"server_id":"srv1","resource_uri":"ui://app/widget.html"}' \
#     'mcp.servers.read_resource fetches a ui:// resource'
#
# Assert the method name is registered in the Protocol dispatch surface:
#
#   if grep -q 'mcp.servers.read_resource' internal/protocol/methods/methods.go; then
#       ok 'mcp.servers.read_resource is registered in methods.go'
#   else
#       skip 'mcp.servers.read_resource not yet registered'
#   fi
#
# Static assertion that _meta.ui.resourceUri parsing exists in the driver:
#
#   if grep -rq '_meta' internal/tools/drivers/mcp/content.go; then
#       ok 'MCP driver content.go carries the _meta slot'
#   else
#       skip 'driver _meta.ui parse not yet implemented'
#   fi
# ----------------------------------------------------------------------------

skip "phase 109a: not yet implemented — runtime + Protocol surface for MCP Apps"

smoke_summary
