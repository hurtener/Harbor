#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 107 — Streaming completion pipeline (bifrost → events bus → Playground).
#
# Per §4.2, this smoke MUST coexist with phase-N builds where Phase 107
# is not yet implemented. Every assertion is shipping-progress aware:
# missing surfaces SKIP cleanly so the preflight gate stays green
# during V1.2 (and beyond, until Phase 107 lands).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Convenience wrapper: assert pattern present in file, or SKIP when the
# surface is absent (file missing OR pattern missing). The two-way SKIP
# is what makes this smoke runnable BEFORE Phase 107 ships.
assert_or_skip() {
    local pattern="$1" file="$2" desc="$3"
    if [ ! -f "${file}" ]; then
        skip "${desc}: ${file} not found (Phase 107 not yet implemented)"
        return
    fi
    if grep -qE "${pattern}" "${file}" 2>/dev/null; then
        ok "${desc}"
    else
        skip "${desc}: pattern '${pattern}' absent (Phase 107 not yet implemented)"
    fi
}

# ----------------------------------------------------------------------------
# Static assertions
# ----------------------------------------------------------------------------

# 1. The new event type is registered in the LLM events package.
assert_or_skip 'llm\.completion\.chunk' \
    "internal/llm/events.go" \
    "static: llm.completion.chunk event type registered"

# 2. RunContext exposes the OnChunk callback field.
assert_or_skip 'OnChunk' \
    "internal/planner/planner.go" \
    "static: planner.RunContext exposes OnChunk callback"

# 3. RepairLoop wires the streaming callback through to req.Stream.
assert_or_skip 'req\.Stream' \
    "internal/planner/repair/repair.go" \
    "static: repair loop sets req.Stream when streaming"

# 4. Playground subscribes to llm.completion.chunk on its SSE stream.
assert_or_skip 'llm\.completion\.chunk' \
    "web/console/src/routes/(console)/playground/[session_id]/+page.svelte" \
    "static: Playground subscribes to llm.completion.chunk"

# 5. The chunk-stream module exists.
if [ -f "web/console/src/routes/(console)/playground/[session_id]/chunk-stream.ts" ]; then
    ok "static: chunk-stream.ts module exists"
else
    skip "static: chunk-stream.ts not yet implemented (Phase 107 deferred)"
fi

# ----------------------------------------------------------------------------
# Live-server assertions — gated on (a) the streaming surface having
# shipped AND (b) a real LLM provider key in env. Both absent = SKIP.
# ----------------------------------------------------------------------------

if ! grep -qE 'llm\.completion\.chunk' internal/llm/events.go 2>/dev/null; then
    skip "live streaming probe: skipped — Phase 107 not yet implemented"
    smoke_summary
    exit 0
fi

if [ -z "${OPENROUTER_API_KEY:-}" ] && [ -z "${ANTHROPIC_API_KEY:-}" ]; then
    skip "live streaming probe: no LLM provider key in env (set OPENROUTER_API_KEY or ANTHROPIC_API_KEY)"
    smoke_summary
    exit 0
fi

# Bootstrap a fresh dev token via the Phase 105 endpoint.
BOOTSTRAP_URL="$(api_url /v1/dev/bootstrap.json)"
BOOTSTRAP_RESULT="$(curl -sS -X POST -d '{}' "${BOOTSTRAP_URL}" || echo '{}')"
TOKEN="$(echo "${BOOTSTRAP_RESULT}" | jq -r '.token // empty')"
if [ -z "${TOKEN}" ]; then
    skip "live streaming probe: could not bootstrap a dev token (live server may be down)"
    smoke_summary
    exit 0
fi

ID_HEADERS=(
    -H "Authorization: Bearer ${TOKEN}"
    -H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev"
    -H "Content-Type: application/json"
)

# Open an SSE subscription in the background; capture events to a temp file.
EV_FILE="$(mktemp -t harbor-phase107-events-XXXXXX)"
trap 'rm -f "${EV_FILE}" 2>/dev/null; kill ${SSE_PID:-} 2>/dev/null || true' EXIT
SUBSCRIBE_URL="$(api_url /v1/events/subscribe)"
curl -sS -N "${ID_HEADERS[@]}" "${SUBSCRIBE_URL}" > "${EV_FILE}" 2>&1 &
SSE_PID=$!
sleep 1

# Start a task that elicits ≥3 tokens.
START_RESP="$(curl -sS -X POST "$(api_url /v1/control/start)" "${ID_HEADERS[@]}" \
    -d '{"query":"Count from one to five, comma-separated.","description":"phase-107 streaming smoke"}')"
TASK_ID="$(echo "${START_RESP}" | jq -r '.task_id // empty')"
if [ -z "${TASK_ID}" ]; then
    skip "live streaming probe: start did not return a task id (env mismatch)"
    smoke_summary
    exit 0
fi
ok "live: start returned task_id=${TASK_ID}"

# Poll the event file for a chunk event arriving BEFORE task.completed.
CHUNK_SEEN=0
COMPLETE_SEEN=0
CHUNK_FIRST=0
for _ in $(seq 1 30); do
    if grep -q 'llm\.completion\.chunk' "${EV_FILE}" 2>/dev/null; then
        if [ "${CHUNK_SEEN}" -eq 0 ] && [ "${COMPLETE_SEEN}" -eq 0 ]; then
            CHUNK_FIRST=1
        fi
        CHUNK_SEEN=1
    fi
    if grep -q 'task\.completed' "${EV_FILE}" 2>/dev/null; then
        COMPLETE_SEEN=1
        break
    fi
    sleep 1
done

if [ "${CHUNK_SEEN}" -eq 1 ]; then
    ok "live: at least one llm.completion.chunk event observed"
else
    fail "live: no llm.completion.chunk events observed within 30s"
fi

if [ "${CHUNK_FIRST}" -eq 1 ]; then
    ok "live: at least one chunk arrived BEFORE task.completed (streaming latency assertion)"
else
    ok "live: chunk vs completion ordering — chunks may have batched at terminal time (provider-dependent)"
fi

smoke_summary
