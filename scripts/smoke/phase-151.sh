#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 151 smoke — runtime loading-mode control on tool exposure (D-281):
# the `server_loading_modes` / `tool_loading_modes` overrides on the
# agent-config ToolExposure section, the `tools.LoadingOverrideView`
# run-start projection, the `Tool.Form` classification, the effective
# `loading_mode` on `tools.describe` (optional `agent_id`), the D-223 TS
# mirror, and the live flip round-trip against the dev server:
#
#   - static greps: ToolExposure map fields (Go + wire + TS mirror),
#     LoadingOverrideView, projection wiring, ToolForm, regenerated docs.
#   - live: an admin dev token (HARBOR_DEV_TOKEN, minted at `harbor dev`
#     boot); tools.describe a known builtin (`tool_search`) and pin its
#     boot-effective loading_mode; agent_config.set_tool_exposure with a
#     tool_loading_modes flip -> re-describe with agent_id asserting the
#     EFFECTIVE loading_mode flipped; an unknown loading value -> 400
#     invalid_request (no revision recorded, asserted via agent_config.get);
#     restore (remove the override) -> describe reports the original mode;
#     unauthenticated POST to set_tool_exposure stays non-200 (the 92d gate,
#     re-checked).
#
# Deviation from the plan's smoke skeleton (CLAUDE.md §4.3): the skeleton
# sketched resolving the dev agent id via `agents.list`. The
# `agent_config.*` / `tools.describe` agent_id is an opaque per-agent KEY
# into the agentcfg.Registry (CLAUDE.md §6 clarifying note — agent_id is
# never an isolation principal), not a lookup against the Agent Registry, so
# a fixed smoke-owned id needs no agents.list round trip and avoids a false
# SKIP on a build with no self-registered agents.
#
# Conventions (AGENTS.md §4.2): 404/405/501 -> SKIP; OK >= 1 once shipped;
# use scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# --- Static: the two loading-mode override maps on the domain + wire types. ---
assert_grep_present 'ServerLoadingModes map\[string\]string' \
    internal/agentcfg/agentcfg.go 'phase 151: ToolExposure.ServerLoadingModes field present'
assert_grep_present 'ToolLoadingModes map\[string\]string' \
    internal/agentcfg/agentcfg.go 'phase 151: ToolExposure.ToolLoadingModes field present'
assert_grep_present 'ServerLoadingModes map\[string\]string' \
    internal/protocol/types/agentconfig.go 'phase 151: wire AgentConfigToolExposure.server_loading_modes present'
assert_grep_present 'LoadingModeChange' \
    internal/agentcfg/agentcfg.go 'phase 151: LoadingModeChange diff element present'

# --- Static: the LoadingOverrideView projection primitive + wiring. ---
assert_grep_present 'type LoadingOverrideView struct' \
    internal/tools/planner_view.go 'phase 151: LoadingOverrideView type present'
assert_grep_present 'func NewLoadingOverrideView' \
    internal/tools/planner_view.go 'phase 151: NewLoadingOverrideView constructor present'
assert_grep_present 'tools.NewLoadingOverrideView' \
    internal/runtime/agentcfg/projection/projection.go 'phase 151: ActivePlannerCatalogView wires LoadingOverrideView'
assert_grep_present 'func EffectiveLoadingMode' \
    internal/runtime/agentcfg/projection/projection.go 'phase 151: EffectiveLoadingMode read-surface resolver present'

# --- Static: the additive Tool.Form classification + the MCP driver stamps. ---
assert_grep_present 'type ToolForm string' \
    internal/tools/tools.go 'phase 151: ToolForm type present'
assert_grep_present 'Form:.*tools.ToolFormResource' \
    internal/tools/drivers/mcp/mcp.go 'phase 151: MCP resource descriptor stamps ToolFormResource'
assert_grep_present 'Form:.*tools.ToolFormPrompt' \
    internal/tools/drivers/mcp/mcp.go 'phase 151: MCP prompt descriptor stamps ToolFormPrompt'

# --- Static: tools.describe's optional agent_id read surface. ---
assert_grep_present 'AgentID string `json:"agent_id,omitempty"`' \
    internal/protocol/types/tools.go 'phase 151: ToolDescribeRequest.agent_id field present'
assert_grep_present 'LoadingResolver' \
    internal/tools/protocol/catalog_projector.go 'phase 151: CatalogProjector LoadingResolver seam present'

# --- Static: the D-223 TS mirror + manifest coverage. ---
assert_grep_present 'server_loading_modes' \
    web/console/src/lib/protocol/agentconfig.ts 'phase 151: TS mirror carries server_loading_modes'
assert_grep_present 'tool_loading_modes' \
    web/console/src/lib/protocol/agentconfig.ts 'phase 151: TS mirror carries tool_loading_modes'
assert_grep_present 'AgentConfigLoadingModeChange' \
    web/console/src/lib/protocol/wire-manifest.gen.json 'phase 151: manifest covers AgentConfigLoadingModeChange'

# --- Static: the generated Protocol docs regenerated. ---
assert_grep_present 'server_loading_modes' \
    docs/site/protocol/types.md 'phase 151: generated types.md carries server_loading_modes'

# --- Live: bootstrap-free (HARBOR_DEV_TOKEN, minted at `harbor dev` boot,
#     exported by preflight). Probe first so 404/405/501 -> SKIP cleanly on
#     a build that does not mount set_tool_exposure. ---
SET_EXPOSURE_URL="$(api_url /v1/agent_config/set_tool_exposure)"
DESCRIBE_URL="$(api_url /v1/tools/describe)"
GET_URL="$(api_url /v1/agent_config/get)"

PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${SET_EXPOSURE_URL}" 2>/dev/null || true)
case "${PROBE:-000}" in
    404|405|501|000|'')
        skip "phase 151: agent_config.set_tool_exposure route not present (${PROBE:-000})"
        smoke_summary
        exit 0
        ;;
esac

# Unauthenticated POST stays non-200 (the 92d admin gate, re-checked here
# because this phase deepens the same section).
if [ "${PROBE}" = "200" ]; then
    fail "phase 151: unauthenticated set_tool_exposure answered 200 — admin gate missing"
else
    ok "phase 151: unauthenticated set_tool_exposure is non-200 (${PROBE})"
fi

if [ -z "${HARBOR_DEV_TOKEN:-}" ] || ! command -v jq >/dev/null 2>&1; then
    skip "phase 151: HARBOR_DEV_TOKEN/jq unavailable — live flip assertions skipped (run under 'make preflight')"
    smoke_summary
    exit 0
fi

TOKEN="${HARBOR_DEV_TOKEN}"
ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")
AGENT_ID="phase151-smoke-agent"

describe_loading_mode() {
    local tool_name="$1"
    curl -sS -X POST "${DESCRIBE_URL}" -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' \
        -d "{\"id\":\"${tool_name}\",\"agent_id\":\"${AGENT_ID}\"}" 2>/dev/null \
        | jq -r '.loading_mode // empty'
}

active_revision_id() {
    curl -sS -X POST "${GET_URL}" -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' \
        -d "{\"agent_id\":\"${AGENT_ID}\"}" 2>/dev/null \
        | jq -r '.revision.revision_id // empty'
}

# --- The loading-value edge validation is a pure agentcfg-layer check — it
#     never dereferences the catalog, so it needs no real tool to exist.
#     Unknown loading value -> 400 invalid_request; no revision recorded. ---
REV_BEFORE="$(active_revision_id)"
INVALID_BODY="$(mktemp)"
trap 'rm -f "${INVALID_BODY}"' EXIT
INVALID_STATUS=$(curl -s -o "${INVALID_BODY}" -w '%{http_code}' --max-time 5 \
    -X POST "${SET_EXPOSURE_URL}" -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
    -H 'Content-Type: application/json' \
    -d "{\"agent_id\":\"${AGENT_ID}\",\"tool_exposure\":{\"tool_loading_modes\":{\"smoke_probe_tool\":\"sometimes\"}}}")
if [ "${INVALID_STATUS}" = "400" ]; then
    CODE="$(jq -r '.code // empty' "${INVALID_BODY}" 2>/dev/null || true)"
    if [ "${CODE}" = "invalid_request" ]; then
        ok "phase 151: an unknown loading value fails loud (400 invalid_request)"
    else
        fail "phase 151: unknown loading value 400 body carried code='${CODE}', want invalid_request"
    fi
else
    fail "phase 151: unknown loading value expected 400, got ${INVALID_STATUS}"
fi
REV_AFTER="$(active_revision_id)"
if [ "${REV_BEFORE}" = "${REV_AFTER}" ]; then
    ok "phase 151: the invalid set recorded NO new revision (${REV_AFTER:-<none>})"
else
    fail "phase 151: a revision was recorded on an invalid loading value (before=${REV_BEFORE:-<none>} after=${REV_AFTER:-<none>})"
fi

# --- The describe/flip round trip needs ONE real catalog tool. The example
#     dev config ships with no built-in tools or MCP servers registered by
#     default (tools.built_in / tools.mcp_servers are both empty), so this
#     resolves the first tools.list entry and SKIPs the flip assertions
#     (rather than false-failing) when the catalog is empty on this build. ---
TOOL_NAME="$(curl -sS -X POST "$(api_url /v1/tools/list)" -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
    -H 'Content-Type: application/json' -d '{}' 2>/dev/null | jq -r '.tools[0].id // empty')"

if [ -z "${TOOL_NAME}" ]; then
    skip "phase 151: no catalog tool registered on this build's config — describe/flip round trip skipped"
    smoke_summary
    exit 0
fi

BOOT_MODE="$(describe_loading_mode "${TOOL_NAME}")"
if [ -n "${BOOT_MODE}" ]; then
    ok "phase 151: tools.describe(${TOOL_NAME}) boot-effective loading_mode=${BOOT_MODE}"
else
    fail "phase 151: tools.describe(${TOOL_NAME}) returned no loading_mode"
fi

# Flip to the OPPOSITE of the boot-effective mode via set_tool_exposure.
if [ "${BOOT_MODE}" = "deferred" ]; then
    TARGET_MODE="always"
else
    TARGET_MODE="deferred"
fi
FLIP_STATUS=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST "${SET_EXPOSURE_URL}" -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
    -H 'Content-Type: application/json' \
    -d "{\"agent_id\":\"${AGENT_ID}\",\"tool_exposure\":{\"tool_loading_modes\":{\"${TOOL_NAME}\":\"${TARGET_MODE}\"}}}")
if [ "${FLIP_STATUS}" = "200" ]; then
    ok "phase 151: set_tool_exposure flips ${TOOL_NAME} to ${TARGET_MODE} (200)"
else
    fail "phase 151: set_tool_exposure flip expected 200, got ${FLIP_STATUS}"
fi

FLIPPED_MODE="$(describe_loading_mode "${TOOL_NAME}")"
if [ "${FLIPPED_MODE}" = "${TARGET_MODE}" ]; then
    ok "phase 151: tools.describe(${TOOL_NAME}, agent_id) reports the EFFECTIVE loading_mode=${TARGET_MODE}"
else
    fail "phase 151: tools.describe post-flip loading_mode='${FLIPPED_MODE}', want ${TARGET_MODE}"
fi

# Restore: remove the override -> describe reports the original mode.
RESTORE_STATUS=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST "${SET_EXPOSURE_URL}" -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
    -H 'Content-Type: application/json' \
    -d "{\"agent_id\":\"${AGENT_ID}\",\"tool_exposure\":{}}")
if [ "${RESTORE_STATUS}" = "200" ]; then
    ok "phase 151: set_tool_exposure restore (200)"
else
    fail "phase 151: restore expected 200, got ${RESTORE_STATUS}"
fi
RESTORED_MODE="$(describe_loading_mode "${TOOL_NAME}")"
if [ "${RESTORED_MODE}" = "${BOOT_MODE}" ]; then
    ok "phase 151: tools.describe reports the original loading_mode=${BOOT_MODE} after restore"
else
    fail "phase 151: tools.describe post-restore loading_mode='${RESTORED_MODE}', want ${BOOT_MODE}"
fi

smoke_summary
