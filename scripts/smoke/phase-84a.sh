#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 84a smoke — runtime-capability gate + session aggregates
# (round-8 F1 + F8 closeout).
#
# Asserts:
#   1. `runtime.info` round-trips on the booted dev server (Phase 60
#      surface) — same 404 → SKIP convention every other live-server
#      smoke uses.
#   2. The response advertises a `capabilities` array.
#   3. On the dev posture (planner/RunLoop, no engine), `topology_snapshot`
#      is ABSENT from `capabilities` — this is the gate the Console
#      reads on Live Runtime + Playground to skip the
#      `topology.snapshot` fetch and keep the browser console clean.
#   4. `topology.snapshot` itself still returns 404 / `unknown_method`
#      — the capability advertisement is the Console gate; the wire
#      behaviour stays unchanged.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# POST helper — mirrors phase-72f.sh's pattern. Authenticates with the
# Phase 61 dev bearer when available; SKIPs cleanly when the surface is
# not yet routed (404/405/501) or when the dev server is auth-only and
# no token is in scope.
probe_caps() {
    local url body actual_status response
    url="$(api_url '/v1/control/runtime.info')"
    body='{}'

    if ! command -v curl >/dev/null 2>&1; then
        skip 'phase 84a: curl not available'
        return
    fi
    if ! command -v jq >/dev/null 2>&1; then
        skip 'phase 84a: jq not available'
        return
    fi

    actual_status=$(curl -s -o /tmp/phase-84a-info.json -w '%{http_code}' --max-time 5 \
        -X POST \
        -H 'Content-Type: application/json' \
        ${HARBOR_DEV_TOKEN:+-H "Authorization: Bearer ${HARBOR_DEV_TOKEN}"} \
        --data "${body}" \
        "${url}" 2>/dev/null) || actual_status="000"

    case "${actual_status}" in
        404|405|501|000|"")
            skip "phase 84a: runtime.info ${actual_status:-000} (surface not yet wired)"
            return
            ;;
        401)
            if [[ -z "${HARBOR_DEV_TOKEN:-}" ]]; then
                skip 'phase 84a: runtime.info 401 (no dev token in scope)'
                return
            fi
            fail "phase 84a: runtime.info 401 (auth misconfigured)"
            return
            ;;
        200)
            ;;
        *)
            fail "phase 84a: runtime.info HTTP ${actual_status}"
            return
            ;;
    esac

    response=$(cat /tmp/phase-84a-info.json 2>/dev/null || echo '{}')

    local caps_type
    caps_type=$(printf '%s' "$response" | jq -r '.capabilities | type' 2>/dev/null || echo "null")
    if [ "$caps_type" != "array" ]; then
        fail "phase 84a: runtime.info.capabilities is type=${caps_type}, want array"
        return
    fi
    ok 'phase 84a: runtime.info advertises capabilities[] (array)'

    local has_topology
    has_topology=$(printf '%s' "$response" | jq -r '.capabilities | index("topology_snapshot") | tostring' 2>/dev/null || echo "null")
    if [ "$has_topology" = "null" ]; then
        ok 'phase 84a: topology_snapshot absent from dev capabilities (planner/RunLoop runtime)'
    else
        fail "phase 84a: topology_snapshot leaked into dev capabilities (index=${has_topology}) — F1 regression"
    fi

    rm -f /tmp/phase-84a-info.json
}

probe_caps

# 4 — topology.snapshot wire-side stays 404. The Console's capability
# gate is the V1.1 fix; the wire is unchanged (a runtime-capability
# advertisement is the *layer above* the wire, not a replacement for
# it). The skip_if_404 convention matches every other smoke.
probe_topology_404() {
    local url status
    url="$(api_url '/v1/control/topology.snapshot')"
    if ! command -v curl >/dev/null 2>&1; then
        skip 'phase 84a: topology.snapshot probe — curl not available'
        return
    fi
    status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST \
        -H 'Content-Type: application/json' \
        ${HARBOR_DEV_TOKEN:+-H "Authorization: Bearer ${HARBOR_DEV_TOKEN}"} \
        --data '{"identity":{"tenant":"dev","user":"dev","session":"dev"}}' \
        "${url}" 2>/dev/null) || status="000"
    case "${status}" in
        404)
            ok 'phase 84a: topology.snapshot still returns 404 on planner/RunLoop runtime (advertisement is the gate)'
            ;;
        000|"")
            skip 'phase 84a: topology.snapshot probe — no connection'
            ;;
        *)
            # Any other status would mean the runtime DID wire topology
            # while still advertising no `topology_snapshot` capability
            # — a wire/cap drift the gate should catch.
            fail "phase 84a: topology.snapshot returned HTTP ${status} — expected 404 (wire/capability drift)"
            ;;
    esac
}

probe_topology_404

smoke_summary
