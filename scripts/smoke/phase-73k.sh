#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
# Phase 73k smoke — Console MCP Connections page.
#
# Phase 73k ships the Console MCP Connections page bundled with its
# `[wave-13-extends]` Protocol surface:
#   - mcp.servers.list
#   - mcp.servers.get
#   - mcp.servers.resources
#   - mcp.servers.prompts
#   - mcp.servers.refresh_discovery (control-claim gated, D-066)
#   - mcp.servers.probe (control-claim gated, D-066)
#   - mcp.servers.health
#   - mcp.servers.bindings.list
#   - mcp.servers.policy
#   - mcp.servers.refresh_binding (tools.admin gated, D-066 + D-083)
#   - mcp.servers.revoke_binding  (tools.admin gated, D-066 + D-083)
#   - mcp.servers.set_raw_html_trust (tools.admin gated; emits the
#     new mcp.raw_html_trust_toggled audit event)
#
# OAuth Connect / Reconnect / Revoke on the OAuth & Auth tab is a
# pure consumer of the SHIPPED tool.auth_required / tool.auth_completed
# event flow (D-083) — NO parallel binding-state machine.
#
# The 404/405/501-on-unimplemented-surface convention keeps this
# script green against earlier-phase builds.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

BASE="${HARBOR_BASE_URL:-http://127.0.0.1:18080}"
TOKEN_OPERATOR="${HARBOR_DEV_TOKEN:-}"
TOKEN_ADMIN="${HARBOR_DEV_ADMIN_TOKEN:-${HARBOR_DEV_TOKEN:-}}"
TOKEN_CONTROL="${HARBOR_DEV_CONTROL_TOKEN:-${HARBOR_DEV_ADMIN_TOKEN:-${HARBOR_DEV_TOKEN:-}}}"

# protocol_post <token> <method-path> <json-body> <description>
# Helper: POST to the Phase 60 HTTP+SSE Protocol surface with the
# given bearer token. Prints the response body; returns the HTTP
# status code via $?'s sibling output to a global variable
# `PROTOCOL_STATUS`. Skips when curl/jq missing or the surface
# 404/405/501s (the standard skip convention).
PROTOCOL_STATUS=000
PROTOCOL_BODY=''
protocol_post() {
    local token="$1" path="$2" body="$3" desc="$4"
    if ! command -v curl >/dev/null 2>&1; then
        skip "${desc}: curl not available"
        return 1
    fi
    if ! command -v jq >/dev/null 2>&1; then
        skip "${desc}: jq not available"
        return 1
    fi
    local url
    url="$(api_url "${path}")"
    local hdrs=(-H 'Content-Type: application/json')
    if [ -n "${token}" ]; then
        hdrs+=(-H "Authorization: Bearer ${token}")
    fi
    PROTOCOL_STATUS=$(curl -s --max-time 5 -o /tmp/phase73k.body \
        -w '%{http_code}' "${hdrs[@]}" -X POST -d "${body}" "${url}" \
        2>/dev/null || true)
    if [ -z "${PROTOCOL_STATUS}" ]; then
        PROTOCOL_STATUS='000'
    fi
    PROTOCOL_BODY=$(cat /tmp/phase73k.body 2>/dev/null || echo '{}')
    case "${PROTOCOL_STATUS}" in
        404|405|501|000)
            skip "${desc}: ${PROTOCOL_STATUS} (surface not yet implemented)"
            return 1
            ;;
    esac
    return 0
}

# assert_2xx_json <jq_path> <desc>
assert_2xx_json() {
    local jq_path="$1" desc="$2"
    case "${PROTOCOL_STATUS}" in
        2*) : ;;
        *)
            fail "${desc}: expected 2xx, got ${PROTOCOL_STATUS}: ${PROTOCOL_BODY}"
            return
            ;;
    esac
    local val
    val=$(printf '%s' "${PROTOCOL_BODY}" | jq -r "${jq_path}" 2>/dev/null || echo "")
    if [ -n "${val}" ] && [ "${val}" != "null" ]; then
        ok "${desc}: ${jq_path} present"
    else
        fail "${desc}: ${jq_path} missing in body ${PROTOCOL_BODY}"
    fi
}

# assert_error_code <expected_code> <desc>
assert_error_code() {
    local expected="$1" desc="$2"
    case "${PROTOCOL_STATUS}" in
        2*)
            fail "${desc}: expected non-2xx, got ${PROTOCOL_STATUS}"
            return
            ;;
    esac
    local code
    code=$(printf '%s' "${PROTOCOL_BODY}" | jq -r '.code // .error.code // ""' 2>/dev/null || echo "")
    if [ "${code}" = "${expected}" ]; then
        ok "${desc}: code = ${expected}"
    else
        fail "${desc}: expected code=${expected}, got code=${code} (status ${PROTOCOL_STATUS}): ${PROTOCOL_BODY}"
    fi
}

# ----------------------------------------------------------------------------
# 1. Sanity: server up.
# ----------------------------------------------------------------------------
assert_status 200 "$(api_url /healthz)" "phase 73k: healthz returns 200"

# ----------------------------------------------------------------------------
# 2. mcp.servers.list with valid identity → 2xx + .servers is an array.
# ----------------------------------------------------------------------------
if protocol_post "${TOKEN_OPERATOR}" '/v1/control/mcp.servers.list' \
    '{"identity":{"tenant":"dev","user":"dev","session":"dev"}}' \
    'phase 73k: mcp.servers.list (operator scope)'; then
    assert_2xx_json '.servers' 'phase 73k: mcp.servers.list returns .servers array'
fi

# ----------------------------------------------------------------------------
# 3. mcp.servers.list with missing identity → identity_required.
# ----------------------------------------------------------------------------
if protocol_post '' '/v1/control/mcp.servers.list' '{}' \
    'phase 73k: mcp.servers.list rejects missing identity'; then
    assert_error_code 'identity_required' \
        'phase 73k: mcp.servers.list missing identity → identity_required'
fi

# ----------------------------------------------------------------------------
# 4. mcp.servers.get for an unknown server → not_found.
# ----------------------------------------------------------------------------
if protocol_post "${TOKEN_OPERATOR}" '/v1/control/mcp.servers.get' \
    '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"name":"__nonexistent__"}' \
    'phase 73k: mcp.servers.get unknown name'; then
    assert_error_code 'not_found' \
        'phase 73k: mcp.servers.get unknown → not_found'
fi

# ----------------------------------------------------------------------------
# 5. mcp.servers.resources for a likely-empty dev runtime → 2xx + array.
# ----------------------------------------------------------------------------
if protocol_post "${TOKEN_OPERATOR}" '/v1/control/mcp.servers.resources' \
    '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"name":"smoke-mcp"}' \
    'phase 73k: mcp.servers.resources'; then
    case "${PROTOCOL_STATUS}" in
        2*)
            ok 'phase 73k: mcp.servers.resources reachable (2xx)'
            ;;
        404)
            skip 'phase 73k: mcp.servers.resources — no smoke-mcp configured'
            ;;
        *)
            assert_error_code 'not_found' \
                'phase 73k: mcp.servers.resources unknown → not_found'
            ;;
    esac
fi

# ----------------------------------------------------------------------------
# 6. mcp.servers.prompts — same shape.
# ----------------------------------------------------------------------------
if protocol_post "${TOKEN_OPERATOR}" '/v1/control/mcp.servers.prompts' \
    '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"name":"smoke-mcp"}' \
    'phase 73k: mcp.servers.prompts'; then
    case "${PROTOCOL_STATUS}" in
        2*) ok 'phase 73k: mcp.servers.prompts reachable (2xx)' ;;
        404) skip 'phase 73k: mcp.servers.prompts — no smoke-mcp configured' ;;
        *) assert_error_code 'not_found' \
             'phase 73k: mcp.servers.prompts unknown → not_found' ;;
    esac
fi

# ----------------------------------------------------------------------------
# 7. mcp.servers.health — 2xx with handshake-latency buckets when a
# server is configured; otherwise not_found.
# ----------------------------------------------------------------------------
if protocol_post "${TOKEN_OPERATOR}" '/v1/control/mcp.servers.health' \
    '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"name":"smoke-mcp"}' \
    'phase 73k: mcp.servers.health'; then
    case "${PROTOCOL_STATUS}" in
        2*) ok 'phase 73k: mcp.servers.health reachable (2xx)' ;;
        404) skip 'phase 73k: mcp.servers.health — no smoke-mcp configured' ;;
        *) assert_error_code 'not_found' \
             'phase 73k: mcp.servers.health unknown → not_found' ;;
    esac
fi

# ----------------------------------------------------------------------------
# 8. mcp.servers.policy — read-only ToolPolicy projection.
# ----------------------------------------------------------------------------
if protocol_post "${TOKEN_OPERATOR}" '/v1/control/mcp.servers.policy' \
    '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"name":"smoke-mcp"}' \
    'phase 73k: mcp.servers.policy'; then
    case "${PROTOCOL_STATUS}" in
        2*) ok 'phase 73k: mcp.servers.policy reachable (2xx)' ;;
        404) skip 'phase 73k: mcp.servers.policy — no smoke-mcp configured' ;;
        *) assert_error_code 'not_found' \
             'phase 73k: mcp.servers.policy unknown → not_found' ;;
    esac
fi

# ----------------------------------------------------------------------------
# 9. mcp.servers.bindings.list — operator's own bindings only.
# ----------------------------------------------------------------------------
if protocol_post "${TOKEN_OPERATOR}" '/v1/control/mcp.servers.bindings.list' \
    '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"name":"smoke-mcp"}' \
    'phase 73k: mcp.servers.bindings.list'; then
    case "${PROTOCOL_STATUS}" in
        2*) ok 'phase 73k: mcp.servers.bindings.list reachable (2xx)' ;;
        404) skip 'phase 73k: mcp.servers.bindings.list — no smoke-mcp configured' ;;
        *) assert_error_code 'not_found' \
             'phase 73k: mcp.servers.bindings.list unknown → not_found' ;;
    esac
fi

# ----------------------------------------------------------------------------
# 10. mcp.servers.refresh_discovery without control claim → scope_mismatch.
# ----------------------------------------------------------------------------
if protocol_post "${TOKEN_OPERATOR}" '/v1/control/mcp.servers.refresh_discovery' \
    '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"name":"smoke-mcp"}' \
    'phase 73k: mcp.servers.refresh_discovery (operator scope, expect scope_mismatch)'; then
    assert_error_code 'scope_mismatch' \
        'phase 73k: refresh_discovery without control claim → scope_mismatch'
fi

# ----------------------------------------------------------------------------
# 11. mcp.servers.probe without control claim → scope_mismatch.
# ----------------------------------------------------------------------------
if protocol_post "${TOKEN_OPERATOR}" '/v1/control/mcp.servers.probe' \
    '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"name":"smoke-mcp"}' \
    'phase 73k: mcp.servers.probe (operator scope, expect scope_mismatch)'; then
    assert_error_code 'scope_mismatch' \
        'phase 73k: probe without control claim → scope_mismatch'
fi

# ----------------------------------------------------------------------------
# 12. mcp.servers.refresh_binding without tools.admin → scope_mismatch.
#     Only run when an operator-only token is genuinely distinct from
#     the admin token. In plain `harbor dev` the same dev token carries
#     all scopes, so the scope check passes; the call then reaches the
#     OAuth dispatch which under the V1 dev posture (Phase 83w F6) is
#     served by mcpconsole.NoOAuthAccessor — it fails loudly with
#     ErrNoOAuthConfigured (§13). Asserting scope_mismatch in that
#     posture is structurally impossible.
# ----------------------------------------------------------------------------
if [ -n "${TOKEN_ADMIN}" ] && [ "${TOKEN_ADMIN}" != "${TOKEN_OPERATOR}" ]; then
    if protocol_post "${TOKEN_OPERATOR}" '/v1/control/mcp.servers.refresh_binding' \
        '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"name":"smoke-mcp","principal_id":"smoke"}' \
        'phase 73k: mcp.servers.refresh_binding (operator scope, expect scope_mismatch)'; then
        assert_error_code 'scope_mismatch' \
            'phase 73k: refresh_binding without tools.admin → scope_mismatch'
    fi
else
    skip 'phase 73k: refresh_binding scope-mismatch negative — no operator-only token (HARBOR_DEV_OPERATOR_TOKEN distinct from admin)'
fi

# ----------------------------------------------------------------------------
# 13. mcp.servers.revoke_binding without tools.admin → scope_mismatch.
#     Same operator-vs-admin token distinctness gate as refresh_binding.
# ----------------------------------------------------------------------------
if [ -n "${TOKEN_ADMIN}" ] && [ "${TOKEN_ADMIN}" != "${TOKEN_OPERATOR}" ]; then
    if protocol_post "${TOKEN_OPERATOR}" '/v1/control/mcp.servers.revoke_binding' \
        '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"name":"smoke-mcp","principal_id":"smoke"}' \
        'phase 73k: mcp.servers.revoke_binding (operator scope, expect scope_mismatch)'; then
        assert_error_code 'scope_mismatch' \
            'phase 73k: revoke_binding without tools.admin → scope_mismatch'
    fi
else
    skip 'phase 73k: revoke_binding scope-mismatch negative — no operator-only token (HARBOR_DEV_OPERATOR_TOKEN distinct from admin)'
fi

# ----------------------------------------------------------------------------
# 14. mcp.servers.set_raw_html_trust without tools.admin → scope_mismatch.
#     Then (if admin token present) with tools.admin → 2xx + a follow-up
#     mcp.servers.get shows .raw_html_trusted=true.
# ----------------------------------------------------------------------------
if protocol_post "${TOKEN_OPERATOR}" '/v1/control/mcp.servers.set_raw_html_trust' \
    '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"name":"smoke-mcp","trusted":true}' \
    'phase 73k: set_raw_html_trust (operator scope, expect scope_mismatch)'; then
    assert_error_code 'scope_mismatch' \
        'phase 73k: set_raw_html_trust without tools.admin → scope_mismatch'
fi

if [ -n "${TOKEN_ADMIN}" ] && [ "${TOKEN_ADMIN}" != "${TOKEN_OPERATOR}" ]; then
    if protocol_post "${TOKEN_ADMIN}" '/v1/control/mcp.servers.set_raw_html_trust' \
        '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"name":"smoke-mcp","trusted":true}' \
        'phase 73k: set_raw_html_trust (admin scope)'; then
        case "${PROTOCOL_STATUS}" in
            2*) ok 'phase 73k: set_raw_html_trust admin → 2xx' ;;
            404) skip 'phase 73k: set_raw_html_trust — no smoke-mcp configured' ;;
            *) fail "phase 73k: set_raw_html_trust admin expected 2xx, got ${PROTOCOL_STATUS}" ;;
        esac
    fi
else
    skip 'phase 73k: set_raw_html_trust admin path — no admin token configured (HARBOR_DEV_ADMIN_TOKEN)'
fi

# ----------------------------------------------------------------------------
# 15. Static guards.
# ----------------------------------------------------------------------------
if grep -q '"mcp.servers.list"' internal/protocol/methods/methods.go 2>/dev/null; then
    ok 'phase 73k: mcp.servers.list constant declared in methods.go'
else
    skip 'phase 73k: methods.go does not yet declare mcp.servers.list (pre-Phase 73k build)'
fi

# The mcp.raw_html_trust_toggled audit event is registered in the
# canonical event taxonomy at internal/events/events.go (alongside
# topology.changed) — the closed EventType registry, not a separate
# internal/audit/events.go file. Deviation documented in the Phase 73k
# plan (the plan referenced internal/audit/events.go which does not
# exist; audit events live in the events taxonomy).
if grep -q 'EventTypeMCPRawHTMLTrustToggled' internal/events/events.go 2>/dev/null; then
    ok 'phase 73k: EventTypeMCPRawHTMLTrustToggled registered in internal/events'
else
    skip 'phase 73k: mcp.raw_html_trust_toggled event not yet registered (pre-Phase 73k build)'
fi

# ----------------------------------------------------------------------------
# 16. D-093 / D-132 (Wave 13 §17.5 W10): the `cmd/harbor-gen-protocol-ts`
#     generator was never built — `protocol.ts` is hand-maintained. The
#     checkpoint corrected the formerly-false `CODE GENERATED … DO NOT
#     EDIT` header to an accurate "HAND-MAINTAINED" notice.
# ----------------------------------------------------------------------------
if [ -f web/console/src/lib/protocol.ts ]; then
    if grep -q 'HAND-MAINTAINED' web/console/src/lib/protocol.ts 2>/dev/null; then
        ok 'phase 73k: protocol.ts carries the accurate hand-maintained header (D-093 / D-132)'
    else
        fail 'phase 73k: protocol.ts missing the hand-maintained header (D-093 / D-132)'
    fi
else
    skip 'phase 73k: web/console/src/lib/protocol.ts not present (pre-Phase 73k Console build)'
fi

# ----------------------------------------------------------------------------
# 17. CLAUDE.md §13: no hand-rolled fetch() in MCP Connections route
#     files. Every Protocol call must route through the typed client.
# ----------------------------------------------------------------------------
if [ -d web/console/src/routes ]; then
    MCPDIR='web/console/src/routes/(console)/mcp-connections'
    if [ -d "${MCPDIR}" ]; then
        if grep -RIn 'fetch(' "${MCPDIR}" >/dev/null 2>&1; then
            fail 'phase 73k: hand-rolled fetch() found in mcp-connections routes (CLAUDE.md §13 + D-093)'
        else
            ok 'phase 73k: no hand-rolled fetch() in mcp-connections routes'
        fi
    else
        skip 'phase 73k: mcp-connections route dir not yet present'
    fi
else
    skip 'phase 73k: web/console/src/routes not present (pre-Phase 73k Console build)'
fi

# ----------------------------------------------------------------------------
# 18. CLAUDE.md §4.5 #3 / D-062: no bespoke per-MCP-server renderer.
#     The Resources / Prompts tabs must consume the canonical renderer
#     registry only.
# ----------------------------------------------------------------------------
if [ -d 'web/console/src/lib/chat/renderers' ]; then
    if grep -RInE 'per[-_]server[-_]renderer|bespokeMcpRenderer' \
        web/console/src/ >/dev/null 2>&1; then
        fail 'phase 73k: bespoke per-MCP-server renderer detected (brief 11 §PG-3 / D-062)'
    else
        ok 'phase 73k: no bespoke per-MCP-server renderer'
    fi
else
    skip 'phase 73k: chat/renderers/ not yet present'
fi

smoke_summary
