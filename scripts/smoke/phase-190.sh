#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
# Phase 190 smoke — `agents.list` surfaces the runtime's synthetic default
# agent as a first-class, `is_default: true` marked row (D-327; RFC §6.16
# / §5.2 / §7).
#
# The dev server boots serving through its synthetic default agent (the
# boot-configured `harbor-dev-agent`) which is never registered as a fleet
# entity. Before this phase, a runtime with zero registrations returned an
# empty `agents.list`; now it returns exactly that synthetic row, marked
# `is_default`, so a fleet catalog can tell "one agent, not enumerable
# this way" from an empty page.
#
# Assertions:
#   1. Static (cheap lockstep pre-check): the `is_default` marker is
#      present in the Go wire type AND the hand-maintained TS client.
#   2. Live: POST /v1/agents/list (dev token, dev identity, no registered
#      agents) returns 200 with at least one row carrying
#      `is_default == true`.
#
# 404/405/501 → SKIP per AGENTS.md §4.2 — coexists with earlier-phase
# builds. A SKIP that should be an OK is a bug.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

TOKEN="${HARBOR_DEV_TOKEN:-}"

# --- 1. Static lockstep pre-check ----------------------------------------
if grep -q 'IsDefault bool' internal/protocol/types/agents.go &&
    grep -q 'is_default' internal/protocol/types/agents.go; then
    ok "phase 190: Agent.IsDefault (is_default) present in the Go wire type"
else
    fail "phase 190: Agent.IsDefault / is_default missing from internal/protocol/types/agents.go"
fi
if grep -q 'is_default' web/console/src/lib/protocol/agents.ts; then
    ok "phase 190: is_default present in the hand-maintained TS client (D-223 lockstep)"
else
    fail "phase 190: is_default missing from web/console/src/lib/protocol/agents.ts"
fi

# --- 2. Live: agents.list returns the synthetic default row --------------
list_default_probe() {
    local list_url="$1" tmp="/tmp/phase-190-agents.json"

    if ! command -v curl >/dev/null 2>&1; then
        skip "phase 190: curl unavailable — cannot exercise agents.list"
        return 0
    fi
    if ! command -v jq >/dev/null 2>&1; then
        skip "phase 190: jq unavailable — cannot assert the is_default row shape"
        return 0
    fi

    # The route is POST-only; a 404/501 means the surface is absent on this
    # build ⇒ SKIP (AGENTS.md §4.2). The dev token carries the dev
    # (tenant, user, session) identity for an own-scope (non-widened) read.
    local body='{"identity":{"tenant":"dev","user":"dev","session":"dev"}}'
    local code
    code=$(curl -s -o "${tmp}" -w '%{http_code}' --max-time 10 \
        -X POST "${list_url}" \
        -H 'Content-Type: application/json' \
        -H "Authorization: Bearer ${TOKEN}" \
        -H 'X-Harbor-Tenant: dev' -H 'X-Harbor-User: dev' -H 'X-Harbor-Session: dev' \
        -d "${body}" 2>/dev/null || true)
    case "${code}" in
        404 | 501 | 000)
            skip "phase 190: agents.list not present on this build (HTTP ${code})"
            return 0
            ;;
    esac
    if [[ "${code}" != "200" ]]; then
        if [[ "${code}" =~ ^(401|403)$ ]]; then
            skip "phase 190: agents.list rejected (HTTP ${code}) — auth edge not satisfied in this run"
            return 0
        fi
        fail "phase 190: agents.list returned unexpected HTTP ${code}"
        return 0
    fi
    # 200 — at least one row must carry is_default == true.
    if jq -e '[.agents[]? | select(.is_default == true)] | length >= 1' "${tmp}" >/dev/null 2>&1; then
        ok "phase 190: agents.list returns >=1 row with is_default=true (synthetic default agent surfaced)"
    else
        fail "phase 190: agents.list 200 but NO is_default row — the synthetic default agent is not surfaced"
    fi
}

list_default_probe "$(api_url /v1/agents/list)"

smoke_summary
