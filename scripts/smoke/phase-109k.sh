#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 109k smoke — MCP Apps spec-conformance hardening.
#
# WHY THIS SCRIPT IS ALL-FAIL, NOT SKIP-TOLERANT
# ----------------------------------------------
# The 404/405/501 → SKIP convention (AGENTS.md §4.2 item 4) is for a
# forward-phase script running against a build that predates the surface. It is
# NOT for a SHIPPED phase's own guards. Every assertion below covers a surface
# that has landed, so every one of them is a hard `assert_grep_present`: absent
# means FAIL.
#
# This script previously guarded each obligation with a bare `grep … && ok ||
# skip`. The Console half of 109k was reverted in the very merge that shipped
# it, so five obligations silently reported SKIP for four phases while the
# master plan, D-227, and this phase's plan all recorded them as delivered —
# exactly the "a SKIP that should be an OK is a bug" failure §4.2 item 5 names.
# The re-land is Phase 207 (D-351); this script is the gate that makes a second
# silent regression impossible.
#
# Conventions (AGENTS.md §4.2):
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#   - The conformance revert-guards for the WIRE behaviour are the
#     `HARBOR_LIVE_MCP` probes + the real-iframe Playwright handshake spec (a
#     real ext-apps server / a real vendored App client); these are the static
#     structural guards that keep the code from vanishing again.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

BRIDGE='web/console/src/lib/chat/renderers/app-bridge-host.ts'
RENDERER='web/console/src/lib/chat/renderers/mcp-app.svelte'
MCPDRV='internal/tools/drivers/mcp/mcp.go'
POSTURE='internal/protocol/types/posture.go'

# ----------------------------------------------------------------------------
# 1. FAIL-1: the UI capability advertises the spec `mimeTypes` shape.
# ----------------------------------------------------------------------------
assert_grep_present 'text/html;profile=mcp-app' "${MCPDRV}" \
    'phase 109k: UI capability advertises the spec app MIME'
assert_grep_present '"mimeTypes"' "${MCPDRV}" \
    'phase 109k: UI capability uses the spec mimeTypes field'
# …and the non-spec `displayModes` capability payload stays gone.
assert_grep_absent '"displayModes": modes' "${MCPDRV}" \
    'phase 109k: non-spec displayModes capability payload removed'

# ----------------------------------------------------------------------------
# 1b. runtime.info surfaces the configured MCP-app display modes (the Console
#     reads them for the host-context availableDisplayModes).
# ----------------------------------------------------------------------------
assert_grep_present 'MCPAppDisplayModes' "${POSTURE}" \
    'phase 109k: runtime.info carries the configured MCP-app display modes'

# ----------------------------------------------------------------------------
# 2. FAIL-2 (the CONFINEMENT control, HA-41): an app→host tool call is
#    qualified into the calling app's own server namespace before dispatch, so
#    a sandboxed App can never name a tool outside `<serverID>_`.
#
#    Two assertions on purpose: the helper must exist AND `oncalltool` must
#    actually route through it. Pinning only the helper would let a refactor
#    strand it unused — which is precisely how the property evaporated before.
# ----------------------------------------------------------------------------
assert_grep_present 'function qualifyAppToolName\(serverID' "${BRIDGE}" \
    'phase 109k: the server-namespace qualifier exists'
assert_grep_present 'qualifyAppToolName\(serverID, name\)' "${BRIDGE}" \
    'phase 109k: app→host tool call is qualified into the server namespace'

# ----------------------------------------------------------------------------
# 3. Host obligation: `ui/notifications/size-changed` is CONSUMED (HA-38) and
#    the inline frame height is driven from it.
# ----------------------------------------------------------------------------
assert_grep_present 'bridge\.onsizechange' "${BRIDGE}" \
    'phase 109k: size-changed is consumed on the bridge'
assert_grep_present 'onAppSizeChanged\(size\)' "${RENDERER}" \
    'phase 109k: the renderer drives the frame height from the reported size'

# ----------------------------------------------------------------------------
# 4. Host obligation: graceful `ui/resource-teardown` on unmount + the
#    app-initiated `request-teardown` handler.
# ----------------------------------------------------------------------------
assert_grep_present 'teardownResource' "${BRIDGE}" \
    'phase 109k: graceful ui/resource-teardown is sent before close'
assert_grep_present 'bridge\.onrequestteardown' "${BRIDGE}" \
    'phase 109k: the app-initiated request-teardown is handled'

# ----------------------------------------------------------------------------
# 5. Host obligation: live theme relayed onto the running bridge
#    (host-context-changed), never a teardown-rebuild (D-342).
# ----------------------------------------------------------------------------
assert_grep_present 'setHostContext' "${BRIDGE}" \
    'phase 109k: live theme is relayed via host-context-changed'

# ----------------------------------------------------------------------------
# 6. Host obligations: host-context `toolInfo` + `containerDimensions`.
# ----------------------------------------------------------------------------
assert_grep_present 'hostContext\.toolInfo' "${BRIDGE}" \
    'phase 109k: host-context toolInfo names the originating tool call'
assert_grep_present 'hostContext\.containerDimensions' "${BRIDGE}" \
    'phase 109k: host-context containerDimensions describes the app container'

# ----------------------------------------------------------------------------
# 7. Host obligation: `resources/templates/list` is handled, so the advertised
#    `serverResources` capability is honestly serviceable.
# ----------------------------------------------------------------------------
assert_grep_present 'bridge\.onlistresourcetemplates = ' "${BRIDGE}" \
    'phase 109k: resources/templates/list is handled'

smoke_summary
