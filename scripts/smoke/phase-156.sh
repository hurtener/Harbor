#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 156 smoke — agent_config.remove_mcp_connection (D-287): a runtime-added
# MCP connection is removed as a new revision that drops the descriptor AND
# prunes that server's tool-exposure residue, carrying every sibling section
# forward; an unknown name fails loud with a distinct typed error. This smoke
# drives the verb live against the dev server without a real attach: it seeds a
# connection + its residue via set_revision (revisioned desired state), removes
# it, and asserts the descriptor + residue are gone on get:
#
#   agent_config.set_revision (pin a connection + its paused/disabled residue)
#     -> agent_config.remove_mcp_connection (drops descriptor + prunes residue)
#     -> agent_config.get asserts connections empty + residue pruned
#     -> agent_config.remove_mcp_connection (unknown name) asserts 404 shape
#
# Conventions (AGENTS.md §4.2): 404/405/501 -> SKIP; OK >= 2 once shipped;
# use scripts/smoke/common.sh helpers.

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

SET_REVISION_URL="$(api_url /v1/agent_config/set_revision)"
REMOVE_URL="$(api_url /v1/agent_config/remove_mcp_connection)"
GET_URL="$(api_url /v1/agent_config/get)"

# Probe the remove route first so 404/405/501 -> SKIP cleanly on a build that
# does not mount it (the §4.2 SKIP convention).
PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${REMOVE_URL}" 2>/dev/null || true)
case "${PROBE:-000}" in
    404|405|501|000|'')
        skip "phase 156: agent_config.remove_mcp_connection route not present (${PROBE:-000})"
        smoke_summary
        exit 0
        ;;
esac

if [ -z "${HARBOR_DEV_TOKEN:-}" ] || ! command -v jq >/dev/null 2>&1; then
    skip "phase 156: HARBOR_DEV_TOKEN/jq unavailable — remove-connection live assertions skipped (run under 'make preflight')"
    smoke_summary
    exit 0
fi

TOKEN="${HARBOR_DEV_TOKEN}"
ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")
AGENT_ID="phase156-smoke-agent"
SRV="phase156-smoke-srv"

agentcfg_post() {
    local url="$1" body="$2"
    curl -sS -X POST "${url}" -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' -d "${body}" 2>/dev/null
}

# --- Seed a connection + its tool-exposure residue via set_revision. ---
SEED_BODY="$(agentcfg_post "${SET_REVISION_URL}" "$(cat <<JSON
{"agent_id":"${AGENT_ID}","payload":{"connections":{"servers":[{"name":"${SRV}","transport":"http","url":"https://example.invalid/x"}]},"tool_exposure":{"paused_servers":["${SRV}"],"disabled_tools":["${SRV}_echo"]}}}
JSON
)")"
SEED_CONN="$(printf '%s' "${SEED_BODY}" | jq -r '.revision.payload.connections.servers[0].name // empty')"
if [ "${SEED_CONN}" = "${SRV}" ]; then
    ok "phase 156: set_revision seeded the connection + residue"
else
    fail "phase 156: set_revision did not seed the connection: ${SEED_BODY}"
fi

# --- Remove the connection. ---
RM_BODY="$(agentcfg_post "${REMOVE_URL}" "{\"agent_id\":\"${AGENT_ID}\",\"name\":\"${SRV}\"}")"
RM_NAME="$(printf '%s' "${RM_BODY}" | jq -r '.name // empty')"
if [ "${RM_NAME}" = "${SRV}" ]; then
    ok "phase 156: remove_mcp_connection removed ${SRV} as a new revision"
else
    fail "phase 156: remove_mcp_connection did not remove ${SRV}: ${RM_BODY}"
fi

# --- get asserts the descriptor is gone AND the residue was pruned. ---
GET_BODY="$(agentcfg_post "${GET_URL}" "{\"agent_id\":\"${AGENT_ID}\"}")"
CONN_COUNT="$(printf '%s' "${GET_BODY}" | jq -r '(.revision.payload.connections.servers // []) | length')"
PAUSED_COUNT="$(printf '%s' "${GET_BODY}" | jq -r '(.revision.payload.tool_exposure.paused_servers // []) | length')"
if [ "${CONN_COUNT}" = "0" ]; then
    ok "phase 156: the removed connection descriptor is gone on get"
else
    fail "phase 156: connection descriptor still present after remove: ${GET_BODY}"
fi
if [ "${PAUSED_COUNT}" = "0" ]; then
    ok "phase 156: the removed server's tool-exposure residue was pruned"
else
    fail "phase 156: residue not pruned after remove: ${GET_BODY}"
fi

# --- Removing an unknown name fails loud (404 not-found). ---
UNK_CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${REMOVE_URL}" \
    -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
    -H 'Content-Type: application/json' -d "{\"agent_id\":\"${AGENT_ID}\",\"name\":\"does-not-exist\"}" 2>/dev/null || true)
if [ "${UNK_CODE}" = "404" ]; then
    ok "phase 156: remove of an unknown name fails loud (404)"
else
    fail "phase 156: remove of an unknown name returned ${UNK_CODE}, want 404"
fi

smoke_summary
