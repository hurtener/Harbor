#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 72a — events.subscribe filter extensions + events.aggregate.
#
# Surface assertions (404/405/501 → SKIP per CLAUDE.md §4.2):
#   1. The events.aggregate route (POST /v1/events/aggregate) is mounted
#      on the live dev mux. When the Phase 72a surface has not landed,
#      404 → SKIP gracefully (per the 404-SKIP convention).
#   2. A POST /v1/events/aggregate with NO Authorization header is
#      rejected at the wire edge with 401 — Phase 61 auth.Middleware
#      fail-closes any unauthenticated request before the handler runs
#      (CLAUDE.md §6 rule 9, fail-loudly per §13).
#   3. A POST with a valid dev bearer (HARBOR_DEV_TOKEN, captured from
#      the dev binary's stderr banner into ${HARBOR_DATA_DIR}/server.log)
#      and a structurally-valid request body returns 200 + a JSON
#      response carrying a `buckets` array whose length matches
#      Window/Bucket. The dev token carries the `admin` scope per
#      cmd/harbor/devauth.go, so it satisfies the cross-tenant gate.
#   4. A POST with an invalid Window/Bucket pair (7-minute bucket on a
#      1-hour window — does not divide evenly) returns 400 with
#      CodeInvalidRequest. The aggregator fails loudly on a non-
#      dividing pair so a rendering client never sees a fractional
#      trailing bucket.
#   5. A POST /v1/events/aggregate with a body naming TWO tenants but
#      a NON-admin bearer returns 403 + identity_scope_required. The
#      reject-path is end-to-end-covered by the Phase 72a integration
#      test (real ES256 testdata keypair); the smoke records it as a
#      SKIP because minting a non-admin token in shell is non-trivial.
#
# The script SKIPs cleanly on pre-Phase-72a builds (the route 404s) and
# on builds where the dev binary's stderr banner is unreachable
# (HARBOR_DATA_DIR or server.log missing).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

AGG_URL="$(api_url /v1/events/aggregate)"

# Surface probe — POST with empty body. Distinguishes a missing route
# (404) from "route exists but auth rejected" (401). The latter is
# expected and exercised below as assertion 2; the former triggers a
# clean SKIP of the rest of the script.
if ! command -v curl >/dev/null 2>&1; then
  skip 'phase 72a: curl not available'
  smoke_summary
  exit 0
fi
set +e
PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
  -X POST -H 'Content-Type: application/json' -d '{}' \
  "${AGG_URL}")
PROBE_RC=$?
set -e
if [ "${PROBE_RC}" -ne 0 ] || [ -z "${PROBE}" ]; then
  PROBE="000"
fi
case "${PROBE}" in
  404|405|501|000)
    skip "phase 72a: /v1/events/aggregate route not present (${PROBE})"
    smoke_summary
    exit 0
    ;;
esac

# 2. Missing-bearer rejection — the Phase 61 auth middleware fails
# closed without a verified bearer (401) BEFORE the handler runs.
assert_status 401 "${AGG_URL}" \
  'phase 72a: events.aggregate without identity/bearer rejected (401)'

# 3. Resolve the dev bearer from the preflight harness's captured
# server log. The dev binary prints `HARBOR_DEV_TOKEN=...` to stderr
# at boot.
DEV_TOKEN=""
if [ -n "${HARBOR_DATA_DIR:-}" ] && [ -f "${HARBOR_DATA_DIR}/server.log" ]; then
  DEV_TOKEN="$(grep -m1 '^HARBOR_DEV_TOKEN=' "${HARBOR_DATA_DIR}/server.log" 2>/dev/null | sed 's/^HARBOR_DEV_TOKEN=//' || true)"
fi

if [ -n "${DEV_TOKEN}" ]; then
  # Happy path: valid bearer + structurally-valid request body. Expect
  # 200 + a JSON response with a `buckets` array of length 60 (1h
  # window / 1m bucket).
  TMP_BODY="$(mktemp)"
  trap 'rm -f "${TMP_BODY}"' EXIT
  set +e
  STATUS=$(curl -s -o "${TMP_BODY}" -w '%{http_code}' --max-time 10 \
    -X POST \
    -H "Authorization: Bearer ${DEV_TOKEN}" \
    -H 'Content-Type: application/json' \
    -d '{"window": 3600000000000, "bucket": 60000000000}' \
    "${AGG_URL}")
  set -e
  case "${STATUS}" in
    200)
      if command -v jq >/dev/null 2>&1; then
        BUCKETS=$(jq -r '.buckets | length' "${TMP_BODY}" 2>/dev/null || echo "")
        if [ "${BUCKETS}" = "60" ]; then
          ok 'phase 72a: events.aggregate returns 60 buckets for 1h window / 1m bucket'
        else
          fail "phase 72a: events.aggregate buckets length = ${BUCKETS}, want 60"
        fi
      else
        skip 'phase 72a: bucket-length assertion needs jq'
      fi
      ;;
    404|405|501)
      skip "phase 72a: events.aggregate route not yet implemented (${STATUS})"
      ;;
    *)
      fail "phase 72a: events.aggregate happy-path expected 200, got ${STATUS}"
      ;;
  esac

  # 4. Bad Window/Bucket pair — 7-minute bucket on a 1-hour window.
  # The aggregator fails with CodeInvalidRequest (400) so a rendering
  # client never sees a fractional trailing bucket.
  set +e
  BADSTATUS=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
    -X POST \
    -H "Authorization: Bearer ${DEV_TOKEN}" \
    -H 'Content-Type: application/json' \
    -d '{"window": 3600000000000, "bucket": 420000000000}' \
    "${AGG_URL}")
  set -e
  if [ "${BADSTATUS}" = "400" ]; then
    ok 'phase 72a: events.aggregate rejects non-dividing Window/Bucket with 400'
  else
    fail "phase 72a: bad Window/Bucket expected 400, got ${BADSTATUS}"
  fi
else
  skip 'phase 72a: events.aggregate happy path (HARBOR_DEV_TOKEN not found in server log)'
  skip 'phase 72a: events.aggregate bad-window rejection (needs dev token)'
fi

# 5. Cross-tenant filter WITHOUT scope claim → 403 + identity_scope_required.
# Minting a non-admin token in shell is non-trivial; the integration
# test (test/integration/events_filter_aggregate_test.go) covers this
# end-to-end with the real ES256 testdata keypair.
skip 'phase 72a: cross-tenant filter WITHOUT scope claim → 403 identity_scope_required (covered by integration test)'

smoke_summary
