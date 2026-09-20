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
if go test -race ./internal/llm/summarizer ./internal/llm/drivers/bifrost \
    -run 'TestTrajectoryChronological_|TestTrajectoryBudget_|TestTrajectorySummariser_Payload_|TestFinishReason_|TestE2E_PortableCompaction_|TestRequestContext_' -count=1; then
    ok "bounded chronological summary and real Bifrost regressions pass"
else
    fail "chronological summary or Bifrost regression failed"
fi
if go test -race ./internal/llm -run 'TestRequestInputLimit|TestSafety_OutputReservation|TestContextPreparation_' -count=1; then
    ok "assembled input and output-reservation safety regressions pass"
else
    fail "request capacity regression failed"
fi
if go test -race ./internal/llm ./internal/llm/grant -run '^TestCompaction' -count=1; then
    ok "model-sized and granted maintenance regressions pass"
else
    fail "model-sized or granted maintenance regression failed"
fi
if go test -race -p 1 ./internal/planner -run 'TestCompressionDiagnostics_|TestCompressionErrorMessage_ContentFreeBounded' -count=1; then
    ok "compaction failure diagnostics preserve scope without source content"
else
    fail "compaction failure diagnostics regression failed"
fi
if go test -race -p 1 ./internal/llm/drivers/bifrost -run '^TestContextCache_' -count=1; then
    ok "Bifrost live prefixes stay stable; compaction and authority changes remain explicit"
else
    fail "Bifrost cache-prefix regression failed"
fi
smoke_summary
