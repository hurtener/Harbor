#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 126c smoke — the USER-scope tool-policy run-start projection. This
# phase is projection-only: it adds no store, verb, route, scope, or wire
# type, so the smoke is static. It asserts that the run-start tool-exposure
# projection (ActivePlannerCatalogView) reads the durable user-scope disable
# set (ConfigScopeUser) and unions it into the grow-only exclusion set
# (admin ∪ user ∪ session). The run-start behaviour itself, persistence across
# sessions, and cross-user isolation are covered by
# test/integration/agentcfg_user_policy_test.go; the connection-add-stays-
# admin-only boundary 126c does NOT widen is covered by 126a's live smoke.
#
# Conventions (AGENTS.md §4.2): scripts/smoke/common.sh helpers; FAIL = 0.

set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. The projection reads the durable user-scope disable set.
assert_grep_present 'agentcfg.ConfigScopeUser' \
    internal/runtime/agentcfg/projection/projection.go \
    'phase 126c: ActivePlannerCatalogView reads the ConfigScopeUser disable set'

# 2. The three disable sets fold into one grow-only exclusion set —
#    admin ∪ user ∪ session, order-independent (the nested unionSorted).
assert_grep_present 'unionSorted\(unionSorted\(' \
    internal/runtime/agentcfg/projection/projection.go \
    'phase 126c: admin ∪ user ∪ session — order-independent, grow-only'

smoke_summary
