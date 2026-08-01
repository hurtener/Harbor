#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 231 — deterministic reliability and stale-issue proof.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source scripts/smoke/common.sh
assert_file docs/plans/phase-231-deterministic-reliability-closure.md 'phase 231: corrective plan exists'
assert_grep_present '^## D-393 ' docs/decisions.md 'phase 231: deterministic-reliability decision is recorded'

P231_TMP="$(mktemp -d "${TMPDIR:-/tmp}/harbor-phase-231.XXXXXX")"
trap 'rm -rf "${P231_TMP}"' EXIT
P231_GOLOG="${P231_TMP}/go-test.log"

assert_go_tests_pass "${P231_GOLOG}" '-race -count=1 ./internal/tools/auth/ ./internal/tools/drivers/inproc/ ./internal/protocol/client/ ./internal/runtime/dispatch/ ./internal/tui/app/ ./test/integration/' \
    'phase 231: deterministic reliability regressions execute together under race' \
    TestProvider_ConcurrentReuse_RefreshSingleFlight \
    TestInProc_Conformance \
    TestClient_ConcurrentReuse_SessionIsolationCancellationAndLeak \
    TestExecutor_ParallelCancel_NoCrossTalk \
    TestProviderSet_Uninstall_OwnerScoped_MatchingDrops \
    TestProviderSet_Uninstall_OwnerScoped_FailsBoundCallsLoud \
    TestRuntimeModel_ChordCommandCommitsBeforeTheNextQueuedKey \
    TestRuntimeModel_FailedFollowUpRetryAndDiscardResumeOrder \
    TestRuntimeModel_StaleSameScopeInspectionPreservesNewActionModal \
    TestE2E_Phase111b_FullOAuthChoreography \
    TestE2E_NotificationsTopic_GroupCancelledMirror \
    TestE2E_TUIFunctionKeys_KittyCSIU \
    TestE2E_TUIConversationPTY_KeyDrivenAuthenticatedWorkflow
smoke_summary
