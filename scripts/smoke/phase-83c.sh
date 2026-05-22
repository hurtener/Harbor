#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83c — ReAct dynamic repair guidance + planning hints smoke.
#
# The repair-guidance / planning-hints surface is planner-internal —
# no HTTP endpoint. Correctness is validated by
# `go test ./internal/planner/...` (preflight runs `go test`
# separately). This smoke pins STATIC invariants: the nine checked-in
# golden tier fixtures exist and each names its own tier, and the new
# event type is registered in the planner event taxonomy.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

GUIDANCE_DIR="internal/planner/react/testdata/repair_guidance"

# The nine repair-guidance golden fixtures must exist — three tiers
# (reminder / warning / critical) for each of the three counters
# (finish / args / multi_action).
for counter in finish args multi_action; do
  for tier in reminder warning critical; do
    fixture="${GUIDANCE_DIR}/${counter}_${tier}.txt"
    assert_file "${fixture}" "repair-guidance golden ${counter}_${tier} exists"
    # Defensive: each tier body must open with its own tier name —
    # catches a copy-paste typo that reuses the wrong tier's text.
    assert_grep_present "^${tier}:" "${fixture}" \
      "golden ${counter}_${tier} body opens with the '${tier}' tier name"
  done
done

# The new event type is registered in the planner event taxonomy.
assert_grep_present 'EventTypePlannerRepairGuidanceInjected' \
  "internal/planner/events.go" \
  "planner.repair_guidance_injected event type is registered"
assert_grep_present 'planner.repair_guidance_injected' \
  "internal/planner/events.go" \
  "planner.repair_guidance_injected event string is defined"

# The PlanningHints / RepairCounters RunContext surface exists.
assert_grep_present 'RepairCounters \*RepairCounters' \
  "internal/planner/planner.go" \
  "RunContext carries the RepairCounters pointer field"
assert_grep_present 'PlanningHints \*PlanningHints' \
  "internal/planner/planner.go" \
  "RunContext carries the PlanningHints pointer field"

smoke_summary
