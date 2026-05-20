#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 73e smoke — Console Agents page (Protocol + UI; D-124).
#
# Phase 73e lands the eight `agents.*` Protocol methods (`agents.list` /
# `agents.get` / `agents.tools` / `agents.memory` / `agents.governance` /
# `agents.skills` / `agents.permissions` / `agents.metrics`) + the
# Console Agents page UI. The wire-transport route is
# `POST /v1/agents/{method}`.
#
# This script:
#
#   1. Runs the touched Go packages under -race (wire types + methods
#      registry + singlesource lockstep + the registry/protocol Service +
#      the stream-package agents handler).
#   2. Live-server checks against the preflight-booted dev server:
#      a. `agents.list` with a valid dev token returns 200 + an `agents`
#         array + an `aggregates` object.
#      b. `agents.list` with a status facet round-trips 200.
#      c. `agents.get` for an unknown id returns 404 + `not_found`.
#      d. The five detail methods (`tools` / `memory` / `governance` /
#         `skills` / `permissions`) are reachable -- an unknown id
#         returns 404; `agents.metrics` returns 200.
#      e. `agents.list` without identity returns 401 + `identity_required`.
#   3. The `/agents` page route ships with `harbor console` (Phase 73m)
#      -- SKIPped here per the decomposition.
#
# All eight `agents.*` methods are READ-ONLY projections of the Agent
# Registry; the five agent-control verbs the Agents page exposes (Pause /
# Drain / Restart / Force-Stop / Deregister) are the EXISTING shipped
# `registry.*` control verbs (D-066), gated on the control-scope claim --
# Phase 73e mints NO control method. Their scope-gate is exercised by
# test/integration/agents_page_test.go (TestE2E_Phase73e_ControlVerbScopeGate).
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
        ./internal/runtime/registry/protocol/... \
        ./internal/protocol/transports/stream/... >"${test_log}" 2>&1; then
    ok 'phase 73e: agents.* unit + concurrent-reuse tests pass under -race'
    rm -f "${test_log}"
else
    fail 'phase 73e: agents.* package tests failed (run: go test -race ./internal/runtime/registry/protocol/... ./internal/protocol/transports/stream/...)'
    echo "    --- go test output (tail 80 lines) ---"
    tail -80 "${test_log}" | sed 's/^/    /'
    echo "    --- end ---"
    rm -f "${test_log}"
fi

# 2. Live-server checks. SKIP the whole block when not booted.
if [[ -z "${HARBOR_DEV_TOKEN:-}" ]]; then
    skip 'phase 73e: HARBOR_DEV_TOKEN not set -- live-server smoke skipped (run under `make preflight`)'
    skip 'phase 73e: /agents route lands with 73m harbor console subcommand'
    smoke_summary
    exit 0
fi
if ! command -v jq >/dev/null 2>&1; then
    skip 'phase 73e: jq not available -- live-server assertions skipped'
    skip 'phase 73e: /agents route lands with 73m harbor console subcommand'
    smoke_summary
    exit 0
fi

LIST_URL="$(api_url /v1/agents/list)"
DEV_IDENTITY='{"identity":{"tenant":"dev","user":"dev","session":"dev"}}'

# 2a. agents.list happy path -- the route may be un-mounted on a partial
# build (404 -> SKIP per the 404/405/501 convention).
list_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "${LIST_URL}" -d "${DEV_IDENTITY}" 2>/dev/null || echo '000')
case "${list_status}" in
    404|405|501)
        skip 'phase 73e: /v1/agents/* surface not mounted on this build -- SKIP'
        skip 'phase 73e: /agents route lands with 73m harbor console subcommand'
        smoke_summary
        exit 0
        ;;
esac

list_body=$(curl -s --max-time 5 \
    -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "${LIST_URL}" -d "${DEV_IDENTITY}" 2>/dev/null || echo '{}')
if [[ "${list_status}" == "200" ]] \
        && printf '%s' "${list_body}" | jq -e '.agents' >/dev/null 2>&1 \
        && printf '%s' "${list_body}" | jq -e '.aggregates' >/dev/null 2>&1; then
    ok 'phase 73e: agents.list returns an agents array + aggregates'
else
    fail "phase 73e: agents.list expected 200 + agents/aggregates, got ${list_status}"
fi

# 2b. agents.list with a status facet round-trips.
facet_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "${LIST_URL}" \
    -d '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"filter":{"status":["active"]}}' \
    2>/dev/null || echo '000')
if [[ "${facet_status}" == "200" ]]; then
    ok 'phase 73e: agents.list honours the status facet'
else
    fail "phase 73e: agents.list facet expected 200, got ${facet_status}"
fi

# 2c. agents.get for an unknown id -> 404 not_found.
get_body=$(curl -s --max-time 5 \
    -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "$(api_url /v1/agents/get)" \
    -d '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"id":"smoke-unknown-agent"}' \
    2>/dev/null || echo '{}')
get_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "$(api_url /v1/agents/get)" \
    -d '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"id":"smoke-unknown-agent"}' \
    2>/dev/null || echo '000')
get_code=$(printf '%s' "${get_body}" | jq -r '.code // empty' 2>/dev/null || echo '')
if [[ "${get_status}" == "404" && "${get_code}" == "not_found" ]]; then
    ok 'phase 73e: agents.get unknown id -> 404 not_found'
else
    fail "phase 73e: agents.get unknown id expected 404/not_found, got ${get_status}/${get_code}"
fi

# 2d. The detail methods are reachable. tools/memory/governance/skills/
# permissions for an unknown id return 404 (route mounted + agent
# resolution applied); metrics takes no id and returns 200.
for method in tools memory governance skills permissions; do
    st=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
        -H "Content-Type: application/json" \
        -X POST "$(api_url "/v1/agents/${method}")" \
        -d '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"id":"smoke-unknown-agent"}' \
        2>/dev/null || echo '000')
    if [[ "${st}" == "404" ]]; then
        ok "phase 73e: agents.${method} reachable (404 on unknown id)"
    else
        fail "phase 73e: agents.${method} expected 404 on unknown id, got ${st}"
    fi
done
metrics_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
    -H "Content-Type: application/json" \
    -X POST "$(api_url /v1/agents/metrics)" -d "${DEV_IDENTITY}" \
    2>/dev/null || echo '000')
if [[ "${metrics_status}" == "200" ]]; then
    ok 'phase 73e: agents.metrics returns the registry-wide rollup'
else
    fail "phase 73e: agents.metrics expected 200, got ${metrics_status}"
fi

# 2e. agents.list without identity -> 401 identity_required.
noident_body=$(curl -s --max-time 5 \
    -H "Content-Type: application/json" \
    -X POST "${LIST_URL}" -d '{}' 2>/dev/null || echo '{}')
noident_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H "Content-Type: application/json" \
    -X POST "${LIST_URL}" -d '{}' 2>/dev/null || echo '000')
ni_code=$(printf '%s' "${noident_body}" | jq -r '.code // empty' 2>/dev/null || echo '')
if [[ "${noident_status}" == "401" && "${ni_code}" == "identity_required" ]]; then
    ok 'phase 73e: agents.list without identity -> 401 identity_required'
else
    fail "phase 73e: agents.list without identity expected 401/identity_required, got ${noident_status}/${ni_code}"
fi

# 3. The /agents page route ships with `harbor console` (73m).
skip 'phase 73e: /agents route lands with 73m harbor console subcommand'

smoke_summary
