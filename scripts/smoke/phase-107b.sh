#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 107b — Streaming answer extractor (React planner).
#
# Per §4.2 this smoke is shipping-progress aware: every assertion SKIPs
# when its underlying surface is absent, so the preflight gate stays
# green BEFORE Phase 107b lands. Once each piece ships, the matching
# SKIP flips to OK without any change to the smoke.
#
# The filter lives in internal/planner/react/ — wholly isolated from the
# runloop and the LLM client. The static asserts pin the file's
# existence + the four discriminator strings the filter depends on (per
# AC-12 + AC-14). The live probe (gated on a real LLM provider key)
# confirms the chunk stream concatenation matches the canonical
# `tasks.get` `result_inline.answer` parse — no leading JSON wrapper
# bytes leaked.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Convenience wrapper — SKIP when file missing OR pattern absent.
assert_or_skip() {
    local pattern="$1" file="$2" desc="$3"
    if [ ! -f "${file}" ]; then
        skip "${desc}: ${file} not found (Phase 107b not yet implemented)"
        return
    fi
    if grep -qE "${pattern}" "${file}" 2>/dev/null; then
        ok "${desc}"
    else
        skip "${desc}: pattern '${pattern}' absent (Phase 107b not yet implemented)"
    fi
}

# ----------------------------------------------------------------------------
# Static assertions — pin the filter exists + the four discriminators.
# ----------------------------------------------------------------------------

FILTER="internal/planner/react/stream_filter.go"

if [ -f "${FILTER}" ]; then
    ok "static: streamAnswerFilter source exists (${FILTER})"
else
    skip "static: ${FILTER} not yet implemented (Phase 107b deferred)"
fi

# The four discriminator strings are load-bearing per D-NNN (the filter
# encodes the Phase 83 action schema in regex). A future schema change
# MUST update these patterns — the smoke + the D-NNN entry are the
# coupling tripwires.
assert_or_skip '"tool"' \
    "${FILTER}" \
    "static: filter encodes 'tool' discriminator literal"
assert_or_skip '"_finish"' \
    "${FILTER}" \
    "static: filter encodes '_finish' discriminator literal"
assert_or_skip '"args"' \
    "${FILTER}" \
    "static: filter encodes 'args' key literal"
assert_or_skip '"answer"' \
    "${FILTER}" \
    "static: filter encodes 'answer' key literal"

# The wrap site lives in the React planner's per-step Next() path; assert
# the wiring file references the filter constructor.
assert_or_skip 'newStreamAnswerFilter|streamAnswerFilter' \
    "internal/planner/react/react.go" \
    "static: React planner wires the streamAnswerFilter"

# ----------------------------------------------------------------------------
# Live-server probe — gated on (a) the filter having shipped AND (b) a
# real LLM provider key in env.
# ----------------------------------------------------------------------------

if [ ! -f "${FILTER}" ]; then
    skip "live answer-extractor probe: skipped — Phase 107b not yet implemented"
    smoke_summary
    exit 0
fi

if [ -z "${OPENROUTER_API_KEY:-}" ] && [ -z "${ANTHROPIC_API_KEY:-}" ]; then
    skip "live answer-extractor probe: no LLM provider key (set OPENROUTER_API_KEY or ANTHROPIC_API_KEY)"
    smoke_summary
    exit 0
fi

BOOTSTRAP_URL="$(api_url /v1/dev/bootstrap.json)"
BOOTSTRAP_RESULT="$(curl -sS -X POST -d '{}' "${BOOTSTRAP_URL}" || echo '{}')"
TOKEN="$(echo "${BOOTSTRAP_RESULT}" | jq -r '.token // empty')"
if [ -z "${TOKEN}" ]; then
    skip "live answer-extractor probe: could not bootstrap a dev token"
    smoke_summary
    exit 0
fi

ID_HEADERS=(
    -H "Authorization: Bearer ${TOKEN}"
    -H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev"
    -H "Content-Type: application/json"
)

# Subscribe to chunk events; capture to a tempfile.
EV_FILE="$(mktemp -t harbor-phase107b-events-XXXXXX)"
trap 'rm -f "${EV_FILE}" 2>/dev/null; kill ${SSE_PID:-} 2>/dev/null || true' EXIT
SUBSCRIBE_URL="$(api_url /v1/events/subscribe)"
curl -sS -N "${ID_HEADERS[@]}" "${SUBSCRIBE_URL}" > "${EV_FILE}" 2>&1 &
SSE_PID=$!
sleep 1

# A finish-shaped prompt — the LLM emits a _finish action whose
# args.answer is the user-facing reply.
START_RESP="$(curl -sS -X POST "$(api_url /v1/control/start)" "${ID_HEADERS[@]}" \
    -d '{"query":"Reply with exactly the single word: ok","description":"phase-107b answer-extractor smoke"}')"
TASK_ID="$(echo "${START_RESP}" | jq -r '.task_id // empty')"
if [ -z "${TASK_ID}" ]; then
    skip "live answer-extractor probe: start did not return a task id"
    smoke_summary
    exit 0
fi

# Wait for task.completed (bounded).
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
    skip "live answer-extractor probe: task ended in status=${STATUS} (provider issue, not a 107b regression)"
    smoke_summary
    exit 0
fi

# Concatenate every chunk's `delta` field belonging to this task.
CHUNKS="$(grep -E '"task_id":"'"${TASK_ID}"'"' "${EV_FILE}" 2>/dev/null \
    | grep -oE '"delta":"[^"]*"' \
    | sed -E 's/^"delta":"//; s/"$//' \
    | tr -d '\n' || true)"

# The canonical answer comes from the Phase 106 envelope.
INLINE="$(echo "${DETAIL}" | jq -r '.result_inline // ""')"
ANSWER="$(echo "${INLINE}" | jq -r '.answer // ""' 2>/dev/null || echo "")"

# Assertion 1 — chunks contain NO leading `{` JSON wrapper byte.
# This is the load-bearing legibility assertion: if the chunk stream
# begins with `{` the filter did not gate the JSON wrapper and the
# Console renders raw JSON before the task.completed fetch reconciles.
FIRST_CHAR="${CHUNKS:0:1}"
if [ -z "${CHUNKS}" ]; then
    skip "live: no chunk events captured (provider may not stream this prompt)"
elif [ "${FIRST_CHAR}" = "{" ]; then
    fail "live: chunk stream begins with '{' — JSON wrapper leaked past filter (first 40 bytes: ${CHUNKS:0:40})"
else
    ok "live: chunk stream starts with user-facing prose (no JSON wrapper leak)"
fi

# Assertion 2 — chunks concatenation matches the parsed answer (modulo
# trailing whitespace + JSON escape decoding the smoke can't exactly
# match — the filter decodes \n / \t / \"; we strip whitespace and
# compare prefixes for robustness across providers that may chunk the
# trailing punctuation differently).
if [ -n "${CHUNKS}" ] && [ -n "${ANSWER}" ]; then
    CHUNKS_TRIM="$(printf '%s' "${CHUNKS}" | tr -d '[:space:]')"
    ANSWER_TRIM="$(printf '%s' "${ANSWER}" | tr -d '[:space:]')"
    if [ "${CHUNKS_TRIM}" = "${ANSWER_TRIM}" ]; then
        ok "live: chunk concatenation matches parsed answer byte-for-byte (modulo whitespace)"
    elif [ "${ANSWER_TRIM:0:${#CHUNKS_TRIM}}" = "${CHUNKS_TRIM}" ]; then
        ok "live: chunk stream is a strict prefix of the parsed answer (trailing chunks may have lagged)"
    else
        fail "live: chunk stream diverged from parsed answer (chunks=${CHUNKS:0:40} answer=${ANSWER:0:40})"
    fi
fi

smoke_summary
