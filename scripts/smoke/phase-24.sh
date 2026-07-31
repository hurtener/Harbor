#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 24 smoke — memory strategies (truncation, rolling_summary).
#
# internal/memory/strategy ships no HTTP / Protocol surface, but `make
# preflight` does not run `go test`, so a skip-only script left this shipped
# phase with zero preflight coverage (AGENTS.md §4.2 item 5). Shape follows
# scripts/smoke/phase-05.sh: NAMED tests, so a rename fails loud.
#
# Both named tests are on the fail-loudly seam (§5): a strategy must restore
# its snapshot faithfully, and must REJECT an invalid one rather than degrade
# to an empty state.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Per-phase log path — the unit-tests batch runs concurrently.
LOG="${TMPDIR:-/tmp}/harbor-smoke-phase-24-go-test.log"

if [ -d "internal/memory/strategy" ]; then
    assert_go_tests_pass "${LOG}" './internal/memory/strategy' \
        'phase 24: memory strategy snapshot restore + invalid-snapshot refusal hold' \
        TestRollingSummary_Restore \
        TestNone_RejectsInvalidSnapshot
else
    skip 'phase 24: internal/memory/strategy absent (package not yet implemented)'
fi

smoke_summary
