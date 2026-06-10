#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 111e smoke — trajectory compression consumer (D-202).
#
# Static guards: MaybeCompress has a non-test call site under
# internal/runtime/steering/ (the audit's regression grep); the
# production TrajectorySummariser exists and satisfies
# planner.Summariser; Budget.TokenBudget has its production writers
# (cmd + devstack run-loop drivers); the assembly constructs the
# runner from `planner.token_budget`; the knob is documented in
# examples/harbor.yaml + docs/CONFIG.md. Then the focused unit slice
# + the Phase111e integration E2E pass under -race.
#
# Conventions (AGENTS.md §4.2): 404/405/501 → SKIP; ≥1 OK once shipped;
# use scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. The §13 primitive-with-consumer closure: MaybeCompress has its
#    production call site in the steering RunLoop (non-test file).
assert_grep_present 'MaybeCompress' "internal/runtime/steering/runloop.go" \
    "RunLoop step loop calls CompressionRunner.MaybeCompress (the audit's regression grep)"
assert_grep_present 'Compression \*planner\.CompressionRunner' "internal/runtime/steering/runloop.go" \
    "RunSpec carries the Compression runner field"

# 2. The production summariser exists, satisfies planner.Summariser,
#    and is distinct from the memory Summarizer (the non-conflation
#    rule).
assert_file "internal/llm/summarizer/trajectory.go" "TrajectorySummariser file exists"
assert_grep_present 'func NewTrajectorySummariser' "internal/llm/summarizer/trajectory.go" \
    "NewTrajectorySummariser constructor exported"
assert_grep_present 'planner\.Summariser = \(\*TrajectorySummariser\)\(nil\)' "internal/llm/summarizer/trajectory.go" \
    "compile-time planner.Summariser assertion present"
assert_grep_present 'Two interfaces' "internal/llm/summarizer/summarizer.go" \
    "package godoc disambiguates the two summarizer interfaces"

# 3. Budget.TokenBudget gains its production writers: both run-loop
#    driver shells project the budget + the runner (D-094 both-sides).
assert_grep_present 'Budget: planner\.Budget{TokenBudget: d\.tokenBudget}' "cmd/harbor/cmd_dev_runloop.go" \
    "cmd run-loop driver projects planner.token_budget onto RunSpec.Base.Budget"
assert_grep_present 'Compression:      d\.compression' "cmd/harbor/cmd_dev_runloop.go" \
    "cmd run-loop driver wires RunSpec.Compression"
assert_grep_present 'Budget: planner\.Budget{TokenBudget: d\.tokenBudget}' "harbortest/devstack/devstack.go" \
    "devstack run-loop driver projects the budget (D-094 mirror)"
assert_grep_present 'Compression:      d\.compression' "harbortest/devstack/devstack.go" \
    "devstack run-loop driver wires RunSpec.Compression (D-094 mirror)"

# 4. The merged 110d assembly constructs the runner when the budget is
#    non-zero; cmd + devstack inherit as thin callers.
assert_grep_present 'NewTrajectorySummariser' "internal/runtime/assemble/assemble.go" \
    "assembly constructs the TrajectorySummariser"
assert_grep_present 'planner\.NewCompressionRunner' "internal/runtime/assemble/assemble.go" \
    "assembly constructs the CompressionRunner"

# 5. The config knob is documented everywhere §4.2 item 7 requires.
assert_grep_present 'token_budget' "examples/harbor.yaml" \
    "examples/harbor.yaml documents planner.token_budget"
assert_grep_present '### planner.token_budget' "docs/CONFIG.md" \
    "docs/CONFIG.md carries the planner.token_budget reference entry"
assert_grep_present 'TokenBudget' "internal/config/validate.go" \
    "planner.token_budget is validated"

# 6. The godoc-honesty reverts: the dormant-seam markers are gone.
assert_grep_absent 'CURRENTLY INERT' "internal/planner/planner.go" \
    "Budget.TokenBudget dormant marker removed (the godoc is true again)"
assert_grep_absent 'Production consumer pending' "internal/planner/compression.go" \
    "Summariser dormant marker removed (the godoc is true again)"

# 7. The focused unit slice + the long-trajectory E2E pass under -race.
if go test ./internal/llm/summarizer/ ./internal/runtime/steering/ \
    -run 'Trajectory|Compress' -race -count=1 >/dev/null 2>&1; then
    ok "summarizer + steering compression test slice passes (-race)"
else
    fail "summarizer + steering compression test slice failed (-race)"
fi
if go test ./test/integration/ -run 'Phase111e' -race -count=1 >/dev/null 2>&1; then
    ok "Phase111e long-trajectory compression E2E passes (-race)"
else
    fail "Phase111e long-trajectory compression E2E failed (-race)"
fi

smoke_summary
