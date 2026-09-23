#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 123 smoke — memory context-budget enforcement + D-026
# conversation-scope refinement.
#
# rolling_summary now enforces its `budget_tokens` (recent-turn set +
# recursive/chunked compaction that preserves prior summaries), the
# recent-turn set is operator-configurable (memory.recent_turns), and
# the D-026 byte heavy-content check is scoped to offloadable content
# (RoleTool text + binary DataURL parts) — conversation text is
# governed by the token-window guard instead. These assertions are
# unit-test-backed (the live "session no longer poisons" behaviour is
# the Console live-check, not scriptable here).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. Actual assembled requests stay within budget across cumulative rollovers.
assert_go_tests_pass "${TMPDIR:-/tmp}/harbor-smoke-phase-123-go-test.log" \
    './test/integration' 'phase 123: cumulative request budgets and failed-summary preservation' \
    TestE2E_Phase123_MemoryLLMBudget_StaysRunnable \
    TestE2E_Phase123_MemoryLLMBudget_ZeroUsesModelCapacity \
    TestE2E_Phase123_MemoryLLMBudget_OversizedSummaryPreservesSources

# 2. D-026 byte check is role-scoped (tool/binary only; conversation exempt).
if go test -race -count=1 -timeout 120s -run 'FindContextLeak_RoleScope' ./internal/llm/... >/dev/null 2>&1; then
    ok "phase 123: findContextLeak role-scoping"
else
    fail "phase 123: findContextLeak role-scoping failed (run \`go test -race -run FindContextLeak_RoleScope ./internal/llm/...\`)"
fi

# 3. recent_turns config knob is documented in the example config.
assert_grep_present 'recent_turns' "examples/harbor.yaml" \
    "phase 123: memory.recent_turns documented in example config"

smoke_summary
