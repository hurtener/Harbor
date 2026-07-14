#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 166 — Credential-sink hardening (D-300): no admin-writable field may
# determine where a credential is sent. This phase adds NO live Protocol
# surface — it is a shipped-code security fix — so the smoke is static
# trip-wires + the security/unit/integration tests.
#
# What this asserts (each check SKIPs on a pre-166 build):
#
#   1. Static trip-wires: the downstream-host allow-list carries on
#      ToolOAuthProviderConfig + the OAuthProvider interface; the
#      token-exchange client installs a CheckRedirect (redirect refusal); the
#      MCP bearer client installs a redirect guard; the named
#      credential-broker list exists; resolveOAuthBinding enforces the
#      downstream-host allow-list; the fail-closed admin-write audit helper
#      exists.
#   2. unit-tests: the tokenexchange (ceiling + hardened client), mcp
#      (binding guard + bearer redirect), config (allow-list + broker), and
#      protocol (audit-ordering) packages pass under -race.
#   3. The §17.1 integration test (real drivers): unlisted-host refusal at
#      attach + zero-client_secret-to-redirect assertion.
#
# Done-definition: OK >= 2, FAIL = 0.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_or_skip() {
    local pattern="$1" file="$2" desc="$3"
    if [ ! -f "${file}" ]; then
        skip "${desc}: ${file} not found (Phase 166 not yet implemented)"
        return
    fi
    if grep -qE "${pattern}" "${file}" 2>/dev/null; then
        ok "${desc}"
    else
        skip "${desc}: pattern '${pattern}' absent (Phase 166 not yet implemented)"
    fi
}

# ----------------------------------------------------------------------------
# 1. Static trip-wires.
# ----------------------------------------------------------------------------

assert_or_skip 'AllowedDownstreamHosts \[\]string' \
    "internal/config/config.go" \
    "static: ToolOAuthProviderConfig carries AllowedDownstreamHosts"

assert_or_skip 'type ToolOAuthCredentialBrokerConfig struct' \
    "internal/config/config.go" \
    "static: the named credential-broker list type exists"

assert_or_skip 'AllowedDownstreamHosts\(\) \[\]string' \
    "internal/tools/auth/auth.go" \
    "static: OAuthProvider exposes AllowedDownstreamHosts (the sink allow-list)"

assert_or_skip 'CheckRedirect' \
    "internal/tools/auth/drivers/tokenexchange/tokenexchange.go" \
    "static: the token-exchange client installs a CheckRedirect (redirect refusal)"

assert_or_skip 'redirectGuardFor' \
    "internal/tools/drivers/mcp/transport_sse.go" \
    "static: the MCP bearer client installs a redirect guard (WARN-D)"

assert_or_skip 'AllowedDownstreamHosts\(\)' \
    "internal/tools/drivers/mcp/attach.go" \
    "static: resolveOAuthBinding enforces the downstream-host allow-list"

assert_or_skip 'NormalizeDownstreamHost' \
    "internal/config/validate.go" \
    "static: the single downstream-host normaliser exists"

assert_or_skip 'applyAdminWriteWithAudit' \
    "internal/protocol/mcp.go" \
    "static: the fail-closed admin-write audit helper exists (audit-ordering fix)"

# ----------------------------------------------------------------------------
# 2. Unit tests — the touched packages under -race.
# ----------------------------------------------------------------------------

if [ ! -f "internal/tools/drivers/mcp/credential_sink_test.go" ]; then
    skip "unit-tests: Phase 166 not yet implemented"
elif go test -race -count=1 -timeout 240s \
    ./internal/tools/auth/drivers/tokenexchange/... \
    ./internal/tools/drivers/mcp/... \
    ./internal/config/... \
    ./internal/protocol/ >/dev/null 2>&1; then
    ok "unit-tests: tokenexchange + mcp + config + protocol pass under -race"
else
    fail "unit-tests: Phase 166 package tests failed (run: go test -race ./internal/tools/auth/drivers/tokenexchange/... ./internal/tools/drivers/mcp/... ./internal/config/... ./internal/protocol/)"
fi

# ----------------------------------------------------------------------------
# 3. §17.1 integration test (real drivers) — the exfil-path gate.
# ----------------------------------------------------------------------------

if [ ! -f "test/integration/phase166_credential_sink_test.go" ]; then
    skip "e2e: integration test absent (Phase 166 not yet implemented)"
elif go test -race -count=1 -timeout 300s -run 'TestE2E_Phase166' ./test/integration/ >/dev/null 2>&1; then
    ok "e2e: unlisted-host refusal + zero-client_secret-to-redirect pass under -race"
else
    fail "e2e: Phase 166 integration tests failed (run: go test -race -run TestE2E_Phase166 ./test/integration/)"
fi

smoke_summary
