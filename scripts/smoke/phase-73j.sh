#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 73j — Console Memory page (Protocol + UI), D-118.
#
# Surface assertions (404/405/501 → SKIP per CLAUDE.md §4.2):
#   1. The three memory.* routes (POST /v1/memory/{list,get,health}) are
#      mounted on the live dev mux. On a pre-Phase-73j build they 404 →
#      SKIP gracefully.
#   2. A POST with NO Authorization header is rejected at the wire edge
#      with 401 — Phase 61 auth.Middleware fail-closes any
#      unauthenticated request before the handler runs (CLAUDE.md §6
#      rule 9, fail-loudly per §13).
#   3. A POST with a valid dev bearer (HARBOR_DEV_TOKEN, captured from
#      the dev binary's stderr banner into ${HARBOR_DATA_DIR}/server.log)
#      and an empty body returns 200 + a JSON response carrying the
#      expected shape (memory.list → an `items` array + page 1;
#      memory.health → an `aggregate.total` number).
#   4. A memory.list with a foreign-tenant filter and a NON-admin bearer
#      is rejected 403 — cross-tenant memory listing gates on
#      auth.ScopeAdmin from the D-079 closed set (NO new memory scope —
#      audit B1). Minting a non-admin token in shell is non-trivial; the
#      reject path is covered end-to-end by the Phase 73j integration
#      test, so the smoke records it as a SKIP.
#   5. The /memory SvelteKit page route is served by `harbor console`
#      (Phase 73m), NOT `harbor dev` — SKIPped here.
#
# The script SKIPs cleanly on pre-Phase-73j builds (the routes 404) and
# on builds where the dev binary's stderr banner is unreachable
# (HARBOR_DATA_DIR or server.log missing).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

LIST_URL="$(api_url /v1/memory/list)"
GET_URL="$(api_url /v1/memory/get)"
HEALTH_URL="$(api_url /v1/memory/health)"

if ! command -v curl >/dev/null 2>&1; then
  skip 'phase 73j: curl not available'
  smoke_summary
  exit 0
fi

# Surface probe — POST with empty body. Distinguishes a missing route
# (404) from "route exists but auth rejected" (401). The latter is
# expected and exercised below; the former triggers a clean SKIP.
set +e
PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
  -X POST -H 'Content-Type: application/json' -d '{}' "${LIST_URL}")
PROBE_RC=$?
set -e
if [ "${PROBE_RC}" -ne 0 ] || [ -z "${PROBE}" ]; then
  PROBE="000"
fi
case "${PROBE}" in
  404|405|501|000)
    skip "phase 73j: /v1/memory/list route not present (${PROBE})"
    smoke_summary
    exit 0
    ;;
esac

# 2. Missing-bearer rejection — the Phase 61 auth middleware fails
# closed without a verified bearer (401) BEFORE the handler runs. The
# routes are POST-only.
for pair in "list ${LIST_URL}" "get ${GET_URL}" "health ${HEALTH_URL}"; do
  name="${pair%% *}"
  url="${pair##* }"
  set +e
  NOAUTH=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${url}")
  set -e
  if [ "${NOAUTH}" = "401" ]; then
    ok "phase 73j: memory.${name} without identity/bearer rejected (401)"
  else
    fail "phase 73j: memory.${name} without bearer expected 401, got ${NOAUTH}"
  fi
done

# 3. Resolve the dev bearer from the preflight harness's captured
# server log. The dev binary prints `HARBOR_DEV_TOKEN=...` to stderr.
DEV_TOKEN=""
if [ -n "${HARBOR_DATA_DIR:-}" ] && [ -f "${HARBOR_DATA_DIR}/server.log" ]; then
  DEV_TOKEN="$(grep -m1 '^HARBOR_DEV_TOKEN=' "${HARBOR_DATA_DIR}/server.log" 2>/dev/null | sed 's/^HARBOR_DEV_TOKEN=//' || true)"
fi

if [ -n "${DEV_TOKEN}" ]; then
  TMP_BODY="$(mktemp)"
  trap 'rm -f "${TMP_BODY}"' EXIT

  # memory.list happy path — 200 + an `items` array + page 1.
  set +e
  STATUS=$(curl -s -o "${TMP_BODY}" -w '%{http_code}' --max-time 10 \
    -X POST -H "Authorization: Bearer ${DEV_TOKEN}" \
    -H 'Content-Type: application/json' -d '{}' "${LIST_URL}")
  set -e
  case "${STATUS}" in
    200)
      if command -v jq >/dev/null 2>&1; then
        ITEMS_TYPE=$(jq -r '.items | type' "${TMP_BODY}" 2>/dev/null || echo "")
        PAGE=$(jq -r '.page' "${TMP_BODY}" 2>/dev/null || echo "")
        if [ "${ITEMS_TYPE}" = "array" ]; then
          ok 'phase 73j: memory.list returns an items array'
        else
          fail "phase 73j: memory.list items type = ${ITEMS_TYPE}, want array"
        fi
        if [ "${PAGE}" = "1" ]; then
          ok 'phase 73j: memory.list defaults page=1'
        else
          fail "phase 73j: memory.list page = ${PAGE}, want 1"
        fi
      else
        skip 'phase 73j: memory.list shape assertions need jq'
      fi
      ;;
    404|405|501)
      skip "phase 73j: memory.list route not yet implemented (${STATUS})"
      ;;
    *)
      fail "phase 73j: memory.list happy-path expected 200, got ${STATUS}"
      ;;
  esac

  # memory.health happy path — 200 + an `aggregate.total` number.
  set +e
  HSTATUS=$(curl -s -o "${TMP_BODY}" -w '%{http_code}' --max-time 10 \
    -X POST -H "Authorization: Bearer ${DEV_TOKEN}" \
    -H 'Content-Type: application/json' -d '{}' "${HEALTH_URL}")
  set -e
  if [ "${HSTATUS}" = "200" ]; then
    if command -v jq >/dev/null 2>&1; then
      TOTAL_TYPE=$(jq -r '.aggregate.total | type' "${TMP_BODY}" 2>/dev/null || echo "")
      if [ "${TOTAL_TYPE}" = "number" ]; then
        ok 'phase 73j: memory.health returns aggregate.total'
      else
        fail "phase 73j: memory.health aggregate.total type = ${TOTAL_TYPE}, want number"
      fi
    else
      skip 'phase 73j: memory.health shape assertion needs jq'
    fi
  else
    fail "phase 73j: memory.health happy-path expected 200, got ${HSTATUS}"
  fi

  # memory.get with an unknown key → 404 (CodeNotFound). The runtime
  # fails loudly; never a silent empty detail.
  set +e
  GSTATUS=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
    -X POST -H "Authorization: Bearer ${DEV_TOKEN}" \
    -H 'Content-Type: application/json' -d '{"key":"mem_smoke_absent"}' "${GET_URL}")
  set -e
  if [ "${GSTATUS}" = "404" ]; then
    ok 'phase 73j: memory.get unknown key rejected (404 not_found)'
  else
    fail "phase 73j: memory.get unknown key expected 404, got ${GSTATUS}"
  fi
else
  skip 'phase 73j: memory.* happy path (HARBOR_DEV_TOKEN not found in server log)'
  skip 'phase 73j: memory.health happy path (needs dev token)'
  skip 'phase 73j: memory.get unknown-key rejection (needs dev token)'
fi

# 4. Cross-tenant filter WITHOUT the auth.ScopeAdmin claim → 403. The
# dev token is admin-scoped, so a foreign-tenant filter would NOT be
# rejected with it; minting a non-admin token in shell is non-trivial.
# The reject path is covered end-to-end by the Phase 73j integration
# test (test/integration/memory_page_test.go) with a real ES256
# keypair. NO new memory scope is involved — gating is on the D-079
# closed `auth.ScopeAdmin` set (audit B1).
skip 'phase 73j: cross-tenant memory.list WITHOUT auth.ScopeAdmin → 403 (covered by integration test)'

# 5. The /memory SvelteKit page route is served by `harbor console`
# (Phase 73m), NOT `harbor dev`.
skip 'phase 73j: /memory page route lands with 73m harbor console subcommand'

smoke_summary
