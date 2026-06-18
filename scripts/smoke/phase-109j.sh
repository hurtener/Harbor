#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 109j smoke — Console pushes tool-input/tool-result into the MCP App.
#
# Conventions (AGENTS.md §4.2):
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# REVERTED (handshake regression): the 109j Console Data-Delivery push, along
# with the rest of the post-v1.4 Console MCP-Apps surface, was reverted to v1.4
# (commit 848e588). The post-v1.4 renderer/host changes broke the
# `ui/initialize` postMessage handshake — a rendered ui:// App timed out with
# "Connection Error" because the host's response never reached the iframe
# (live-proven: the host posts a schema-valid response to the exact live iframe
# window, yet the View receives nothing). The BACKEND tool-context surface
# (109i `mcp.apps.tool_context` + its Console wire mirror) is KEPT; only the
# Console PUSH (sendToolInput/sendToolResult + the toolContext seam +
# tool_call_id decode) is gone, pending a proper re-land once the exact
# breaking change is bisected. These guards therefore SKIP rather than assert
# presence — re-flip them to OK-on-presence when the feature re-lands.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

BRIDGE='web/console/src/lib/chat/renderers/app-bridge-host.ts'
WIRE='web/console/src/routes/(console)/playground/[session_id]/wire-events.ts'

# ----------------------------------------------------------------------------
# 1. The host pushes the originating tool data after init (Data Delivery).
#    REVERTED — the Console push was rolled back to v1.4 (handshake regression).
# ----------------------------------------------------------------------------
if grep -q 'sendToolInput' "${BRIDGE}" 2>/dev/null &&
    grep -q 'sendToolResult' "${BRIDGE}" 2>/dev/null; then
    ok 'phase 109j: app-bridge-host pushes sendToolInput + sendToolResult'
else
    skip 'phase 109j: Data Delivery push reverted to v1.4 (handshake regression) — re-land tracked'
fi

# ----------------------------------------------------------------------------
# 2. The host fetches the tool context through the injected client (the only
#    path off the iframe — D-173). The toolContext seam exists.
#    REVERTED — the Console seam was rolled back to v1.4 (handshake regression).
# ----------------------------------------------------------------------------
if grep -q 'toolContext' "${BRIDGE}" 2>/dev/null; then
    ok 'phase 109j: host fetches tool context via the injected MCPAppHostClient'
else
    skip 'phase 109j: toolContext seam reverted to v1.4 (handshake regression) — re-land tracked'
fi

# ----------------------------------------------------------------------------
# 3. The discovery decoder carries the correlation id through to the renderer.
#    REVERTED — wire-events was rolled back to v1.4 (handshake regression).
# ----------------------------------------------------------------------------
if grep -q 'tool_call_id\|toolCallId' "${WIRE}" 2>/dev/null; then
    ok 'phase 109j: wire-events decodes tool_call_id for app correlation'
else
    skip 'phase 109j: wire-events tool_call_id decode reverted to v1.4 (handshake regression) — re-land tracked'
fi

smoke_summary
