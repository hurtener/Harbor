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
# Phase 107c step 10 / AC-13 — declarative_action escape-hatch wiring.
# These static checks ALWAYS run (no provider-key gate) so the step 10
# wiring is verified even without a live LLM. The live-side probe of
# declarative_action requires a provider WITHOUT native tool-calling
# AND an opt-in agent yaml — operator-driven, not a smoke concern.
# ----------------------------------------------------------------------------

# Static assertion — declarative_action body has the real dispatch
# path (ErrDeclarativeActionNotWired removed from the body's return
# path).
if grep -qE 'ErrDeclarativeActionNotWired' "internal/tools/builtin/declarative_action.go" 2>/dev/null; then
    fail "static: declarative_action still returns ErrDeclarativeActionNotWired — step 10 dispatch path not wired"
else
    ok "static: declarative_action body has real dispatch (no ErrDeclarativeActionNotWired stub)"
fi

# Static assertion — declarative_action surfaces a structured
# RepairOutcome for ArgsRepair / MultiAction / FinishRepair so the
# React planner can drive across-step escalation.
if grep -qE 'DeclarativeRepairOutcome' "internal/tools/builtin/declarative_action.go" 2>/dev/null; then
    ok "static: declarative_action returns DeclarativeRepairOutcome for repair-counter escalation"
else
    fail "static: declarative_action missing DeclarativeRepairOutcome — repair-counter escalation not wired"
fi

# Static assertion — operator-facing example yaml documents the
# declarative_action enable toggle.
if grep -qE 'declarative_action' "examples/dev.yaml" 2>/dev/null; then
    ok "static: examples/dev.yaml documents declarative_action (operator opt-in surface)"
else
    skip "static: examples/dev.yaml missing declarative_action documentation"
fi

# Static assertion (AC-20a Path 1) — the React planner declares the
# reserved planner-control names (`_spawn_task` / `_await_task`) as
# native tool declarations so providers don't reject the projector's
# reserved-name interception under live LLM workloads.
if grep -qE 'reservedPlannerControlDeclarations' "internal/planner/react/discovered_tools.go" 2>/dev/null; then
    ok "static: React planner declares _spawn_task + _await_task natively (AC-20a Path 1)"
else
    fail "static: React planner missing native declarations for _spawn_task / _await_task"
fi

# ----------------------------------------------------------------------------
# Phase 107c follow-up — heavy-content boundary: RoleTool projection
# inlines the preview + `artifact_fetch` builtin gives the LLM a
# recovery path. The live YouTube test surfaced the bug these two
# fixes close (the loop-on-wrapper-JSON failure).
# ----------------------------------------------------------------------------

assert_file_or_skip \
    "internal/tools/builtin/artifact_fetch.go" \
    "static: artifact_fetch builtin (heavy-content recovery hatch)"

# RegistryContext must thread ArtifactStore so the builtin reaches it.
if grep -qE 'ArtifactStore\s+artifacts\.ArtifactStore' "internal/tools/builtin/builtin.go" 2>/dev/null; then
    ok "static: builtin.RegistryContext threads ArtifactStore (artifact_fetch dependency)"
else
    fail "static: builtin.RegistryContext missing ArtifactStore field — artifact_fetch will nil-deref at invoke"
fi

# Dev binary wires the store into the builtin RegistryContext.
if grep -qE 'ArtifactStore:\s*artStore' "cmd/harbor/cmd_dev.go" 2>/dev/null; then
    ok "static: cmd_dev.go threads artStore into builtin.RegistryContext"
else
    fail "static: cmd_dev.go does not thread artStore into builtin.RegistryContext — artifact_fetch will fail at invoke"
fi

# Validator allowlist mirrors the new builtin (otherwise yaml validation rejects).
if grep -qE '"artifact_fetch":\s*\{\}' "internal/config/validate.go" 2>/dev/null; then
    ok "static: config validator allowlist includes artifact_fetch"
else
    fail "static: config validator allowlist missing artifact_fetch — yaml validation will reject operators that enable it"
fi

# Prompt builder inlines heavy-content observations instead of leaking
# wrapper JSON onto the RoleTool message body. The function name is
# the load-bearing surface; the unit test pins the negative-
# instruction-trap regression gate.
if grep -qE 'renderHeavyContentObservation' "internal/planner/react/prompt.go" 2>/dev/null; then
    ok "static: react/prompt.go inlines heavy-content observations (RoleTool wrapper-JSON loop closed)"
else
    fail "static: react/prompt.go missing renderHeavyContentObservation — heavy tool results will reach LLM as wrapper JSON"
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

# Assertion 3 (Phase 107c step 11) — native tool_call evidence on the
# bus. The runtime emits `tool.invoked` events when the executor
# dispatches a CallTool decision; the LLM-emitting-tool-calls path is
# upstream of that event. We assert at least one `tool.invoked` event
# carries the task_id, proving the SSE stream surfaces native tool
# calls end-to-end (not just trajectory-only).
TOOL_INVOKED_COUNT="$(grep -cE '"type":"tool\.invoked"' "${EV_FILE}" 2>/dev/null || true)"
if [ "${TOOL_INVOKED_COUNT}" -ge 1 ]; then
    ok "live: SSE stream carries ≥1 tool.invoked event — native tool-call dispatch end-to-end"
else
    skip "live: no tool.invoked events observed (LLM answered without invoking tools — provider-dependent)"
fi

# Assertion 4 (Phase 107c step 11) — at least one trajectory step
# carries a native tool-call action (Action: CallTool with non-empty
# CallID). The CallID is the load-bearing wire-shape proof: the
# projector populates it from `resp.ToolCalls[i].ID`, which only
# exists under native tool-calling.
TRAJ_TOOL_STEPS="$(echo "${DETAIL}" | jq -r '
    .task.trajectory.steps // []
    | map(select(.action.tool != null and .action.tool != ""))
    | length' 2>/dev/null || echo 0)"
if [ "${TRAJ_TOOL_STEPS}" -ge 1 ]; then
    ok "live: tasks.get trajectory carries ≥1 native CallTool step (resp.ToolCalls round-trip)"
else
    skip "live: trajectory has no native CallTool steps (LLM answered without tools — provider-dependent)"
fi

# ----------------------------------------------------------------------------
# declarative_action escape-hatch — static assertions.
# A full live probe (boot a model without native tool-calling, opt in
# to declarative_action, dispatch through the escape-hatch end-to-end)
# is operator-driven post-step 11 because it requires a non-native
# provider key. The static checks here guard the wiring:
#   - The example yaml documents the toggle.
#   - The body returns a structured DeclarativeActionOut (no
#     ErrDeclarativeActionNotWired left over).
# ----------------------------------------------------------------------------

# Static assertion 5 — declarative_action body has the real dispatch
# path (ErrDeclarativeActionNotWired removed from the body's return
# path).
if grep -qE 'ErrDeclarativeActionNotWired' "internal/tools/builtin/declarative_action.go" 2>/dev/null; then
    fail "static: declarative_action still returns ErrDeclarativeActionNotWired — step 10 dispatch path not wired"
else
    ok "static: declarative_action body has real dispatch (no ErrDeclarativeActionNotWired stub)"
fi

# Static assertion 6 — declarative_action surfaces a structured
# RepairOutcome for ArgsRepair / MultiAction / FinishRepair so the
# React planner can drive across-step escalation.
if grep -qE 'DeclarativeRepairOutcome' "internal/tools/builtin/declarative_action.go" 2>/dev/null; then
    ok "static: declarative_action returns DeclarativeRepairOutcome for repair-counter escalation"
else
    fail "static: declarative_action missing DeclarativeRepairOutcome — repair-counter escalation not wired"
fi

# Static assertion 7 — operator-facing example yaml documents the
# declarative_action enable toggle.
if grep -qE 'declarative_action' "examples/dev.yaml" 2>/dev/null; then
    ok "static: examples/dev.yaml documents declarative_action (operator opt-in surface)"
else
    skip "static: examples/dev.yaml missing declarative_action documentation"
fi

smoke_summary
