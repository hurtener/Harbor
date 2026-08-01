#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"; cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source scripts/smoke/common.sh
assert_file docs/plans/phase-230-scoped-state-audit-convergence.md 'phase 230: corrective plan exists'
assert_grep_present '^## D-392 ' docs/decisions.md 'phase 230: scoped-state decision is recorded'

P230_TMP="$(mktemp -d "${TMPDIR:-/tmp}/harbor-phase-230.XXXXXX")"
trap 'rm -rf "${P230_TMP}"' EXIT
P230_GOLOG="${P230_TMP}/go-test.log"

# Run the independent packages in one go invocation so the race builds execute
# concurrently, then assert the load-bearing subtests' PASS markers explicitly.
# `go test -run` splits patterns at `/`, so feeding a subtest name through the
# top-level helper would silently match nothing.
assert_go_tests_pass "${P230_GOLOG}" '-race -count=1 ./internal/state/drivers/inmem/ ./internal/state/drivers/sqlite/ ./internal/agentcfg/drivers/statestore/ ./internal/search/... ./internal/sessions/' \
    'phase 230: scoped state, widening audit, and stale-ledger convergence execute together' \
    TestInMem_Conformance \
    TestSQLite_Conformance_TempDirFile \
    TestSQLite_Conformance_InMemory \
    TestStateStore_UserScope_ListRevisionsFilter \
    TestStateStore_ConcurrentReuse \
    TestAuthorizeScope_CanonicalIndexesAuditBothWideningAxes \
    TestAuthorizeScope_NoWidenAndRefusalNeverEmit \
    TestAuthorizeScope_AuditFailureFailsClosed \
    TestAuthorizeScope_RedactionFailureNeverPublishes \
    TestAuthorizeScope_ConcurrentReuseKeepsAuditIdentityIsolated \
    TestEventsSearcher_WideningEmitsExactlyOnceViaReplay \
    TestQuery_PropagatesAuditFailureInsteadOfReturningPartialRows \
    TestCascadeEraser_StaleLedger_EmitsOldLifecycleBeforeDeletingCheckpoint \
    TestCascadeEraser_StaleLedger_DeleteFailureRetryDoesNotDuplicateOldRecord \
    TestCascadeEraser_StaleLedger_EmitFailureRetainsCheckpointAndLiveSession \
    TestCascadeEraser_Concurrent_DistinctSessions_NoCrossTalk
assert_grep_present 'PASS: TestInMem_Conformance/ListKindForIdentity_IsolatedAndFailClosed ' "${P230_GOLOG}" \
    'phase 230: in-memory StateStore filters identity and kind inside the driver'
assert_grep_present 'PASS: TestInMem_Conformance/Concurrent_SaveLoad_NoRace ' "${P230_GOLOG}" \
    'phase 230: in-memory StateStore retains the N=128 concurrent-reuse contract'

assert_grep_present 'PASS: TestSQLite_Conformance_TempDirFile/ListKindForIdentity_IsolatedAndFailClosed ' "${P230_GOLOG}" \
    'phase 230: file-backed SQLite filters identity and kind inside the driver'
assert_grep_present 'PASS: TestSQLite_Conformance_InMemory/ListKindForIdentity_IsolatedAndFailClosed ' "${P230_GOLOG}" \
    'phase 230: in-memory SQLite filters identity and kind inside the driver'
assert_grep_present 'PASS: TestSQLite_Conformance_TempDirFile/Concurrent_SaveLoad_NoRace ' "${P230_GOLOG}" \
    'phase 230: file-backed SQLite retains the N=128 concurrent-reuse contract'
assert_grep_present 'PASS: TestSQLite_Conformance_InMemory/Concurrent_SaveLoad_NoRace ' "${P230_GOLOG}" \
    'phase 230: in-memory SQLite retains the N=128 concurrent-reuse contract'

smoke_summary
