#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 109d smoke — inline MCP-app discovery (mcp.app_available event +
# ChatMessage app ref + renderer mount).
#
# Classification: static-only — pure file-existence + text greps against the
# runtime + Console source. Runs in the parallel batch BEFORE the dev server
# boots. The behavioural coverage lives in the Go integration test
# (internal/tools/drivers/mcp/mcp_app_available_test.go) and the Vitest
# real-component suite (web/console/src/routes/(console)/playground/[session_id]/mcp-app-discovery.spec.ts).
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# (1) Runtime: the new canonical discovery event is declared + registered.
# ----------------------------------------------------------------------------
MCP_EVENTS="internal/tools/drivers/mcp/events.go"
MCP_DRIVER="internal/tools/drivers/mcp/mcp.go"

assert_grep_present 'EventTypeMCPAppAvailable events.EventType = "mcp.app_available"' "${MCP_EVENTS}" \
    "phase-109d: the mcp.app_available canonical event is declared"
assert_grep_present 'RegisterEventType\(EventTypeMCPAppAvailable\)' "${MCP_EVENTS}" \
    "phase-109d: mcp.app_available is registered with the event taxonomy"
assert_grep_present 'type AppAvailablePayload struct' "${MCP_EVENTS}" \
    "phase-109d: the SafePayload AppAvailablePayload is declared"
assert_grep_present 'publishAppAvailable' "${MCP_DRIVER}" \
    "phase-109d: the MCP driver emits the discovery event from its invoke site"

# The generated Protocol reference carries the new event.
assert_grep_present 'mcp.app_available' "docs/site/protocol/events.md" \
    "phase-109d: the generated Protocol events reference carries mcp.app_available"

# ----------------------------------------------------------------------------
# (2) Protocol wire: MCPAppRef carries server_id (single-sourced).
# ----------------------------------------------------------------------------
assert_grep_present 'ServerID string `json:"server_id,omitempty"`' "internal/protocol/types/mcp_apps.go" \
    "phase-109d: the wire MCPAppRef carries server_id (single source)"
assert_grep_present 'server_id\?: string' "web/console/src/lib/protocol/mcp.ts" \
    "phase-109d: the Console wire type hand-syncs server_id"

# ----------------------------------------------------------------------------
# (3) Console: the ChatMessage model + MessageBubble dispatch + page wiring.
# ----------------------------------------------------------------------------
CHAT_TYPES="web/console/src/lib/chat/types.ts"
BUBBLE="web/console/src/lib/chat/MessageBubble.svelte"
PAGE="web/console/src/routes/(console)/playground/[session_id]/+page.svelte"
WIRE="web/console/src/routes/(console)/playground/[session_id]/wire-events.ts"

assert_grep_present 'app\?: MCPAppRefView' "${CHAT_TYPES}" \
    "phase-109d: ChatMessage carries the inline MCP App ref"
assert_grep_present 'if message.app && message.serverID && appHostClient' "${BUBBLE}" \
    "phase-109d: MessageBubble mounts the renderer when an app ref is present"
assert_grep_present 'MCP_APP_INLINE_MIME' "${BUBBLE}" \
    "phase-109d: MessageBubble dispatches the app under the inline MCP-app MIME"
assert_grep_present "decodeAppAvailable" "${WIRE}" \
    "phase-109d: the page decodes the mcp.app_available SSE frame"
assert_grep_present "'mcp.app_available'" "${PAGE}" \
    "phase-109d: the Playground subscribes to mcp.app_available"
assert_grep_present 'applyAppAvailable' "${PAGE}" \
    "phase-109d: the page attaches the discovered app to the run's bubble"

# ----------------------------------------------------------------------------
# (4) Chat-module encapsulation (D-091): the shared chat module gains ZERO
#     imports from Console internals (no $lib/protocol, $lib/connection,
#     $lib/stores, $lib/components). Test/spec files are exempt (they may reach
#     across the boundary). A leak here breaks the future web/shared/chat
#     extraction.
# ----------------------------------------------------------------------------
chat_leaks="$(grep -rnE "from ['\"]\\\$lib/(protocol|connection|stores|components)" \
    web/console/src/lib/chat/ --include='*.ts' --include='*.svelte' 2>/dev/null \
    | grep -vE '\.(spec|test)\.ts:' || true)"
if [ -n "${chat_leaks}" ]; then
    fail "phase-109d: chat module imports a Console internal (D-091 encapsulation): ${chat_leaks}"
else
    ok "phase-109d: chat module imports no Console internals (protocol/connection/stores/components)"
fi

# The real-component regression guard + Go integration test exist.
assert_file "web/console/src/routes/(console)/playground/[session_id]/mcp-app-discovery.spec.ts" \
    "phase-109d: the real-component discovery→render regression guard landed"
assert_file "internal/tools/drivers/mcp/mcp_app_available_test.go" \
    "phase-109d: the Go discovery-event integration test landed"

smoke_summary
