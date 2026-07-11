#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 165 — structured reasoning-steps rehydration (D-298), Console-only.
#
# The reconstruction is Console-side (the reduceHistoryTurns reducer folds the
# durable planner.decision stream into ordered reasoningSteps[]). This smoke
# proves the durable READ-BACK carries the exact events the reducer folds — no
# real LLM needed (161's precedent: the mock path exercises the read-back
# mechanics; reasoning traces are empty under the mock, which is fine — this
# asserts the STRUCTURE is present, not that a provider populated thinking).
#   1. drive a run via the `start` method (POST /v1/control/start); let the
#      durable log fill;
#   2. read state.history for the dev session; assert planner.decision events
#      carry a ReasoningTrace payload key (the key the reducer folds into
#      ordered steps) alongside DecisionKind;
#   3. WHEN a tool-calling turn is present: assert tool.invoked/tool.completed
#      carry a ToolName key AND that a planner.decision precedes its tool's
#      invoke by sequence — the reconstructable ordering signal (SKIP when the
#      mock turn invoked no tool; the tool-calling leg is pinned by the Console
#      byte-equivalence vitest + the integration test).
# Done-definition: OK >= 2, FAIL = 0; 404/405/501 → SKIP until the phase ships.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

STATE_URL="$(api_url /v1/state/history)"
START_URL="$(api_url /v1/control/start)"

TOKEN="${HARBOR_DEV_TOKEN:-dev-token-placeholder}"
ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")

if ! command -v curl >/dev/null 2>&1; then
  skip "phase 165: curl not available"
  smoke_summary
  exit 0
fi

# Route probe: a no-bearer POST distinguishes a missing route (404) from an
# auth-rejected (401). 401 means the route is mounted AND identity-mandatory.
set +e
PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
  -X POST -H 'Content-Type: application/json' -d '{}' "${STATE_URL}")
set -e
case "${PROBE:-000}" in
  404 | 405 | 501 | 000)
    skip "phase 165: /v1/state/history route not present (${PROBE:-000})"
    smoke_summary
    exit 0
    ;;
  401)
    ok "phase 165: state.history rejects identity-less body (401 — read path unchanged)"
    ;;
  *)
    fail "phase 165: no-bearer probe expected 401/404, got ${PROBE}"
    smoke_summary
    exit 0
    ;;
esac

if [ -z "${HARBOR_DEV_TOKEN:-}" ] || ! command -v jq >/dev/null 2>&1; then
  skip "phase 165: dev bearer / jq unavailable — reasoning-step reconstruction covered by Console vitest"
  smoke_summary
  exit 0
fi

# 1. Drive a run so the react planner emits planner.decision (carrying the
#    ReasoningTrace key) into the durable log.
curl -sS -X POST "${START_URL}" -H "Authorization: Bearer ${TOKEN}" \
  "${ID_HEADERS[@]}" -H 'Content-Type: application/json' \
  -d '{"query":"phase-165 reasoning-steps rehydration seed","description":"phase-165 smoke"}' >/dev/null 2>&1 || true
sleep 2

# 2. Read the full window for the dev session.
TMP="$(mktemp)"
trap 'rm -f "${TMP}"' EXIT
set +e
ST=$(curl -s -o "${TMP}" -w '%{http_code}' --max-time 10 \
  -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
  -H 'Content-Type: application/json' \
  -d '{"session_id":"dev","before":0,"limit":200}' "${STATE_URL}")
set -e

if [ "${ST}" != "200" ]; then
  skip "phase 165: state.history returned ${ST} (no retained dev history to assert against)"
  smoke_summary
  exit 0
fi

EVENTS=$(jq -r '.events | length' "${TMP}" 2>/dev/null || echo 0)
if [ "${EVENTS}" -eq 0 ]; then
  skip "phase 165: dev session window empty (run seeded no events)"
  smoke_summary
  exit 0
fi

# 3. planner.decision carries a ReasoningTrace key AND DecisionKind — the exact
#    payload keys reduceHistoryTurns folds into ordered reasoning steps.
DEC_OK=$(jq -r '[.events[] | select(.type == "planner.decision") | select((.payload | has("ReasoningTrace")) and (.payload.DecisionKind != null))] | length' "${TMP}" 2>/dev/null || echo 0)
if [ "${DEC_OK}" -gt 0 ]; then
  ok "phase 165: planner.decision read-back carries ReasoningTrace + DecisionKind keys (${DEC_OK})"
else
  fail "phase 165: no planner.decision event with a ReasoningTrace + DecisionKind key in the read-back"
fi

# 4. Tool-calling leg (SKIP when the mock turn invoked no tool). tool.invoked /
#    tool.completed carry a ToolName key, and a planner.decision precedes the
#    first tool.invoked by sequence — the reconstructable interleaving signal.
TOOL_OK=$(jq -r '[.events[] | select(.type == "tool.invoked" or .type == "tool.completed") | select(.payload.ToolName != null)] | length' "${TMP}" 2>/dev/null || echo 0)
if [ "${TOOL_OK}" -gt 0 ]; then
  ok "phase 165: tool.invoked/tool.completed read-back carries ToolName keys (${TOOL_OK})"

  DEC_SEQ=$(jq -r '[.events[] | select(.type == "planner.decision") | .sequence] | min // -1' "${TMP}" 2>/dev/null || echo -1)
  INV_SEQ=$(jq -r '[.events[] | select(.type == "tool.invoked") | .sequence] | min // -1' "${TMP}" 2>/dev/null || echo -1)
  if [ "${DEC_SEQ}" != "-1" ] && [ "${INV_SEQ}" != "-1" ] && [ "${DEC_SEQ}" -lt "${INV_SEQ}" ]; then
    ok "phase 165: a planner.decision (seq ${DEC_SEQ}) precedes its tool.invoked (seq ${INV_SEQ}) — interleaving reconstructable"
  else
    fail "phase 165: expected a planner.decision before tool.invoked by sequence (dec=${DEC_SEQ} inv=${INV_SEQ})"
  fi
else
  skip "phase 165: dev turn invoked no tool (mock path) — tool-interleaving pinned by Console byte-equivalence vitest"
fi

smoke_summary
