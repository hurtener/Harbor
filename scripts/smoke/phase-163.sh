#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 163 — windowed-reads honesty pair (flows since/until + retention
# horizons on runtime.health).
#
# When the phase lands, this asserts (two health-side assertions so the
# done-definition is meetable even when the dev config declares no flows):
#   - runtime.health carries the additive `retention` block;
#   - after a scripted run (the `start` method) the block's `events` entry
#     carries a non-empty, RFC-3339-parsable oldest_retained_at;
#   - flows.runs.list accepts since/until without invalid_request
#     (skip_if_404 when the dev config declares no flows — the bounds
#     semantics are then covered by the integration test).
# Done-definition: OK >= 2, FAIL = 0 (achievable from the two health
# assertions alone) once the phase ships.
# Until then it SKIPs. Real assertions land with the implementation PR.
#
#   cp scripts/smoke/_template.sh scripts/smoke/phase-NN.sh
#   chmod +x scripts/smoke/phase-NN.sh
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# Classification (D-104 — the `# PREFLIGHT_REQUIRES:` header above):
#   - static-only — pure file/text greps, golden compares, file-existence
#     assertions. Runs in the parallel batch BEFORE the dev server boots.
#   - live-server — hits the booted dev server over HTTP (`api_url`,
#     `assert_status`, `skip_if_404`, `assert_json_path`) or reads the
#     preflight server log. Runs serially against the booted instance.
#   - unit-tests — runs `go test` for one or more packages. Parallelisable;
#     `go test` schedules its own internal parallelism.
#
# Pick `live-server` whenever the smoke depends on `HARBOR_BIND` /
# `HARBOR_BASE_URL` / `HARBOR_DEV_TOKEN` / `${HARBOR_DATA_DIR}/server.log`
# or invokes the built `bin/harbor` against a network endpoint. When in
# doubt, `live-server` is the safe default — misclassifying a
# server-touching smoke as `static-only` produces nondeterministic flakes.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Phase NN assertions go below. Examples:
#
#   assert_status 200 "$(api_url /healthz)" "healthz returns 200"
#   assert_json_path '.status' 'ok' "$(api_url /readyz)" "readyz reports status=ok"
#   protocol_call 'sessions/create' '{"tenant":"t1","user":"u1"}' "create session"
#
# Until the phase ships, the script can be empty assertions or a single
# `skip "phase NN: not yet implemented"` to keep preflight green.
# ----------------------------------------------------------------------------

HEALTH_URL="$(api_url /v1/control/runtime.health)"
START_URL="$(api_url /v1/control/start)"
RUNS_URL="$(api_url /v1/flows/runs/list)"

TOKEN="${HARBOR_DEV_TOKEN:-dev-token-placeholder}"
ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")

if ! command -v curl >/dev/null 2>&1; then
  skip "phase 163: curl not available"
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
    skip "phase 163: /v1/control/runtime.health route not present (${PROBE:-000})"
    smoke_summary
    exit 0
    ;;
esac

if [ -z "${HARBOR_DEV_TOKEN:-}" ] || ! command -v jq >/dev/null 2>&1; then
  skip "phase 163: dev bearer / jq unavailable — retention + bounds semantics covered by Go integration tests"
  smoke_summary
  exit 0
fi

# 1. Drive a run so the mock LLM + react planner emit events into the bus —
#    this gives the events retention horizon a non-empty head to report.
curl -sS -X POST "${START_URL}" -H "Authorization: Bearer ${TOKEN}" \
  "${ID_HEADERS[@]}" -H 'Content-Type: application/json' \
  -d '{"query":"phase-163 retention-horizon seed","description":"phase-163 smoke"}' >/dev/null 2>&1 || true
sleep 2

# 2. Read runtime.health and assert the additive retention block is present.
TMP="$(mktemp)"
trap 'rm -f "${TMP}"' EXIT
set +e
ST=$(curl -s -o "${TMP}" -w '%{http_code}' --max-time 10 \
  -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
  -H 'Content-Type: application/json' -d '{}' "${HEALTH_URL}")
set -e
if [ "${ST}" != "200" ]; then
  skip "phase 163: runtime.health returned ${ST}"
  smoke_summary
  exit 0
fi

RET_LEN=$(jq -r '.retention | length' "${TMP}" 2>/dev/null || echo 0)
if [ "${RET_LEN}" -gt 0 ]; then
  ok "phase 163: runtime.health carries the additive retention block (${RET_LEN} surface(s))"
else
  fail "phase 163: runtime.health carries no retention block after a scripted run (D-296 seam not wired?)"
fi

# 3. The events retention entry carries a non-empty, RFC-3339-shaped
#    oldest_retained_at (the observed head of the durable event log).
EVENTS_AT=$(jq -r '.retention[]? | select(.surface == "events") | .oldest_retained_at // empty' "${TMP}" 2>/dev/null || echo "")
if printf '%s' "${EVENTS_AT}" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}'; then
  ok "phase 163: events retention horizon is a non-empty RFC-3339 instant (${EVENTS_AT})"
else
  fail "phase 163: events retention horizon missing or not RFC-3339 (got '${EVENTS_AT}')"
fi

# 4. Flows leg (skip_if_404 when the dev config declares no flows — the
#    bounds semantics are integration-covered): flows.runs.list ACCEPTS
#    since/until without an invalid_request. Any flow_id is fine; a
#    not_found flow still proves the bounds parsed. invalid_request (the
#    until<since / malformed-bounds error) is the only FAIL shape.
set +e
FCODE=$(curl -s -o "${TMP}" -w '%{http_code}' --max-time 5 \
  -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
  -H 'Content-Type: application/json' \
  -d '{"flow_id":"__smoke__","since":"2026-01-01T00:00:00Z","until":"2026-12-31T00:00:00Z","page_size":1}' \
  "${RUNS_URL}")
set -e
case "${FCODE:-000}" in
  404 | 405 | 501 | 000)
    skip "phase 163: flows.runs.list route not present (${FCODE:-000}) — bounds covered by integration"
    ;;
  400)
    ERRC=$(jq -r '.error.code // .code // empty' "${TMP}" 2>/dev/null || echo "")
    if [ "${ERRC}" = "invalid_request" ]; then
      fail "phase 163: flows.runs.list rejected valid since/until as invalid_request"
    else
      ok "phase 163: flows.runs.list accepted since/until (400 ${ERRC:-non-bounds} — not a bounds rejection)"
    fi
    ;;
  *)
    ok "phase 163: flows.runs.list accepted since/until (${FCODE})"
    ;;
esac

smoke_summary
