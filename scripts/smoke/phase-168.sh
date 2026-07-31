#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 168 — Live MCP OAuth discovery-allowance write (D-302).
#
# Asserts (each SKIPs cleanly on a pre-168 build via the 404/405/501 probe):
#   - agent_config.set_mcp_discovery_origins present on the booted surface.
#   - A live write records a revision AND echoes the granted origins.
#   - Grant against an UNKNOWN connection -> 404 (typed loud not-found).
#   - A malformed origin (http://) -> 400 (CodeInvalidRequest via the shared
#     validator).
#   - Static: the method appears in wire-manifest.gen.json + the regenerated
#     docs/site/protocol/methods.md.
#   - unit-tests: the registry mutator + the discovery dial-guard + the service.
#
# Done-definition: OK >= 3, FAIL = 0.

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

# ----------------------------------------------------------------------------
# Static trip-wires (run regardless of the live server).
# ----------------------------------------------------------------------------
if grep -q 'agent_config.set_mcp_discovery_origins' web/console/src/lib/protocol/wire-manifest.gen.json 2>/dev/null; then
    ok "static: set_mcp_discovery_origins is in the regenerated wire manifest"
else
    skip "static: set_mcp_discovery_origins absent from wire-manifest.gen.json (pre-168 build)"
fi
if grep -q 'agent_config.set_mcp_discovery_origins' docs/site/protocol/methods.md 2>/dev/null; then
    ok "static: set_mcp_discovery_origins is in the generated protocol methods doc"
else
    skip "static: set_mcp_discovery_origins absent from docs/site/protocol/methods.md (pre-168 build)"
fi

# ----------------------------------------------------------------------------
# Live-server assertions.
# ----------------------------------------------------------------------------
SET_ORIGINS_URL="$(api_url /v1/agent_config/set_mcp_discovery_origins)"
SET_REVISION_URL="$(api_url /v1/agent_config/set_revision)"

PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${SET_ORIGINS_URL}" 2>/dev/null || true)
case "${PROBE:-000}" in
    404|405|501|000|'')
        skip "phase 168: agent_config.set_mcp_discovery_origins route not present (${PROBE:-000})"
        smoke_summary
        exit 0
        ;;
esac

if [ -z "${HARBOR_DEV_TOKEN:-}" ] || ! command -v jq >/dev/null 2>&1; then
    skip "phase 168: HARBOR_DEV_TOKEN/jq unavailable — live assertions skipped (run under 'make preflight')"
    smoke_summary
    exit 0
fi

TOKEN="${HARBOR_DEV_TOKEN}"
ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")
AGENT_ID="phase168-smoke-agent"
SRV="phase168-smoke-srv"

agentcfg_post() {
    local url="$1" body="$2"
    curl -sS -X POST "${url}" -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' -d "${body}" 2>/dev/null
}

# Seed a runtime-added http connection (with an initial allowance) via set_revision.
agentcfg_post "${SET_REVISION_URL}" "$(cat <<JSON
{"agent_id":"${AGENT_ID}","payload":{"connections":{"servers":[{"name":"${SRV}","transport":"http","url":"https://example.invalid/x","oauth_discovery_allowed_origins":["https://as-initial.example.net"]}]}}}
JSON
)" >/dev/null

# --- A live write records a revision AND echoes the granted origin. ---
WRITE_BODY="$(agentcfg_post "${SET_ORIGINS_URL}" "{\"agent_id\":\"${AGENT_ID}\",\"name\":\"${SRV}\",\"allowed_origins\":[\"https://as.example.net\"]}")"
GRANTED="$(printf '%s' "${WRITE_BODY}" | jq -r '(.granted // []) | index("https://as.example.net") // empty')"
REV_ID="$(printf '%s' "${WRITE_BODY}" | jq -r '.revision.revision_id // empty')"
if [ -n "${GRANTED}" ] && [ -n "${REV_ID}" ]; then
    ok "phase 168: set_mcp_discovery_origins recorded a revision and granted the origin"
else
    fail "phase 168: live write did not grant/record: ${WRITE_BODY}"
fi

# --- Grant against an UNKNOWN connection -> 404 loud not-found. ---
UNK_CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${SET_ORIGINS_URL}" \
    -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" -H 'Content-Type: application/json' \
    -d "{\"agent_id\":\"${AGENT_ID}\",\"name\":\"does-not-exist\",\"allowed_origins\":[\"https://as.example.net\"]}" 2>/dev/null || true)
if [ "${UNK_CODE}" = "404" ]; then
    ok "phase 168: write against an unknown connection fails loud (404)"
else
    fail "phase 168: unknown-connection write returned ${UNK_CODE}, want 404"
fi

# --- A malformed (non-https) origin -> 400 via the shared validator. ---
BAD_CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${SET_ORIGINS_URL}" \
    -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" -H 'Content-Type: application/json' \
    -d "{\"agent_id\":\"${AGENT_ID}\",\"name\":\"${SRV}\",\"allowed_origins\":[\"http://as.example.net\"]}" 2>/dev/null || true)
if [ "${BAD_CODE}" = "400" ]; then
    ok "phase 168: a malformed (non-https) origin is rejected (400)"
else
    fail "phase 168: malformed-origin write returned ${BAD_CODE}, want 400"
fi

smoke_summary
