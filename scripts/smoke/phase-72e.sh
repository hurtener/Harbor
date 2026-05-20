#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 72e — pause.list snapshot Protocol method.
#
# Surface assertions (404/405/501 → SKIP per CLAUDE.md §4.2):
#   1. The pause.list route (POST /v1/pause/list) is mounted on the
#      live dev mux. When the Phase 72e surface has not landed, 404 →
#      SKIP gracefully (per the 404-SKIP convention).
#   2. A POST /v1/pause/list with NO Authorization header is rejected
#      at the wire edge with 401 — Phase 61 auth.Middleware fail-closes
#      any unauthenticated request before the handler runs (CLAUDE.md
#      §6 rule 9, fail-loudly per §13).
#   3. A POST with a valid dev bearer (HARBOR_DEV_TOKEN, captured from
#      the dev binary's stderr banner into ${HARBOR_DATA_DIR}/server.log)
#      and an empty body returns 200 + a JSON response carrying a
#      `snapshots` array, `page` 1 and `page_size` 50.
#   4. A POST with an oversized page_size (5000 > 200) returns 400 with
#      CodeInvalidRequest — the bound is enforced loud, never silently
#      clamped.
#   5. A POST with a cross-tenant filter naming a foreign tenant but a
#      NON-admin bearer returns 403 + identity_scope_required. The
#      reject-path is end-to-end covered by the Phase 72e integration
#      test (real ES256 testdata keypair); the smoke records it as a
#      SKIP because minting a non-admin token in shell is non-trivial.
#
# The script SKIPs cleanly on pre-Phase-72e builds (the route 404s) and
# on builds where the dev binary's stderr banner is unreachable
# (HARBOR_DATA_DIR or server.log missing).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

PL_URL="$(api_url /v1/pause/list)"

# Surface probe — POST with empty body. Distinguishes a missing route
# (404) from "route exists but auth rejected" (401). The latter is
# expected and exercised below as assertion 2; the former triggers a
# clean SKIP of the rest of the script.
if ! command -v curl >/dev/null 2>&1; then
  skip 'phase 72e: curl not available'
  smoke_summary
  exit 0
fi
set +e
PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
  -X POST -H 'Content-Type: application/json' -d '{}' \
  "${PL_URL}")
PROBE_RC=$?
set -e
if [ "${PROBE_RC}" -ne 0 ] || [ -z "${PROBE}" ]; then
  PROBE="000"
fi
case "${PROBE}" in
  404|405|501|000)
    skip "phase 72e: /v1/pause/list route not present (${PROBE})"
    smoke_summary
    exit 0
    ;;
esac

# 2. Missing-bearer rejection — the Phase 61 auth middleware fails
# closed without a verified bearer (401) BEFORE the handler runs. The
# route is POST-only, so this is an explicit POST (a GET would hit the
# 405 → SKIP path and never exercise the auth gate).
set +e
NOAUTH=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
  -X POST -H 'Content-Type: application/json' -d '{}' "${PL_URL}")
set -e
if [ "${NOAUTH}" = "401" ]; then
  ok 'phase 72e: pause.list without identity/bearer rejected (401)'
else
  fail "phase 72e: pause.list without bearer expected 401, got ${NOAUTH}"
fi

# 3. Resolve the dev bearer from the preflight harness's captured
# server log. The dev binary prints `HARBOR_DEV_TOKEN=...` to stderr
# at boot.
DEV_TOKEN=""
if [ -n "${HARBOR_DATA_DIR:-}" ] && [ -f "${HARBOR_DATA_DIR}/server.log" ]; then
  DEV_TOKEN="$(grep -m1 '^HARBOR_DEV_TOKEN=' "${HARBOR_DATA_DIR}/server.log" 2>/dev/null | sed 's/^HARBOR_DEV_TOKEN=//' || true)"
fi

if [ -n "${DEV_TOKEN}" ]; then
  # Happy path: valid bearer + empty body. Expect 200 + a JSON
  # response with a `snapshots` array, page 1, page_size 50.
  TMP_BODY="$(mktemp)"
  trap 'rm -f "${TMP_BODY}"' EXIT
  set +e
  STATUS=$(curl -s -o "${TMP_BODY}" -w '%{http_code}' --max-time 10 \
    -X POST \
    -H "Authorization: Bearer ${DEV_TOKEN}" \
    -H 'Content-Type: application/json' \
    -d '{}' \
    "${PL_URL}")
  set -e
  case "${STATUS}" in
    200)
      if command -v jq >/dev/null 2>&1; then
        SNAP_TYPE=$(jq -r '.snapshots | type' "${TMP_BODY}" 2>/dev/null || echo "")
        PAGE=$(jq -r '.page' "${TMP_BODY}" 2>/dev/null || echo "")
        PAGE_SIZE=$(jq -r '.page_size' "${TMP_BODY}" 2>/dev/null || echo "")
        if [ "${SNAP_TYPE}" = "array" ]; then
          ok 'phase 72e: pause.list returns a snapshots array'
        else
          fail "phase 72e: pause.list snapshots type = ${SNAP_TYPE}, want array"
        fi
        if [ "${PAGE}" = "1" ]; then
          ok 'phase 72e: pause.list defaults page=1'
        else
          fail "phase 72e: pause.list page = ${PAGE}, want 1"
        fi
        if [ "${PAGE_SIZE}" = "50" ]; then
          ok 'phase 72e: pause.list defaults page_size=50'
        else
          fail "phase 72e: pause.list page_size = ${PAGE_SIZE}, want 50"
        fi
      else
        skip 'phase 72e: snapshot-shape assertions need jq'
      fi
      ;;
    404|405|501)
      skip "phase 72e: pause.list route not yet implemented (${STATUS})"
      ;;
    *)
      fail "phase 72e: pause.list happy-path expected 200, got ${STATUS}"
      ;;
  esac

  # 4. Oversized page_size — 5000 is above the documented max of 200.
  # The handler rejects it with CodeInvalidRequest (400); a silent
  # clamp would defeat the per-row identity boundary the snapshot
  # guarantees.
  set +e
  BADSTATUS=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
    -X POST \
    -H "Authorization: Bearer ${DEV_TOKEN}" \
    -H 'Content-Type: application/json' \
    -d '{"page_size": 5000}' \
    "${PL_URL}")
  set -e
  if [ "${BADSTATUS}" = "400" ]; then
    ok 'phase 72e: pause.list rejects page_size out of [1,200] range with 400'
  else
    fail "phase 72e: oversized page_size expected 400, got ${BADSTATUS}"
  fi
else
  skip 'phase 72e: pause.list happy path (HARBOR_DEV_TOKEN not found in server log)'
  skip 'phase 72e: pause.list oversized-page_size rejection (needs dev token)'
fi

# 5. Cross-tenant filter WITHOUT the admin scope claim → 403 +
# identity_scope_required. Minting a non-admin token in shell is
# non-trivial; the integration test
# (test/integration/pause_list_test.go) covers this end-to-end with the
# real ES256 testdata keypair.
skip 'phase 72e: cross-tenant filter WITHOUT scope claim → 403 identity_scope_required (covered by integration test)'

smoke_summary
