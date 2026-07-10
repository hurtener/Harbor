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
assert_grep_present 'memory.MemoryStore' "internal/runtime/serve/runloop.go" \
    "perTaskRunLoopDriver opts carry the MemoryStore dep (D-149)"
# Phase 111d (D-201): the D-149 SkillStore dep became the Phase-39
# Directory — the `<skills_context>` producer the driver consumes.
assert_grep_present 'skillsDirectory \*skills.Directory' "internal/runtime/serve/runloop.go" \
    "perTaskRunLoopDriver opts carry the skills Directory dep (D-149 → D-201)"
# Phase 110b (D-195) re-homed the projection helpers to the exported
# internal/runtime/runctx package; the run loop is a thin caller.
assert_grep_present 'runctx\.FetchMemoryBlocks' "internal/runtime/serve/runloop.go" \
    "runloop calls runctx.FetchMemoryBlocks (promotes ProjectMemoryBlocks + semantic recall)"
assert_grep_present 'runctx\.ProjectSkillsDirectory' "internal/runtime/serve/runloop.go" \
    "runloop projects the Directory view via runctx.ProjectSkillsDirectory (110b → 111d)"
assert_grep_present 'RepairCounters{' "internal/runtime/serve/runloop.go" \
    "per-run *RepairCounters allocated in runOne (D-145 producer-side, D-149)"
assert_grep_present 'runtime_fetch_error' "internal/runtime/serve/runloop.go" \
    "memory/skills fetch errors map to MarkFailed(code=runtime_fetch_error)"
# Phase 110c (D-196) re-homed the YAML->PlanningHints projection from
# the cmd-local plannerHintsFromConfig helper onto the owning package
# (planner.HintsFromConfig); bootDevStack consumes the exported one.
assert_grep_present 'planner\.HintsFromConfig' "internal/runtime/serve/serve.go" \
    "bootDevStack projects YAML planning_hints onto *planner.PlanningHints (via planner.HintsFromConfig - 110c)"

# ----------------------------------------------------------------------------
# Test fixture (D-094 source-of-truth) mirror lands the same wiring.
# ----------------------------------------------------------------------------
# Phase 110b (D-195): the D-094 mirror copies are deleted; devstack
# calls the SAME promoted projections production calls.
assert_grep_present 'runctx\.FetchMemoryBlocks' "internal/runtime/serve/runloop.go" \
    "devstack calls runctx.FetchMemoryBlocks (mirror collapsed; 110b + semantic recall)"
assert_grep_present 'runctx\.ProjectSkillsDirectory' "internal/runtime/serve/runloop.go" \
    "devstack projects the Directory view via runctx.ProjectSkillsDirectory (mirror; 110b → 111d)"

smoke_summary
