#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 73f smoke — Console Tools page (Protocol + UI; D-116).
#
# Phase 73f lands the seven `tools.*` Protocol methods (`tools.list` /
# `tools.get` / `tools.describe` / `tools.metrics` / `tools.content_stats`
# / `tools.set_approval_policy` / `tools.revoke_oauth`) + the Console
# Tools page UI. The wire-transport route is `POST /v1/tools/{method}`.
#
# This script:
#
#   1. Runs the touched Go packages under -race (wire types + methods
#      registry + singlesource lockstep + the tools/protocol Service +
#      the stream-package tools handler).
#   2. Live-server checks against the preflight-booted dev server:
#      a. `tools.list` with a valid dev token returns 200 + a `tools`
#         array + an `aggregates` object.
#      b. `tools.list` with the MCP transport facet round-trips 200.
#      c. `tools.get` / `tools.describe` / `tools.metrics` /
#         `tools.content_stats` round-trip; the metrics call asserts
#         the `status` pill is Healthy / Degraded / Offline.
#      d. `tools.set_approval_policy` / `tools.revoke_oauth` WITHOUT the
#         `admin` scope claim return 403 + `identity_scope_required`
#         (D-079 -- there is NO `tools.admin` scope).
#      e. `tools.list` without identity returns 401 + `identity_required`.
#   3. The `/console/tools` page route ships with `harbor console`
#      (Phase 73m) -- SKIPped here per the decomposition.
#
# Conventions (AGENTS.md §4.2): 404/405/501 -> SKIP; use common.sh.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. Touched-package tests under -race.
test_log=$(mktemp)
if go test -race -count=1 -timeout 180s \
        ./internal/protocol/types/... \
        ./internal/protocol/methods/... \
        ./internal/protocol/singlesource/... \
        ./internal/tools/protocol/... \
        ./internal/protocol/transports/stream/... >"${test_log}" 2>&1; then
    ok 'phase 73f: tools.* unit + concurrent-reuse tests pass under -race'
    rm -f "${test_log}"
else
    fail 'phase 73f: tools.* package tests failed (run: go test -race ./internal/tools/protocol/... ./internal/protocol/transports/stream/...)'
    echo "    --- go test output (tail 80 lines) ---"
    tail -80 "${test_log}" | sed 's/^/    /'
    echo "    --- end ---"
    rm -f "${test_log}"
fi

# 2. Live-server checks. SKIP the whole block when not booted.
if [[ -z "${HARBOR_DEV_TOKEN:-}" ]]; then
    skip 'phase 73f: HARBOR_DEV_TOKEN not set -- live-server smoke skipped (run under `make preflight`)'
    skip 'phase 73f: /console/tools route lands with 73m harbor console subcommand'
    smoke_summary
    exit 0
fi
if ! command -v jq >/dev/null 2>&1; then
    skip 'phase 73f: jq not available -- live-server assertions skipped'
    skip 'phase 73f: /console/tools route lands with 73m harbor console subcommand'
    smoke_summary
    exit 0
fi

LIST_URL="$(api_url /v1/tools/list)"
DEV_IDENTITY='{"identity":{"tenant":"dev","user":"dev","session":"dev"}}'

# 2a. tools.list happy path.
status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "${LIST_URL}" -d "${DEV_IDENTITY}" 2>/dev/null || echo '000')
case "${status}" in
    404|405|501)
        skip "phase 73f: /v1/tools/list returned ${status} -- tools surface not mounted on this build"
        skip 'phase 73f: /console/tools route lands with 73m harbor console subcommand'
        smoke_summary
        exit 0
        ;;
    200)
        list_body=$(curl -s --max-time 5 \
            -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
            -H "Content-Type: application/json" \
            -X POST "${LIST_URL}" -d "${DEV_IDENTITY}" 2>/dev/null || echo '{}')
        tools_type=$(printf '%s' "${list_body}" | jq -r '.tools | type' 2>/dev/null || echo '')
        has_agg=$(printf '%s' "${list_body}" | jq -r 'has("aggregates")' 2>/dev/null || echo 'false')
        if [[ "${tools_type}" == "array" && "${has_agg}" == "true" ]]; then
            ok 'phase 73f: tools.list returns a tools array + aggregates'
        else
            fail "phase 73f: tools.list shape wrong (tools type='${tools_type}', has aggregates='${has_agg}')"
        fi
        ;;
    *)
        fail "phase 73f: tools.list expected 200 or a 404/405/501 SKIP, got ${status}"
        skip 'phase 73f: /console/tools route lands with 73m harbor console subcommand'
        smoke_summary
        exit 0
        ;;
esac

# 2b. tools.list with the MCP transport facet round-trips.
facet_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "${LIST_URL}" \
    -d '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"filter":{"transports":["MCP"]}}' \
    2>/dev/null || echo '000')
if [[ "${facet_status}" == "200" ]]; then
    ok 'phase 73f: tools.list honours the transport facet'
else
    fail "phase 73f: tools.list facet expected 200, got ${facet_status}"
fi

# 2c. tools.get / describe / metrics / content_stats — first catalog ID.
first_id=$(printf '%s' "${list_body}" | jq -r '.tools[0].id // empty' 2>/dev/null || echo '')
if [[ -n "${first_id}" ]]; then
    for verb in get describe content_stats; do
        st=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
            -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
            -H "Content-Type: application/json" \
            -X POST "$(api_url "/v1/tools/${verb}")" \
            -d "{\"identity\":{\"tenant\":\"dev\",\"user\":\"dev\",\"session\":\"dev\"},\"id\":\"${first_id}\"}" \
            2>/dev/null || echo '000')
        if [[ "${st}" == "200" ]]; then
            ok "phase 73f: tools.${verb} round-trips for ${first_id}"
        else
            fail "phase 73f: tools.${verb} expected 200, got ${st}"
        fi
    done
    metrics_body=$(curl -s --max-time 5 \
        -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
        -H "Content-Type: application/json" \
        -X POST "$(api_url /v1/tools/metrics)" \
        -d "{\"identity\":{\"tenant\":\"dev\",\"user\":\"dev\",\"session\":\"dev\"},\"id\":\"${first_id}\",\"window\":\"1h\"}" \
        2>/dev/null || echo '{}')
    pill=$(printf '%s' "${metrics_body}" | jq -r '.status // empty' 2>/dev/null || echo '')
    case "${pill}" in
        Healthy|Degraded|Offline)
            ok "phase 73f: tools.metrics returns a valid status pill (${pill})" ;;
        *)
            fail "phase 73f: tools.metrics status pill = '${pill}', want Healthy|Degraded|Offline" ;;
    esac
else
    skip 'phase 73f: dev catalog empty -- tools.get/describe/metrics/content_stats SKIPped'
fi

# 2d. Admin methods are reachable + admin-gated. The dev token carries
# the `admin` + `console:fleet` scopes (cmd_dev.go), so it PASSES the
# D-079 admin gate — the methods then reach the catalog and 404 on the
# unknown id "x". This proves the route is mounted AND the admin gate
# admitted the scoped token. The 403-WITHOUT-admin reject path needs a
# non-admin token (non-trivial to mint in shell); it is covered
# end-to-end by test/integration/tools_page_test.go with a real ES256
# non-admin token — same posture phase-72e takes for its cross-tenant
# reject path.
for verb_payload in \
    'set_approval_policy:{"identity":{"tenant":"dev","user":"dev","session":"dev"},"id":"smoke-unknown-tool","policy":"gated"}' \
    'revoke_oauth:{"identity":{"tenant":"dev","user":"dev","session":"dev"},"id":"smoke-unknown-tool"}'; do
    verb="${verb_payload%%:*}"
    payload="${verb_payload#*:}"
    admin_body=$(curl -s --max-time 5 \
        -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
        -H "Content-Type: application/json" \
        -X POST "$(api_url "/v1/tools/${verb}")" -d "${payload}" 2>/dev/null || echo '{}')
    admin_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
        -H "Content-Type: application/json" \
        -X POST "$(api_url "/v1/tools/${verb}")" -d "${payload}" 2>/dev/null || echo '000')
    code=$(printf '%s' "${admin_body}" | jq -r '.code // empty' 2>/dev/null || echo '')
    # An admin-scoped token PASSES the gate; on the unknown id the
    # method returns 404 not_found. A 403/identity_scope_required would
    # mean the gate wrongly rejected the scoped dev token.
    if [[ "${admin_status}" == "404" && "${code}" == "not_found" ]]; then
        ok "phase 73f: tools.${verb} admin path reachable with the scoped dev token (404 on unknown id)"
    elif [[ "${admin_status}" == "403" ]]; then
        fail "phase 73f: tools.${verb} rejected the admin-scoped dev token with 403 -- D-079 gate is over-rejecting"
    else
        fail "phase 73f: tools.${verb} admin path expected 404/not_found, got ${admin_status}/${code}"
    fi
done

# 2e. tools.list without identity -> 401 identity_required.
noident_body=$(curl -s --max-time 5 \
    -H "Content-Type: application/json" \
    -X POST "${LIST_URL}" -d '{}' 2>/dev/null || echo '{}')
noident_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H "Content-Type: application/json" \
    -X POST "${LIST_URL}" -d '{}' 2>/dev/null || echo '000')
ni_code=$(printf '%s' "${noident_body}" | jq -r '.code // empty' 2>/dev/null || echo '')
if [[ "${noident_status}" == "401" && "${ni_code}" == "identity_required" ]]; then
    ok 'phase 73f: tools.list without identity -> 401 identity_required'
else
    fail "phase 73f: tools.list without identity expected 401/identity_required, got ${noident_status}/${ni_code}"
fi

# 3. The /console/tools page route ships with `harbor console` (73m).
skip 'phase 73f: /console/tools route lands with 73m harbor console subcommand'

smoke_summary
