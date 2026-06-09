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
# The production executor + catalog view. Phase 110a (D-194) promoted
# both out of cmd/harbor: the executor lives in internal/runtime/dispatch
# and the view is tools.PlannerView — the 83i guarantees (typed executor,
# D-026 projectForLLM heavy promotion, identity-filtered view) hold at
# the promoted homes.
# ----------------------------------------------------------------------------
assert_file "internal/runtime/dispatch/dispatch.go" \
    "production executor lives at the promoted path (110a / D-194)"
assert_grep_present 'type toolExecutor struct' "internal/runtime/dispatch/dispatch.go" \
    "promoted executor type declared"
assert_grep_present 'func.*projectForLLM' "internal/runtime/dispatch/dispatch.go" \
    "promoted executor projects heavy results via the artifact store"
assert_file "internal/tools/planner_view.go" \
    "catalog view lives at the promoted path (110a / D-194)"
assert_grep_present 'type PlannerView struct' "internal/tools/planner_view.go" \
    "promoted PlannerView type declared"

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
# Devstack parity (D-094 → 110a / D-194: the hand-maintained executor +
# view mirrors are DELETED; devstack wires the promoted constructors).
# ----------------------------------------------------------------------------
assert_grep_present 'dispatch\.NewToolExecutor' "harbortest/devstack/devstack.go" \
    "devstack wires the promoted executor (110a / D-194)"
assert_grep_present 'tools\.NewPlannerView' "harbortest/devstack/devstack.go" \
    "devstack wires the promoted catalog view (110a / D-194)"
assert_grep_present 'd\.memory\.AddTurn' "harbortest/devstack/devstack.go" \
    "devstack mirror carries memory writeback (D-094)"

smoke_summary
