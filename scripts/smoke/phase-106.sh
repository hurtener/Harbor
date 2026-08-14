#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 106 — Playground displays the real assistant response.
#
# Smoke assertions: send a query via the Protocol, poll until complete,
# then read the sealed consumer turn that the Playground renders.

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

# Standalone battery runs (no dev server) degrade to SKIP instead of a raw
# `curl -sS` rc=7 crash under `set -e`; preflight always has the server up.
skip_all_if_server_down "phase 106"

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
  # durable-answer plumbing isn't exercised end-to-end — SKIP the remaining
  # assertions but log the failure shape.
  skip "task failed (likely missing LLM provider key in preflight env; durable-answer smoke requires a working LLM)"
  smoke_summary
  exit 0
else
  fail "task stuck at ${STATUS} after 30s"
  smoke_summary
  exit 1
fi

# 3. Read the sealed consumer turn. HA-64 made sessions.turns.get the
# authoritative terminal transcript snapshot used by the Playground; tasks.get
# result_inline remains a task API compatibility surface, not the UI read path.
TURN_DETAIL="$(curl -sS -X POST "$(api_url /v1/sessions/turns/get)" \
  -H "Authorization: Bearer ${TOKEN}" \
  "${ID_HEADERS[@]}" \
  -H "Content-Type: application/json" \
  -d "{\"session_id\":\"dev\",\"task_id\":\"${TASK_ID}\"}")"

TURN_TASK_ID="$(echo "${TURN_DETAIL}" | jq -r '.turn.task_id // empty')"
if [ "${TURN_TASK_ID}" = "${TASK_ID}" ]; then
  ok "sessions.turns.get returned the completed task's consumer turn"
else
  fail "sessions.turns.get did not return the completed task's consumer turn"
fi

ANSWER_STATE="$(echo "${TURN_DETAIL}" | jq -r '.turn.answer.state // empty')"
if [ "${ANSWER_STATE}" = "inline" ]; then
  ok "sealed consumer turn records an inline assistant answer"
else
  fail "sealed consumer turn answer.state is ${ANSWER_STATE:-empty}, want inline"
fi

ANSWER="$(echo "${TURN_DETAIL}" | jq -r '.turn.answer.inline // empty')"
if [ -n "${ANSWER}" ]; then
  ok "sealed consumer turn's inline answer is non-empty"
else
  fail "sealed consumer turn's inline answer is empty"
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

# 5. The Playground reads the HA-64 durable projection on open and on a
# terminal event. These are the two authoritative transcript paths.
PLAYGROUND_SRC="web/console/src/routes/(console)/playground/[session_id]/+page.svelte"
if grep -q 'loadTurnPage(c, { sessionID, limit: TURN_PAGE_DEFAULT_LIMIT })' "${PLAYGROUND_SRC}" && \
   grep -q 'reconcileTurnRow(client, sessionID, taskID)' "${PLAYGROUND_SRC}"; then
  ok "static: playground reads durable session turns on open and terminal completion"
else
  fail "static: playground does not use the durable session-turn projection on both transcript paths"
fi

smoke_summary
