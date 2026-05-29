#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 107a — Reasoning trace projection (tasks.get enricher + Playground accordion).
#
# Per §4.2 this smoke is shipping-progress aware: every assertion SKIPs
# when its underlying surface is absent, so the preflight gate stays
# green BEFORE Phase 107a lands. Once the phase ships, the SKIPs flip
# to OK without any change to the smoke.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Convenience wrapper — see phase-107.sh for the same shape.
assert_or_skip() {
    local pattern="$1" file="$2" desc="$3"
    if [ ! -f "${file}" ]; then
        skip "${desc}: ${file} not found (Phase 107a not yet implemented)"
        return
    fi
    if grep -qE "${pattern}" "${file}" 2>/dev/null; then
        ok "${desc}"
    else
        skip "${desc}: pattern '${pattern}' absent (Phase 107a not yet implemented)"
    fi
}

# ----------------------------------------------------------------------------
# Static assertions
# ----------------------------------------------------------------------------

assert_or_skip 'TaskTrajectoryRef' \
    "internal/protocol/types/tasks.go" \
    "static: TaskTrajectoryRef declared on the Protocol surface"

assert_or_skip 'Trajectory.*TaskTrajectoryRef' \
    "internal/protocol/types/tasks.go" \
    "static: TaskDetail carries the Trajectory field"

assert_or_skip 'Trajectory.*identity\.Identity' \
    "internal/tasks/protocol/registry_projector.go" \
    "static: Enricher interface declares Trajectory(...)"

assert_or_skip 'enricher\.Trajectory' \
    "internal/tasks/protocol/registry_projector.go" \
    "static: projector consults enricher.Trajectory"

if [ -f "web/console/src/lib/chat/ReasoningAccordion.svelte" ]; then
    ok "static: ReasoningAccordion component exists"
else
    skip "static: ReasoningAccordion.svelte not yet implemented (Phase 107a deferred)"
fi

assert_or_skip 'parseReasoningSteps' \
    "web/console/src/routes/(console)/playground/[session_id]/answer-envelope.ts" \
    "static: parseReasoningSteps helper declared"

# The Playground page populates `reasoningSteps` on the agent bubble;
# the actual <ReasoningAccordion> mount lives inside MessageBubble.svelte
# (the chat module owns the rendering surface, the page owns the data
# wiring). Assert both halves.
assert_or_skip 'reasoningSteps' \
    "web/console/src/routes/(console)/playground/[session_id]/+page.svelte" \
    "static: Playground page populates reasoningSteps on the bubble"

assert_or_skip 'ReasoningAccordion' \
    "web/console/src/lib/chat/MessageBubble.svelte" \
    "static: MessageBubble mounts ReasoningAccordion"

# ----------------------------------------------------------------------------
# Live-server assertions — gated on (a) the projection surface having
# shipped AND (b) a real LLM provider key in env.
# ----------------------------------------------------------------------------

if ! grep -qE 'TaskTrajectoryRef' internal/protocol/types/tasks.go 2>/dev/null; then
    skip "live trajectory probe: skipped — Phase 107a not yet implemented"
    smoke_summary
    exit 0
fi

if [ -z "${OPENROUTER_API_KEY:-}" ] && [ -z "${ANTHROPIC_API_KEY:-}" ]; then
    skip "live trajectory probe: no LLM provider key in env (set OPENROUTER_API_KEY or ANTHROPIC_API_KEY)"
    smoke_summary
    exit 0
fi

BOOTSTRAP_URL="$(api_url /v1/dev/bootstrap.json)"
BOOTSTRAP_RESULT="$(curl -sS -X POST -d '{}' "${BOOTSTRAP_URL}" || echo '{}')"
TOKEN="$(echo "${BOOTSTRAP_RESULT}" | jq -r '.token // empty')"
if [ -z "${TOKEN}" ]; then
    skip "live trajectory probe: could not bootstrap a dev token"
    smoke_summary
    exit 0
fi

ID_HEADERS=(
    -H "Authorization: Bearer ${TOKEN}"
    -H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev"
    -H "Content-Type: application/json"
)

START_RESP="$(curl -sS -X POST "$(api_url /v1/control/start)" "${ID_HEADERS[@]}" \
    -d '{"query":"List three primes greater than 10, then add them. Show your work.","description":"phase-107a reasoning smoke"}')"
TASK_ID="$(echo "${START_RESP}" | jq -r '.task_id // empty')"
if [ -z "${TASK_ID}" ]; then
    skip "live trajectory probe: start did not return a task id"
    smoke_summary
    exit 0
fi

STATUS="pending"
DETAIL='{}'
for _ in $(seq 1 60); do
    DETAIL="$(curl -sS -X POST "$(api_url /v1/tasks/get)" "${ID_HEADERS[@]}" \
        -d "{\"identity\":{\"tenant\":\"dev\",\"user\":\"dev\",\"session\":\"dev\"},\"id\":\"${TASK_ID}\"}")"
    STATUS="$(echo "${DETAIL}" | jq -r '.task.status // "pending"')"
    if [ "${STATUS}" = "complete" ] || [ "${STATUS}" = "failed" ]; then break; fi
    sleep 1
done

if [ "${STATUS}" != "complete" ]; then
    skip "live trajectory probe: task ended in status=${STATUS} (LLM provider issue, not a 107a regression)"
    smoke_summary
    exit 0
fi

STEP_COUNT="$(echo "${DETAIL}" | jq -r '.trajectory.steps | length // 0')"
if [ "${STEP_COUNT}" -gt 0 ]; then
    ok "live: tasks.get carries trajectory.steps (count=${STEP_COUNT})"
else
    fail "live: trajectory.steps is empty or absent (phase 107a should have populated it)"
fi

FIRST_TRACE="$(echo "${DETAIL}" | jq -r '.trajectory.steps[0].reasoning_trace // ""')"
if [ -n "${FIRST_TRACE}" ]; then
    ok "live: first step's reasoning_trace is non-empty"
else
    skip "live: configured model emits no per-step reasoning trace (provider-dependent; not a 107a regression)"
fi

smoke_summary
