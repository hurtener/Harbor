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
# Production bug fix — snapshot threads CustomProviders + NetworkDefaults
# + Corrections (Phase 83l / D-155). Phase 110c (D-196) re-homed the
# projection from the cmd-local copy* helpers onto the owning package:
# the ONE exported llm.SnapshotFromConfig maps all three D-155 fields
# (pinned by the reflection field-parity gate in
# internal/llm/from_config_test.go), and BOTH cmd_dev.go and the
# devstack consume it -- the D-094 mirror copy is gone by design.
# ----------------------------------------------------------------------------
assert_grep_present 'CustomProviders:\s*customProvidersFromConfig' "internal/llm/from_config.go" \
    "llm.SnapshotFromConfig projects CustomProviders onto the snapshot (D-155 via 110c)"
assert_grep_present 'NetworkDefaults:\s*networkDefaultsFromConfig' "internal/llm/from_config.go" \
    "llm.SnapshotFromConfig projects NetworkDefaults onto the snapshot (D-155 via 110c)"
assert_grep_present 'DisableCorrections:\s*disableCorrectionsFromConfig' "internal/llm/from_config.go" \
    "llm.SnapshotFromConfig projects llm.corrections onto the snapshot (D-155 via 110c)"
assert_grep_present 'llm\.SnapshotFromConfig' "cmd/harbor/devcompose.go" \
    "bootDevStack consumes the exported projection (110c)"

# Devstack routes through the ONE assembly fan-out (110d / D-197),
# which consumes the same exported projection -- parity by construction.
assert_grep_present 'llm\.SnapshotFromConfig' "internal/runtime/assemble/assemble.go" \
    "the assembly consumes the exported projection (110c via 110d)"

smoke_summary
