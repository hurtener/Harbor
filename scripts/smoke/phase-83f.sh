#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83f — wire the dev RunLoop driver to populate RunContext's
# 83-band primitives (MemoryBlocks / SkillsContext / RepairCounters /
# PlanningHints) and the user-facing Query/Goal. Static-only smoke:
# the driver wiring is exercised end-to-end by
# `test/integration/phase83f_runloop_consumers_test.go`; this script
# asserts the YAML + Go surfaces are in place so the documented
# operator-facing knobs cannot silently disappear.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Operator-facing YAML config keys land in examples/harbor.yaml.
# ----------------------------------------------------------------------------
assert_grep_present 'skills_context_max' "examples/harbor.yaml" \
    "examples/harbor.yaml documents planner.skills_context_max (D-149)"
assert_grep_present 'planning_hints' "examples/harbor.yaml" \
    "examples/harbor.yaml documents planner.planning_hints (D-149)"
assert_grep_present 'preferred_tools' "examples/harbor.yaml" \
    "examples/harbor.yaml shows the planner.planning_hints.preferred_tools subfield"

# ----------------------------------------------------------------------------
# Config schema + validator carry the two new fields.
# ----------------------------------------------------------------------------
assert_grep_present 'SkillsContextMax' "internal/config/config.go" \
    "PlannerConfig declares SkillsContextMax (D-149)"
assert_grep_present 'PlannerPlanningHintsCfg' "internal/config/config.go" \
    "PlannerPlanningHintsCfg type defined for the new YAML surface (D-149)"
assert_grep_present 'planner.skills_context_max' "internal/config/validate.go" \
    "validator rejects negative planner.skills_context_max"

# ----------------------------------------------------------------------------
# Driver fetches the four primitives + project helpers exist.
# ----------------------------------------------------------------------------
assert_grep_present 'memory.MemoryStore' "cmd/harbor/cmd_dev_runloop.go" \
    "perTaskRunLoopDriver opts carry the MemoryStore dep (D-149)"
assert_grep_present 'skills.SkillStore' "cmd/harbor/cmd_dev_runloop.go" \
    "perTaskRunLoopDriver opts carry the SkillStore dep (D-149)"
assert_grep_present 'projectMemoryBlocks' "cmd/harbor/cmd_dev_runloop.go" \
    "projectMemoryBlocks helper present (LLMContextPatch → MemoryBlocks)"
assert_grep_present 'projectSkillsContext' "cmd/harbor/cmd_dev_runloop.go" \
    "projectSkillsContext helper present (RankedSkill → SkillsContext)"
assert_grep_present 'RepairCounters{' "cmd/harbor/cmd_dev_runloop.go" \
    "per-run *RepairCounters allocated in runOne (D-145 producer-side, D-149)"
assert_grep_present 'runtime_fetch_error' "cmd/harbor/cmd_dev_runloop.go" \
    "memory/skills fetch errors map to MarkFailed(code=runtime_fetch_error)"
assert_grep_present 'plannerHintsFromConfig' "cmd/harbor/cmd_dev.go" \
    "bootDevStack projects YAML planning_hints onto *planner.PlanningHints"

# ----------------------------------------------------------------------------
# Test fixture (D-094 source-of-truth) mirror lands the same wiring.
# ----------------------------------------------------------------------------
assert_grep_present 'devStackProjectMemoryBlocks' "harbortest/devstack/devstack.go" \
    "devstack mirror carries the memory projector (D-094 mirror invariant)"
assert_grep_present 'devStackProjectSkillsContext' "harbortest/devstack/devstack.go" \
    "devstack mirror carries the skills projector (D-094 mirror invariant)"

smoke_summary
