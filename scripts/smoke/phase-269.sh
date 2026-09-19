#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 269 — explicitly enabled retained execution and dispatch checkpoints.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
source scripts/smoke/common.sh
assert_file docs/plans/phase-269-retained-session-context.md "retained context plan exists"
assert_grep_present '^## D-464 ' docs/decisions.md "retained window decision exists"
if go test -race -p 1 ./internal/runtime/runctx ./internal/runtime/assemble ./sdk/assemble ./internal/runtime/serve ./internal/config \
    -run 'TestRetainedRecovery_|TestRunOnce_RetainedRecovery|TestRetainedCheckpoint_|TestRunOnce_RetainedCheckpoint|TestRetainedContext_|TestRunOnce_RetainedContext|TestRunOnce_RetainedNative|TestRunOnce_RetainedDiscovery|TestRetainedServer_|TestSessionsRetainedContext_' -count=1; then
    ok "retained evidence, restore, erasure and served/embedded request regressions pass"
else
    fail "retained execution context regression failed"
fi
if go test -race -p 1 ./internal/runtime/runctx ./internal/runtime/assemble \
    -run 'TestRetainedJournal_|TestRunOnce_RetainedJournal' -count=1; then
    ok "required intent, settlement, restart and cleanup regressions pass"
else
    fail "required dispatch checkpoint regression failed"
fi
if go test -race ./internal/runtime/steering -run 'TestRunLoop_DispatchCheckpoint' -count=1; then
    ok "cancellation, observer and approval-bridge failures preserve returned outcomes"
else
    fail "dispatch failure lost returned execution evidence"
fi
if go test -race -p 1 ./internal/planner ./internal/planner/react ./internal/llm/summarizer ./internal/llm/drivers/bifrost \
    -run 'TestRetainedDiscovery_|TestHistoricalValidation_|TestHistoricalProjection_|TestTrajectoryHistorical_|TestPortableProviders_' -count=1; then
    ok "native historical projection, migration and provider portability regressions pass"
else
    fail "native historical projection regression failed"
fi
if go test -race ./internal/tools -run 'TestPlannerView_ResolveRechecks|TestPlannerView_ConcurrentScoped' -count=1; then
    ok "remembered tool names preserve current scope authority"
else
    fail "catalog name resolution bypassed current scopes"
fi
if go test -race -p 1 ./internal/runtime/serve ./internal/protocol/client \
    -run 'TestE2E_ServedContextRecovery_|TestClient_SessionsReconcileContext_' -count=1; then
    ok "served recovery, own-session authorization and typed client regressions pass"
else
    fail "served context reconciliation regression failed"
fi
if go test -race -p 1 ./internal/runtime/runctx ./internal/runtime/assemble ./internal/runtime/serve \
    -run 'TestRetainedResults_|TestRunOnce_RetainedResults_|TestRetainedServer_ResultReferences' -count=1; then
    ok "exact retained result retrieval and deletion fencing regressions pass"
else
    fail "retained result reference recovery failed"
fi
smoke_summary
