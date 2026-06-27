#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 134 smoke — runnable Example_ functions across the sdk facade
# (RFC §3.6). (docs/plans/phase-134-sdk-examples.md)
#
# Static + unit-test assertions:
#   1. Each sdk facade surface ships an example file declaring its
#      Example function(s): assemble (golden RunOnce + WithStream),
#      config, planner, steering.
#   2. The example BODIES reach into NO internal/ package. The driver
#      aggregator rides its sdk facade (sdk/drivers/prod); the ONE
#      internal/ reference is the dev-only mock LLM (no sdk facade by
#      D-089), only in sdk/assemble. The other three example files carry
#      zero internal/ references.
#   3. The examples compile and run, and the // Output: markers match:
#      `go test ./sdk/... -run Example` is green under -race.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# --- 1. Each facade surface ships its example file + Example funcs -----------

assert_file sdk/assemble/example_test.go 'phase 134: sdk/assemble example file'
assert_grep_present 'func Example\(' \
    sdk/assemble/example_test.go \
    'phase 134: sdk/assemble ships the golden RunOnce Example'
assert_grep_present 'func Example_streaming\(' \
    sdk/assemble/example_test.go \
    'phase 134: sdk/assemble ships the WithStream streaming Example'
assert_grep_present 'assemble\.WithStream\(' \
    sdk/assemble/example_test.go \
    'phase 134: the streaming Example exercises WithStream'

assert_file sdk/config/example_test.go 'phase 134: sdk/config example file'
assert_grep_present 'func Example\(' \
    sdk/config/example_test.go \
    'phase 134: sdk/config ships an Example'

assert_file sdk/planner/example_test.go 'phase 134: sdk/planner example file'
assert_grep_present 'func Example\(' \
    sdk/planner/example_test.go \
    'phase 134: sdk/planner ships an Example'

assert_file sdk/steering/example_test.go 'phase 134: sdk/steering example file'
assert_grep_present 'func Example\(' \
    sdk/steering/example_test.go \
    'phase 134: sdk/steering ships an Example'

# --- 2. Example bodies are internal-free (dev-only mock is the exception) ----

# The dev-only mock LLM (no sdk facade by D-089) is the ONE allowed
# internal/ reference, and only in sdk/assemble (the runnable RunOnce
# path). The production driver aggregator rides its sdk facade
# (sdk/drivers/prod), so it is NOT an internal/ reference.
assert_grep_present '_ "github.com/hurtener/Harbor/internal/llm/mock"' \
    sdk/assemble/example_test.go \
    'phase 134: the runnable examples blank-import the dev-only mock LLM (D-089)'
assert_grep_present '_ "github.com/hurtener/Harbor/sdk/drivers/prod"' \
    sdk/assemble/example_test.go \
    'phase 134: the runnable examples seat drivers through the sdk/drivers/prod facade'

# The non-assemble example files reach into NO internal/ package at all.
for f in sdk/config/example_test.go sdk/planner/example_test.go sdk/steering/example_test.go; do
    assert_grep_absent 'github.com/hurtener/Harbor/internal/' \
        "${f}" \
        "phase 134: ${f} imports no internal/ package"
done

# --- 3. The examples compile + run; // Output: markers match -----------------

if go test ./sdk/... -run Example -race -count=1 -timeout 300s >/dev/null 2>&1; then
    ok 'phase 134: go test ./sdk/... -run Example is green under -race'
else
    fail 'phase 134: sdk Example functions failed (run: go test ./sdk/... -run Example -race)'
fi

smoke_summary
