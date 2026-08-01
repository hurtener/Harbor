#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 152 smoke — agentcfg section setters carry Hooks forward (D-283):
# every section-scoped agent-config setter (set_tool_exposure,
# add_mcp_connection, skills.*, set_prompt_layers, set_llm_params) rebuilds
# the config envelope by hand, and none of them carried the Hooks section
# (D-280) forward — a section-scoped edit silently erased a pinned
# run-completion hook (CLAUDE.md §13). This smoke drives the headline
# regression live against the dev server:
#
#   agent_config.set_revision (pin hooks.run_completion)
#     -> agent_config.set_tool_exposure (an unrelated section-scoped edit)
#     -> agent_config.get asserts payload.hooks.run_completion.tool unchanged
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

SET_REVISION_URL="$(api_url /v1/agent_config/set_revision)"
SET_EXPOSURE_URL="$(api_url /v1/agent_config/set_tool_exposure)"
GET_URL="$(api_url /v1/agent_config/get)"

# Probe first so 404/405/501 -> SKIP cleanly on a build that does not mount
# set_revision (the §4.2 SKIP convention).
PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${SET_REVISION_URL}" 2>/dev/null || true)
case "${PROBE:-000}" in
    404|405|501|000|'')
        skip "phase 152: agent_config.set_revision route not present (${PROBE:-000})"
        smoke_summary
        exit 0
        ;;
esac

if [ -z "${HARBOR_DEV_TOKEN:-}" ] || ! command -v jq >/dev/null 2>&1; then
    skip "phase 152: HARBOR_DEV_TOKEN/jq unavailable — hooks-carry-forward live assertions skipped (run under 'make preflight')"
    smoke_summary
    exit 0
fi

TOKEN="${HARBOR_DEV_TOKEN}"
ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")
AGENT_ID="phase152-smoke-agent"

agentcfg_post() {
    local url="$1" body="$2"
    curl -sS -X POST "${url}" -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' -d "${body}" 2>/dev/null
}

# --- Pin a run-completion hook via set_revision. ---
SET_REV_BODY="$(agentcfg_post "${SET_REVISION_URL}" "$(cat <<JSON
{"agent_id":"${AGENT_ID}","payload":{"hooks":{"run_completion":{"tool":"phase152_smoke_sink","timeout_ms":4000}}}}
JSON
)")"
HOOK_TOOL_AFTER_SET="$(printf '%s' "${SET_REV_BODY}" | jq -r '.revision.payload.hooks.run_completion.tool // empty')"
if [ "${HOOK_TOOL_AFTER_SET}" = "phase152_smoke_sink" ]; then
    ok "phase 152: set_revision pins hooks.run_completion.tool=phase152_smoke_sink"
else
    fail "phase 152: set_revision did not record the hooks section: ${SET_REV_BODY}"
fi

# --- An UNRELATED section-scoped edit: set_tool_exposure (owns only the
#     tool-exposure section) — before this phase, this silently wiped the
#     hook pinned above. ---
EXPO_BODY="$(agentcfg_post "${SET_EXPOSURE_URL}" "$(cat <<JSON
{"agent_id":"${AGENT_ID}","tool_exposure":{"paused_servers":["phase152-smoke-srv"]}}
JSON
)")"
EXPO_PAUSED="$(printf '%s' "${EXPO_BODY}" | jq -r '.revision.payload.tool_exposure.paused_servers[0] // empty')"
if [ "${EXPO_PAUSED}" = "phase152-smoke-srv" ]; then
    ok "phase 152: set_tool_exposure applies its own section (paused_servers)"
else
    fail "phase 152: set_tool_exposure did not apply: ${EXPO_BODY}"
fi

# --- agent_config.get asserts the hooks section is STILL intact. ---
GET_BODY="$(agentcfg_post "${GET_URL}" "{\"agent_id\":\"${AGENT_ID}\"}")"
HOOK_TOOL_AFTER_EXPO="$(printf '%s' "${GET_BODY}" | jq -r '.revision.payload.hooks.run_completion.tool // empty')"
HOOK_TIMEOUT_AFTER_EXPO="$(printf '%s' "${GET_BODY}" | jq -r '.revision.payload.hooks.run_completion.timeout_ms // empty')"
if [ "${HOOK_TOOL_AFTER_EXPO}" = "phase152_smoke_sink" ] && [ "${HOOK_TIMEOUT_AFTER_EXPO}" = "4000" ]; then
    ok "phase 152: hooks.run_completion survives an interleaved set_tool_exposure edit (D-283 fixed)"
else
    fail "phase 152: hooks.run_completion did NOT survive set_tool_exposure (D-283 regression): ${GET_BODY}"
fi

# --- Sanity: the tool-exposure edit itself is still visible on the final
#     get (proves this was a real section-scoped write, not a no-op). ---
GET_PAUSED="$(printf '%s' "${GET_BODY}" | jq -r '.revision.payload.tool_exposure.paused_servers[0] // empty')"
if [ "${GET_PAUSED}" = "phase152-smoke-srv" ]; then
    ok "phase 152: the interleaved tool-exposure edit is itself intact on the final get"
else
    fail "phase 152: the interleaved tool-exposure edit was lost on the final get: ${GET_BODY}"
fi

smoke_summary
