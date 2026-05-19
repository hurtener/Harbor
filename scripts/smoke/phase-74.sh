#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
# Phase 74 smoke — Console topology projection events (D-106).
#
# Phase 74 lands the `topology.snapshot` Protocol method + the
# `topology.changed` canonical event. The two surfaces share the
# `TopologyProjection` wire type (`internal/protocol/types/topology.go`).
# This script:
#
#   1. Runs the package tests under -race (covers wire types, methods
#      registry, events registry, the engine's Topology() builder, and
#      the D-025 N>=128 concurrent-reuse stress).
#   2. Live-server checks against the preflight-booted dev server:
#      a. `topology.snapshot` with identity headers returns 200 +
#         a body whose `nodes` and `edges` arrays are non-empty.
#      b. `topology.snapshot` without identity headers returns the
#         structured `identity_required` shape (401).
#      c. `topology.snapshot` with foreign-tenant identity returns the
#         structured `auth_rejected` shape (401) unless the dev token
#         carries `admin` scope (the dev stack's default token does NOT).
#      d. (Optional) An SSE subscription to `events.subscribe` filtered on
#         `topology.changed` carries at least one event when a synthetic
#         engine rebuild is triggered. SKIPs cleanly when the dev rebuild
#         endpoint returns 404/405 (so phase-N+1 builds reworking that
#         seam stay green).
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 -> SKIP so phase-N+1 scripts coexist with phase-N builds.
#   - Use the helpers from scripts/smoke/common.sh; do not roll new curl wrappers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. Package tests under -race.
test_log=$(mktemp)
if go test -race -count=1 -timeout 90s \
        ./internal/protocol/types/... \
        ./internal/protocol/methods/... \
        ./internal/protocol/singlesource/... \
        ./internal/events/... \
        ./internal/runtime/engine/... >"${test_log}" 2>&1; then
    ok 'phase 74: topology unit + concurrent-reuse tests pass under -race'
    rm -f "${test_log}"
else
    fail 'phase 74: topology package tests failed (run: go test -race ./internal/runtime/engine/... ./internal/protocol/...)'
    echo "    --- go test output (tail 80 lines) ---"
    tail -80 "${test_log}" | sed 's/^/    /'
    echo "    --- end ---"
    rm -f "${test_log}"
fi

# 2. Live-server checks. SKIP the entire block when the dev server is
# not booted (manual `bash scripts/smoke/phase-74.sh` outside the
# preflight harness).
if [[ -z "${HARBOR_DEV_TOKEN:-}" ]]; then
    skip 'phase 74: HARBOR_DEV_TOKEN not set in env -- live-server smoke skipped (set HARBOR_DEV_TOKEN to enable, or run under `make preflight`)'
    smoke_summary
    exit 0
fi
if ! command -v jq >/dev/null 2>&1; then
    skip 'phase 74: jq not available -- live-server assertions skipped'
    smoke_summary
    exit 0
fi

SNAPSHOT_URL="$(api_url /v1/protocol/topology.snapshot)"

# 2a. Identity-bearing call returns 200 + non-empty nodes/edges.
#
# The dev stack issues a dev token whose claims carry the `dev` tenant /
# `dev` user / a synthetic session. We mirror those into the request
# body's IdentityScope so the Phase 61 auth-vs-body match passes.
body=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "${SNAPSHOT_URL}" \
    -d '{"identity":{"tenant_id":"dev","user_id":"dev","session_id":"phase74-smoke"}}' \
    2>/dev/null || echo '000')
case "${body}" in
    404|405|501)
        skip "phase 74: topology.snapshot returned ${body} -- surface not yet wired; preflight stays green"
        smoke_summary
        exit 0
        ;;
    200)
        # Fetch the body for the nodes/edges assertion.
        snapshot_body=$(curl -s --max-time 5 \
            -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
            -H "Content-Type: application/json" \
            -X POST "${SNAPSHOT_URL}" \
            -d '{"identity":{"tenant_id":"dev","user_id":"dev","session_id":"phase74-smoke"}}' \
            2>/dev/null || echo '{}')
        nodes_count=$(printf '%s' "${snapshot_body}" | jq -r '.nodes | length // 0' 2>/dev/null || echo '0')
        edges_count=$(printf '%s' "${snapshot_body}" | jq -r '.edges | length // 0' 2>/dev/null || echo '0')
        if [[ "${nodes_count}" -gt 0 && "${edges_count}" -gt 0 ]]; then
            ok "phase 74: topology.snapshot returned 200 with nodes=${nodes_count} edges=${edges_count}"
        else
            fail "phase 74: topology.snapshot returned 200 but nodes=${nodes_count} edges=${edges_count} (expected both > 0)"
        fi
        ;;
    *)
        fail "phase 74: topology.snapshot expected 200, got ${body}"
        ;;
esac

# 2b. Identity-less call returns CodeIdentityRequired (401 with the
# structured error shape).
no_ident_body=$(curl -s --max-time 5 \
    -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "${SNAPSHOT_URL}" \
    -d '{}' \
    2>/dev/null || echo '{}')
code=$(printf '%s' "${no_ident_body}" | jq -r '.code // empty' 2>/dev/null || echo '')
case "${code}" in
    identity_required)
        ok 'phase 74: topology.snapshot without identity returns code=identity_required'
        ;;
    auth_rejected)
        # Acceptable: a body lacking identity may also be rejected at the
        # auth-vs-body match perimeter (D-079 defence in depth).
        ok 'phase 74: topology.snapshot without identity rejected at auth perimeter (auth_rejected) -- defence in depth'
        ;;
    *)
        fail "phase 74: topology.snapshot without identity expected identity_required or auth_rejected, got code='${code}' (body: ${no_ident_body})"
        ;;
esac

# 2c. Foreign-tenant identity returns CodeAuthRejected (the dev token
# does NOT carry admin scope, so a cross-tenant call must be rejected).
foreign_body=$(curl -s --max-time 5 \
    -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "${SNAPSHOT_URL}" \
    -d '{"identity":{"tenant_id":"foreign-tenant","user_id":"dev","session_id":"phase74-smoke"}}' \
    2>/dev/null || echo '{}')
fcode=$(printf '%s' "${foreign_body}" | jq -r '.code // empty' 2>/dev/null || echo '')
case "${fcode}" in
    auth_rejected)
        ok 'phase 74: topology.snapshot cross-tenant without admin scope returns code=auth_rejected'
        ;;
    *)
        fail "phase 74: topology.snapshot cross-tenant without admin expected auth_rejected, got code='${fcode}' (body: ${foreign_body})"
        ;;
esac

# 2d. SSE subscription carries at least one topology.changed event when
# the dev rebuild endpoint exists. SKIP cleanly otherwise.
SUB_URL="$(api_url /v1/events/subscribe?types=topology.changed)"
REBUILD_URL="$(api_url /v1/dev/engine/rebuild)"

# Probe the rebuild endpoint with a 1s timeout HEAD/POST.
rebuild_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 \
    -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
    -X POST "${REBUILD_URL}" \
    -d '{}' 2>/dev/null || echo '000')
case "${rebuild_status}" in
    404|405|501|000)
        skip "phase 74: /v1/dev/engine/rebuild returned ${rebuild_status} -- event-stream assertion skipped (phase-N+1 reworking the dev seam stays green)"
        ;;
    *)
        # Open the SSE subscription in the background, write events to a
        # tempfile, kill after 2s, then count topology.changed frames.
        sse_log=$(mktemp)
        curl -s --max-time 3 -N \
            -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
            -H "Accept: text/event-stream" \
            "${SUB_URL}" >"${sse_log}" 2>&1 &
        sse_pid=$!
        # Allow the subscription to settle, then trigger the rebuild.
        # Using sleep here is acceptable for smoke (smokes are
        # human-scale; the §17.4 ban on sleep-as-synchronisation applies
        # to Go integration tests, not shell smokes).
        sleep 1
        curl -s --max-time 2 \
            -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
            -X POST "${REBUILD_URL}" \
            -d '{}' >/dev/null 2>&1 || true
        sleep 2
        kill "${sse_pid}" 2>/dev/null || true
        wait "${sse_pid}" 2>/dev/null || true
        if grep -q 'topology.changed' "${sse_log}"; then
            ok 'phase 74: SSE subscription carried at least one topology.changed event after dev rebuild'
        else
            fail "phase 74: SSE subscription did NOT carry topology.changed after rebuild (log: $(wc -c < "${sse_log}") bytes; head: $(head -c 200 "${sse_log}"))"
        fi
        rm -f "${sse_log}"
        ;;
esac

smoke_summary
