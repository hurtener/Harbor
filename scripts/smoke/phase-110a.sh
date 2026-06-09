#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 110a smoke — tool-executor promotion (D-194).
#
# Static guards: the executor + catalog-view files are GONE from
# cmd/harbor, the promoted package exists, no internal/ or harbortest/
# file cites the deleted cmd file, and the promotion's focused test
# slice passes under -race.
#
# Conventions (AGENTS.md §4.2): 404/405/501 → SKIP; ≥1 OK once shipped;
# use scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. The promoted package + exported contract files exist.
assert_file "internal/runtime/dispatch/dispatch.go" "promoted executor package exists"
assert_file "internal/planner/answer_envelope.go" "exported answer envelope exists"
assert_file "internal/tools/planner_view.go" "exported PlannerView exists"

# 2. The package-main copies are deleted.
if [ -e "cmd/harbor/cmd_dev_executor.go" ]; then
    fail "cmd/harbor/cmd_dev_executor.go still exists — the executor must live in internal/runtime/dispatch"
else
    ok "cmd/harbor/cmd_dev_executor.go deleted"
fi
if [ -e "cmd/harbor/cmd_dev_catalog_view.go" ]; then
    fail "cmd/harbor/cmd_dev_catalog_view.go still exists — the view must live in internal/tools"
else
    ok "cmd/harbor/cmd_dev_catalog_view.go deleted"
fi

# 3. No file under internal/ or harbortest/ cites the deleted cmd file
#    (the react prompt shape-contract is re-pointed at the dispatch
#    package's exported identifier).
if grep -rq "cmd_dev_executor" internal/ harbortest/ 2>/dev/null; then
    fail "internal/ or harbortest/ still references cmd_dev_executor.go"
else
    ok "no internal/ or harbortest/ reference to cmd_dev_executor.go"
fi
assert_grep_present "internal/runtime/dispatch" "internal/planner/react/prompt.go" \
    "react prompt shape-contract cites internal/runtime/dispatch"

# 4. No second executor implementation survives in devstack (§13).
if grep -q "devStackToolExecutor" harbortest/devstack/devstack.go 2>/dev/null; then
    fail "devstack degraded executor (devStackToolExecutor) still exists"
else
    ok "devstack degraded executor deleted"
fi

# 5. The promotion's focused test slice passes under -race.
if go test ./internal/runtime/dispatch/ ./internal/tools/ ./internal/planner/ \
    -run 'Executor|PlannerView|AnswerEnvelope' -race -count=1 >/dev/null 2>&1; then
    ok "dispatch/PlannerView/AnswerEnvelope test slice passes (-race)"
else
    fail "dispatch/PlannerView/AnswerEnvelope test slice failed (-race)"
fi

smoke_summary
