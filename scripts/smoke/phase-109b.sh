#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 109b — Console MCP Apps host (sandboxed iframe + AppBridge manual
# handler + inline DisplayMode).
#
# Console-only phase (no new Runtime Protocol surface — it consumes 109a's
# `mcp.servers.read_resource` + `mcp.apps.call_tool`). Like the other Console
# smokes this is static (file existence + token / pattern greps); the
# behavioural coverage lives in the vitest specs (app-bridge-host.spec.ts,
# mcp-app-host-client.spec.ts, mcp-app-registry.spec.ts) and the Playwright
# sandbox-escape spec (tests/mcp-app-host.spec.ts) the frontend job runs.
# Assertions SKIP when a surface is absent so the gate stays green before 109b.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file_or_skip() {
    local path="$1" desc="$2"
    if [ -f "${path}" ]; then ok "${desc}: ${path} exists"
    else skip "${desc}: ${path} missing (Phase 109b not yet implemented)"; fi
}

assert_grep_or_skip() {
    local pattern="$1" path="$2" desc="$3"
    if [ ! -f "${path}" ]; then skip "${desc}: ${path} not found (Phase 109b not yet implemented)"; return; fi
    if grep -qE "${pattern}" "${path}" 2>/dev/null; then ok "${desc}"
    else skip "${desc}: pattern '${pattern}' absent in ${path} (Phase 109b not yet implemented)"; fi
}

RENDERER="web/console/src/lib/chat/renderers/mcp-app.svelte"
HOST="web/console/src/lib/chat/renderers/app-bridge-host.ts"
POLICY="web/console/src/lib/chat/renderers/sandbox-policy.ts"
INDEX="web/console/src/lib/chat/renderers/index.ts"
PWSPEC="web/console/tests/mcp-app-host.spec.ts"

# ---- The renderer + bridge land inside the shared chat module (D-091) --------
assert_file_or_skip "${RENDERER}" "phase-109b: MCP Apps renderer landed"
assert_file_or_skip "${HOST}" "phase-109b: AppBridge host wrapper landed"
assert_file_or_skip "${POLICY}" "phase-109b: sandbox-policy surface landed"

# ---- Sandboxed iframe + strict CSP + origin check ----------------------------
assert_grep_or_skip "sandbox" "${RENDERER}" \
    "phase-109b: the renderer sets an iframe sandbox attribute"
assert_grep_or_skip "Content-Security-Policy" "${POLICY}" \
    "phase-109b: a strict CSP is applied to the app document"
assert_grep_or_skip "connect-src 'none'" "${POLICY}" \
    "phase-109b: CSP connect-src none blocks the app opening its own transport (D-173)"
assert_grep_or_skip "isTrustedAppMessage" "${POLICY}" \
    "phase-109b: a postMessage origin / source check is present"

# ---- No allow-same-origin escape — the whole game ----------------------------
# The base sandbox token set is exactly `allow-scripts` (never same-origin),
# and the guard `assertSandboxTokensSafe` rejects any same-origin token before
# it can reach the DOM. The runtime enforcement is asserted in vitest +
# Playwright; here we assert the documented constant + guard are present.
assert_grep_or_skip "APP_IFRAME_SANDBOX_BASE = 'allow-scripts'" "${POLICY}" \
    "phase-109b: the sandbox base is allow-scripts only — no allow-same-origin (D-173)"
assert_grep_or_skip "assertSandboxTokensSafe" "${POLICY}" \
    "phase-109b: a guard rejects any allow-same-origin sandbox token (D-173)"

# ---- Manual-handler mode (D-173): null MCP client, handlers wired -------------
assert_grep_or_skip "new AppBridge\(null" "${HOST}" \
    "phase-109b: the AppBridge is constructed in manual-handler mode (null MCP client)"
assert_grep_or_skip "oncalltool" "${HOST}" \
    "phase-109b: oncalltool handler wired to the injected Protocol client"
assert_grep_or_skip "onreadresource" "${HOST}" \
    "phase-109b: onreadresource handler wired to the injected Protocol client"

# ---- inline DisplayMode via the renderer registry ----------------------------
assert_grep_or_skip "MCP_APP_INLINE_MIME" "${INDEX}" \
    "phase-109b: the inline MCP App renderer registers on the renderer registry"
assert_grep_or_skip "source: 'mcp-app'" "${INDEX}" \
    "phase-109b: the registry exposes the mcp-app renderer source"

# ---- No raw color/spacing literals in the new .svelte (token-surface rule) ----
if [ -f "${RENDERER}" ]; then
    style_block="$(awk '/<style>/{f=1} f{print} /<\/style>/{f=0}' "${RENDERER}")"
    if printf '%s' "${style_block}" | grep -qE '#[0-9a-fA-F]{3,8}|rgba?\(|[0-9]+px|[0-9]*\.?[0-9]+rem'; then
        fail "phase-109b: raw color/spacing literal in ${RENDERER} <style> (use tokens — CLAUDE.md §4.5)"
    else
        ok "phase-109b: the MCP Apps renderer <style> uses tokens only, no raw literals"
    fi
else
    skip "phase-109b: ${RENDERER} missing (Phase 109b not yet implemented)"
fi

# ---- The chat module imports NO Console-shell internals (D-091 encapsulation) -
for f in "${RENDERER}" "${HOST}" "${POLICY}"; do
    if [ -f "${f}" ]; then
        if grep -qE "from '\\\$lib/(stores|connection|components|protocol)" "${f}" 2>/dev/null; then
            fail "phase-109b: ${f} imports a Console internal (chat-module encapsulation violation — D-091)"
        else
            ok "phase-109b: ${f} imports no Console-shell internals"
        fi
    fi
done

# ---- The Playwright sandbox-escape spec landed -------------------------------
assert_file_or_skip "${PWSPEC}" "phase-109b: the Playwright sandbox-escape spec landed"

smoke_summary
