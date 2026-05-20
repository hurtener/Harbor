#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
# Phase 74 smoke — Console topology projection (D-114).
#
# Phase 74 lands the `topology.snapshot` Protocol method + the
# `topology.changed` canonical event. The two surfaces share the
# `TopologyProjection` wire type (`internal/protocol/types/topology.go`).
#
# This script:
#
#   1. Runs the touched-package tests under -race (covers the wire
#      types, the methods registry, the singlesource lockstep, the
#      events registry, the engine's Topology() builder, the
#      construction-time emit, and the D-114 N>=128 concurrent-reuse
#      stress).
#   2. Live-server checks against the preflight-booted dev server:
#      a. `topology.snapshot` over the REST control transport with a
#         valid dev token returns 200 + a body whose `nodes` and
#         `edges` arrays are non-empty. The production `harbor dev`
#         runtime is planner/RunLoop-shaped and hosts NO engine-graph,
#         so its ControlSurface leaves the topology accessor nil and
#         the method returns CodeUnknownMethod (HTTP 404) — the
#         404 -> SKIP convention picks that up cleanly. A build that
#         wires an engine flips the SKIP to an OK.
#      b. `topology.snapshot` without identity returns the structured
#         `identity_required` shape (401) — only asserted when (a)
#         showed the surface is wired (else the route 404s anyway).
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 -> SKIP so phase-N+1 scripts coexist with phase-N
#     builds (and so the engine-less dev stack stays green).
#   - Use the helpers from scripts/smoke/common.sh; do not roll new
#     curl wrappers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. Touched-package tests under -race.
test_log=$(mktemp)
if go test -race -count=1 -timeout 120s \
        ./internal/protocol/types/... \
        ./internal/protocol/methods/... \
        ./internal/protocol/singlesource/... \
        ./internal/events/ \
        ./internal/protocol/ \
        ./internal/runtime/engine/ >"${test_log}" 2>&1; then
    ok 'phase 74: topology unit + concurrent-reuse tests pass under -race'
    rm -f "${test_log}"
else
    fail 'phase 74: topology package tests failed (run: go test -race ./internal/runtime/engine/ ./internal/protocol/...)'
    echo "    --- go test output (tail 80 lines) ---"
    tail -80 "${test_log}" | sed 's/^/    /'
    echo "    --- end ---"
    rm -f "${test_log}"
fi

# 2. Live-server checks. SKIP the whole block when the dev server is
# not booted (manual `bash scripts/smoke/phase-74.sh` outside the
# preflight harness).
if [[ -z "${HARBOR_DEV_TOKEN:-}" ]]; then
    skip 'phase 74: HARBOR_DEV_TOKEN not set in env -- live-server smoke skipped (run under `make preflight`)'
    smoke_summary
    exit 0
fi
if ! command -v jq >/dev/null 2>&1; then
    skip 'phase 74: jq not available -- live-server assertions skipped'
    smoke_summary
    exit 0
fi

SNAPSHOT_URL="$(api_url /v1/control/topology.snapshot)"

# 2a. Identity-bearing call. The dev token carries (tenant=dev,
# user=dev, session=dev); we mirror that triple into the request
# body's flat IdentityScope (wire keys: tenant / user / session) so the
# Phase 61 auth-vs-body match passes.
status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "${SNAPSHOT_URL}" \
    -d '{"identity":{"tenant":"dev","user":"dev","session":"dev"}}' \
    2>/dev/null || echo '000')
case "${status}" in
    404|405|501)
        skip "phase 74: topology.snapshot returned ${status} -- the dev stack hosts no engine-graph; an engine-bearing build flips this to OK (preflight stays green)"
        ;;
    200)
        snapshot_body=$(curl -s --max-time 5 \
            -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
            -H "Content-Type: application/json" \
            -X POST "${SNAPSHOT_URL}" \
            -d '{"identity":{"tenant":"dev","user":"dev","session":"dev"}}' \
            2>/dev/null || echo '{}')
        nodes_count=$(printf '%s' "${snapshot_body}" | jq -r '.nodes | length // 0' 2>/dev/null || echo '0')
        edges_count=$(printf '%s' "${snapshot_body}" | jq -r '.edges | length // 0' 2>/dev/null || echo '0')
        if [[ "${nodes_count}" -gt 0 && "${edges_count}" -gt 0 ]]; then
            ok "phase 74: topology.snapshot returned 200 with nodes=${nodes_count} edges=${edges_count}"
        else
            fail "phase 74: topology.snapshot returned 200 but nodes=${nodes_count} edges=${edges_count} (expected both > 0)"
        fi

        # 2b. Identity-less call -> CodeIdentityRequired (401). Only
        # meaningful once 2a proved the surface is wired.
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
            *)
                fail "phase 74: topology.snapshot without identity expected identity_required, got code='${code}' (body: ${no_ident_body})"
                ;;
        esac
        ;;
    *)
        fail "phase 74: topology.snapshot expected 200 or a 404/405/501 SKIP, got ${status}"
        ;;
esac

smoke_summary
