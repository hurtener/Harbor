#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 42 smoke — planner interface + Decision sum + RunContext.
#
# Phase 42 is a pure-code phase: the Planner interface, the Decision sum-type,
# RunContext, the views, the stub finish.Planner, and the conformance harness
# skeleton. None of that is reachable through the Protocol — but `make
# preflight` does not run `go test`, so a skip-only script left this shipped
# phase with zero preflight coverage (AGENTS.md §4.2 item 5). Shape follows
# scripts/smoke/phase-05.sh: a NAMED test, so a rename fails loud.
#
# The named test is the AnswerEnvelope golden: the planner's answer shape is
# consumed byte-for-byte downstream, so a silent field change is a wire break.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Per-phase log path — the unit-tests batch runs concurrently.
LOG="${TMPDIR:-/tmp}/harbor-smoke-phase-42-go-test.log"

if [ -d "internal/planner" ]; then
    assert_go_tests_pass "${LOG}" './internal/planner' \
        'phase 42: planner AnswerEnvelope golden is byte-stable' \
        TestAnswerEnvelope_GoldenJSON_Phase106ByteCompat
else
    skip 'phase 42: internal/planner absent (package not yet implemented)'
fi

smoke_summary
