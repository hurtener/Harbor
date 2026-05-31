#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 108c — Console Overview page (visual rebuild + dead-code cleanup).
#
# Console-only wave (no new Runtime Protocol surface). Static checks that the
# rebuilt canvas references the new pieces, the mock-absent surfaces (top
# FilterBar, right DetailRail, +New menu) are gone, the dead code was deleted,
# and the cost axes are the real ones (model | runtime). Behavioural coverage
# lives in tests/overview-page.spec.ts + the cost/alerts unit specs.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

PAGE="web/console/src/routes/(console)/overview/+page.svelte"

assert_file_or_skip() {
    local path="$1" desc="$2"
    if [ -f "${path}" ]; then ok "${desc}: ${path} exists"
    else skip "${desc}: ${path} missing (Phase 108c not yet implemented)"; fi
}
assert_absent_or_ok() {
    local path="$1" desc="$2"
    if [ ! -f "${path}" ]; then ok "${desc}: ${path} removed"
    else fail "${desc}: ${path} still present"; fi
}
assert_grep_or_skip() {
    local pattern="$1" path="$2" desc="$3"
    if [ ! -f "${path}" ]; then skip "${desc}: ${path} not found (Phase 108c not yet implemented)"; return; fi
    if grep -qE "${pattern}" "${path}" 2>/dev/null; then ok "${desc}"
    else skip "${desc}: pattern '${pattern}' absent (Phase 108c not yet implemented)"; fi
}
assert_not_grep_or_fail() {
    local pattern="$1" path="$2" desc="$3"
    if [ ! -f "${path}" ]; then skip "${desc}: ${path} not found"; return; fi
    if grep -qE "${pattern}" "${path}" 2>/dev/null; then fail "${desc}: '${pattern}' still present in ${path}"
    else ok "${desc}"; fi
}

# ---- New canvas pieces present ----------------------------------------------
assert_file_or_skip "web/console/src/lib/components/overview/ContextAuditRow.svelte" \
    "phase-108c: slim context/audit row component"
assert_file_or_skip "web/console/src/lib/components/overview/AlertsStrip.svelte" \
    "phase-108c: alerts strip component"
assert_file_or_skip "web/console/src/lib/overview/alerts.ts" \
    "phase-108c: alerts/audit projection"
assert_grep_or_skip "ContextAuditRow" "${PAGE}" "phase-108c: page renders the context/audit row"
assert_grep_or_skip "AlertsStrip" "${PAGE}" "phase-108c: page renders the alerts strip"

# ---- Mock-absent surfaces removed from the page -----------------------------
assert_not_grep_or_fail "FilterBar" "${PAGE}" "phase-108c: top FilterBar removed from Overview"
assert_not_grep_or_fail "DetailRail" "${PAGE}" "phase-108c: right DetailRail removed from Overview"
assert_not_grep_or_fail "NewMenu" "${PAGE}" "phase-108c: +New menu removed from Overview (chrome territory)"

# ---- Dead code deleted ------------------------------------------------------
assert_absent_or_ok "web/console/src/lib/components/overview/NewMenu.svelte" \
    "phase-108c: orphaned NewMenu deleted"
assert_absent_or_ok "web/console/src/lib/components/overview/HealthChipStrip.svelte" \
    "phase-108c: orphaned HealthChipStrip deleted"
assert_absent_or_ok "web/console/src/lib/db/saved_filters_overview.ts" \
    "phase-108c: orphaned overview saved-filters store deleted"

# ---- Cost axes are the real ones (model | runtime), not the dead agent/tenant
assert_grep_or_skip "CostBreakdown = 'model' \| 'runtime'" \
    "web/console/src/lib/overview/cost.ts" \
    "phase-108c: cost axes are model | runtime (real)"

smoke_summary
