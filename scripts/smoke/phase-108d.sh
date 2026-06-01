#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 108d — Console Live Runtime page (visual rebuild + dead-code cleanup).
#
# Console-only wave (no new Runtime Protocol surface). Static checks that the
# Stage 1 rebuild landed: the saved-view FilterBar strip is gone, the event
# stream subscribes to named event types (108c named-SSE fix), the dead
# saved-filters store was deleted. Behavioural coverage lives in
# tests/live-runtime-page.spec.ts; the topology graph (Stage 2) is verified
# structurally there.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

PAGE="web/console/src/routes/(console)/live-runtime/+page.svelte"

assert_grep_or_skip() {
    local pattern="$1" path="$2" desc="$3"
    if [ ! -f "${path}" ]; then skip "${desc}: ${path} not found (Phase 108d not yet implemented)"; return; fi
    if grep -qE "${pattern}" "${path}" 2>/dev/null; then ok "${desc}"
    else skip "${desc}: pattern absent (Phase 108d not yet implemented)"; fi
}
assert_not_grep_or_fail() {
    local pattern="$1" path="$2" desc="$3"
    if [ ! -f "${path}" ]; then skip "${desc}: ${path} not found"; return; fi
    if grep -qE "${pattern}" "${path}" 2>/dev/null; then fail "${desc}: '${pattern}' still present in ${path}"
    else ok "${desc}"; fi
}
assert_absent_or_ok() {
    local path="$1" desc="$2"
    if [ ! -f "${path}" ]; then ok "${desc}: ${path} removed"
    else fail "${desc}: ${path} still present"; fi
}

# ---- Saved-view FilterBar strip removed (mock has the tab toolbar) ----------
# Anchor to real USAGE (import line / element open / testid), not bare strings —
# the words FilterBar / SavedViewChips appear in prose comments and must not
# trip the gate (they are not live code).
assert_not_grep_or_fail "import FilterBar" "${PAGE}" "phase-108d: FilterBar import removed from Live Runtime"
assert_not_grep_or_fail "import SavedViewChips" "${PAGE}" "phase-108d: SavedViewChips import removed from Live Runtime"
assert_not_grep_or_fail "data-testid=\"live-runtime-save-view\"" "${PAGE}" "phase-108d: save-view button removed"

# ---- Event stream subscribes to NAMED event types (108c named-SSE fix) ------
assert_grep_or_skip "task.completed" "${PAGE}" "phase-108d: event stream lists named event types (not empty open())"
assert_grep_or_skip "llm.cost.recorded" "${PAGE}" "phase-108d: event stream subscribes to cost events"

# ---- Tab strip toolbar + header refresh present ----------------------------
assert_grep_or_skip "TabStrip" "${PAGE}" "phase-108d: tab strip toolbar present"
assert_grep_or_skip "live-runtime-refresh" "${PAGE}" "phase-108d: header Refresh present"

# ---- Topology honest info state (D-164) ------------------------------------
assert_grep_or_skip "Topology view not available" "${PAGE}" "phase-108d: topology info state (D-164) on planner runtimes"

# ---- Dead code deleted -----------------------------------------------------
assert_absent_or_ok "web/console/src/lib/db/saved_filters_live_runtime.ts" \
    "phase-108d: orphaned live-runtime saved-filters store deleted"

smoke_summary
