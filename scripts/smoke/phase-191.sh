#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 191 smoke — OAuth broker legs: step-up visibility (HA-26),
# resource-bound exchange with per-tool binding (HA-27), actor chain (HA-28)
# + the wave-v1.16 E2E.
#
# The phase adds NO new Protocol method / REST endpoint (the LastScopeShortfall
# field rides the existing mcp.servers.get detail read), so the class
# assertions are unit-test-shaped. The live mcp.servers.get surface needs an
# authenticated admin caller AND a boot-wired MCP fixture answering 403 to
# populate LastScopeShortfall — neither is present on a bare preflight dev
# server, so the live check records SKIP per the sacred convention.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# --- Class assertions (unit tests) -----------------------------------------

run_go_test() {
    local label="$1" runexpr="$2" pkgs="$3" logf
    logf="$(mktemp)"
    # shellcheck disable=SC2086 # pkgs is a deliberate space-separated list
    if go test -run "${runexpr}" ${pkgs} >"${logf}" 2>&1; then
        ok "phase 191: ${label}"
    else
        fail "phase 191: ${label}"
        printf '--- go test output ---\n'
        cat "${logf}"
    fi
    rm -f "${logf}"
}

# HA-26: 403 + insufficient_scope → typed structured error + permanent class.
run_go_test "insufficient-scope classified permanent + one attempt + tool.failed shortfall" \
    'TestClassifyError_InsufficientScope_Permanent|TestRunWithPolicy_InsufficientScope_OneAttempt|TestCatalogLifecycle_ScopeShortfall_OnToolFailed' \
    './internal/tools/'

# HA-26 + HA-27b: the MCP driver upgrades a 403 challenge into
# *tools.ErrInsufficientScope, surfaces LastScopeShortfall on the connection
# view, and keeps per-tool bindings bleed-free under -race.
run_go_test "mcp 403 -> ErrInsufficientScope + LastScopeShortfall + per-tool no-bleed" \
    'TestCallTool_InsufficientScope_TypedError|TestRegistry_LastScopeShortfall_Surfaced|TestConcurrentReuse_ToolOAuthProviders_NoTokenBleed' \
    './internal/tools/drivers/mcp/'

# HA-27a + HA-28: resource indicator + audience check + actor_token.
run_go_test "tokenexchange resource/audience/actor legs" \
    'TestExchange_' \
    './internal/tools/auth/drivers/tokenexchange/'

# Boot validation for the three additive config fields (driver mismatch,
# per-tool binding rules).
run_go_test "config validation for oauth broker legs" \
    'TestValidateTools_OAuthBrokerLegs' \
    './internal/config/'

# --- Live-server surface (graceful skip) -----------------------------------

skip "phase 191: LastScopeShortfall rides mcp.servers.get (existing detail read); a live assertion needs an authenticated admin caller + a boot-wired MCP fixture answering 403 — covered by the wave-v1.16 E2E, not this bare-server smoke"

smoke_summary
