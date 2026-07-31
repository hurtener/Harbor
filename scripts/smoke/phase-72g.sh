#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
# Phase 72g smoke — `governance.posture` + `llm.posture` Protocol methods.
#
# The preflight harness boots `./bin/harbor dev` on an ephemeral port
# (HARBOR_BIND=127.0.0.1:0 — D-104; the bound port is parsed from the
# server log and exported as HARBOR_BASE_URL / HARBOR_BIND / HARBOR_PORT)
# and runs this script against the live server. The harness also sets
# HARBOR_DEV_ALLOW_MOCK=1 (the Phase 64 / D-089 convention), so the
# bound LLM driver is the mock and `llm.posture.mock_mode` MUST be true.
#
# Assertions cover:
#
#   1. Identity-mandatory rejection — both methods return 401 without a
#      Bearer token (CLAUDE.md §6 + RFC §5.5).
#   2. Cross-tenant rejection — non-admin caller requesting a different
#      tenant_id returns 403 (D-079 admin scope claim).
#   3. Happy-path own-tenant read — both methods return 200 with the
#      documented JSON shape.
#   4. Mock-mode flag round-trip — when HARBOR_DEV_ALLOW_MOCK=1 fired at
#      boot (the preflight harness exports it), llm.posture.mock_mode
#      MUST be true. The structural invariant from D-089: MockMode ==
#      true iff the runtime booted with the dev-only mock escape hatch.
#
# All assertions honour the 404/405/501 → SKIP convention (common.sh) so
# this smoke coexists with builds that have not yet shipped the surface.

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

# ----------------------------------------------------------------------
# Discover the dev Bearer token (parsed from the preflight server log
# per the Phase 64 convention). If the log isn't present we SKIP the
# auth-gated assertions but still exercise the unauthenticated reject
# paths — the same posture phase-64.sh takes.
# ----------------------------------------------------------------------
TOKEN=""
if [ -n "${HARBOR_DATA_DIR:-}" ] && [ -f "${HARBOR_DATA_DIR}/server.log" ]; then
    TOKEN="$(dev_bearer)"
fi

CONTROL_GOV_URL="$(api_url /v1/control/governance.posture)"
CONTROL_LLM_URL="$(api_url /v1/control/llm.posture)"

# ----------------------------------------------------------------------
# Assertion 1 — identity-mandatory: governance.posture without a Bearer
# token returns 401. The auth.Middleware (Phase 61) fails closed on
# missing authentication; the handler is never reached. We probe with
# POST + empty JSON body so a route that's POST-only does not return
# 405 (which would mask the 401 as SKIP).
# ----------------------------------------------------------------------
unauth_gov=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H "Content-Type: application/json" \
    --data '{}' \
    "${CONTROL_GOV_URL}" || true)
case "${unauth_gov}" in
    401)
        ok "phase-72g: governance.posture rejects unauthenticated request (401)"
        ;;
    404|405|501|000)
        skip "phase-72g: governance.posture surface not yet implemented (${unauth_gov})"
        ;;
    *)
        fail "phase-72g: governance.posture unauthenticated status = ${unauth_gov}, want 401"
        ;;
esac

# ----------------------------------------------------------------------
# Assertion 2 — identity-mandatory: llm.posture without a Bearer token
# returns 401.
# ----------------------------------------------------------------------
unauth_llm=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H "Content-Type: application/json" \
    --data '{}' \
    "${CONTROL_LLM_URL}" || true)
case "${unauth_llm}" in
    401)
        ok "phase-72g: llm.posture rejects unauthenticated request (401)"
        ;;
    404|405|501|000)
        skip "phase-72g: llm.posture surface not yet implemented (${unauth_llm})"
        ;;
    *)
        fail "phase-72g: llm.posture unauthenticated status = ${unauth_llm}, want 401"
        ;;
esac

# ----------------------------------------------------------------------
# The authenticated assertions need the dev token. If the preflight log
# didn't expose it we SKIP — same posture as phase-64.sh assertion 5.
# ----------------------------------------------------------------------
if [ -z "${TOKEN}" ]; then
    skip "phase-72g: HARBOR_DEV_TOKEN not parseable from server log; cannot exercise authenticated paths"
    smoke_summary
    exit $?
fi

GOV_BODY=/tmp/harbor-smoke-72g-gov.json
LLM_BODY=/tmp/harbor-smoke-72g-llm.json
rm -f "${GOV_BODY}" "${LLM_BODY}"

# ----------------------------------------------------------------------
# Assertion 3 — happy-path own-tenant read of governance.posture.
# Empty-body request (`{}`) means "read the caller's own tenant" — no
# admin scope claim required. The expected JSON shape:
#   {
#     "default_tier": "<string>",
#     "resolved_tier": "<string>",
#     "identity_tiers": { ... }
#   }
# We assert the top-level keys are present (200 + non-null object).
# ----------------------------------------------------------------------
status_gov=$(curl -s -o "${GOV_BODY}" -w '%{http_code}' --max-time 5 \
    -X POST -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${TOKEN}" \
    --data '{}' \
    "${CONTROL_GOV_URL}" || true)
case "${status_gov}" in
    200)
        if command -v jq >/dev/null 2>&1; then
            tiers_type=$(jq -r '.identity_tiers | type' "${GOV_BODY}" 2>/dev/null || echo "")
            default_type=$(jq -r '.default_tier | type' "${GOV_BODY}" 2>/dev/null || echo "")
            if [ "${tiers_type}" = "object" ] && [ "${default_type}" = "string" ]; then
                ok "phase-72g: governance.posture own-tenant returns shape {identity_tiers:object, default_tier:string}"
            else
                fail "phase-72g: governance.posture body shape unexpected (identity_tiers:${tiers_type}, default_tier:${default_type})"
            fi
        else
            ok "phase-72g: governance.posture own-tenant returns 200 (jq not available; shape check skipped)"
        fi
        ;;
    404|405|501|000)
        skip "phase-72g: governance.posture not yet implemented (${status_gov})"
        ;;
    *)
        fail "phase-72g: governance.posture authenticated status = ${status_gov}, want 200; body=$(cat "${GOV_BODY}" 2>/dev/null | head -c 200)"
        ;;
esac

# ----------------------------------------------------------------------
# Assertion 4 — happy-path own-tenant read of llm.posture.
# Expected JSON shape:
#   {
#     "provider": "<string>",
#     "model": "<string>",
#     "region": "<string>",
#     "mock_mode": <bool>
#   }
# ----------------------------------------------------------------------
status_llm=$(curl -s -o "${LLM_BODY}" -w '%{http_code}' --max-time 5 \
    -X POST -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${TOKEN}" \
    --data '{}' \
    "${CONTROL_LLM_URL}" || true)
case "${status_llm}" in
    200)
        if command -v jq >/dev/null 2>&1; then
            provider_type=$(jq -r '.provider | type' "${LLM_BODY}" 2>/dev/null || echo "")
            mock_type=$(jq -r '.mock_mode | type' "${LLM_BODY}" 2>/dev/null || echo "")
            if [ "${provider_type}" = "string" ] && [ "${mock_type}" = "boolean" ]; then
                ok "phase-72g: llm.posture own-tenant returns shape {provider:string, mock_mode:boolean}"
            else
                fail "phase-72g: llm.posture body shape unexpected (provider:${provider_type}, mock_mode:${mock_type})"
            fi
        else
            ok "phase-72g: llm.posture own-tenant returns 200 (jq not available; shape check skipped)"
        fi
        ;;
    404|405|501|000)
        skip "phase-72g: llm.posture not yet implemented (${status_llm})"
        ;;
    *)
        fail "phase-72g: llm.posture authenticated status = ${status_llm}, want 200; body=$(cat "${LLM_BODY}" 2>/dev/null | head -c 200)"
        ;;
esac

# ----------------------------------------------------------------------
# Assertion 5 — D-089 mock-mode round-trip: the preflight harness boots
# with HARBOR_DEV_ALLOW_MOCK=1 (Phase 64 convention), so a successful
# llm.posture response MUST report mock_mode=true. This is the binary
# structural invariant: MockMode == true iff the runtime booted with
# the dev escape hatch. A mock-mode mismatch is a silent-degradation
# bug — CLAUDE.md §13's "Test stubs as production defaults" trip-wire
# is the matching production-side guard.
# ----------------------------------------------------------------------
if [ "${status_llm}" = "200" ] && [ -f "${LLM_BODY}" ] && command -v jq >/dev/null 2>&1; then
    if [ "${HARBOR_DEV_ALLOW_MOCK:-}" = "1" ]; then
        mock_actual=$(jq -r '.mock_mode' "${LLM_BODY}" 2>/dev/null || echo "")
        if [ "${mock_actual}" = "true" ]; then
            ok "phase-72g: llm.posture.mock_mode = true under HARBOR_DEV_ALLOW_MOCK=1 (D-089 round-trip)"
        else
            fail "phase-72g: llm.posture.mock_mode = ${mock_actual} under HARBOR_DEV_ALLOW_MOCK=1, want true (D-089 capture-path desync)"
        fi
    else
        skip "phase-72g: HARBOR_DEV_ALLOW_MOCK not exported; cannot exercise mock-mode round-trip"
    fi
fi

# ----------------------------------------------------------------------
# Assertions 6 + 7 — the D-079 cross-tenant path on governance.posture /
# llm.posture.
#
# THESE ASSERTIONS WERE VACUOUS, AND THE SKIP TEXT MISDESCRIBED WHY.
# They posted `{"tenant_id":"other-tenant"}` expecting 403. But the posture
# family is decoded into the SHARED `RuntimeInfoRequest` envelope
# (internal/protocol/transports/control/posture_handler.go), which has no
# `tenant_id` field — nothing has ever decoded one. The member was
# discarded, the request reduced to an empty body identity, the handler
# backfilled it from the verified identity, and the answer was 200: the
# caller's OWN posture. The `200` arm then reported SKIP blaming "dev token
# carries admin scope", which was not the reason and is exactly why nobody
# looked. The cross-tenant gate was never exercised.
#
# THE REAL SELECTOR IS `identity.tenant`. `PostureSurface.Dispatch`
# (internal/protocol/posture.go) compares the body tenant against the
# ctx-verified tenant and demands `auth.ScopeAdmin` / `auth.ScopeConsoleFleet`,
# emitting `governance.posture.read_admin`. The `tenant_id` field was a second,
# never-implemented spelling of that and has been REMOVED (D-374).
#
# WHAT A LIVE PROBE CAN AND CANNOT REACH. `harbor dev` mints ONE bearer and it
# carries both admin-tier claims, so every crossing this bearer sends is
# legitimately GRANTED — the refusal half is unreachable here, the same
# constraint phase-218 records for its own widening probe. So these legs assert
# the half a live probe CAN reach: the crossing is ACCEPTED and answers. The
# refusal half is pinned by `TestPostureDispatch_CrossTenantRequiresAdmin`
# (both directions) and the audit emit by
# `TestPostureDispatch_CrossTenantConfigReadEmitsAudit`.
#
# A 400 here is a FAIL, not a skip: it means the body shape stopped matching
# the envelope the handler decodes, which is the defect this pair just had.
# ----------------------------------------------------------------------
bearer_claim() {
    local claim="$1"
    if [ -z "${TOKEN:-}" ] || ! command -v base64 >/dev/null 2>&1; then
        return 1
    fi
    local payload
    payload="$(printf '%s' "${TOKEN}" | cut -d. -f2)"
    payload="${payload//-/+}"
    payload="${payload//_//}"
    while [ $(( ${#payload} % 4 )) -ne 0 ]; do payload="${payload}="; done
    local decoded
    decoded="$(printf '%s' "${payload}" | base64 -d 2>/dev/null || printf '%s' "${payload}" | base64 -D 2>/dev/null)" || return 1
    printf '%s' "${decoded}" | sed -n "s/.*\"${claim}\":\"\([^\"]*\)\".*/\1/p"
}

BEARER_USER="$(bearer_claim user || true)"
BEARER_SESSION="$(bearer_claim session || true)"

if [ -z "${TOKEN}" ]; then
    skip 'phase-72g: no dev bearer resolved — the cross-tenant legs need an authenticated caller'
elif [ -z "${BEARER_USER}" ] || [ -z "${BEARER_SESSION}" ]; then
    # The posture body-scope policy PINS user and session; only the tenant is
    # admin-elevatable. Without the bearer's own two components the probe
    # cannot express "another tenant, my own user/session" and would be
    # testing the pinned components instead of the gate.
    skip 'phase-72g: could not read the bearer own (user, session) claims — the cross-tenant legs are pinned by the posture unit suite'
else
    CROSS_BODY="{\"identity\":{\"tenant\":\"phase72g-foreign-tenant\",\"user\":\"${BEARER_USER}\",\"session\":\"${BEARER_SESSION}\"}}"
    for probe in "governance.posture:${CONTROL_GOV_URL}" "llm.posture:${CONTROL_LLM_URL}"; do
        pname="${probe%%:*}"
        purl="${probe#*:}"
        cross=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
            -X POST -H "Content-Type: application/json" \
            -H "Authorization: Bearer ${TOKEN}" \
            --data "${CROSS_BODY}" \
            "${purl}" || true)
        case "${cross}" in
            200)
                ok "phase-72g: ${pname} GRANTS a cross-tenant read to the admin-claimed dev bearer (200) — the D-079 crossing is live and the body shape matches the decoded envelope"
                ;;
            403)
                fail "phase-72g: ${pname} refused a cross-tenant read (403) to a bearer carrying the admin claim — the D-079 grant path is broken"
                ;;
            400)
                fail "phase-72g: ${pname} answered 400 to the cross-tenant body — the request shape no longer matches the envelope the posture handler decodes (RuntimeInfoRequest); this is the vacuous-assertion defect recurring"
                ;;
            404|405|501|000)
                fail "phase-72g: ${pname} answered ${cross} — the posture surface is mounted for the assertions above, so an unreachable cross-tenant path here is a regression, not a not-yet-shipped surface"
                ;;
            *)
                fail "phase-72g: ${pname} cross-tenant status = ${cross}, want 200 (granted under the admin claim)"
                ;;
        esac
    done
fi

smoke_summary
