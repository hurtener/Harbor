#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 161 — session rehydration carries per-turn metadata (D-293).
#
# The durable event stream's read-back must carry the CONTENT-FREE per-turn
# metadata a session reopen reconstructs from. Under the preflight dev posture
# the mock LLM driver reports synthetic usage (HARBOR_DEV_ALLOW_MOCK=1), and
# this phase's driver-neutral cost emit makes llm.cost.recorded fire for the
# mock path too — so a scripted run seeds the exact metadata a reopen folds:
#   1. drive a run via the `start` method (POST /v1/control/start); let the
#      durable log fill;
#   2. read state.history for the dev session; assert an llm.cost.recorded
#      event whose payload carries the Usage + Model keys;
#   3. assert a planner.decision event with a DecisionKind key and a populated
#      envelope run (the turn-attributable correlation key);
#   4. NEGATIVE (§7 rule 7): no raw tool args/results keys (Args / Result /
#      Arguments) appear anywhere in the page — the boundary is content-free.
# The tool-calling (MCP fixture) leg lives in the integration test.
# Done-definition: OK >= 3, FAIL = 0.

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
  skip "phase 161: curl not available"
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
    skip "phase 161: /v1/state/history route not present (${PROBE:-000})"
    smoke_summary
    exit 0
    ;;
  401)
    ok "phase 161: state.history rejects identity-less body (401 — read path unchanged)"
    ;;
  *)
    fail "phase 161: no-bearer probe expected 401/404, got ${PROBE}"
    smoke_summary
    exit 0
    ;;
esac

if [ -z "${HARBOR_DEV_TOKEN:-}" ] || ! command -v jq >/dev/null 2>&1; then
  skip "phase 161: dev bearer / jq unavailable — metadata read-back covered by Go integration tests"
  smoke_summary
  exit 0
fi

# 1. Drive a run so the mock LLM emits llm.cost.recorded + the react planner
#    emits planner.decision into the durable log.
curl -sS -X POST "${START_URL}" -H "Authorization: Bearer ${TOKEN}" \
  "${ID_HEADERS[@]}" -H 'Content-Type: application/json' \
  -d '{"query":"phase-161 rehydration metadata seed","description":"phase-161 smoke"}' >/dev/null 2>&1 || true
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
  skip "phase 161: state.history returned ${ST} (no retained dev history to assert against)"
  smoke_summary
  exit 0
fi

EVENTS=$(jq -r '.events | length' "${TMP}" 2>/dev/null || echo 0)
if [ "${EVENTS}" -eq 0 ]; then
  skip "phase 161: dev session window empty (run seeded no events)"
  smoke_summary
  exit 0
fi
ok "phase 161: state.history read-back returned ${EVENTS} events (200)"

# 3. llm.cost.recorded with Usage + Model keys (the driver-neutral R1 emit,
#    firing for the mock path).
COST_OK=$(jq -r '[.events[] | select(.type == "llm.cost.recorded") | select((.payload.Usage != null) and (.payload.Model != null))] | length' "${TMP}" 2>/dev/null || echo 0)
if [ "${COST_OK}" -gt 0 ]; then
  ok "phase 161: llm.cost.recorded read-back carries Usage + Model keys (${COST_OK})"
else
  fail "phase 161: no llm.cost.recorded event with Usage + Model keys in the read-back (R1 emit missing on the mock path?)"
fi

# 4. planner.decision with a DecisionKind key AND a populated envelope run
#    (the turn-attributable correlation key the reducer folds against).
DEC_OK=$(jq -r '[.events[] | select(.type == "planner.decision") | select((.payload.DecisionKind != null) and (.run != null and .run != ""))] | length' "${TMP}" 2>/dev/null || echo 0)
if [ "${DEC_OK}" -gt 0 ]; then
  ok "phase 161: planner.decision read-back carries DecisionKind + a populated envelope run (${DEC_OK})"
else
  fail "phase 161: no planner.decision event with DecisionKind + envelope run in the read-back"
fi

# 5. NEGATIVE — the content-free boundary (§7 rule 7): no raw tool args/results
#    keys anywhere in any payload on the page.
LEAK=$(jq -r '[.events[] | .payload | select(. != null) | select((has("Args")) or (has("Arguments")) or (has("Result")) or (has("args")) or (has("result")))] | length' "${TMP}" 2>/dev/null || echo 0)
if [ "${LEAK}" -eq 0 ]; then
  ok "phase 161: no raw args/results keys in any read-back payload (content-free boundary holds)"
else
  fail "phase 161: ${LEAK} payload(s) carry raw args/results keys — content-free boundary VIOLATED (§7 rule 7)"
fi

smoke_summary
