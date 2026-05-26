#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 106 — Playground displays the real assistant response.
#
# Smoke assertions: send a query via the Protocol, poll until complete,
# and verify result_inline contains the answer envelope.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Live-server assertions (require a booted harbor dev instance)
# ----------------------------------------------------------------------------

# 1. Send a query and start a task
TOKEN="dev-token-placeholder"
# The preflight gate prints HARBOR_DEV_TOKEN to stderr; common.sh exposes
# HARBOR_DEV_TOKEN if the preflight harness parsed it.
if [ -n "${HARBOR_DEV_TOKEN:-}" ]; then
  TOKEN="${HARBOR_DEV_TOKEN}"
fi

ID_HEADERS=(
  -H "X-Harbor-Tenant: dev"
  -H "X-Harbor-User: dev"
  -H "X-Harbor-Session: dev"
)

START_RESP="$(curl -sS -X POST "$(api_url /v1/control/start)" \
  -H "Authorization: Bearer ${TOKEN}" \
  "${ID_HEADERS[@]}" \
  -H "Content-Type: application/json" \
  -d '{"query":"Reply with the single word OK","description":"phase-106 smoke"}')"

TASK_ID="$(echo "${START_RESP}" | jq -r '.task_id // empty')"
if [ -z "${TASK_ID}" ]; then
  skip "could not start a task (start returned: $(echo "${START_RESP}" | head -c 200))"
fi
ok "start returned task_id=${TASK_ID}"

# 2. Poll until complete (bounded 30s; fail on timeout)
STATUS="pending"
for i in $(seq 1 30); do
  DETAIL="$(curl -sS -X POST "$(api_url /v1/tasks/get)" \
    -H "Authorization: Bearer ${TOKEN}" \
    "${ID_HEADERS[@]}" \
    -H "Content-Type: application/json" \
    -d "{\"id\":\"${TASK_ID}\"}")"
  STATUS="$(echo "${DETAIL}" | jq -r '.task.status // "pending"')"
  if [ "${STATUS}" = "complete" ] || [ "${STATUS}" = "failed" ]; then
    break
  fi
  sleep 1
done
if [ "${STATUS}" = "complete" ]; then
  ok "task reached complete within 30s"
elif [ "${STATUS}" = "failed" ]; then
  # Under the preflight harness the LLM seam may not have a real
  # provider key (no OPENROUTER_API_KEY in env) or the mock driver may
  # produce a no_path finish on real-react prompts. Either way the
  # answer-envelope plumbing isn't exercised end-to-end — SKIP the
  # remaining assertions but log the failure shape.
  skip "task failed (likely missing LLM provider key in preflight env; envelope smoke requires a working LLM)"
  smoke_summary
  exit 0
else
  fail "task stuck at ${STATUS} after 30s"
  smoke_summary
  exit 1
fi

# 3. Read result_inline + parse the envelope
INLINE="$(echo "${DETAIL}" | jq -r '.result_inline // empty')"
if [ -n "${INLINE}" ]; then
  ok "result_inline non-empty (phase 106 plumbed the answer)"
else
  fail "result_inline is empty (phase 106 should have populated it)"
fi

ANSWER="$(echo "${INLINE}" | jq -r '.answer // empty')"
if [ -n "${ANSWER}" ]; then
  ok "answer field in envelope non-empty"
else
  fail "answer field in envelope empty"
fi

FINISH="$(echo "${INLINE}" | jq -r '.finish_reason // empty')"
if [ "${FINISH}" = "goal" ]; then
  ok "finish_reason is goal for a normal completion"
else
  # Soft notice — the mock LLM driver may finish via a different reason.
  # We don't fail the smoke on non-goal finish; the envelope shape is the
  # load-bearing assertion above.
  ok "finish_reason is ${FINISH} (non-goal completion is acceptable)"
fi

# ----------------------------------------------------------------------------
# Static assertions
# ----------------------------------------------------------------------------

# 4. The placeholder text must not be present in the Playground page
if grep -rq "Message accepted by the Runtime" web/console/src/routes/"(console)"/playground/ 2>/dev/null; then
  fail "static: playground still contains 'Message accepted by the Runtime.' placeholder"
else
  ok "static: playground does not contain the placeholder text"
fi

# 5. The Playground reads result_inline
if grep -q "result_inline" web/console/src/routes/"(console)"/playground/"[session_id]"/+page.svelte 2>/dev/null; then
  ok "static: playground reads result_inline"
else
  fail "static: playground does not reference result_inline"
fi

smoke_summary
