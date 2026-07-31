#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 07 smoke — StateStore foundation.
#
# internal/state ships no HTTP / Protocol surface, but `make preflight` does
# not run `go test`, so a skip-only script left this shipped phase with zero
# preflight coverage (AGENTS.md §4.2 item 5). Shape follows
# scripts/smoke/phase-05.sh: NAMED tests, so a rename fails loud.
#
# Two things are pinned, and both are load-bearing:
#   - the identity-validation table (§6 item 9 — identity is mandatory, the
#     store fails closed on an incomplete triple);
#   - the in-mem driver's entry point into the shared conformance suite
#     (RFC §4.3 — one suite, every driver passes it). Naming the entry point
#     rather than running the whole package is what makes its deletion a
#     preflight FAIL instead of a silently smaller run.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Per-phase log path — the unit-tests batch runs concurrently.
LOG="${TMPDIR:-/tmp}/harbor-smoke-phase-07-go-test.log"

if [ -d "internal/state" ]; then
    assert_go_tests_pass "${LOG}" './internal/state ./internal/state/drivers/inmem' \
        'phase 07: state identity validation + in-mem driver conformance suite pass' \
        TestValidateIdentity_Cases \
        TestInMem_Conformance
else
    skip 'phase 07: internal/state absent (package not yet implemented)'
fi

smoke_summary
