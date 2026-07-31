#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 146 smoke — per-task structured output (the `output_schema` field
# on the `start` request → validated `answer_payload` on the task
# envelope, D-276).
#
# Legs:
#   1. Live (always-on): POST /v1/control/start with a NON-COMPILING
#      output_schema → non-200 + invalid_request-shaped error (the
#      Protocol-edge gate; no LLM dependency).
#   2. Live (degradable, phase-106 pattern): start a schema-constrained
#      task, poll /v1/tasks/get to terminal; on `complete` assert
#      result_inline carries an `answer_payload` conforming to the sent
#      schema; on `failed` under a keyless/mock-LLM preflight env → SKIP
#      with the failure shape logged.
#   3. Static: `output_schema` present in the committed
#      wire-manifest.gen.json (StartRequest) AND in the generated
#      docs/site/protocol/types.md (D-209 regen); exactly ONE
#      terminal-validation implementation (capturePayloadJSON /
#      FinishAnswerEnvelope live in the shared runctx package only) and
#      both drivers build the envelope through it before MarkComplete.
#   4. Unit-test leg: go test -race over ./internal/tasks/...,
#      ./internal/runtime/runctx/... and TestE2E_Phase146_*.

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

# ----------------------------------------------------------------------------
# Live-server assertions (require a booted harbor dev instance)
# ----------------------------------------------------------------------------

TOKEN="dev-token-placeholder"
if [ -n "${HARBOR_DEV_TOKEN:-}" ]; then
  TOKEN="${HARBOR_DEV_TOKEN}"
fi
ID_HEADERS=(
  -H "X-Harbor-Tenant: dev"
  -H "X-Harbor-User: dev"
  -H "X-Harbor-Session: dev"
)

START_URL="$(api_url /v1/control/start)"

# 1. Edge gate (always-on): a non-compiling output_schema is rejected
#    BEFORE a task spawns, with a non-200 + invalid_request error. No LLM
#    dependency — this leg keeps the phase's smoke OK > 0 regardless of the
#    preflight LLM env.
if skip_if_404 "${START_URL}" "phase 146: control.start edge gate"; then
  BAD_BODY='{"query":"classify","output_schema":{"type":}}'
  BAD_HTTP="$(curl -sS -o "/tmp/ph146_bad.$$" -w '%{http_code}' --max-time 5 \
    -X POST "${START_URL}" \
    -H "Authorization: Bearer ${TOKEN}" \
    "${ID_HEADERS[@]}" \
    -H "Content-Type: application/json" \
    -d "${BAD_BODY}" || true)"
  BAD_RESP="$(cat "/tmp/ph146_bad.$$" 2>/dev/null || echo '')"
  rm -f "/tmp/ph146_bad.$$"
  case "${BAD_HTTP}" in
    401|403)
      skip "phase 146: edge gate needs a valid dev token (got ${BAD_HTTP}); auth not available in this env"
      ;;
    200)
      fail "phase 146: a non-compiling output_schema was ACCEPTED (200) — the edge gate did not reject it"
      ;;
    *)
      if printf '%s' "${BAD_RESP}" | grep -q 'invalid_request'; then
        ok "phase 146: non-compiling output_schema rejected at the edge (${BAD_HTTP} invalid_request)"
      else
        fail "phase 146: bad output_schema returned ${BAD_HTTP} but no invalid_request code (body: $(printf '%s' "${BAD_RESP}" | head -c 200))"
      fi
      ;;
  esac

  # 2. Degradable happy path: a schema-constrained task's validated
  #    answer_payload lands on result_inline.
  SCHEMA='{"type":"object","required":["answer"],"properties":{"answer":{"type":"string"}},"additionalProperties":false}'
  START_RESP="$(curl -sS -X POST "${START_URL}" \
    -H "Authorization: Bearer ${TOKEN}" \
    "${ID_HEADERS[@]}" \
    -H "Content-Type: application/json" \
    -d "{\"query\":\"Reply with a JSON object {\\\"answer\\\":\\\"OK\\\"}\",\"output_schema\":${SCHEMA}}" || echo '{}')"
  TASK_ID="$(printf '%s' "${START_RESP}" | jq -r '.task_id // empty' 2>/dev/null || echo '')"
  if [ -z "${TASK_ID}" ]; then
    skip "phase 146: could not start a schema-constrained task (start returned: $(printf '%s' "${START_RESP}" | head -c 160))"
  else
    ok "phase 146: schema-constrained start returned task_id=${TASK_ID}"
    STATUS="pending"; DETAIL='{}'
    for _ in $(seq 1 30); do
      DETAIL="$(curl -sS -X POST "$(api_url /v1/tasks/get)" \
        -H "Authorization: Bearer ${TOKEN}" \
        "${ID_HEADERS[@]}" \
        -H "Content-Type: application/json" \
        -d "{\"id\":\"${TASK_ID}\"}" || echo '{}')"
      STATUS="$(printf '%s' "${DETAIL}" | jq -r '.task.status // "pending"' 2>/dev/null || echo 'pending')"
      { [ "${STATUS}" = "complete" ] || [ "${STATUS}" = "failed" ]; } && break
      sleep 1
    done
    if [ "${STATUS}" = "complete" ]; then
      PAYLOAD="$(printf '%s' "${DETAIL}" | jq -c '.result_inline.answer_payload // empty' 2>/dev/null || echo '')"
      if [ -n "${PAYLOAD}" ] && printf '%s' "${PAYLOAD}" | jq -e 'has("answer")' >/dev/null 2>&1; then
        ok "phase 146: completed task carries a schema-conforming answer_payload (${PAYLOAD})"
      else
        fail "phase 146: completed schema task missing a conforming answer_payload (result_inline: $(printf '%s' "${DETAIL}" | jq -c '.result_inline' | head -c 200))"
      fi
    else
      skip "phase 146: schema task terminal=${STATUS} (likely no working LLM in preflight env; unit/integration legs carry correctness)"
    fi
  fi
fi

# ----------------------------------------------------------------------------
# Static assertions
# ----------------------------------------------------------------------------

# 3a. output_schema is in the committed wire manifest under StartRequest.
if grep -q '"output_schema"' web/console/src/lib/protocol/wire-manifest.gen.json; then
  ok "phase 146: output_schema present in the committed wire-manifest.gen.json"
else
  fail "phase 146: output_schema missing from wire-manifest.gen.json (run make protocol-ts-gen)"
fi

# 3b. output_schema is in the generated Protocol types doc (D-209 regen).
if grep -q 'output_schema' docs/site/protocol/types.md; then
  ok "phase 146: output_schema present in the generated docs/site/protocol/types.md"
else
  fail "phase 146: output_schema missing from docs/site/protocol/types.md (run make protocol-docs-gen)"
fi

# 3c. Exactly ONE terminal-validation implementation: capturePayloadJSON
#     and FinishAnswerEnvelope live only in the shared runctx package.
CAP_COUNT="$(grep -rln 'func capturePayloadJSON' internal/ | wc -l | tr -d ' ')"
if [ "${CAP_COUNT}" = "1" ] && grep -q 'func capturePayloadJSON' internal/runtime/runctx/answer_envelope.go; then
  ok "phase 146: capturePayloadJSON defined in exactly one place (runctx)"
else
  fail "phase 146: capturePayloadJSON defined in ${CAP_COUNT} files, want exactly 1 (the shared runctx builder)"
fi
FAE_COUNT="$(grep -rln 'func FinishAnswerEnvelope' internal/ | wc -l | tr -d ' ')"
if [ "${FAE_COUNT}" = "1" ]; then
  ok "phase 146: FinishAnswerEnvelope defined in exactly one place (runctx)"
else
  fail "phase 146: FinishAnswerEnvelope defined in ${FAE_COUNT} files, want exactly 1"
fi

# 3d. Both per-task drivers + RunOnce consume the shared builder — no
#     private terminal-validation copy survives.
MISSING=0
for f in internal/runtime/assemble/runonce.go internal/runtime/serve/runloop.go; do
  grep -q 'runctx.FinishAnswerEnvelope' "$f" || { fail "phase 146: ${f} does not call runctx.FinishAnswerEnvelope"; MISSING=1; }
done
[ "${MISSING}" = "0" ] && ok "phase 146: RunOnce + the promoted per-task driver build the envelope via runctx.FinishAnswerEnvelope"

# 3e. The typed terminal code is defined and the promoted driver stamps it
# (the devstack twin is single-homed onto this driver — phase 159).
if grep -q 'TaskErrorCodeOutputInvalid' internal/planner/answer_envelope.go &&
   grep -q 'TaskErrorCodeOutputInvalid' internal/runtime/serve/runloop.go; then
  ok "phase 146: output_invalid terminal code defined and stamped by the promoted driver"
else
  fail "phase 146: TaskErrorCodeOutputInvalid wiring missing from the driver"
fi

# ----------------------------------------------------------------------------
# Unit / integration test leg (race-enabled)
# ----------------------------------------------------------------------------

if go test -race -count=1 -timeout 180s \
  ./internal/tasks/... \
  ./internal/runtime/runctx/... >/dev/null 2>&1; then
  ok "phase 146: tasks + runctx tests pass under -race"
else
  fail "phase 146: tasks/runctx unit tests failed (run go test -race ./internal/tasks/... ./internal/runtime/runctx/...)"
fi

if go test -race -count=1 -timeout 180s -run '^TestE2E_Phase146_' ./test/integration/... >/dev/null 2>&1; then
  ok "phase 146: per-task structured-output consumer E2E passes (TestE2E_Phase146_*)"
else
  fail "phase 146: consumer E2E failed (run go test -race -run TestE2E_Phase146_ ./test/integration/...)"
fi

smoke_summary
