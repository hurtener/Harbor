#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 195 — governance identity-tier policy write (`governance.set_posture`, D-332).
#
# The write sibling of the read-only `governance.posture`: a full-replace,
# admin-only (auth.ScopeAdmin), StateStore-backed identity-tier policy write.
# Skeleton until the surface lands: every assertion SKIPs on a pre-195 build.
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

# ----------------------------------------------------------------------------
# Static trip-wires (run regardless of the live server).
# ----------------------------------------------------------------------------
if grep -q 'governance.set_posture' web/console/src/lib/protocol/wire-manifest.gen.json 2>/dev/null; then
    ok "static: governance.set_posture is in the regenerated wire manifest"
else
    skip "static: governance.set_posture absent from wire-manifest.gen.json (pre-195 build)"
fi
if grep -q 'governance.set_posture' docs/site/protocol/methods.md 2>/dev/null; then
    ok "static: governance.set_posture is in the generated protocol methods doc"
else
    skip "static: governance.set_posture absent from docs/site/protocol/methods.md (pre-195 build)"
fi

# ----------------------------------------------------------------------------
# Live-server assertions.
# ----------------------------------------------------------------------------
SET_URL="$(api_url /v1/governance/set_posture)"
# The read sibling routes through the control surface (POST /v1/control/{method}).
POSTURE_URL="$(api_url /v1/control/governance.posture)"

PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${SET_URL}" 2>/dev/null || true)
case "${PROBE:-000}" in
    404|405|501|000|'')
        skip "phase 195: governance.set_posture route not present (${PROBE:-000})"
        smoke_summary
        exit 0
        ;;
esac

if [ -z "${HARBOR_DEV_TOKEN:-}" ]; then
    skip "phase 195: HARBOR_DEV_TOKEN unavailable — live assertions skipped (run under 'make preflight')"
    smoke_summary
    exit 0
fi

TOKEN="${HARBOR_DEV_TOKEN}"
ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")

post_code() {
    local url="$1" body="$2" tok="${3:-${TOKEN}}"
    curl -s -o /dev/null -w '%{http_code}' -X POST "${url}" \
        -H "Authorization: Bearer ${tok}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' -d "${body}" 2>/dev/null || true
}

# --- ROUND-TRIP: a valid full-table write returns 200 and the read reflects it. ---
FULL_BODY='{"default_tier":"team","identity_tiers":{"team":{"budget_ceiling_usd":25,"max_tokens":4096,"rate_limit":{"capacity":100,"refill_tokens":100,"refill_interval_ms":60000}}}}'
SET_CODE="$(post_code "${SET_URL}" "${FULL_BODY}")"
if [ "${SET_CODE}" = "200" ]; then
    ok "phase 195: a valid full-table set_posture returns 200"
else
    fail "phase 195: valid set_posture returned ${SET_CODE}, want 200"
fi

READBACK=$(curl -s --max-time 5 -X POST "${POSTURE_URL}" \
    -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
    -H 'Content-Type: application/json' -d '{}' 2>/dev/null || true)
if printf '%s' "${READBACK}" | grep -q '"budget_ceiling_usd":25'; then
    ok "phase 195: governance.posture round-trips the written ceiling (25)"
else
    fail "phase 195: posture did not reflect the written ceiling; got: ${READBACK}"
fi

# --- FAIL-CLOSED: a write that ZEROES an enforced ceiling is rejected. ---
ZERO_BODY='{"default_tier":"team","identity_tiers":{"team":{"budget_ceiling_usd":0,"max_tokens":0,"rate_limit":{"capacity":0,"refill_tokens":0,"refill_interval_ms":0}}}}'
ZERO_CODE="$(post_code "${SET_URL}" "${ZERO_BODY}")"
case "${ZERO_CODE}" in
    400|422)
        ok "phase 195: a ceiling-zeroing write is rejected fail-closed (${ZERO_CODE})"
        ;;
    *)
        fail "phase 195: a ceiling-zeroing write returned ${ZERO_CODE}, want 400/422 (never budget-widening)"
        ;;
esac

# The prior enforced ceiling must survive the rejected write.
READBACK2=$(curl -s --max-time 5 -X POST "${POSTURE_URL}" \
    -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
    -H 'Content-Type: application/json' -d '{}' 2>/dev/null || true)
if printf '%s' "${READBACK2}" | grep -q '"budget_ceiling_usd":25'; then
    ok "phase 195: the prior enforced ceiling survives the rejected write (not widened)"
else
    fail "phase 195: the rejected write leaked through / widened the policy; got: ${READBACK2}"
fi

# --- F1 (security): a DefaultTier repoint to a present-but-UNENFORCED tier
#     silently de-enforces every default caller — must be rejected fail-closed
#     even though the previously-enforced tier stays in the map. ---
REPOINT_BODY='{"default_tier":"unbounded","identity_tiers":{"team":{"budget_ceiling_usd":25,"max_tokens":0,"rate_limit":{"capacity":0,"refill_tokens":0,"refill_interval_ms":0}},"unbounded":{"budget_ceiling_usd":0,"max_tokens":0,"rate_limit":{"capacity":0,"refill_tokens":0,"refill_interval_ms":0}}}}'
REPOINT_CODE="$(post_code "${SET_URL}" "${REPOINT_BODY}")"
case "${REPOINT_CODE}" in
    400|422)
        ok "phase 195: a default-tier repoint to an unenforced tier is rejected fail-closed (${REPOINT_CODE})"
        ;;
    *)
        fail "phase 195: a default-repoint-to-unenforced write returned ${REPOINT_CODE}, want 400/422 (F1 de-enforcement guard)"
        ;;
esac

# --- F1 (dimension-swap): the current default class (team) enforces budget 25;
#     repoint the default to a tier that enforces a DIFFERENT dimension
#     (max_tokens) but DROPS the budget cap. The new default enforces SOMETHING,
#     so the coarse all-zero check would miss it — the per-dimension guard must
#     still reject it (400). ---
SWAP_BODY='{"default_tier":"swap","identity_tiers":{"team":{"budget_ceiling_usd":25,"max_tokens":0,"rate_limit":{"capacity":0,"refill_tokens":0,"refill_interval_ms":0}},"swap":{"budget_ceiling_usd":0,"max_tokens":50,"rate_limit":{"capacity":0,"refill_tokens":0,"refill_interval_ms":0}}}}'
SWAP_CODE="$(post_code "${SET_URL}" "${SWAP_BODY}")"
case "${SWAP_CODE}" in
    400|422)
        ok "phase 195: a default-tier repoint that swaps dimensions (drops budget) is rejected fail-closed (${SWAP_CODE})"
        ;;
    *)
        fail "phase 195: a dimension-swap default repoint returned ${SWAP_CODE}, want 400/422 (per-dimension de-enforcement guard)"
        ;;
esac

# --- W1 (honesty): the write response carries enforcement_pending_restart.
#     The preflight dev config is latent (no identity_tiers), so no governance
#     wrapper is composed and a persisted write reports pending_restart:true. ---
PENDING_BODY=$(curl -s --max-time 5 -X POST "${SET_URL}" \
    -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
    -H 'Content-Type: application/json' -d "${FULL_BODY}" 2>/dev/null || true)
if printf '%s' "${PENDING_BODY}" | grep -q '"enforcement_pending_restart"'; then
    ok "phase 195: set_posture response carries enforcement_pending_restart (honest enforcement signal)"
else
    fail "phase 195: set_posture response missing enforcement_pending_restart; got: ${PENDING_BODY}"
fi

# --- SCOPE GATE: a console:fleet-only token (no admin) cannot widen a budget. ---
if [ -n "${HARBOR_DEV_FLEET_TOKEN:-}" ]; then
    FLEET_CODE="$(post_code "${SET_URL}" "${FULL_BODY}" "${HARBOR_DEV_FLEET_TOKEN}")"
    if [ "${FLEET_CODE}" = "403" ]; then
        ok "phase 195: a console:fleet-only token is rejected (403) — read scope cannot widen a budget"
    else
        fail "phase 195: a console:fleet-only set_posture returned ${FLEET_CODE}, want 403 (D-066/D-079)"
    fi
else
    skip "phase 195: HARBOR_DEV_FLEET_TOKEN unavailable — scope-gate assertion skipped"
fi

smoke_summary
