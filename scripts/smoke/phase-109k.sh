#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 109k smoke — MCP Apps spec-conformance hardening.
#
# Conventions (AGENTS.md §4.2):
#   - Surface-not-yet-present → SKIP (so this forward-phase script coexists
#     with builds that predate 109k). The implementer flips the SKIPs to
#     OK/FAIL once each surface lands so the script guards against regression.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# Fixes the wave-end adversarial review's findings: the UI host capability is
# advertised in the spec `mimeTypes` shape (not the hand-rolled `displayModes`);
# app→host tool calls resolve against the server namespace; and the
# size-changed / teardown / theme host obligations are honoured. The conformance
# revert-guards are the `HARBOR_LIVE_MCP` probes (a real ext-apps server), not
# preflight; these are static guards.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

BRIDGE='web/console/src/lib/chat/renderers/app-bridge-host.ts'

# ----------------------------------------------------------------------------
# 1. FAIL-1: the UI capability advertises the spec `mimeTypes` shape.
# ----------------------------------------------------------------------------
if grep -q 'text/html;profile=mcp-app' internal/tools/drivers/mcp/mcp.go 2>/dev/null &&
    grep -q 'mimeTypes' internal/tools/drivers/mcp/mcp.go 2>/dev/null; then
    ok 'phase 109k: UI capability advertises the spec mimeTypes shape'
else
    skip 'phase 109k: mimeTypes UI capability not yet implemented'
fi

# ----------------------------------------------------------------------------
# 2. FAIL-2: app→host tool calls resolve against the server namespace
#    (the bridge prefixes the app server id before the call).
# ----------------------------------------------------------------------------
if grep -qE 'serverID.*\$\{|`\$\{serverID\}_|serverID \+' "${BRIDGE}" 2>/dev/null; then
    ok 'phase 109k: app→host tool call resolves in the server namespace'
else
    skip 'phase 109k: server-namespaced tool resolution not yet implemented'
fi

# ----------------------------------------------------------------------------
# 3. Host obligations: size-changed + graceful teardown wired.
# ----------------------------------------------------------------------------
if grep -q 'sizechange' "${BRIDGE}" 2>/dev/null && grep -q 'teardownResource' "${BRIDGE}" 2>/dev/null; then
    ok 'phase 109k: size-changed + resource-teardown host obligations wired'
else
    skip 'phase 109k: size-changed / teardown host obligations not yet wired'
fi

smoke_summary
