#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Cumulative memory durability and fail-closed validation. Named tests prevent
# an absent or renamed implementation from turning this into an empty pass.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Per-phase log path — the unit-tests batch runs concurrently.
LOG="${TMPDIR:-/tmp}/harbor-smoke-phase-24-go-test.log"

assert_go_tests_pass "${LOG}" './internal/memory/session' \
    'phase 24: cumulative rollover preserves state on failure and rejects corruption' \
    TestRetainedCumulative_FailedRolloverPreservesCommittedState \
    TestRetainedContext_ErasureAndCorruptionFailClosed

smoke_summary
