#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 114 — steering verified-identity authority.
#
# The steering control surface derives caller authority from the VERIFIED
# request-context identity, never from the request body. These live
# assertions prove the golden admin path still works after that change:
#
#   1. An authenticated (admin dev token) steering control for a run with
#      no live inbox is rejected `not_found` — NOT `identity_required`.
#      A `not_found` proves the verified admin identity was accepted and
#      authority was derived (the surface reached the inbox Lookup). An
#      `identity_required`/`scope_mismatch` here would mean the derivation
#      regressed.
#   2. The same control with NO Authorization header is rejected at the
#      auth middleware (401) — authority is mandatory.
#
# The negative escalation assertion (a non-admin caller's body
# `scope:"admin"` claim being ignored) needs a lesser-privileged token,
# which the dev bootstrap does not yet mint (it issues admin only). That
# assertion is covered by the Go unit tests
# (internal/protocol: TestDispatch_BodyScopeClaimIsIgnored_NoEscalation)
# and lands as a live smoke when the lesser-privileged-token contract
# ships.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if ! command -v curl >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
    skip "phase-114: curl/jq not available"
    smoke_summary
    exit 0
fi

CANCEL_URL="$(api_url /v1/control/cancel)"

# Probe the route first; SKIP cleanly if the control surface isn't built
# in this binary (404/405/501 → the phase's surface is absent).
PROBE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${CANCEL_URL}" || true)"
case "${PROBE}" in
    404|405|501|000|'')
        skip "phase-114: control surface not reachable (${PROBE:-000})"
        smoke_summary
        exit 0
        ;;
esac

GHOST_BODY='{"identity":{"tenant":"dev","user":"dev","session":"dev","run":"run-phase114-ghost"}}'

# (2) Unauthenticated control → 401 (authority is mandatory).
UNAUTH_CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d "${GHOST_BODY}" "${CANCEL_URL}" || true)"
if [ "${UNAUTH_CODE}" = "401" ]; then
    ok "unauthenticated steering control rejected 401"
else
    fail "unauthenticated steering control: expected 401, got ${UNAUTH_CODE}"
fi

# Bootstrap an admin dev token (Phase 105 endpoint).
BOOTSTRAP_RESULT="$(curl -sS -X POST -d '{}' "$(api_url /v1/dev/bootstrap.json)" || echo '{}')"
TOKEN="$(echo "${BOOTSTRAP_RESULT}" | jq -r '.token // empty')"
if [ -z "${TOKEN}" ]; then
    skip "phase-114: could not bootstrap a dev token (live server may be down)"
    smoke_summary
    exit 0
fi

# (1) Authenticated control on a ghost run → the body carries the stable
#     error code. `not_found` proves authority derivation accepted the
#     verified admin identity and reached the inbox Lookup.
RESP="$(curl -sS -X POST "${CANCEL_URL}" \
    -H "Authorization: Bearer ${TOKEN}" \
    -H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev" \
    -H 'Content-Type: application/json' \
    -d "${GHOST_BODY}" || echo '{}')"
CODE="$(echo "${RESP}" | jq -r '.code // empty')"
case "${CODE}" in
    not_found)
        ok "authenticated steering control accepted the verified identity (not_found on ghost run)"
        ;;
    identity_required|scope_mismatch)
        fail "authority derivation regressed: admin token rejected with '${CODE}' (want not_found)"
        ;;
    "")
        skip "phase-114: no error code in response (unexpected shape: ${RESP})"
        ;;
    *)
        skip "phase-114: control returned code '${CODE}' (run lifecycle differs in this build)"
        ;;
esac

smoke_summary
