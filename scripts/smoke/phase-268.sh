#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 268 — portable checkpoint replay, incrementally implemented.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
source scripts/smoke/common.sh
assert_file docs/plans/phase-268-portable-compaction.md "phase 268 plan exists"
assert_grep_present '^## D-462 ' docs/decisions.md "portable coverage decision exists"
if go test -race ./internal/planner ./internal/planner/react ./internal/runtime/steering \
    -run 'TestPortableCompaction_|TestRunLoop_Compression_InspectionDuringGeneration' -count=1; then
    ok "portable replay and inspection regressions pass"
else
    fail "portable replay or inspection regression failed"
fi
smoke_summary
