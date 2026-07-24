#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 109l — MCP Apps host theme + data delivery (re-land, handshake-safe).
#
# When the surface lands, assert (static, against the TS source):
#   - app-bridge-host.ts constructs the ui/initialize host-context with
#     styles/variables (theme + design tokens) and has a post-init setHostContext
#     theme relay + the sendToolInput/sendToolResult delivery.
#   - the lifecycle $effect in mcp-app.svelte does NOT depend on the theme store
#     (guard against the reverted teardown-mid-handshake pattern).
#   - flip the re-land SKIP guards in scripts/smoke/phase-109j.sh to OK-on-presence.
# The real gates are the real-iframe Playwright handshake test + the binding
# live-render (analytics-widgets under a real gpt-5.6-luna agent, screenshot).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

BRIDGE='web/console/src/lib/chat/renderers/app-bridge-host.ts'
THEME='web/console/src/lib/chat/renderers/theme-tokens.ts'
RENDERER='web/console/src/lib/chat/renderers/mcp-app.svelte'
HANDSHAKE='web/console/tests/mcp-app-host-handshake.spec.ts'

# ----------------------------------------------------------------------------
# 1. The host bakes theme + styles.variables into the ui/initialize host-context.
# ----------------------------------------------------------------------------
if [[ -f "${THEME}" ]] &&
    grep -q 'McpUiStyleVariableKey' "${THEME}" 2>/dev/null &&
    grep -q 'resolveHostTheme' "${THEME}" 2>/dev/null &&
    grep -q 'hostContext.styles' "${BRIDGE}" 2>/dev/null; then
    ok 'phase 109l: host-context carries theme + styles.variables (theme-tokens map)'
else
    fail 'phase 109l: theme/styles host-context construction missing'
fi

# ----------------------------------------------------------------------------
# 2. A live theme change relays onto the LIVE bridge via setHostContext.
# ----------------------------------------------------------------------------
if grep -q 'setHostContext' "${BRIDGE}" 2>/dev/null &&
    grep -q 'setHostContext' "${RENDERER}" 2>/dev/null; then
    ok 'phase 109l: live theme change relays via setHostContext (no rebuild)'
else
    fail 'phase 109l: setHostContext theme relay missing'
fi

# ----------------------------------------------------------------------------
# 3. Data Delivery — sendToolInput then sendToolResult after init.
# ----------------------------------------------------------------------------
if grep -q 'sendToolInput' "${BRIDGE}" 2>/dev/null &&
    grep -q 'sendToolResult' "${BRIDGE}" 2>/dev/null &&
    grep -q 'toolContext' "${BRIDGE}" 2>/dev/null; then
    ok 'phase 109l: host delivers sendToolInput -> sendToolResult from tool context'
else
    fail 'phase 109l: Data Delivery push missing'
fi

# ----------------------------------------------------------------------------
# 4. Lifecycle isolation guard — the bridge-owning $effect must NOT depend on
#    the theme (the reverted-work break). Assert the effect block that owns
#    connectBridge/host.close does not reference hostTheme/setHostContext, and
#    that the construction reads the theme untracked.
# ----------------------------------------------------------------------------
lifecycle_block="$(awk '/void connectBridge\(\)/,/^  \}\);/' "${RENDERER}" 2>/dev/null || true)"
if [[ -n "${lifecycle_block}" ]] &&
    ! grep -q 'hostTheme' <<<"${lifecycle_block}" &&
    ! grep -q 'setHostContext' <<<"${lifecycle_block}" &&
    grep -q 'untrack' "${RENDERER}" 2>/dev/null; then
    ok 'phase 109l: bridge lifecycle effect is isolated from theme reactivity'
else
    fail 'phase 109l: bridge lifecycle effect may depend on theme (reverted-work break)'
fi

# ----------------------------------------------------------------------------
# 5. The real-iframe Playwright handshake gate exists and drives the REAL App.
# ----------------------------------------------------------------------------
if [[ -f "${HANDSHAKE}" ]] &&
    grep -q "@modelcontextprotocol/ext-apps" "${HANDSHAKE}" 2>/dev/null &&
    grep -q 'ui/initialize' "${HANDSHAKE}" 2>/dev/null; then
    ok 'phase 109l: real-iframe Playwright handshake gate present (drives vendored App)'
else
    fail 'phase 109l: real-iframe handshake gate missing'
fi

smoke_summary
