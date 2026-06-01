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
# When the reframe is absent (panels.ts missing) every assertion SKIPs (the
# 404/405/501 → SKIP analogue); once it lands the assertions are HARD (present /
# absent), so a regressed spine panel or a re-introduced composer FAILs.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

PAGE="web/console/src/routes/(console)/live-runtime/+page.svelte"
PANELS="web/console/src/lib/live-runtime/panels.ts"
HEALTH="web/console/src/lib/components/live-runtime/health-panel.svelte"
TOPO_PANEL="web/console/src/lib/components/live-runtime/topology-panel.svelte"

if [[ ! -f "${PANELS}" ]]; then
    skip "phase-108e: capability panel registry (panels.ts) not present yet — reframe not implemented"
    smoke_summary
    return 0 2>/dev/null || exit 0
fi

# ---- Composition is capability-driven (declarative registry) ---------------
assert_grep_present "resolvePanels" "${PANELS}" "phase-108e: panel registry exposes resolvePanels"
assert_grep_present "resolvePanels" "${PAGE}" "phase-108e: page composes panels via resolvePanels (not a hardcoded list)"

# ---- Capability vocabulary stays guarded -----------------------------------
assert_grep_present "topology_snapshot" "${PANELS}" "phase-108e: registry keys the topology capability"
assert_grep_present "runtime_health" "${PANELS}" "phase-108e: registry keys the health capability"
assert_grep_present "governance_posture" "${PANELS}" "phase-108e: registry keys the cost/governance capability"

# ---- Spine panels present --------------------------------------------------
assert_grep_present "needs-attention" "${PAGE}" "phase-108e: Needs-attention spine panel present"
assert_grep_present "active-sessions" "${PAGE}" "phase-108e: Active-sessions spine panel present"
assert_grep_present "runtime-posture-header|RuntimePostureHeader" "${PAGE}" "phase-108e: runtime posture header present"
assert_grep_present "panel-live-events" "${PAGE}" "phase-108e: Live-events spine panel present"
assert_grep_present "panel-health" "${PAGE}" "phase-108e: Health spine panel present"
assert_grep_present "panel-cost" "${PAGE}" "phase-108e: Cost spine panel present"

# ---- Header Refresh moved onto the posture header --------------------------
assert_grep_present "live-runtime-refresh" "web/console/src/lib/components/live-runtime/runtime-posture-header.svelte" \
    "phase-108e: Refresh lives on the posture header"

# ---- Topology is capability-GATED, not the spine ---------------------------
assert_grep_present "topology_snapshot|CAP_TOPOLOGY_SNAPSHOT" "${PAGE}" "phase-108e: topology panel is capability-gated on the page"
assert_grep_present "Topology view not available" "${TOPO_PANEL}" "phase-108e: honest D-164 topology-absent copy retained"

# ---- Tabs are GONE (the cockpit has no tab strip) --------------------------
assert_grep_absent "TabStrip" "${PAGE}" "phase-108e: tab strip removed from the cockpit"

# ---- Playground overlap removed --------------------------------------------
assert_grep_absent "run-composer" "${PAGE}" "phase-108e: free-floating run composer removed from the cockpit"
assert_grep_absent "\\\$lib/chat/" "${PAGE}" "phase-108e: no chat-module import on the cockpit (D-062/D-091)"

# ---- Honest no-fabrication copy for spine self-probing panels --------------
assert_grep_present "not available on this runtime" "${HEALTH}" \
    "phase-108e: Health panel keeps honest 'not available' copy (no fabrication)"
assert_grep_present "No cost recorded yet" "web/console/src/lib/components/live-runtime/cost-governance-panel.svelte" \
    "phase-108e: Cost panel keeps honest 'no cost recorded' copy (no fabrication)"

smoke_summary
