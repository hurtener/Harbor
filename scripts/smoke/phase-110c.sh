#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 110c smoke — config-projection exporters (D-196).
# (RFC §6.5 / §6.6 / §6.7 / §9 / §10; docs/plans/phase-110c-config-projection-exporters.md)
#
# Static + unit-test assertions:
#   1. The five exported FromConfig projections + the production driver
#      aggregator exist on their owning packages.
#   2. The cmd-local duplicate helpers and ALL devstack duplicates are
#      deleted (the D-155 / audit-B3 silent-field-drop mechanism).
#   3. cmd/harbor/main.go AND harbortest/devstack import
#      internal/drivers/prod (the single sanctioned blank-import home).
#   4. config.Defaults() / config.ValidateCore exist; the re-homed
#      planner-knob defaults have ONE source each.
#   5. The projection/profile unit suites pass under -race.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# --- 1. The exported projections + the aggregator exist -------------------

assert_file internal/llm/from_config.go        'phase 110c: llm.SnapshotFromConfig projection file'
assert_file internal/memory/from_config.go     'phase 110c: memory.SnapshotFromConfig projection file'
assert_file internal/skills/from_config.go     'phase 110c: skills.SnapshotFromConfig projection file'
assert_file internal/planner/from_config.go    'phase 110c: planner.ConfigFromOperator/HintsFromConfig projection file'
assert_file internal/governance/from_config.go 'phase 110c: governance.ConfigFromOperator projection file'
assert_file internal/drivers/prod/prod.go      'phase 110c: internal/drivers/prod aggregator'

assert_grep_present 'func SnapshotFromConfig\(cfg config\.LLMConfig, art config\.ArtifactsConfig\) ConfigSnapshot' internal/llm/from_config.go 'phase 110c: llm.SnapshotFromConfig exported'
assert_grep_present 'func SnapshotFromConfig\(cfg config\.MemoryConfig\) ConfigSnapshot' internal/memory/from_config.go 'phase 110c: memory.SnapshotFromConfig exported'
assert_grep_present 'func SnapshotFromConfig\(cfg config\.SkillsConfig\) ConfigSnapshot' internal/skills/from_config.go 'phase 110c: skills.SnapshotFromConfig exported'
assert_grep_present 'func ConfigFromOperator\(cfg config\.PlannerConfig\) PlannerConfig' internal/planner/from_config.go 'phase 110c: planner.ConfigFromOperator exported'
assert_grep_present 'func HintsFromConfig\(cfg config\.PlannerPlanningHintsCfg\) \*PlanningHints' internal/planner/from_config.go 'phase 110c: planner.HintsFromConfig exported'
assert_grep_present 'func ConfigFromOperator\(in config\.GovernanceConfig\) Config' internal/governance/from_config.go 'phase 110c: governance.ConfigFromOperator exported'

# --- 2. The duplicate projection helpers are GONE -------------------------

assert_grep_absent 'func plannerConfigFromConfig'      cmd/harbor/cmd_dev.go 'phase 110c: cmd plannerConfigFromConfig deleted'
assert_grep_absent 'func governanceConfigFromConfig'   cmd/harbor/cmd_dev.go 'phase 110c: cmd governanceConfigFromConfig deleted'
assert_grep_absent 'func copyCustomProviders'          cmd/harbor/cmd_dev.go 'phase 110c: cmd copyCustomProviders deleted'
assert_grep_absent 'func copyNetworkDefaults'          cmd/harbor/cmd_dev.go 'phase 110c: cmd copyNetworkDefaults deleted'
assert_grep_absent 'func copyModelProfiles'            cmd/harbor/cmd_dev.go 'phase 110c: cmd copyModelProfiles deleted'
assert_grep_absent 'func plannerHintsFromConfig'       cmd/harbor/cmd_dev.go 'phase 110c: cmd plannerHintsFromConfig deleted'
assert_grep_absent 'func governanceConfigForDevstack'  harbortest/devstack/devstack.go 'phase 110c: devstack governanceConfigForDevstack deleted'
assert_grep_absent 'func plannerConfigFromConfig'      harbortest/devstack/devstack.go 'phase 110c: devstack plannerConfigFromConfig deleted'
assert_grep_absent 'func copyCustomProviders'          harbortest/devstack/devstack.go 'phase 110c: devstack copyCustomProviders deleted'
assert_grep_absent 'func copyModelProfiles'            harbortest/devstack/devstack.go 'phase 110c: devstack copyModelProfiles deleted'

# --- 3. The aggregator is the blank-import home ----------------------------

assert_grep_present 'internal/drivers/prod' cmd/harbor/main.go 'phase 110c: main.go imports the production driver aggregator'
assert_grep_present 'internal/drivers/prod' harbortest/devstack/devstack.go 'phase 110c: devstack imports the production driver aggregator'
assert_grep_absent 'internal/llm/corrections' cmd/harbor/main.go 'phase 110c: main.go per-driver blank imports collapsed (corrections via aggregator)'
assert_grep_absent 'internal/state/drivers'   cmd/harbor/main.go 'phase 110c: main.go per-driver blank imports collapsed (state via aggregator)'

# --- 4. Defaults / ValidateCore / single-source knob defaults --------------

assert_grep_present 'func Defaults\(\) \*Config' internal/config/loader.go 'phase 110c: config.Defaults() exported'
assert_grep_present 'func \(c \*Config\) ValidateCore\(\) error' internal/config/validate.go 'phase 110c: config.ValidateCore profile exists'
assert_grep_count 'DefaultSkillsContextMax = ' internal/config/config.go 1 'phase 110c: skills_context_max default has ONE source (config.DefaultSkillsContextMax)'
assert_grep_count 'DefaultSpawnDepthCap = '    internal/config/config.go 1 'phase 110c: spawn-depth default has ONE source (config.DefaultSpawnDepthCap)'
assert_grep_absent 'devRuntimeSkillsContextMaxDefault'      internal/runtime/serve/runloop.go 'phase 110c: cmd run-loop skills-cap literal deleted'
assert_grep_absent 'devStackRuntimeSkillsContextMaxDefault' harbortest/devstack/devstack.go 'phase 110c: devstack run-loop skills-cap literal deleted'

# --- 5. The projection + profile unit suites pass under -race --------------

if go test ./internal/config/ ./internal/planner/ ./internal/llm/ ./internal/memory/ ./internal/skills/ ./internal/governance/ -run 'FromConfig|FromOperator|Defaults|ValidateCore|SkillsContextMaxResolved|SpawnDepthCap' -race -count=1 -timeout 300s >/dev/null 2>&1; then
    ok 'phase 110c: projection + Defaults/ValidateCore unit suites pass under -race (field-parity gates included)'
else
    fail 'phase 110c: projection unit suites failed (run the go test line in scripts/smoke/phase-110c.sh for detail)'
fi

smoke_summary
