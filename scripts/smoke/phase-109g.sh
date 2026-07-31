#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 109g smoke — MCP App documents render inline on every artifact driver.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# This phase re-scopes the LLM-context heavy threshold OUT of `ui://` App
# documents: `mcp.servers.read_resource` returns an App document INLINE up to
# a dedicated App-document cap (2 MiB), so it renders on the inmem / fs /
# sqlite / postgres stores — not just S3. Above the cap, the D-026
# offload→artifactRef fallback (the loud `mcp.resource_offloaded` bypass) is
# preserved.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# The dev bearer is resolved through common.sh's `dev_bearer`, never by a raw
# ${HARBOR_DEV_TOKEN} read: the raw read is EMPTY outside preflight, so every
# live leg below degrades to a SKIP while the script still exits 0 — "a SKIP
# that should be an OK is a bug" (AGENTS.md §4.2 item 5, issue #624).
# dev_bearer prefers the exported value and falls back to the dev server log.
HARBOR_DEV_TOKEN="$(dev_bearer)"

# Standalone battery runs (no dev server) degrade to SKIP instead of a
# healthz-000 FAIL; preflight always has the server up (no-op there).
skip_all_if_server_down "phase 109g"

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
    PROTOCOL_STATUS=$(curl -s --max-time 5 -o /tmp/phase109g.body \
        -w '%{http_code}' "${hdrs[@]}" -X POST -d "${body}" "${url}" \
        2>/dev/null || true)
    [ -z "${PROTOCOL_STATUS}" ] && PROTOCOL_STATUS='000'
    PROTOCOL_BODY=$(cat /tmp/phase109g.body 2>/dev/null || echo '{}')
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
assert_status 200 "$(api_url /healthz)" "phase 109g: healthz returns 200"

# ----------------------------------------------------------------------------
# 2. mcp.servers.read_resource is still wired (this phase changes its
#    inline-vs-offload decision for ui:// docs, not its dispatch): a request
#    missing resource_uri returns invalid_request.
# ----------------------------------------------------------------------------
if protocol_post '/v1/control/mcp.servers.read_resource' \
    '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"server_id":"nope"}' \
    'phase 109g: mcp.servers.read_resource is wired'; then
    if command -v jq >/dev/null 2>&1; then
        code=$(printf '%s' "${PROTOCOL_BODY}" | jq -r '.code // .error.code // ""' 2>/dev/null || echo "")
    else
        code=$(printf '%s' "${PROTOCOL_BODY}" | grep -o '"code"[^,]*' | head -1)
    fi
    case "${code}" in
        *invalid_request*) ok 'phase 109g: read_resource validates resource_uri (surface wired)' ;;
        *) fail "phase 109g: expected code=invalid_request, got '${code}' (status ${PROTOCOL_STATUS}): ${PROTOCOL_BODY}" ;;
    esac
fi

# ----------------------------------------------------------------------------
# 3. Static: the App-document cap branch exists — a ui:// document rides
#    inline up to the dedicated cap, bypassing the LLM-context heavy
#    threshold. Guards against a silent revert to the 32 KiB gate.
# ----------------------------------------------------------------------------
if grep -q 'appDocumentInlineCap' internal/mcpconsole/apps.go &&
    grep -q 'IsUIResourceURI(resourceURI)' internal/mcpconsole/apps.go; then
    ok 'phase 109g: apps.go scopes the App-document cap to ui:// documents'
else
    fail 'phase 109g: App-document cap branch missing from apps.go (read_resource may still gate ui:// docs on the 32 KiB heavy threshold)'
fi

smoke_summary
