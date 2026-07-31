#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 02 smoke — configuration loader.
#
# The config package has no HTTP surface, but `make preflight` does not run
# `go test`, so a skip-only script left this shipped phase with zero preflight
# coverage (AGENTS.md §4.2 item 5). Shape follows scripts/smoke/phase-05.sh:
# one NAMED test, so its rename or deletion fails preflight loud.
#
# The defaults golden is the right thing to pin here — it is what makes a
# silently changed default (the config equivalent of a wire-shape drift)
# visible.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Per-phase log path — the unit-tests batch runs concurrently.
LOG="${TMPDIR:-/tmp}/harbor-smoke-phase-02-go-test.log"

if [ -d "internal/config" ]; then
    assert_go_tests_pass "${LOG}" './internal/config/...' \
        'phase 02: config defaults golden intact' \
        TestDefaults_BaselineGolden
else
    skip 'phase 02: internal/config absent (package not yet implemented)'
fi

smoke_summary
