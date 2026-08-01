#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 203 — Wire-carried per-user credential INJECTION for dynamically-added
# receiver-style MCP servers (HA-37 / D-346).
#
# The exfil guard (modeled on phase-199.sh): with the fail-closed opt-in OFF (the
# default preflight boot, no HARBOR_ALLOW_WIRE_INJECTION), an add_mcp_connection
# descriptor carrying an `injection` mapping is REJECTED 400 — a runtime-added
# receiver connection cannot deliver a per-user credential unless the operator
# has explicitly opted in. A connection WITHOUT an injection mapping is
# unaffected by the gate. The `add_mcp_connection` method and the new
# `AgentConfigMCPCredentialInjectionDescriptor` wire type are present in the
# regenerated manifest + generated docs. 404/405/501 → SKIP so preflight stays
# green pre-203.

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
# Static trip-wires — the wire type + method carry the surface in the generated
# artifacts (the lockstep gate keeps them in sync with the Go source).
# ----------------------------------------------------------------------------
if grep -q "AgentConfigMCPCredentialInjectionDescriptor" web/console/src/lib/protocol/wire-manifest.gen.json 2>/dev/null; then
    ok "static: injection descriptor wire type is in the regenerated wire manifest"
else
    skip "static: injection descriptor absent from wire-manifest.gen.json (pre-203 build)"
fi
if grep -q "AgentConfigMCPCredentialInjectionDescriptor" docs/site/protocol/types.md 2>/dev/null; then
    ok "static: injection descriptor wire type is in the generated protocol types doc"
else
    skip "static: injection descriptor absent from docs/site/protocol/types.md (pre-203 build)"
fi
if grep -q "agent_config.add_mcp_connection" docs/site/protocol/methods.md 2>/dev/null; then
    ok "static: add_mcp_connection is in the generated protocol methods doc"
else
    skip "static: add_mcp_connection absent from methods.md (pre-203 build)"
fi

# ----------------------------------------------------------------------------
# Live-server assertions.
# ----------------------------------------------------------------------------
ADD_CONN_URL="$(api_url /v1/agent_config/add_mcp_connection)"

PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${ADD_CONN_URL}" 2>/dev/null || true)
case "${PROBE:-000}" in
    404|405|501|000|'')
        skip "phase 203: agent_config.add_mcp_connection route not present (${PROBE:-000})"
        smoke_summary
        exit 0
        ;;
esac

if [ -z "${HARBOR_DEV_TOKEN:-}" ]; then
    skip "phase 203: HARBOR_DEV_TOKEN unavailable — live assertions skipped (run under 'make preflight')"
    smoke_summary
    exit 0
fi

TOKEN="${HARBOR_DEV_TOKEN}"
ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")
AGENT_ID="phase203-smoke-agent"

post_code() {
    local url="$1" body="$2"
    curl -s -o /dev/null -w '%{http_code}' -X POST "${url}" \
        -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' -d "${body}" 2>/dev/null || true
}

# --- THE EXFIL GUARD: with the opt-in OFF, an add carrying an injection mapping
#     is REJECTED (400). ---
INJ_CODE="$(post_code "${ADD_CONN_URL}" \
    "{\"agent_id\":\"${AGENT_ID}\",\"connection\":{\"name\":\"recv\",\"transport\":\"http\",\"url\":\"https://mcp.example.com/sse\",\"injection\":{\"provider\":\"vendor-broker\",\"form\":\"header\",\"header\":\"x-vendor-api-key\"}}}")"
if [ "${INJ_CODE}" = "400" ]; then
    ok "phase 203: add_mcp_connection carrying an injection mapping is REJECTED (400) with the opt-in off — the D-346 fail-closed gate"
else
    fail "phase 203: an injection-carrying add returned ${INJ_CODE}, want 400 (opt-in-off exfil guard)"
fi

# --- A connection WITHOUT an injection mapping is unaffected by the gate: it
#     reaches the attach lifecycle (an unreachable example host yields a
#     `failed`/`auth_required` state or a non-gate error, NEVER the 400 the
#     injection gate returns). Proves the gate is scoped to the injection field. ---
PLAIN_CODE="$(post_code "${ADD_CONN_URL}" \
    "{\"agent_id\":\"${AGENT_ID}\",\"connection\":{\"name\":\"plain\",\"transport\":\"http\",\"url\":\"https://mcp.example.com/sse\"}}")"
case "${PLAIN_CODE}" in
    400)
        fail "phase 203: a plain (no-injection) add returned 400 — the gate must be scoped to the injection field only"
        ;;
    *)
        ok "phase 203: a plain (no-injection) add is unaffected by the injection gate (${PLAIN_CODE})"
        ;;
esac

# --- THE SECOND DOOR: the SAME fail-closed gate applies at set_revision. A
#     full-payload set_revision carrying connections[].injection is REJECTED (400)
#     with the opt-in off — an injection mapping cannot slip past the gate through
#     the revision-write door (mirrors the add_mcp_connection assertion). ---
SET_REV_URL="$(api_url /v1/agent_config/set_revision)"
SET_REV_PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${SET_REV_URL}" 2>/dev/null || true)
case "${SET_REV_PROBE:-000}" in
    404|405|501|000|'')
        skip "phase 203: agent_config.set_revision route not present (${SET_REV_PROBE:-000})"
        ;;
    *)
        SET_REV_CODE="$(post_code "${SET_REV_URL}" \
            "{\"agent_id\":\"${AGENT_ID}\",\"payload\":{\"connections\":{\"servers\":[{\"name\":\"recv\",\"transport\":\"http\",\"url\":\"https://mcp.example.com/sse\",\"injection\":{\"provider\":\"vendor-broker\",\"form\":\"header\",\"header\":\"x-vendor-api-key\"}}]}}}")"
        if [ "${SET_REV_CODE}" = "400" ]; then
            ok "phase 203: set_revision carrying an injection mapping is REJECTED (400) with the opt-in off — the D-346 gate holds at the second door"
        else
            fail "phase 203: an injection-carrying set_revision returned ${SET_REV_CODE}, want 400 (second-door exfil guard)"
        fi
        ;;
esac

smoke_summary
