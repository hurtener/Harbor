#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 198 smoke — live-layer idempotent MCP re-attach (HA-33, D-339):
# `agent_config.add_mcp_connection` for a name with a still-live same-name
# registration is an atomic upsert (deregister the old tools + close the old
# transport, then register the new connection) instead of failing on a
# duplicate-tool-name collision.
#
#   - static: agent_config.add_mcp_connection is a known method (methods.go +
#     the generated wire manifest) — the verb the idempotency rides on exists.
#   - live (needs a reachable dev MCP fixture in HARBOR_SMOKE_MCP_URL): add a
#     connection, then re-add the SAME name, and assert the SECOND call returns
#     state=online — the D-339 same-name replace — not a duplicate-tool-name
#     failure. SKIPs cleanly when the route is absent (404/405/501) or no
#     fixture is configured (the default preflight dev server registers none).
#
# Conventions (AGENTS.md §4.2): 404/405/501 -> SKIP; OK >= 1 once shipped;
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

# --- Static: the verb the idempotent re-attach behaviour rides on exists. ---
assert_grep_present 'MethodAgentConfigAddMCPConnection Method = "agent_config.add_mcp_connection"' \
    internal/protocol/methods/methods.go 'phase 198: agent_config.add_mcp_connection method constant present'
assert_grep_present 'agent_config.add_mcp_connection' \
    web/console/src/lib/protocol/wire-manifest.gen.json 'phase 198: wire manifest covers agent_config.add_mcp_connection'

ADD_URL="$(api_url /v1/agent_config/add_mcp_connection)"
REMOVE_URL="$(api_url /v1/agent_config/remove_mcp_connection)"

# Probe the add route first so 404/405/501 -> SKIP cleanly on a build that does
# not mount it (the §4.2 SKIP convention).
PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${ADD_URL}" 2>/dev/null || true)
case "${PROBE:-000}" in
    404|405|501|000|'')
        skip "phase 198: agent_config.add_mcp_connection route not present (${PROBE:-000})"
        smoke_summary
        exit 0
        ;;
esac

if [ -z "${HARBOR_DEV_TOKEN:-}" ] || ! command -v jq >/dev/null 2>&1; then
    skip "phase 198: HARBOR_DEV_TOKEN/jq unavailable — live re-attach assertions skipped (run under 'make preflight')"
    smoke_summary
    exit 0
fi

# The idempotent same-name replace needs a REACHABLE MCP server to add twice.
# The default preflight dev server registers none; an operator points this at a
# streamable-HTTP MCP fixture to exercise the live re-attach. Absent it, SKIP
# (never a false FAIL) — the static assertions above already give OK >= 1.
if [ -z "${HARBOR_SMOKE_MCP_URL:-}" ]; then
    skip "phase 198: no dev MCP fixture (set HARBOR_SMOKE_MCP_URL to a reachable http MCP server) — live re-attach skipped"
    smoke_summary
    exit 0
fi

TOKEN="${HARBOR_DEV_TOKEN}"
ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")
AGENT_ID="phase198-smoke-agent"
SRV="phase198-smoke-srv"

agentcfg_post() {
    local url="$1" body="$2"
    curl -sS -X POST "${url}" -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' -d "${body}" 2>/dev/null
}

add_body() {
    cat <<JSON
{"agent_id":"${AGENT_ID}","connection":{"name":"${SRV}","transport":"http","url":"${HARBOR_SMOKE_MCP_URL}"}}
JSON
}

# --- First add: attach the fixture under the smoke name. ---
ADD1="$(agentcfg_post "${ADD_URL}" "$(add_body)")"
STATE1="$(printf '%s' "${ADD1}" | jq -r '.state // empty')"
if [ "${STATE1}" = "online" ]; then
    ok "phase 198: first add_mcp_connection(${SRV}) attached (state=online)"
else
    # The fixture is unreachable/unusable — SKIP rather than FAIL (this smoke
    # gates the RE-ATTACH behaviour, not fixture reachability).
    skip "phase 198: fixture add did not reach online (state='${STATE1}') — re-attach assertion skipped: ${ADD1}"
    smoke_summary
    exit 0
fi

# --- Re-add the SAME name against the still-live registration: the D-339
#     idempotent replace must return online, NOT a duplicate-tool-name failure. ---
ADD2="$(agentcfg_post "${ADD_URL}" "$(add_body)")"
STATE2="$(printf '%s' "${ADD2}" | jq -r '.state // empty')"
if [ "${STATE2}" = "online" ]; then
    ok "phase 198: same-name re-add returns state=online (idempotent live-layer replace, D-339)"
else
    fail "phase 198: same-name re-add returned state='${STATE2}' (want online — duplicate-collision regression?): ${ADD2}"
fi

# --- Cleanup: drop the smoke connection (best-effort; the teardown reconciles
#     at the next run start). ---
RM_CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST "${REMOVE_URL}" \
    -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
    -H 'Content-Type: application/json' -d "{\"agent_id\":\"${AGENT_ID}\",\"name\":\"${SRV}\"}" 2>/dev/null || true)
if [ "${RM_CODE}" = "200" ]; then
    ok "phase 198: smoke connection removed (cleanup)"
else
    skip "phase 198: cleanup remove returned ${RM_CODE} (non-fatal)"
fi

smoke_summary
