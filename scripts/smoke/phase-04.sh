#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 04 smoke — slog Logger + standard attribute set.
#
# internal/telemetry ships no HTTP / Protocol surface, but `make preflight`
# does not run `go test`, so a skip-only script left this shipped phase with
# zero preflight coverage (AGENTS.md §4.2 item 5). Shape follows
# scripts/smoke/phase-05.sh: one NAMED test, so a rename fails loud.
#
# The named test is a fail-LOUDLY assertion (§5 "Fail loudly"): constructing a
# tracer with no service name must error rather than silently produce an
# unattributable span stream.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Per-phase log path — the unit-tests batch runs concurrently.
LOG="${TMPDIR:-/tmp}/harbor-smoke-phase-04-go-test.log"

if [ -d "internal/telemetry" ]; then
    assert_go_tests_pass "${LOG}" './internal/telemetry' \
        'phase 04: telemetry fails loudly on an empty service name' \
        TestNewTracer_EmptyServiceName_FailsLoudly
else
    skip 'phase 04: internal/telemetry absent (package not yet implemented)'
fi

smoke_summary
