#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83i — RunContext wiring closure (D-152). Wave 17 / v1.1
# operator-validation blockers. The runloop's default case now
# dispatches CallTool decisions via a new ToolExecutor seam, appends
# trajectory.Step{Action, Observation, LLMObservation} so the planner
# sees its prior actions, populates Catalog/Trajectory/Emit on
# RunContext, and writes back to memory on FinishGoal. End-to-end
# coverage is the operator validation against mcp-youtube.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Runloop seam.
# ----------------------------------------------------------------------------
assert_grep_present 'type ToolExecutor interface' "internal/runtime/steering/runloop.go" \
    "steering.ToolExecutor interface declared (D-152)"
assert_grep_present 'ErrDecisionShapeUnsupported' "internal/runtime/steering/runloop.go" \
    "steering.ErrDecisionShapeUnsupported sentinel declared"
assert_grep_present 'ToolExecutor ToolExecutor' "internal/runtime/steering/runloop.go" \
    "RunSpec.ToolExecutor field declared"
assert_grep_present 'spec\.Base\.Trajectory\.Steps = append' "internal/runtime/steering/runloop.go" \
    "runloop appends trajectory.Step after each non-Finish step"

# ----------------------------------------------------------------------------
# Dev binary's executor + catalog view.
# ----------------------------------------------------------------------------
assert_file "cmd/harbor/cmd_dev_executor.go" \
    "devToolExecutor lives at the documented path"
assert_grep_present 'type devToolExecutor struct' "cmd/harbor/cmd_dev_executor.go" \
    "devToolExecutor type declared"
assert_grep_present 'func.*projectForLLM' "cmd/harbor/cmd_dev_executor.go" \
    "devToolExecutor projects heavy results via the artifact store"
assert_file "cmd/harbor/cmd_dev_catalog_view.go" \
    "runtimeCatalogView lives at the documented path"
assert_grep_present 'type runtimeCatalogView struct' "cmd/harbor/cmd_dev_catalog_view.go" \
    "runtimeCatalogView type declared"

# ----------------------------------------------------------------------------
# runOne wiring.
# ----------------------------------------------------------------------------
assert_grep_present 'Catalog:\s*catalogView' "cmd/harbor/cmd_dev_runloop.go" \
    "runOne populates RunContext.Catalog"
assert_grep_present 'Trajectory:\s*traj' "cmd/harbor/cmd_dev_runloop.go" \
    "runOne populates RunContext.Trajectory"
assert_grep_present 'Emit:\s*emit' "cmd/harbor/cmd_dev_runloop.go" \
    "runOne populates RunContext.Emit closure"
assert_grep_present 'ToolExecutor:\s*d\.executor' "cmd/harbor/cmd_dev_runloop.go" \
    "runOne sets RunSpec.ToolExecutor"
assert_grep_present 'd\.memory\.AddTurn' "cmd/harbor/cmd_dev_runloop.go" \
    "memory.AddTurn writeback on FinishGoal"

# ----------------------------------------------------------------------------
# Devstack mirror (D-094).
# ----------------------------------------------------------------------------
assert_grep_present 'devStackToolExecutor' "harbortest/devstack/devstack.go" \
    "devstack mirror carries the executor (D-094)"
assert_grep_present 'devStackCatalogView' "harbortest/devstack/devstack.go" \
    "devstack mirror carries the catalog view (D-094)"
assert_grep_present 'd\.memory\.AddTurn' "harbortest/devstack/devstack.go" \
    "devstack mirror carries memory writeback (D-094)"

smoke_summary
