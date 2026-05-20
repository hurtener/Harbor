#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 73d smoke — Console Tasks page (Protocol + UI; D-123).
#
# Phase 73d lands the two `tasks.*` read methods (`tasks.list` /
# `tasks.get`) + the Console Tasks page UI (kanban + bulk control). The
# wire-transport route is `POST /v1/tasks/{method}`. The bulk-action
# toolbar consumes the EXISTING shipped Phase 54 control verbs
# (`cancel` / `pause` / `resume` / `prioritize` / `approve` / `reject`)
# — NO new control method is introduced (§13 no parallel implementations).
#
# This script:
#
#   1. Runs the touched Go packages under -race (wire types + methods
#      registry + singlesource lockstep + the tasks/protocol Service +
#      the stream-package tasks handler).
#   2. Live-server checks against the preflight-booted dev server:
#      a. `tasks.list` with a valid dev token returns 200 + a `rows`
#         array + an `aggregates` object.
#      b. `tasks.list` with a status facet round-trips 200.
#      c. `tasks.list` without identity returns a non-2xx rejection.
#      d. `tasks.list` with a cross-tenant identity filter round-trips
#         200 under the admin-scoped dev token (D-079 — the dev token
#         carries `admin` + `console:fleet`, so the cross-tenant fan-in
#         is admitted; the 403 reject path for a NON-admin token is
#         covered by the Go unit + handler + integration tests).
#      e. `tasks.get` for an unknown id returns 404 (not_found —
#         cross-tenant existence is never revealed).
#   3. The `/tasks` page route ships with `harbor console` (Phase 73m) —
#      SKIPped here per the decomposition.
#
# Conventions (AGENTS.md §4.2): 404/405/501 -> SKIP; use common.sh.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. Touched-package tests under -race.
test_log=$(mktemp)
if go test -race -count=1 -timeout 240s \
        ./internal/protocol/types/... \
        ./internal/protocol/methods/... \
        ./internal/protocol/singlesource/... \
        ./internal/tasks/protocol/... \
        ./internal/protocol/transports/stream/... >"${test_log}" 2>&1; then
    ok 'phase 73d: tasks.* unit + concurrent-reuse tests pass under -race'
    rm -f "${test_log}"
else
    fail 'phase 73d: tasks.* package tests failed (run: go test -race ./internal/tasks/protocol/... ./internal/protocol/transports/stream/...)'
    echo "    --- go test output (tail 80 lines) ---"
    tail -80 "${test_log}" | sed 's/^/    /'
    echo "    --- end ---"
    rm -f "${test_log}"
fi

# 2. Live-server checks. SKIP the whole block when not booted.
if [[ -z "${HARBOR_DEV_TOKEN:-}" ]]; then
    skip 'phase 73d: HARBOR_DEV_TOKEN not set -- live-server smoke skipped (run under `make preflight`)'
    skip 'phase 73d: /tasks route lands with 73m harbor console subcommand'
    smoke_summary
    exit 0
fi
if ! command -v jq >/dev/null 2>&1; then
    skip 'phase 73d: jq not available -- live-server assertions skipped'
    skip 'phase 73d: /tasks route lands with 73m harbor console subcommand'
    smoke_summary
    exit 0
fi

LIST_URL="$(api_url /v1/tasks/list)"
GET_URL="$(api_url /v1/tasks/get)"
DEV_IDENTITY='{"identity":{"tenant":"dev","user":"dev","session":"dev"}}'

# 2a. tasks.list happy path.
status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "${LIST_URL}" -d "${DEV_IDENTITY}" 2>/dev/null || echo '000')
case "${status}" in
    404|405|501)
        skip "phase 73d: /v1/tasks/list returned ${status} -- tasks surface not mounted on this build"
        skip 'phase 73d: /tasks route lands with 73m harbor console subcommand'
        smoke_summary
        exit 0
        ;;
    200)
        list_body=$(curl -s --max-time 5 \
            -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
            -H "Content-Type: application/json" \
            -X POST "${LIST_URL}" -d "${DEV_IDENTITY}" 2>/dev/null || echo '{}')
        rows_type=$(printf '%s' "${list_body}" | jq -r '.rows | type' 2>/dev/null || echo '')
        has_agg=$(printf '%s' "${list_body}" | jq -r 'has("aggregates")' 2>/dev/null || echo 'false')
        if [[ "${rows_type}" == "array" && "${has_agg}" == "true" ]]; then
            ok 'phase 73d: tasks.list returns a rows array + aggregates'
        else
            fail "phase 73d: tasks.list shape wrong (rows type='${rows_type}', has aggregates='${has_agg}')"
        fi
        ;;
    *)
        fail "phase 73d: tasks.list expected 200 or a 404/405/501 SKIP, got ${status}"
        skip 'phase 73d: /tasks route lands with 73m harbor console subcommand'
        smoke_summary
        exit 0
        ;;
esac

# 2b. tasks.list with a status facet round-trips.
facet_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "${LIST_URL}" \
    -d '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"filter":{"statuses":["running"]}}' \
    2>/dev/null || echo '000')
if [[ "${facet_status}" == "200" ]]; then
    ok 'phase 73d: tasks.list honours the status facet'
else
    fail "phase 73d: tasks.list facet expected 200, got ${facet_status}"
fi

# 2c. tasks.list without a token → a non-2xx rejection (auth.Middleware
#     rejects the token-less request at the edge with 401).
noident_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H "Content-Type: application/json" \
    -X POST "${LIST_URL}" -d '{}' 2>/dev/null || echo '000')
if [[ "${noident_status}" != "200" && "${noident_status}" != "000" ]]; then
    ok "phase 73d: tasks.list rejects a missing identity (${noident_status})"
else
    fail "phase 73d: tasks.list without identity expected a rejection, got ${noident_status}"
fi

# 2d. cross-tenant identity filter under the admin-scoped dev token →
#     200 (the dev token carries `admin` — the fan-in is admitted, and
#     an `audit.admin_scope_used` event is emitted server-side). The
#     403 reject path for a non-admin token is covered by the Go unit +
#     handler + integration tests, which can mint a scope-less token.
xtenant_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "${LIST_URL}" \
    -d '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"filter":{"identities":[{"tenant":"dev"},{"tenant":"other"}]}}' \
    2>/dev/null || echo '000')
if [[ "${xtenant_status}" == "200" ]]; then
    ok 'phase 73d: tasks.list admits a cross-tenant filter under the admin-scoped dev token (200)'
elif [[ "${xtenant_status}" == "403" ]]; then
    # A dev build with a non-admin token would land here — also correct.
    ok 'phase 73d: tasks.list rejects a cross-tenant filter without the admin scope claim (403)'
else
    fail "phase 73d: cross-tenant tasks.list expected 200 (admin dev token) or 403, got ${xtenant_status}"
fi

# 2e. tasks.get for an unknown id → 404 (existence never revealed).
get_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "${GET_URL}" \
    -d '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"id":"task-does-not-exist"}' \
    2>/dev/null || echo '000')
if [[ "${get_status}" == "404" ]]; then
    ok 'phase 73d: tasks.get returns 404 for an unknown task id (existence not revealed)'
else
    fail "phase 73d: tasks.get for an unknown id expected 404, got ${get_status}"
fi

# 3. Page route lands with the 73m harbor console subcommand.
skip 'phase 73d: /tasks route lands with 73m harbor console subcommand'

smoke_summary
