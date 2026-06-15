#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 117 smoke — chat-module encapsulation hardening.
#
# Static-only: the chat module is a Console (frontend) surface, so there is no
# HTTP endpoint to hit. These assertions pin the load-bearing encapsulation
# facts in source so a regression fails preflight:
#   1. The chat panel self-applies the sans typeface at its root (renders
#      correctly WITHOUT the Console global stylesheet).
#   2. Host identity + theme are injectable through the module seam
#      (`AppBridgeHostOptions.hostInfo` / `.theme`), not baked in.
#   3. The token contract exists and the encapsulation guard runs clean.
#
# The guard itself (web/console/scripts/check-chat-encapsulation.mjs) uses only
# the node stdlib, so it runs here without `npm ci`.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

CHAT="web/console/src/lib/chat"

# Skip cleanly on a checkout without the Console (keeps preflight green where
# the frontend isn't present).
if [ ! -d "${CHAT}" ]; then
    skip "phase 117: web/console chat module not present"
    smoke_summary
    exit 0
fi

# 1. Gap-1 — chat module root self-applies font-family from a token.
assert_grep_present 'font-family:[[:space:]]*var\(--font-sans\)' \
    "${CHAT}/ChatPanel.svelte" \
    "chat panel root self-applies font-family token (no Console global needed)"

# 2. Gap-2 — host identity + theme are injectable seam fields, not baked in.
assert_grep_present 'hostInfo\?:' \
    "${CHAT}/renderers/app-bridge-host.ts" \
    "AppBridgeHostOptions exposes injectable hostInfo"
assert_grep_present 'theme\?:' \
    "${CHAT}/renderers/app-bridge-host.ts" \
    "AppBridgeHostOptions exposes injectable theme"
assert_grep_present 'export const DEFAULT_HOST_INFO' \
    "${CHAT}/renderers/app-bridge-host.ts" \
    "default host identity is a named, overridable export"

# 3. Token contract exists + names the portability litmus token.
assert_file "${CHAT}/tokens.contract.json" "chat-module token contract present"
assert_grep_present '"--font-sans"' \
    "${CHAT}/tokens.contract.json" \
    "token contract declares the --font-sans litmus token"

# 4. The mechanical encapsulation guard runs clean (boundary + token contract).
if command -v node >/dev/null 2>&1; then
    if node web/console/scripts/check-chat-encapsulation.mjs >/dev/null 2>&1; then
        ok "chat-module encapsulation guard passes (boundary + token contract hold)"
    else
        fail "chat-module encapsulation guard reports violations"
    fi
else
    skip "chat-module encapsulation guard: node not available"
fi

smoke_summary
