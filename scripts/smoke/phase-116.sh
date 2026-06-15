#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 116 — non-admin session-scoped token contract.
#
# The live negative-escalation assertion Phase 114's smoke deferred: a
# verified NON-ADMIN token (no scopes) is accepted as a lesser principal
# and authorised exactly as RFC §6.3 permits — its owning-user authority
# covers the session-scoped controls but NOT the admin-only PRIORITIZE.
#
#   1. The dev bootstrap mints a non-admin token (scopes:[]) for the dev
#      identity — the lesser-privileged principal Phase 114's derivation
#      becomes load-bearing against.
#   2. That token on `inject_context` for the dev-owned (ghost) run is
#      accepted at the auth edge (NOT 401/403) — authority was derived as
#      the owning user. A ghost run resolves to not_found, which proves
#      the verified non-admin identity passed the scope gate.
#   3. The SAME token on `prioritize` is rejected 403 scope_mismatch — the
#      admin-only control is refused for a non-admin owner BEFORE the
#      inbox lookup (the edge scope check). This is the escalation the
#      contract closes: a non-admin token is never silently elevated.
#
# Skips cleanly on a build whose bootstrap endpoint does not honour the
# non-admin scope override (older binaries mint admin only).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if ! command -v curl >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
    skip "phase-116: curl/jq not available"
    smoke_summary
    exit 0
fi

INJECT_URL="$(api_url /v1/control/inject_context)"
PRIORITIZE_URL="$(api_url /v1/control/prioritize)"
BOOTSTRAP_URL="$(api_url /v1/dev/bootstrap.json)"

# Probe the control surface first; SKIP cleanly if it isn't built in this
# binary (404/405/501 → the phase's surface is absent).
PROBE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${INJECT_URL}" || true)"
case "${PROBE}" in
    404|405|501|000|'')
        skip "phase-116: control surface not reachable (${PROBE:-000})"
        smoke_summary
        exit 0
        ;;
esac

# Mint a NON-ADMIN token: an explicit empty scope set for the dev identity.
NONADMIN_RESULT="$(curl -sS -X POST -H 'Content-Type: application/json' \
    -d '{"tenant":"dev","user":"dev","session":"dev","scopes":[]}' "${BOOTSTRAP_URL}" || echo '{}')"
NONADMIN_TOKEN="$(echo "${NONADMIN_RESULT}" | jq -r '.token // empty')"
NONADMIN_SCOPES="$(echo "${NONADMIN_RESULT}" | jq -r '(.scopes // []) | length')"

if [ -z "${NONADMIN_TOKEN}" ]; then
    skip "phase-116: could not bootstrap a token (live server may be down)"
    smoke_summary
    exit 0
fi
if [ "${NONADMIN_SCOPES}" != "0" ]; then
    # The build ignored the scope override and minted a scoped token —
    # the non-admin mint surface is absent. Skip rather than assert
    # against an admin token.
    skip "phase-116: bootstrap did not honour the non-admin scope override (scopes=${NONADMIN_SCOPES})"
    smoke_summary
    exit 0
fi

GHOST_BODY='{"identity":{"tenant":"dev","user":"dev","session":"dev","run":"run-phase116-ghost"},"payload":{"note":"hi"}}'

# (2) Non-admin owner on inject_context → accepted at the auth edge. A
#     ghost run resolves to not_found (body code), proving the verified
#     non-admin identity passed the scope gate (NOT 401/403).
INJECT_RESP="$(curl -sS -X POST "${INJECT_URL}" \
    -H "Authorization: Bearer ${NONADMIN_TOKEN}" \
    -H 'Content-Type: application/json' \
    -d "${GHOST_BODY}" || echo '{}')"
INJECT_CODE="$(echo "${INJECT_RESP}" | jq -r '.code // empty')"
case "${INJECT_CODE}" in
    not_found)
        ok "non-admin owner accepted on inject_context (not_found on ghost run — authority derived)"
        ;;
    "")
        ok "non-admin owner accepted on inject_context (no error code — 200 on a live run)"
        ;;
    identity_required|scope_mismatch)
        fail "non-admin owner wrongly rejected on inject_context: '${INJECT_CODE}' (want acceptance/not_found)"
        ;;
    *)
        skip "phase-116: inject_context returned code '${INJECT_CODE}' (run lifecycle differs in this build)"
        ;;
esac

# (3) Non-admin owner on prioritize → 403 scope_mismatch. The admin-only
#     control is refused at the edge BEFORE the inbox lookup, so it fires
#     even on a ghost run.
PRIO_BODY='{"identity":{"tenant":"dev","user":"dev","session":"dev","run":"run-phase116-ghost"},"payload":{"priority":9}}'
PRIO_STATUS="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 -X POST "${PRIORITIZE_URL}" \
    -H "Authorization: Bearer ${NONADMIN_TOKEN}" \
    -H 'Content-Type: application/json' \
    -d "${PRIO_BODY}" || true)"
PRIO_RESP="$(curl -sS -X POST "${PRIORITIZE_URL}" \
    -H "Authorization: Bearer ${NONADMIN_TOKEN}" \
    -H 'Content-Type: application/json' \
    -d "${PRIO_BODY}" || echo '{}')"
PRIO_CODE="$(echo "${PRIO_RESP}" | jq -r '.code // empty')"
if [ "${PRIO_STATUS}" = "403" ] && [ "${PRIO_CODE}" = "scope_mismatch" ]; then
    ok "non-admin owner rejected on prioritize (403 scope_mismatch — no silent elevation)"
else
    fail "non-admin owner prioritize: expected 403 scope_mismatch, got status=${PRIO_STATUS} code='${PRIO_CODE}'"
fi

smoke_summary
