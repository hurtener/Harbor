#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 194 smoke — per-tool OAuth binding on the resource/prompt paths +
# owner-scoped credential-sink uninstall (D-331).
#
# (a) extends 191's per-tool oauth_provider binding (resolveBearerCtx +
#     MCPServerConfig.ToolOAuthProviders) to ReadResource / GetPrompt and the
#     other identity-stamped MCP RPC paths D-278 lists;
# (b) gives ProviderSet.Uninstall an Owner (owner-scoped, defense-in-depth on
#     D-303) so the store refuses a cross-owner drop.
# No new Protocol method expected, so assertions are unit-test-shaped; the live
# mcp.servers.get check records SKIP on a bare preflight dev server.
#
# Each go-test block SKIPs (not FAILs) when its target test name is absent, so
# this script coexists with pre-194 builds per the sacred SKIP convention.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

run_go_test_if_present() {
    local label="$1" runexpr="$2" pkg="$3" logf
    logf="$(mktemp)"
    if ! go test -list "${runexpr}" "${pkg}" >"${logf}" 2>&1 \
        || ! grep -qE '^Test' "${logf}"; then
        skip "phase 194: ${label} (test/surface absent — pre-194 build)"
        rm -f "${logf}"
        return
    fi
    if go test -run "${runexpr}" -race "${pkg}" >"${logf}" 2>&1; then
        ok "phase 194: ${label}"
    else
        fail "phase 194: ${label}"
        printf '--- go test output ---\n'
        cat "${logf}"
    fi
    rm -f "${logf}"
}

# --- Static assertions -----------------------------------------------------

# AC-4: Uninstall carries an Owner parameter + owner-collision refusal wired.
if grep -qE 'Uninstall\(ctx context\.Context, owner ' internal/tools/auth/providers.go 2>/dev/null \
    || grep -q 'ErrProviderOwnerCollision' internal/tools/auth/providers.go 2>/dev/null; then
    ok "phase 194: owner-scoped Uninstall wired in providers.go"
else
    skip "phase 194: owner-scoped Uninstall absent — pre-194 build"
fi

# --- Class assertions (unit tests) -----------------------------------------

# AC-1/2/3/8: resolveBearerCtx resolves per-resource/per-prompt bindings on the
# ReadResource/Subscribe/resource-read/prompt-get sites + falls back to the
# connection binding + rejects an ambiguous cross-surface key + no token bleed
# across identities/RPCs.
run_go_test_if_present "mcp resource/prompt per-tool binding + no-token-bleed" \
    'TestResolveBearerCtx_ResourcePromptKey|TestProvider_ResourcePromptBinding_RoundTrip|TestDiscover_AmbiguousOAuthBinding_FailsLoud|TestConcurrentReuse_ResourcePromptOAuthProviders_NoTokenBleed' \
    './internal/tools/drivers/mcp/'

# AC-4/6/7: store-boundary owner-scoped uninstall (matching-owner drops;
# cross-owner refused; boot-protected refused; loud bound-call failure after).
run_go_test_if_present "provider set owner-scoped uninstall + collision refusal" \
    'TestProviderSet_Uninstall_' \
    './internal/tools/auth/'

# AC-5: the remove_oauth_provider handler passes the resolved owner.
run_go_test_if_present "remove_oauth_provider handler passes owner" \
    'TestRemoveOAuthProvider_PassesResolvedOwner' \
    './internal/runtime/agentcfg/protocol/'

# AC-9: boot validation rejects a resource/prompt binding on stdio /
# static-Authorization conflict (the tool_oauth_providers applicability).
run_go_test_if_present "config validation for resource/prompt oauth binding" \
    'TestValidateTools_OAuthBrokerLegs' \
    './internal/config/'

# AC-10/11: cross-package E2E — real MCP driver + real provider set +
# owner-scoped uninstall round-trip against the spec-derived go-sdk fixture.
run_go_test_if_present "E2E resource binding + owner-scoped uninstall" \
    'TestE2E_MCPResourceBinding_OwnerScopedUninstall' \
    './test/integration/'

# --- Live-server surface (graceful skip) -----------------------------------

skip "phase 194: the resource/prompt binding rides the existing mcp.servers.get detail read; a live assertion needs an authenticated admin caller + a boot-wired MCP fixture exposing a bound resource/prompt — covered by the in-package mcp driver integration test, not this bare-server smoke"

smoke_summary
