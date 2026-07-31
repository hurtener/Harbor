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
#   4. (env-gated, HARBOR_LIVE_LLM) a HIGH-effort Claude run surfaces at least
#      one NON-EMPTY reasoning chunk (llm.completion.chunk Kind=reasoning) OR a
#      planner.decision with a non-empty ReasoningTrace — the Anthropic
#      reasoning-surfaces behaviour 161/165 depend on. CI (mock) SKIPs.
# Done-definition: OK >= 2, FAIL = 0; 404/405/501 → SKIP until the phase ships.

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

# 5. Reasoning-SURFACES regression (env-gated, 131d precedent). The mock path
#    above proves the STRUCTURE (ReasoningTrace/DecisionKind keys) but leaves
#    the traces EMPTY. This leg locks in the Anthropic-reasoning-surfaces
#    behaviour that 161/165 depend on: a HIGH-effort Claude run MUST yield at
#    least one NON-EMPTY reasoning chunk (llm.completion.chunk, Kind=reasoning)
#    OR a planner.decision with a non-empty ReasoningTrace in the durable
#    read-back. Requires the running server to be backed by a real Claude
#    provider — CI (mock path) SKIPs.
if [ -z "${HARBOR_LIVE_LLM:-}" ]; then
  skip "phase 165: live reasoning-surfaces regression (set HARBOR_LIVE_LLM=1 with a Claude-backed server to run)"
  smoke_summary
  exit 0
fi

# Bias the next turn to high reasoning effort via runs.set_overrides, then
# drive a reasoning-eliciting run under a dedicated session.
RSESS="phase165-reasoning-live"
RID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: ${RSESS}")
OVR_URL="$(api_url /v1/runs/set_overrides)"
curl -sS -X POST "${OVR_URL}" -H "Authorization: Bearer ${TOKEN}" \
  "${RID_HEADERS[@]}" -H 'Content-Type: application/json' \
  -d "{\"overrides\":{\"session_id\":\"${RSESS}\",\"reasoning_effort\":\"high\"}}" >/dev/null 2>&1 || true

curl -sS -X POST "${START_URL}" -H "Authorization: Bearer ${TOKEN}" \
  "${RID_HEADERS[@]}" -H 'Content-Type: application/json' \
  -d '{"query":"A farmer has 17 sheep; all but 9 run away. Think step by step, then state how many remain.","description":"phase-165 live reasoning smoke"}' >/dev/null 2>&1 || true
sleep 4

RTMP="$(mktemp)"
trap 'rm -f "${TMP}" "${RTMP}"' EXIT
set +e
RST=$(curl -s -o "${RTMP}" -w '%{http_code}' --max-time 15 \
  -X POST -H "Authorization: Bearer ${TOKEN}" "${RID_HEADERS[@]}" \
  -H 'Content-Type: application/json' \
  -d "{\"session_id\":\"${RSESS}\",\"before\":0,\"limit\":400}" "${STATE_URL}")
set -e

if [ "${RST}" != "200" ]; then
  fail "phase 165: live reasoning read-back returned ${RST} (want 200 against a Claude-backed server)"
  smoke_summary
  exit 1
fi

REASON_CHUNKS=$(jq -r '[.events[] | select(.type == "llm.completion.chunk") | select(.payload.Kind == "reasoning") | select((.payload.Delta // "") | length > 0)] | length' "${RTMP}" 2>/dev/null || echo 0)
REASON_TRACES=$(jq -r '[.events[] | select(.type == "planner.decision") | select((.payload.ReasoningTrace // "") | length > 0)] | length' "${RTMP}" 2>/dev/null || echo 0)
if [ "${REASON_CHUNKS}" -gt 0 ] || [ "${REASON_TRACES}" -gt 0 ]; then
  ok "phase 165: high-effort Claude run surfaced non-empty reasoning (chunks=${REASON_CHUNKS} traces=${REASON_TRACES}) — reasoning-surfaces regression locked in"
else
  fail "phase 165: high-effort Claude run produced NO non-empty reasoning chunk/trace in state.history — Anthropic reasoning surfaces regressed"
fi

smoke_summary
