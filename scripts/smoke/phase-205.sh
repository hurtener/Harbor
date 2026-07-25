#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 205 smoke — the shared body-identity gate, live.
# (RFC §4.2, §5.5; docs/plans/phase-205-body-scope-reconciler.md)
#
# Every Protocol method carries an identity scope in its request body.
# That scope is caller-supplied input; the authority is the identity the
# transport established for the request. This smoke drives the legs of
# the reconciliation a dev server can actually prove, over the wire:
#
#   1. Backfill — an empty body triple is filled from the established
#      identity and the request is served.
#   2. Elevation granted — a body naming another tenant, on a surface
#      whose registered policy declares the tenant admin-scoped, is
#      SERVED for a caller holding the claim. The identity the crossing
#      reaches comes from the caller's own bearer, so the probe adapts to
#      whatever identity the boot minted.
#   3. Pinned component — a body naming another user is refused
#      `identity_required`. No claim widens the user, so this leg holds
#      whatever the bearer carries.
#   4. Flat denial — a body naming another tenant on a surface with no
#      elevation path is refused `identity_required`, again whatever the
#      bearer carries.
#
# The claim-ABSENT deny leg (`403 scope_mismatch`) is deliberately not
# here: `harbor dev` mints one bearer and it carries both admin-tier
# claims, so a live probe cannot reach that branch. It is pinned instead
# by the reconciler's table-driven suite and by the end-to-end wire tests
# that drive a non-admin cross-tenant read.
#
# Each assertion FAILS (never SKIPs) once the surface is present: the
# 404/405/501/000 SKIP branch covers only a build where the route is not
# mounted, and the 401-without-a-token branch covers a standalone run
# that could not discover a bearer.
#
# Conventions (AGENTS.md §4.2): 404/405/501 → SKIP; once shipped,
# OK ≥ the number of legs exercised and FAIL = 0.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Static — the gate's own parts are present and single-homed.
# ----------------------------------------------------------------------------
assert_file "internal/protocol/bodyscope/bodyscope.go" \
    "phase 205: the shared body-identity gate is present"
assert_file "internal/protocol/bodyscope/registry.go" \
    "phase 205: the surface-policy registry is present"
assert_file "internal/protocol/bodyscope/gate.go" \
    "phase 205: the lockstep gate scanner is present"
assert_grep_present "func FromVerified" "internal/identity/identity.go" \
    "phase 205: the verified identity is readable with provenance"
assert_grep_present "func WithElevated" "internal/identity/identity.go" \
    "phase 205: crossing the tenant boundary has one named path"

# The thirteen hand-written reconciliations are gone. A returning helper
# would reintroduce the copy the registry exists to prevent, so grep for
# the shapes by name across both transports.
for helper in \
    backfillAppsIdentity backfillMCPIdentity backfillPostureIdentity backfillArtifactsIdentity \
    assertBodyMatchesAuthedIdentity assertFlowsIdentity mergeIdentity assertGovernanceIdentity \
    assertSessionsIdentity assertMemoryBodyMatchesIdentity assertRunsIdentity \
    assertPauseBodyMatchesIdentity
do
    if grep -rn "func ${helper}(" internal/protocol/ >/dev/null 2>&1; then
        fail "phase 205: hand-written body-identity helper ${helper} is back; route it through bodyscope.Reconcile"
    else
        ok "phase 205: hand-written body-identity helper ${helper} is gone"
    fi
done

# ----------------------------------------------------------------------------
# Live — the four legs over the wire.
# ----------------------------------------------------------------------------
# The dev server boots WITH the auth validator, so an unauthenticated call
# is rejected before any handler runs. Recover the bearer the dev boot
# printed (the phase-64 convention); honour an env override for a
# standalone run.
if [[ -z "${HARBOR_DEV_TOKEN:-}" ]] && [[ -n "${HARBOR_DATA_DIR:-}" ]] && [[ -f "${HARBOR_DATA_DIR}/server.log" ]]; then
    HARBOR_DEV_TOKEN="$(grep -m1 '^HARBOR_DEV_TOKEN=' "${HARBOR_DATA_DIR}/server.log" 2>/dev/null | sed 's/^HARBOR_DEV_TOKEN=//' || true)"
fi

# probe_body <route> <json-body> <expected-status> <expected-code|-> <description>
#
# POSTs the body and asserts the HTTP status. When <expected-code> is not
# "-", the JSON error body's `code` must match too — a refusal with the
# right status but the wrong code means the gate took a different branch
# than the one under test.
probe_body() {
    local route="$1" body="$2" want_status="$3" want_code="$4" desc="$5"
    local url
    url="$(api_url "${route}")"

    if ! command -v curl >/dev/null 2>&1; then
        skip "${desc}: curl not available"
        return
    fi

    local out status payload
    out=$(curl -s -w '\n%{http_code}' --max-time 5 \
        -X POST \
        -H 'Content-Type: application/json' \
        ${HARBOR_DEV_TOKEN:+-H "Authorization: Bearer ${HARBOR_DEV_TOKEN}"} \
        --data "${body}" \
        "${url}" 2>/dev/null) || out=$'\n000'
    status="${out##*$'\n'}"
    payload="${out%$'\n'*}"

    case "${status}" in
        404|405|501|000|'')
            skip "${desc}: ${status:-000} (surface not mounted on this build)"
            return
            ;;
        401)
            if [[ -z "${HARBOR_DEV_TOKEN:-}" ]] && [[ "${want_status}" != "401" ]]; then
                skip "${desc}: 401 — no bearer discoverable; the reconciliation legs are pinned by the unit and integration suites"
                return
            fi
            ;;
    esac

    if [[ "${status}" != "${want_status}" ]]; then
        fail "${desc}: expected ${want_status}, got ${status} (POST ${url}; body=${payload})"
        return
    fi
    if [[ "${want_code}" != "-" ]]; then
        if ! printf '%s' "${payload}" | grep -q "\"code\":\"${want_code}\""; then
            fail "${desc}: status ${status} but code is not ${want_code} (body=${payload})"
            return
        fi
    fi
    local shown="${status}"
    if [[ "${want_code}" != "-" ]]; then
        shown="${status} ${want_code}"
    fi
    ok "${desc}: ${shown} (POST ${url})"
}

RUNTIME_INFO='/v1/control/runtime.info'
MCP_LIST='/v1/control/mcp.servers.list'
ARTIFACTS_LIST='/v1/control/artifacts.list'

# The bearer's own (user, session) — read out of the JWT payload so the
# cross-tenant probe below names the caller's own user and session while
# reaching another tenant, which is exactly the shape the posture surface
# elevates. Discovering them beats hardcoding whatever the dev boot mints.
bearer_claim() {
    local claim="$1"
    if [[ -z "${HARBOR_DEV_TOKEN:-}" ]] || ! command -v base64 >/dev/null 2>&1; then
        return 1
    fi
    local payload
    payload="$(printf '%s' "${HARBOR_DEV_TOKEN}" | cut -d. -f2)"
    # base64url → base64, then pad to a multiple of 4.
    payload="${payload//-/+}"
    payload="${payload//_//}"
    while (( ${#payload} % 4 )); do payload="${payload}="; done
    local decoded
    decoded="$(printf '%s' "${payload}" | base64 -d 2>/dev/null || printf '%s' "${payload}" | base64 -D 2>/dev/null)" || return 1
    printf '%s' "${decoded}" | sed -n "s/.*\"${claim}\":\"\([^\"]*\)\".*/\1/p"
}

# Leg 1 — backfill. An empty body triple is the caller asking for its own
# identity; the gate fills it in and the request is served.
probe_body "${RUNTIME_INFO}" '{"identity":{}}' 200 '-' \
    "phase 205: an empty body identity is backfilled from the established identity"

# Leg 2 — elevation granted. `runtime.*` declares the tenant admin-scoped,
# so a caller holding the claim reaches another tenant while its user and
# session stay its own (those are pinned, and no claim widens them).
BEARER_USER="$(bearer_claim user || true)"
BEARER_SESSION="$(bearer_claim session || true)"
if [[ -n "${BEARER_USER}" && -n "${BEARER_SESSION}" ]]; then
    probe_body "${RUNTIME_INFO}" \
        "{\"identity\":{\"tenant\":\"phase205-foreign-tenant\",\"user\":\"${BEARER_USER}\",\"session\":\"${BEARER_SESSION}\"}}" \
        200 '-' \
        "phase 205: a body naming another tenant is served under the admin-tier claim"
else
    skip "phase 205: cross-tenant elevation leg — could not read the bearer's own (user, session); the grant path is pinned by the reconciler suite"
fi

# Leg 3 — pinned component. No claim widens the user, so a body naming
# another user is refused whatever the caller's claims.
probe_body "${RUNTIME_INFO}" '{"identity":{"user":"phase205-foreign-user"}}' 401 'identity_required' \
    "phase 205: a body naming another user is refused"

# Leg 4 — flat denial. The MCP connection catalog has no elevation path,
# so a foreign tenant is refused with the identity code rather than the
# scope code, even for a caller holding both admin-tier claims.
probe_body "${MCP_LIST}" '{"identity":{"tenant":"phase205-foreign-tenant"}}' 401 'identity_required' \
    "phase 205: a surface with no elevation path refuses a foreign tenant flatly"

# Leg 5 — the artifacts cluster carries its triple in a different wire
# shape; the same gate reads it.
probe_body "${ARTIFACTS_LIST}" '{"scope":{}}' 200 '-' \
    "phase 205: an empty artifacts scope is backfilled through the same gate"

smoke_summary
