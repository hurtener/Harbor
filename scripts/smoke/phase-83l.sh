#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83l — real-bifrost integration tests + production-bug fix
# (CustomProviders / NetworkDefaults / Corrections projection). D-155.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Integration test file + helpers.
# ----------------------------------------------------------------------------
assert_file "test/integration/phase83l_real_bifrost_test.go" \
    "real-bifrost integration test file ships"
assert_grep_present 'type scriptedLLMServer struct' \
    "test/integration/phase83l_real_bifrost_test.go" \
    "scriptedLLMServer fake-LLM helper declared"
assert_grep_present 'func TestE2E_RealBifrost_PlannerExecutorTrajectory_HappyPath' \
    "test/integration/phase83l_real_bifrost_test.go" \
    "happy-path test declared"
assert_grep_present 'func TestE2E_RealBifrost_ToolValidationFailure_PlannerReplans' \
    "test/integration/phase83l_real_bifrost_test.go" \
    "tool-failure replan test declared"

# ----------------------------------------------------------------------------
# Production bug fix — snapshot now threads CustomProviders + NetworkDefaults
# + Corrections (Phase 83l / D-155).
# ----------------------------------------------------------------------------
assert_grep_present 'func copyCustomProviders' "cmd/harbor/cmd_dev.go" \
    "cmd_dev.go projects CustomProviders onto the LLM snapshot"
assert_grep_present 'func copyNetworkDefaults' "cmd/harbor/cmd_dev.go" \
    "cmd_dev.go projects NetworkDefaults onto the LLM snapshot"
assert_grep_present 'func disableCorrectionsFromConfig' "cmd/harbor/cmd_dev.go" \
    "cmd_dev.go projects llm.corrections onto the snapshot"
assert_grep_present 'CustomProviders:\s*copyCustomProviders' "cmd/harbor/cmd_dev.go" \
    "bootDevStack wires CustomProviders into the snapshot"

# Devstack mirror per D-094.
assert_grep_present 'func copyCustomProviders' "harbortest/devstack/devstack.go" \
    "devstack mirrors copyCustomProviders (D-094)"
assert_grep_present 'CustomProviders:\s*copyCustomProviders' \
    "harbortest/devstack/devstack.go" \
    "devstack snapshot wires CustomProviders (D-094)"

smoke_summary
