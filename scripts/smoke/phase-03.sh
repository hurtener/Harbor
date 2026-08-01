#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 03 smoke — audit redactor.
#
# The audit package ships no HTTP / Protocol surface, but `make preflight`
# does not run `go test`, so a skip-only script left this shipped phase with
# zero preflight coverage (AGENTS.md §4.2 item 5). Shape follows
# scripts/smoke/phase-05.sh: one NAMED test, so a rename fails loud.
#
# The reflective JSON-tag redaction test is the load-bearing one: §7 item 6
# routes every audit payload through the redactor, so a regression here is a
# secrets-leak regression, not a style nit.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Per-phase log path — the unit-tests batch runs concurrently.
LOG="${TMPDIR:-/tmp}/harbor-smoke-phase-03-go-test.log"

if [ -d "internal/audit" ]; then
    assert_go_tests_pass "${LOG}" './internal/audit/...' \
        'phase 03: audit redactor still redacts struct fields by JSON tag' \
        TestReflective_RedactsStructFieldsByJSONTag
else
    skip 'phase 03: internal/audit absent (package not yet implemented)'
fi

smoke_summary
