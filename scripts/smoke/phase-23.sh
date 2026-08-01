#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 23 smoke — MemoryStore foundation.
#
# internal/memory ships no HTTP / Protocol surface, but `make preflight` does
# not run `go test`, so a skip-only script left this shipped phase with zero
# preflight coverage (AGENTS.md §4.2 item 5). Shape follows
# scripts/smoke/phase-05.sh: a NAMED test, so a rename fails loud.
#
# The named test is the in-mem driver's entry point into the shared memory
# conformance suite (RFC §4.3 — one suite, every driver passes it). Naming
# the entry point rather than running the whole package is what makes its
# deletion a preflight FAIL instead of a silently smaller run.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Per-phase log path — the unit-tests batch runs concurrently.
LOG="${TMPDIR:-/tmp}/harbor-smoke-phase-23-go-test.log"

if [ -d "internal/memory" ]; then
    assert_go_tests_pass "${LOG}" './internal/memory/drivers/inmem' \
        'phase 23: in-mem MemoryStore driver passes the shared conformance suite' \
        TestInMem_ConformanceSuite
else
    skip 'phase 23: internal/memory absent (package not yet implemented)'
fi

smoke_summary
