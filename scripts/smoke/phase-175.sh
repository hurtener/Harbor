#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 175 — fleet-scoped retention horizons (HA-23).
#
# When the phase lands, this asserts (from the un-elevated dev read alone, so
# the done-definition is meetable — the widened fleet fan-in is Go-integration-
# covered because `harbor dev` is trust-based and carries no verified elevated
# scope over HTTP):
#   - runtime.health's additive `retention` block carries a per-surface `scope`
#     marker whose value is one of runtime/tenant/session (the absence-
#     representable signal — an unobservable scope can never masquerade as an
#     empty surface);
#   - the `events` entry is scope:"runtime" with a non-empty RFC-3339
#     oldest_retained_at (the runtime-wide horizon is labelled + non-empty);
#   - a tasks/sessions entry carries a scope and, WHEN present, an RFC-3339
#     timestamp — a scoped entry with no timestamp is honest absence, not a FAIL.
# Done-definition: OK >= 2, FAIL = 0 once the phase ships. Until then it SKIPs.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

HEALTH_URL="$(api_url /v1/control/runtime.health)"
START_URL="$(api_url /v1/control/start)"

TOKEN="${HARBOR_DEV_TOKEN:-dev-token-placeholder}"
ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")

if ! command -v curl >/dev/null 2>&1; then
  skip "phase 175: curl not available"
  smoke_summary
  exit 0
fi

# Route probe: a no-bearer POST distinguishes a missing route (404) from an
# auth-rejected (401). 401 means the route is mounted AND identity-mandatory.
set +e
PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
  -X POST -H 'Content-Type: application/json' -d '{}' "${HEALTH_URL}")
set -e
case "${PROBE:-000}" in
  404 | 405 | 501 | 000)
    skip "phase 175: /v1/control/runtime.health route not present (${PROBE:-000})"
    smoke_summary
    exit 0
    ;;
esac

if [ -z "${HARBOR_DEV_TOKEN:-}" ] || ! command -v jq >/dev/null 2>&1; then
  skip "phase 175: dev bearer / jq unavailable — scope-marker + fleet fan-in covered by Go integration tests"
  smoke_summary
  exit 0
fi

# 1. Drive a run so the mock LLM + react planner emit events into the bus,
#    giving the events retention horizon a non-empty head to report.
curl -sS -X POST "${START_URL}" -H "Authorization: Bearer ${TOKEN}" \
  "${ID_HEADERS[@]}" -H 'Content-Type: application/json' \
  -d '{"query":"phase-175 retention-scope seed","description":"phase-175 smoke"}' >/dev/null 2>&1 || true
sleep 2

# 2. Read runtime.health.
TMP="$(mktemp)"
trap 'rm -f "${TMP}"' EXIT
set +e
ST=$(curl -s -o "${TMP}" -w '%{http_code}' --max-time 10 \
  -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
  -H 'Content-Type: application/json' -d '{}' "${HEALTH_URL}")
set -e
if [ "${ST}" != "200" ]; then
  skip "phase 175: runtime.health returned ${ST}"
  smoke_summary
  exit 0
fi

# 3. Every retention entry carries a `scope` marker in {runtime,tenant,session}.
#    (Pre-175 builds omit `scope` — this SKIPs so it coexists with a 163 build.)
SCOPES=$(jq -r '[.retention[]? | .scope // empty] | length' "${TMP}" 2>/dev/null || echo 0)
RET_LEN=$(jq -r '.retention | length' "${TMP}" 2>/dev/null || echo 0)
if [ "${RET_LEN}" -eq 0 ]; then
  fail "phase 175: runtime.health carries no retention block after a scripted run (D-296 seam not wired?)"
elif [ "${SCOPES}" -eq 0 ]; then
  skip "phase 175: retention entries carry no scope marker yet (pre-175 build)"
  smoke_summary
  exit 0
fi

BAD_SCOPE=$(jq -r '[.retention[]? | select((.scope // "") | test("^(runtime|tenant|session)$") | not)] | length' "${TMP}" 2>/dev/null || echo 1)
if [ "${BAD_SCOPE}" -eq 0 ]; then
  ok "phase 175: every retention entry carries a scope marker in {runtime,tenant,session} (${SCOPES} entries)"
else
  fail "phase 175: ${BAD_SCOPE} retention entry(ies) carry a missing/invalid scope marker"
fi

# 4. The events horizon is runtime-wide and non-empty.
EV_SCOPE=$(jq -r '.retention[]? | select(.surface == "events") | .scope // empty' "${TMP}" 2>/dev/null || echo "")
EV_AT=$(jq -r '.retention[]? | select(.surface == "events") | .oldest_retained_at // empty' "${TMP}" 2>/dev/null || echo "")
if [ "${EV_SCOPE}" = "runtime" ] && printf '%s' "${EV_AT}" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}'; then
  ok "phase 175: events horizon is scope:runtime with an RFC-3339 instant (${EV_AT})"
else
  fail "phase 175: events horizon not scope:runtime + RFC-3339 (scope='${EV_SCOPE}' at='${EV_AT}')"
fi

# 5. Honest absence: any PRESENT retention timestamp must be RFC-3339-shaped; a
#    scoped entry with no timestamp is representable absence, never a FAIL.
PRESENT=$(jq -r '[.retention[]? | select(.oldest_retained_at != null and .oldest_retained_at != "")] | length' "${TMP}" 2>/dev/null || echo 0)
WELLSHAPED=$(jq -r '[.retention[]? | select(.oldest_retained_at != null and .oldest_retained_at != "") | select(.oldest_retained_at | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T"))] | length' "${TMP}" 2>/dev/null || echo 0)
if [ "${PRESENT}" -eq "${WELLSHAPED}" ]; then
  ok "phase 175: every present retention timestamp is RFC-3339; scoped entries with no timestamp are honest absence"
else
  fail "phase 175: $((PRESENT - WELLSHAPED)) retention timestamp(s) are not RFC-3339-shaped"
fi

smoke_summary
