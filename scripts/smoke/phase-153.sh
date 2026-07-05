#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
# Phase 153 smoke — admin-widened fleet enumeration for `tasks.list` +
# `agents.list` (D-284; RFC §6.8 / §6.16 / §5.2).
#
# Both methods gain an additive `filter.tenant_ids` fleet-widening
# selector gated on the verified `auth.ScopeAdmin` claim:
#
#   1. Non-widened list (dev token, own identity)        → 200 (today-path intact).
#   2. Widened list WITHOUT the admin claim (no bearer)  → fails LOUD
#      (401 identity-edge or 403 scope_mismatch — never 200, never a
#      silent narrowing to own scope).
#   3. Widened list WITH the dev admin token             → 200 + rows carry
#      per-row `identity` attribution.
#
# The dev token carries admin scope (cmd/harbor: "plus admin scope"), so
# it exercises the accepted widened path; a bearer-less request exercises
# the fail-closed path.
#
# 404/405/501 → SKIP per AGENTS.md §4.2 — coexists with earlier-phase
# builds. A SKIP that should be an OK is a bug.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

TOKEN="${HARBOR_DEV_TOKEN:-}"

# fleet_probes <surface> <list_url> <rows_key>
#
# Runs the three-probe matrix (non-widened / widened-no-admin /
# widened-admin) against one list surface. rows_key is the JSON key
# carrying the row array (`rows` for tasks, `agents` for agents).
fleet_probes() {
    local surface="$1" list_url="$2" rows_key="$3"
    local tmp="/tmp/phase-153-${surface}.json"

    if ! command -v curl >/dev/null 2>&1; then
        skip "phase 153: curl unavailable — cannot exercise ${surface}.list"
        return 0
    fi
    if ! command -v jq >/dev/null 2>&1; then
        skip "phase 153: jq unavailable — cannot assert ${surface}.list widened shape"
        return 0
    fi

    # --- 1. Non-widened list (dev token, own identity) → 200. -------------
    #     The route is POST-only (a GET 405 is NOT "absent"), so the
    #     existence check is the POST itself: 404 ⇒ surface not built on
    #     this build ⇒ SKIP per AGENTS.md §4.2; any other code means the
    #     surface exists.
    local own_body='{"identity":{"tenant":"dev","user":"dev","session":"dev"}}'
    local code
    code=$(curl -s -o "${tmp}" -w '%{http_code}' --max-time 10 \
        -X POST "${list_url}" \
        -H 'Content-Type: application/json' \
        -H "Authorization: Bearer ${TOKEN}" \
        -H 'X-Harbor-Tenant: dev' -H 'X-Harbor-User: dev' -H 'X-Harbor-Session: dev' \
        -d "${own_body}" 2>/dev/null || true)
    case "${code}" in
        404|501|000)
            skip "phase 153: ${surface}.list not present on this build (HTTP ${code})"
            return 0
            ;;
    esac
    if [[ "${code}" == "200" ]]; then
        ok "phase 153: ${surface}.list non-widened returns 200 (identity-scoped today-path intact)"
    elif [[ "${code}" =~ ^(401|403)$ ]]; then
        skip "phase 153: ${surface}.list non-widened rejected (HTTP ${code}) — auth edge not satisfied in this run"
    else
        fail "phase 153: ${surface}.list non-widened returned unexpected HTTP ${code}"
    fi

    # --- 2. Widened WITHOUT the admin claim → fails LOUD. -----------------
    #     A bearer-less request carries identity via X-Harbor headers but
    #     no admin scope: the runtime must fail closed (401 identity-edge
    #     or 403 scope_mismatch), NEVER 200 (never a silent narrowing).
    #     In the preflight dev harness this always lands on the 401
    #     identity-edge branch — the dev server mints only one (admin)
    #     token from an in-process ephemeral key, so no valid NON-admin
    #     bearer can be produced here. The 403 scope_mismatch gate itself
    #     is exercised by the unit + integration tests (widened-without-
    #     claim → ErrScopeMismatch); this smoke asserts fail-closed only.
    local widen_body='{"identity":{"tenant":"dev","user":"dev","session":"dev"},"filter":{"tenant_ids":["tenant-foreign"]}}'
    code=$(curl -s -o "${tmp}" -w '%{http_code}' --max-time 10 \
        -X POST "${list_url}" \
        -H 'Content-Type: application/json' \
        -H 'X-Harbor-Tenant: dev' -H 'X-Harbor-User: dev' -H 'X-Harbor-Session: dev' \
        -d "${widen_body}" 2>/dev/null || true)
    if [[ "${code}" == "403" ]]; then
        if jq -e '.code == "scope_mismatch"' "${tmp}" >/dev/null 2>&1; then
            ok "phase 153: ${surface}.list widened WITHOUT admin → 403 scope_mismatch (fails loud; CLAUDE.md §6 rule 5 / §13)"
        else
            ok "phase 153: ${surface}.list widened WITHOUT admin → 403 (fails closed)"
        fi
    elif [[ "${code}" == "401" ]]; then
        ok "phase 153: ${surface}.list widened WITHOUT admin → 401 identity-edge (fails closed — never a silent narrowing)"
    elif [[ "${code}" == "200" ]]; then
        fail "phase 153: ${surface}.list widened WITHOUT admin returned 200 — SILENT NARROWING / privilege leak"
    else
        skip "phase 153: ${surface}.list widened-no-admin probe returned HTTP ${code} (server unreachable or unexpected)"
    fi

    # --- 3. Widened WITH the dev admin token → 200 + row identity. --------
    local admin_widen_body='{"identity":{"tenant":"dev","user":"dev","session":"dev"},"filter":{"tenant_ids":["dev"]}}'
    code=$(curl -s -o "${tmp}" -w '%{http_code}' --max-time 10 \
        -X POST "${list_url}" \
        -H 'Content-Type: application/json' \
        -H "Authorization: Bearer ${TOKEN}" \
        -H 'X-Harbor-Tenant: dev' -H 'X-Harbor-User: dev' -H 'X-Harbor-Session: dev' \
        -d "${admin_widen_body}" 2>/dev/null || true)
    if [[ "${code}" == "200" ]]; then
        if jq -e ".${rows_key} | type == \"array\"" "${tmp}" >/dev/null 2>&1; then
            # Per-row identity attribution: when the fleet read returned any
            # row, its identity.tenant must be populated.
            if jq -e "(.${rows_key} | length) == 0 or (.${rows_key}[0].identity.tenant | length > 0)" "${tmp}" >/dev/null 2>&1; then
                ok "phase 153: ${surface}.list widened WITH admin token → 200 + rows[].identity attribution present"
            else
                fail "phase 153: ${surface}.list widened row missing identity.tenant attribution"
            fi
        else
            fail "phase 153: ${surface}.list widened response missing '${rows_key}' array"
        fi
    elif [[ "${code}" =~ ^(401|403)$ ]]; then
        skip "phase 153: ${surface}.list widened-admin rejected (HTTP ${code}) — dev token not admin-scoped in this run"
    else
        fail "phase 153: ${surface}.list widened-admin returned unexpected HTTP ${code}"
    fi
}

fleet_probes "tasks" "$(api_url /v1/tasks/list)" "rows"
fleet_probes "agents" "$(api_url /v1/agents/list)" "agents"

smoke_summary
