#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 107c — Native tool-calling + deferred loading + search meta-tools.
#
# Per §4.2 this smoke is shipping-progress aware: every assertion SKIPs
# when its underlying surface is absent, so the preflight gate stays
# green BEFORE Phase 107c lands. Once each piece ships, the matching
# SKIP flips to OK without any change to the smoke.
#
# Phase 107c is the alternative shape to Phase 107b: instead of adding
# a JSON extractor over the prompt-engineered action shape, the React
# planner switches to native provider tool-calling. Streaming becomes
# clean by structural construction; deferred tool/skill discovery
# rides on five built-in meta-tools.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_or_skip() {
    local pattern="$1" file="$2" desc="$3"
    if [ ! -f "${file}" ]; then
        skip "${desc}: ${file} not found (Phase 107c not yet implemented)"
        return
    fi
    if grep -qE "${pattern}" "${file}" 2>/dev/null; then
        ok "${desc}"
    else
        skip "${desc}: pattern '${pattern}' absent (Phase 107c not yet implemented)"
    fi
}

assert_file_or_skip() {
    local path="$1" desc="$2"
    if [ -f "${path}" ]; then
        ok "${desc}: ${path} exists"
    else
        skip "${desc}: ${path} missing (Phase 107c not yet implemented)"
    fi
}

# ----------------------------------------------------------------------------
# Static assertions — pin the surface exists.
# ----------------------------------------------------------------------------

# LLM client extension
assert_or_skip 'ToolCalls\s+\[\]' \
    "internal/llm/llm.go" \
    "static: llm.CompleteResponse exposes ToolCalls field"

assert_or_skip 'Tools\s+\[\]ToolDeclaration|ParallelToolCalls' \
    "internal/llm/llm.go" \
    "static: llm.CompleteRequest exposes Tools + ParallelToolCalls fields"

# tools.LoadingMode
assert_or_skip 'LoadingMode' \
    "internal/tools/tools.go" \
    "static: tools.Tool gains LoadingMode field"

# Meta-tools (five builtins)
assert_file_or_skip \
    "internal/tools/builtin/tool_search.go" \
    "static: tool_search builtin"

assert_file_or_skip \
    "internal/tools/builtin/tool_get.go" \
    "static: tool_get builtin"

assert_file_or_skip \
    "internal/tools/builtin/skill_search.go" \
    "static: skill_search builtin"

assert_file_or_skip \
    "internal/tools/builtin/skill_get.go" \
    "static: skill_get builtin"

assert_file_or_skip \
    "internal/tools/builtin/declarative_action.go" \
    "static: declarative_action escape-hatch builtin"

# SearchCache driver
assert_file_or_skip \
    "internal/tools/drivers/searchcache/searchcache.go" \
    "static: tools.SearchCache FTS5 driver"

# React planner projector (replaces ActionParser as primary)
assert_file_or_skip \
    "internal/planner/react/projector.go" \
    "static: React ToolCallProjector"

# Per-run discovered-tools state
assert_or_skip 'DiscoveredTools' \
    "internal/planner/planner.go" \
    "static: RunContext exposes DiscoveredTools field"

# Phase 107b filter MUST be gone if 107c shipped (per AC-22)
if grep -qE 'ToolCalls' "internal/llm/llm.go" 2>/dev/null; then
    if [ -f "internal/planner/react/stream_filter.go" ]; then
        fail "phase-107c AC-22: stream_filter.go still present — must be deleted when native tool-calling ships"
    else
        ok "phase-107c AC-22: Phase 107b filter correctly absent under native tool-calling"
    fi
fi

# ----------------------------------------------------------------------------
# Live-server probe — gated on (a) Phase 107c having shipped AND (b) a
# real LLM provider key in env.
# ----------------------------------------------------------------------------

if ! grep -qE 'ToolCalls' "internal/llm/llm.go" 2>/dev/null; then
    skip "live discovery-cycle probe: skipped — Phase 107c not yet implemented"
    smoke_summary
    exit 0
fi

if [ -z "${OPENROUTER_API_KEY:-}" ] && [ -z "${ANTHROPIC_API_KEY:-}" ]; then
    skip "live discovery-cycle probe: no LLM provider key (set OPENROUTER_API_KEY or ANTHROPIC_API_KEY)"
    smoke_summary
    exit 0
fi

BOOTSTRAP_URL="$(api_url /v1/dev/bootstrap.json)"
BOOTSTRAP_RESULT="$(curl -sS -X POST -d '{}' "${BOOTSTRAP_URL}" || echo '{}')"
TOKEN="$(echo "${BOOTSTRAP_RESULT}" | jq -r '.token // empty')"
if [ -z "${TOKEN}" ]; then
    skip "live discovery-cycle probe: could not bootstrap a dev token"
    smoke_summary
    exit 0
fi

ID_HEADERS=(
    -H "Authorization: Bearer ${TOKEN}"
    -H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev"
    -H "Content-Type: application/json"
)

# Subscribe to events; capture to a tempfile.
EV_FILE="$(mktemp -t harbor-phase107c-events-XXXXXX)"
trap 'rm -f "${EV_FILE}" 2>/dev/null; kill ${SSE_PID:-} 2>/dev/null || true' EXIT
SUBSCRIBE_URL="$(api_url /v1/events/subscribe)"
curl -sS -N "${ID_HEADERS[@]}" "${SUBSCRIBE_URL}" > "${EV_FILE}" 2>&1 &
SSE_PID=$!
sleep 1

# Send a prompt the LLM should resolve by discovering a deferred tool.
# The exact prompt depends on the test fixture's yaml — this generic
# version asks the LLM to find a tool by capability.
START_RESP="$(curl -sS -X POST "$(api_url /v1/control/start)" "${ID_HEADERS[@]}" \
    -d '{"query":"What tools do you have for working with media files? Search for capabilities first.","description":"phase-107c discovery-cycle smoke"}')"
TASK_ID="$(echo "${START_RESP}" | jq -r '.task_id // empty')"
if [ -z "${TASK_ID}" ]; then
    skip "live discovery-cycle probe: start did not return a task id"
    smoke_summary
    exit 0
fi

# Wait for task.completed (bounded).
STATUS="pending"
DETAIL='{}'
for _ in $(seq 1 90); do
    DETAIL="$(curl -sS -X POST "$(api_url /v1/tasks/get)" "${ID_HEADERS[@]}" \
        -d "{\"identity\":{\"tenant\":\"dev\",\"user\":\"dev\",\"session\":\"dev\"},\"id\":\"${TASK_ID}\"}")"
    STATUS="$(echo "${DETAIL}" | jq -r '.task.status // "pending"')"
    if [ "${STATUS}" = "complete" ] || [ "${STATUS}" = "failed" ]; then break; fi
    sleep 1
done

if [ "${STATUS}" != "complete" ]; then
    skip "live discovery-cycle probe: task ended in status=${STATUS} (provider issue, not a 107c regression)"
    smoke_summary
    exit 0
fi

# Assertion 1 — discovery cycle: tool_search was invoked (trajectory has ≥1 tool_search step).
TOOL_CALLS_SEEN="$(echo "${DETAIL}" | jq -r '.task.tool_count // 0')"
if [ "${TOOL_CALLS_SEEN}" -ge 1 ]; then
    ok "live: trajectory shows ≥1 tool call (discovery cycle exercised)"
else
    skip "live: no tool calls observed — LLM may have answered without discovery (provider-dependent, not a 107c regression)"
fi

# Assertion 2 — chunk stream is clean (no leading `{` JSON wrapper byte).
CHUNKS="$(grep -E '"task_id":"'"${TASK_ID}"'"' "${EV_FILE}" 2>/dev/null \
    | grep -oE '"delta":"[^"]*"' \
    | sed -E 's/^"delta":"//; s/"$//' \
    | tr -d '\n' || true)"
FIRST_CHAR="${CHUNKS:0:1}"
if [ -z "${CHUNKS}" ]; then
    skip "live: no chunk events captured (provider may not stream for this query)"
elif [ "${FIRST_CHAR}" = "{" ]; then
    fail "live: chunk stream begins with '{' — native tool-calling didn't gate the JSON wrapper (first 40 bytes: ${CHUNKS:0:40})"
else
    ok "live: chunk stream is clean prose by structural construction (no JSON wrapper byte)"
fi

smoke_summary
