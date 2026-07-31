#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 01 smoke — identity foundation.
#
# Phase 01 is a pure Go package (internal/identity) with no HTTP / Protocol
# surface. That is NOT a reason to assert nothing: `make preflight` does not
# run `go test`, so a skip-only script left this shipped phase with zero
# preflight coverage (AGENTS.md §4.2 item 5 — "a SKIP that should be an OK is
# a bug").
#
# Shape borrowed from scripts/smoke/phase-05.sh: run the NAMED invariant tests
# rather than the whole package, so deleting or renaming one of them fails
# preflight loud instead of quietly shrinking what is guarded. The two named
# here are the §6 multi-isolation invariants: identity can never be widened,
# and an incomplete triple is refused.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Per-phase log path. The unit-tests batch runs up to CPU-count smokes
# concurrently (scripts/preflight.sh), so a shared path is a cross-talk bug.
LOG="${TMPDIR:-/tmp}/harbor-smoke-phase-01-go-test.log"

if [ -d "internal/identity" ]; then
    assert_go_tests_pass "${LOG}" './internal/identity/...' \
        'phase 01: identity isolation invariants hold' \
        TestWith_RefusesToWidenTheTenant \
        TestWithVerified_RejectsAnIncompleteTriple
else
    skip 'phase 01: internal/identity absent (package not yet implemented)'
fi

smoke_summary
