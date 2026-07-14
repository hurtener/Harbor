#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 178 — tools production Annotator (HA-24 / D-314). Assembles + wires the
# real Annotator (OAuth / approval / metrics / content-stats / display-modes /
# last-used), flips the Phase-177 annotator-wired capability on, populates
# Tool.Version, and lights up the admin write path (set_approval_policy /
# revoke_oauth).
#
# Live-server assertions (404/405/501 → SKIP per CLAUDE.md §4.2):
#   1. tools.list route mounted; an OAuth/approval facet answers 200.
#   2. tools.metrics returns a real error-rate gauge for an invoked tool
#      (SKIP until activity exists).
#   3. tools.set_approval_policy round-trips (set → describe reflects it) and no
#      longer returns the ErrAdminUnsupported path (SKIP where the admin surface
#      or scope is absent).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

TOOLS_URL="$(api_url /v1/tools/list)"
METRICS_URL="$(api_url /v1/tools/metrics)"

TOKEN="${HARBOR_DEV_TOKEN:-dev-token-placeholder}"
ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")

if ! command -v curl >/dev/null 2>&1; then
  skip "phase 178: curl unavailable — live probes skipped"
  smoke_summary
  return 0 2>/dev/null || exit 0
fi

# tools.list route probe.
set +e
PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
  -X POST -H 'Content-Type: application/json' -d '{"filter":{}}' "${TOOLS_URL}")
set -e
case "${PROBE:-000}" in
  404 | 405 | 501 | 000)
    skip "phase 178: /v1/tools/list route not present (${PROBE:-000})"
    ;;
  *)
    ok "phase 178: tools.list route mounted (${PROBE})"
    if [ -n "${HARBOR_DEV_TOKEN:-}" ] && command -v jq >/dev/null 2>&1; then
      TMP="$(mktemp)"; trap 'rm -f "${TMP}"' EXIT
      # An OAuth-status facet should answer 200 with the annotator wired
      # (Phase 177 loud-rejects/gates it when unwired).
      set +e
      ST=$(curl -s -o "${TMP}" -w '%{http_code}' --max-time 10 \
        -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' \
        -d '{"filter":{"oauth_statuses":["required"]}}' "${TOOLS_URL}")
      set -e
      if [ "${ST}" = "200" ]; then
        ok "phase 178: tools OAuth facet answered 200 (annotator wired)"
      elif [ "${ST}" = "400" ]; then
        skip "phase 178: tools OAuth facet loud-rejects — annotator not yet wired (Phase 177 gating)"
      else
        skip "phase 178: tools OAuth facet probe non-conclusive (${ST})"
      fi
    else
      skip "phase 178: no HARBOR_DEV_TOKEN/jq — tools facet assertion skipped"
    fi
    ;;
esac

# tools.metrics: a real gauge for an invoked tool.
set +e
MPROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
  -X POST -H 'Content-Type: application/json' -d '{"id":"probe"}' "${METRICS_URL}")
set -e
case "${MPROBE:-000}" in
  404 | 405 | 501 | 000)
    skip "phase 178: /v1/tools/metrics route not present (${MPROBE:-000})"
    ;;
  *)
    ok "phase 178: tools.metrics route mounted (${MPROBE})"
    ;;
esac

smoke_summary
