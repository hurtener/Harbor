#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 108e — Live Runtime → single-runtime capability-adaptive cockpit.
#
# Console-only reframe (no new Runtime Protocol surface). Static guards that
# the cockpit is composed from the runtime's advertised capabilities (a
# declarative panel registry), that the always-present spine panels exist, that
# topology is capability-GATED (not the page spine), and that the
# Playground-overlapping run composer + any chat import are gone.
#
# Until the reframe lands, the registry module is absent → every assertion
# SKIPs (the 404/405/501 → SKIP analogue), so preflight stays green at 108d.
# When 108e ships, `panels.ts` appears and the assertions flip to OK.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

PAGE="web/console/src/routes/(console)/live-runtime/+page.svelte"
PANELS="web/console/src/lib/live-runtime/panels.ts"

if [[ ! -f "${PANELS}" ]]; then
    skip "phase-108e: capability panel registry (panels.ts) not present yet — reframe not implemented"
    smoke_summary
    return 0 2>/dev/null || exit 0
fi

assert_grep_or_skip() {
    local pattern="$1" path="$2" desc="$3"
    if [ ! -f "${path}" ]; then skip "${desc}: ${path} not found"; return; fi
    if grep -qE "${pattern}" "${path}" 2>/dev/null; then ok "${desc}"
    else skip "${desc}: pattern absent"; fi
}
assert_not_grep_or_fail() {
    local pattern="$1" path="$2" desc="$3"
    if [ ! -f "${path}" ]; then skip "${desc}: ${path} not found"; return; fi
    if grep -qE "${pattern}" "${path}" 2>/dev/null; then fail "${desc}: '${pattern}' still present in ${path}"
    else ok "${desc}"; fi
}

# ---- Composition is capability-driven (declarative registry) ---------------
assert_grep_or_skip "resolvePanels" "${PANELS}" "phase-108e: panel registry exposes resolvePanels"
assert_grep_or_skip "resolvePanels" "${PAGE}" "phase-108e: page composes panels via resolvePanels (not a hardcoded list)"

# ---- Capability vocabulary stays guarded -----------------------------------
assert_grep_or_skip "topology_snapshot" "${PANELS}" "phase-108e: registry keys the topology capability"
assert_grep_or_skip "runtime_health" "${PANELS}" "phase-108e: registry keys the health capability"
assert_grep_or_skip "governance_posture|llm.cost.recorded" "${PANELS}" "phase-108e: registry keys the cost/governance capability"

# ---- Spine panels present --------------------------------------------------
assert_grep_or_skip "needs-attention" "${PAGE}" "phase-108e: Needs-attention spine panel present"
assert_grep_or_skip "active-sessions" "${PAGE}" "phase-108e: Active-sessions spine panel present"
assert_grep_or_skip "runtime-posture-header|RuntimePostureHeader" "${PAGE}" "phase-108e: runtime posture header present"

# ---- Topology is capability-GATED, not the spine ---------------------------
assert_grep_or_skip "topology_snapshot" "${PAGE}" "phase-108e: topology panel is capability-gated on the page"

# ---- Playground overlap removed --------------------------------------------
assert_not_grep_or_fail "run-composer" "${PAGE}" "phase-108e: free-floating run composer removed from the cockpit"
assert_not_grep_or_fail "\\\$lib/chat/" "${PAGE}" "phase-108e: no chat-module import on the cockpit (D-062/D-091)"

# ---- Honest no-fabrication copy for gated-absent panels --------------------
assert_grep_or_skip "does not advertise" "${PAGE_DATA:-web/console/src/lib/components/live-runtime/health-panel.svelte}" \
    "phase-108e: gated-absent panels keep honest 'does not advertise' copy (no fabrication)"

smoke_summary
