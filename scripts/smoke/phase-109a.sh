#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 109a smoke — runtime + Protocol surface for MCP Apps.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# This phase ships:
#   - `mcp.servers.read_resource` — fetch a ui:// resource's HTML under the
#     identity triple, honouring the D-026 heavy-content safety net.
#   - the app-tool-call proxy (`mcp.apps.call_tool`) that re-enters the
#     existing approval / OAuth / identity tool-safety path.
#   - the driver `_meta.ui.resourceUri` parse + ui:// scheme recognition.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

TOKEN_OPERATOR="${HARBOR_DEV_TOKEN:-}"

# protocol_post <method-path> <json-body> <description>
# POST to the control Protocol surface; SKIP on 404/405/501 (surface
# absent). Sets PROTOCOL_STATUS / PROTOCOL_BODY for the caller.
PROTOCOL_STATUS=000
PROTOCOL_BODY=''
protocol_post() {
    local path="$1" body="$2" desc="$3"
    if ! command -v curl >/dev/null 2>&1; then
        skip "${desc}: curl not available"
        return 1
    fi
    local url
    url="$(api_url "${path}")"
    local hdrs=(-H 'Content-Type: application/json')
    if [ -n "${TOKEN_OPERATOR}" ]; then
        hdrs+=(-H "Authorization: Bearer ${TOKEN_OPERATOR}")
    fi
    PROTOCOL_STATUS=$(curl -s --max-time 5 -o /tmp/phase109a.body \
        -w '%{http_code}' "${hdrs[@]}" -X POST -d "${body}" "${url}" \
        2>/dev/null || true)
    [ -z "${PROTOCOL_STATUS}" ] && PROTOCOL_STATUS='000'
    PROTOCOL_BODY=$(cat /tmp/phase109a.body 2>/dev/null || echo '{}')
    case "${PROTOCOL_STATUS}" in
        404|405|501|000)
            skip "${desc}: ${PROTOCOL_STATUS} (surface not yet implemented)"
            return 1
            ;;
    esac
    return 0
}

# assert_error_code <expected_code> <desc>
assert_error_code() {
    local expected="$1" desc="$2"
    local code
    if command -v jq >/dev/null 2>&1; then
        code=$(printf '%s' "${PROTOCOL_BODY}" | jq -r '.code // .error.code // ""' 2>/dev/null || echo "")
    else
        code=$(printf '%s' "${PROTOCOL_BODY}" | grep -o '"code"[^,]*' | head -1)
    fi
    case "${code}" in
        *"${expected}"*) ok "${desc}: code=${expected} (surface wired)" ;;
        *) fail "${desc}: expected code=${expected}, got '${code}' (status ${PROTOCOL_STATUS}): ${PROTOCOL_BODY}" ;;
    esac
}

# ----------------------------------------------------------------------------
# 1. Sanity: server up.
# ----------------------------------------------------------------------------
assert_status 200 "$(api_url /healthz)" "phase 109a: healthz returns 200"

# ----------------------------------------------------------------------------
# 2. mcp.servers.read_resource is wired: a request missing resource_uri
#    (but identity-valid) returns invalid_request — proving the AppsSurface
#    dispatches the method rather than 404'ing it as unknown.
# ----------------------------------------------------------------------------
if protocol_post '/v1/control/mcp.servers.read_resource' \
    '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"server_id":"nope"}' \
    'phase 109a: mcp.servers.read_resource is wired'; then
    assert_error_code 'invalid_request' 'phase 109a: read_resource validates resource_uri'
fi

# ----------------------------------------------------------------------------
# 3. mcp.apps.call_tool is wired: a request missing the tool name returns
#    invalid_request — proving the proxy method dispatches.
# ----------------------------------------------------------------------------
if protocol_post '/v1/control/mcp.apps.call_tool' \
    '{"identity":{"tenant":"dev","user":"dev","session":"dev"}}' \
    'phase 109a: mcp.apps.call_tool is wired'; then
    assert_error_code 'invalid_request' 'phase 109a: call_tool validates tool name'
fi

# ----------------------------------------------------------------------------
# 4. Static: the new methods are registered in the single-source methods file.
# ----------------------------------------------------------------------------
if grep -q 'mcp.servers.read_resource' internal/protocol/methods/methods.go &&
    grep -q 'mcp.apps.call_tool' internal/protocol/methods/methods.go; then
    ok 'phase 109a: read_resource + call_tool registered in methods.go'
else
    fail 'phase 109a: MCP Apps methods missing from methods.go'
fi

# ----------------------------------------------------------------------------
# 5. Static: the driver parses the _meta.ui.resourceUri slot.
# ----------------------------------------------------------------------------
if grep -q 'parseAppRef' internal/tools/drivers/mcp/content.go &&
    grep -q 'resourceUri' internal/tools/drivers/mcp/content.go; then
    ok 'phase 109a: MCP driver content.go parses _meta.ui.resourceUri'
else
    fail 'phase 109a: driver _meta.ui parse path missing'
fi

smoke_summary
