#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 126b smoke — the durable USER-scope prompt-layer PROJECTION. This
# phase is projection-only: it adds no store, verb, or route, so the smoke is
# static. It asserts the 3-segment composeUserLayer, the ConfigScopeUser
# durable read in ApplyPromptLayers, and that BOTH run-loop twins route
# prompt-layer projection through the single shared ApplyPromptLayers seam
# (the §17.6 twin-parity grep, pointed at the run-loop files). The run-start
# behaviour itself is covered by
# test/integration/phase126b_user_prompt_layer_test.go.
#
# Conventions (AGENTS.md §4.2): use scripts/smoke/common.sh helpers; FAIL = 0.

set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. The 3-segment composition (admin user, durable user, session user).
assert_grep_present 'func composeUserLayer\(adminUser, durableUser, sessionUser string\)' \
    internal/runtime/agentcfg/projection/projection.go \
    'phase 126b: composeUserLayer joins three ordered segments'

# 2. ApplyPromptLayers reads the durable USER-scope revision (126a's read).
assert_grep_present 'ConfigScopeUser' \
    internal/runtime/agentcfg/projection/projection.go \
    'phase 126b: ApplyPromptLayers reads the ConfigScopeUser durable layer'

# 3. The §17.6 twin-parity grep — BOTH run-loop drivers route prompt-layer
#    projection through the single shared ApplyPromptLayers seam.
assert_grep_present 'projection.ApplyPromptLayers' \
    cmd/harbor/cmd_dev_runloop.go \
    'phase 126b: prod run-loop driver routes through the shared ApplyPromptLayers seam'
assert_grep_present 'projection.ApplyPromptLayers' \
    harbortest/devstack/devstack.go \
    'phase 126b: devstack twin routes through the shared ApplyPromptLayers seam'

smoke_summary
