#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 109j smoke — Console pushes tool-input/tool-result into the MCP App.
#
# Conventions (AGENTS.md §4.2):
#   - Surface-not-yet-present → SKIP (so this forward-phase script coexists
#     with builds that predate 109j). The implementer flips the SKIPs to
#     OK/FAIL once the surface lands so the script guards against regression.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# This phase closes the MCP Apps Data Delivery lifecycle on the Console side:
# after `ui/notifications/initialized`, the host fetches 109i's
# `mcp.apps.tool_context` and pushes it via the official AppBridge
# `sendToolInput()` / `sendToolResult()`. Console-only; the live render path is
# the Playwright app-render spec, not preflight — these are static guards.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

BRIDGE='web/console/src/lib/chat/renderers/app-bridge-host.ts'
WIRE='web/console/src/routes/(console)/playground/[session_id]/wire-events.ts'

# ----------------------------------------------------------------------------
# 1. The host pushes the originating tool data after init (Data Delivery).
# ----------------------------------------------------------------------------
if grep -q 'sendToolInput' "${BRIDGE}" 2>/dev/null &&
    grep -q 'sendToolResult' "${BRIDGE}" 2>/dev/null; then
    ok 'phase 109j: app-bridge-host pushes sendToolInput + sendToolResult'
else
    skip 'phase 109j: Data Delivery push (sendToolInput/sendToolResult) not yet implemented'
fi

# ----------------------------------------------------------------------------
# 2. The host fetches the tool context through the injected client (the only
#    path off the iframe — D-173). The toolContext seam exists.
# ----------------------------------------------------------------------------
if grep -q 'toolContext' "${BRIDGE}" 2>/dev/null; then
    ok 'phase 109j: host fetches tool context via the injected MCPAppHostClient'
else
    skip 'phase 109j: MCPAppHostClient.toolContext seam not yet implemented'
fi

# ----------------------------------------------------------------------------
# 3. The discovery decoder carries the correlation id through to the renderer.
# ----------------------------------------------------------------------------
if grep -q 'tool_call_id\|toolCallId' "${WIRE}" 2>/dev/null; then
    ok 'phase 109j: wire-events decodes tool_call_id for app correlation'
else
    skip 'phase 109j: wire-events does not yet decode tool_call_id'
fi

smoke_summary
