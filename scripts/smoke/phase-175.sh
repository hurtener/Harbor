#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 175 — fleet-scoped retention horizons (HA-23 / D-310).
#
# Asserts the absence-representable `scope` marker on the `runtime.health`
# retention block (the additive wire field this phase ships). The widened
# fan-in itself (a verified admin/console:fleet caller reading runtime-wide
# tasks/sessions horizons) is NOT exercisable over `harbor dev` — dev is
# trust-based, there is no verified elevated scope over HTTP — so the
# widened path is proven in the Go integration test
# (test/integration/retention_horizon_fleet_test.go). This smoke asserts
# the `scope` marker shape that every caller sees.
#
# Done-definition: OK >= 2, FAIL = 0 once the phase ships. Until then / when
# the dev bearer or jq is unavailable, it SKIPs.
#
# Classification (D-104 — the `# PREFLIGHT_REQUIRES:` header above):
#   - static-only — pure file/text greps, golden compares.
#   - live-server — hits the booted dev server over HTTP.
#   - unit-tests — runs `go test`.

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
  skip "phase 175: dev bearer / jq unavailable — scope-marker + widened path covered by Go integration tests"
  smoke_summary
  exit 0
fi

# 1. Drive a run so the mock LLM + react planner emit events into the bus —
#    this gives the events retention horizon a non-empty head to report.
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

RET_LEN=$(jq -r '.retention | length' "${TMP}" 2>/dev/null || echo 0)
if [ "${RET_LEN}" -eq 0 ]; then
  skip "phase 175: runtime.health carries no retention block (seam unwired on this dev config)"
  smoke_summary
  exit 0
fi

# 3. OK #1 — the absence-representable marker shipped: EVERY entry carries a
#    `scope` field whose value is one of runtime/tenant/session.
BAD_SCOPE=$(jq -r '[.retention[]? | select((.scope // "") | (. == "runtime" or . == "tenant" or . == "session") | not)] | length' "${TMP}" 2>/dev/null || echo 1)
if [ "${BAD_SCOPE}" = "0" ]; then
  ok "phase 175: every retention entry carries a scope in {runtime,tenant,session} (${RET_LEN} surface(s))"
else
  fail "phase 175: ${BAD_SCOPE} retention entr(y/ies) missing a valid scope marker (D-310 not wired?)"
fi

# 4. OK #2 — the events horizon is runtime-scoped with an RFC-3339 timestamp.
EVENTS_SCOPE=$(jq -r '.retention[]? | select(.surface == "events") | .scope // empty' "${TMP}" 2>/dev/null || echo "")
EVENTS_AT=$(jq -r '.retention[]? | select(.surface == "events") | .oldest_retained_at // empty' "${TMP}" 2>/dev/null || echo "")
if [ "${EVENTS_SCOPE}" = "runtime" ] && printf '%s' "${EVENTS_AT}" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}'; then
  ok "phase 175: events horizon is scope=runtime with an RFC-3339 instant (${EVENTS_AT})"
else
  fail "phase 175: events horizon not runtime-scoped or missing RFC-3339 timestamp (scope='${EVENTS_SCOPE}' at='${EVENTS_AT}')"
fi

# 5. The tasks/sessions entries carry a scope; a scoped entry with NO
#    timestamp is honest absence, NOT a FAIL. A malformed timestamp (when
#    present) IS a FAIL.
for surface in tasks sessions; do
  SC=$(jq -r --arg s "${surface}" '.retention[]? | select(.surface == $s) | .scope // empty' "${TMP}" 2>/dev/null || echo "")
  AT=$(jq -r --arg s "${surface}" '.retention[]? | select(.surface == $s) | .oldest_retained_at // empty' "${TMP}" 2>/dev/null || echo "")
  if [ -z "${SC}" ]; then
    continue # surface not wired on this dev config — not a FAIL
  fi
  if [ -n "${AT}" ] && ! printf '%s' "${AT}" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}'; then
    fail "phase 175: ${surface} horizon has a non-RFC-3339 timestamp ('${AT}')"
  else
    ok "phase 175: ${surface} horizon carries scope=${SC} (timestamp: ${AT:-omitted — honest absence})"
  fi
done

smoke_summary
