#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 109i smoke — MCP Apps tool-context capture + `mcp.apps.tool_context`.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# This phase is the BACKEND half of the MCP Apps "Data Delivery" lifecycle:
# the runtime captures a tool call's input + lowered result behind a declared
# `ui://` app at the invocation site, and the new `mcp.apps.tool_context`
# Protocol method reads it back (identity-scoped, heavy-content aware).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Standalone battery runs (no dev server) degrade to SKIP instead of a
# healthz-000 FAIL; preflight always has the server up (no-op there).
skip_all_if_server_down "phase 109i"

TOKEN_OPERATOR="${HARBOR_DEV_TOKEN:-}"

PROTOCOL_STATUS=000
PROTOCOL_BODY=''
# protocol_post <method-path> <json-body> <description>
# POST to the control Protocol surface; SKIP on 404/405/501 (surface absent).
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
    PROTOCOL_STATUS=$(curl -s --max-time 5 -o /tmp/phase109i.body \
        -w '%{http_code}' "${hdrs[@]}" -X POST -d "${body}" "${url}" \
        2>/dev/null || true)
    [ -z "${PROTOCOL_STATUS}" ] && PROTOCOL_STATUS='000'
    PROTOCOL_BODY=$(cat /tmp/phase109i.body 2>/dev/null || echo '{}')
    case "${PROTOCOL_STATUS}" in
        404|405|501|000)
            skip "${desc}: ${PROTOCOL_STATUS} (surface not yet implemented)"
            return 1
            ;;
    esac
    return 0
}

# ----------------------------------------------------------------------------
# 1. Sanity: server up.
# ----------------------------------------------------------------------------
assert_status 200 "$(api_url /healthz)" "phase 109i: healthz returns 200"

# ----------------------------------------------------------------------------
# 2. mcp.apps.tool_context is wired: a request missing tool_call_id returns
#    invalid_request (the method validates its body — proof the surface
#    dispatches through the AppsSurface, not a 404).
# ----------------------------------------------------------------------------
if protocol_post '/v1/control/mcp.apps.tool_context' \
    '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"server_id":"nope"}' \
    'phase 109i: mcp.apps.tool_context is wired'; then
    if command -v jq >/dev/null 2>&1; then
        code=$(printf '%s' "${PROTOCOL_BODY}" | jq -r '.code // .error.code // ""' 2>/dev/null || echo "")
    else
        code=$(printf '%s' "${PROTOCOL_BODY}" | grep -o '"code"[^,]*' | head -1)
    fi
    case "${code}" in
        *invalid_request*) ok 'phase 109i: tool_context validates tool_call_id (surface wired)' ;;
        *) fail "phase 109i: expected code=invalid_request, got '${code}' (status ${PROTOCOL_STATUS}): ${PROTOCOL_BODY}" ;;
    esac
fi

# ----------------------------------------------------------------------------
# 3. An unknown (server_id, tool_call_id) under a valid request returns
#    not_found — existence is never revealed (and never an empty 200). This
#    response is HTTP 404 with a `not_found` BODY code, distinct from an
#    unwired method's 404 (`unknown_method`): inspect the body, do NOT let the
#    blanket 404→SKIP convention swallow a real not_found.
# ----------------------------------------------------------------------------
unknown_id_check() {
    if ! command -v curl >/dev/null 2>&1; then
        skip 'phase 109i: tool_context unknown id: curl not available'
        return
    fi
    local url body status code
    url="$(api_url '/v1/control/mcp.apps.tool_context')"
    local hdrs=(-H 'Content-Type: application/json')
    [ -n "${TOKEN_OPERATOR}" ] && hdrs+=(-H "Authorization: Bearer ${TOKEN_OPERATOR}")
    status=$(curl -s --max-time 5 -o /tmp/phase109i.u.body -w '%{http_code}' \
        "${hdrs[@]}" -X POST \
        -d '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"server_id":"nope","tool_call_id":"does-not-exist"}' \
        "${url}" 2>/dev/null || true)
    [ -z "${status}" ] && status='000'
    body=$(cat /tmp/phase109i.u.body 2>/dev/null || echo '{}')
    if command -v jq >/dev/null 2>&1; then
        code=$(printf '%s' "${body}" | jq -r '.code // .error.code // ""' 2>/dev/null || echo "")
    else
        code=$(printf '%s' "${body}" | grep -o '"code"[^,]*' | head -1)
    fi
    case "${code}" in
        *not_found*) ok 'phase 109i: tool_context unknown id → not_found (surface wired)' ;;
        *unknown_method*) skip 'phase 109i: tool_context unknown id: method unwired on this build' ;;
        '') case "${status}" in
                000|404|405|501) skip "phase 109i: tool_context unknown id: ${status} (surface not yet implemented)" ;;
                *) fail "phase 109i: tool_context unknown id: empty code (status ${status}): ${body}" ;;
            esac ;;
        *) fail "phase 109i: expected code=not_found, got '${code}' (status ${status}): ${body}" ;;
    esac
}
unknown_id_check

# ----------------------------------------------------------------------------
# 4. Static: the capture seam is wired into the MCP driver (the Provider
#    captures at the invocation site) and the read seam onto the AppsAccessor.
# ----------------------------------------------------------------------------
if grep -q 'ToolContextCapturer' internal/tools/drivers/mcp/toolcontext.go &&
    grep -q 'captureToolContext' internal/tools/drivers/mcp/mcp.go; then
    ok 'phase 109i: MCP driver captures tool context at the invocation site'
else
    fail 'phase 109i: tool-context capture seam missing from the MCP driver'
fi

if grep -q 'func (a \*AppsAccessor) ToolContext' internal/mcpconsole/apps.go &&
    grep -q 'AppToolContextReader' internal/protocol/apps.go; then
    ok 'phase 109i: AppsAccessor implements the tool-context read seam'
else
    fail 'phase 109i: tool-context read seam missing from the AppsAccessor / AppsSurface'
fi

smoke_summary
