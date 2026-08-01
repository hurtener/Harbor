#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"; cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source scripts/smoke/common.sh
assert_file docs/plans/phase-226-agentcfg-transaction-integrity.md 'phase 226: corrective plan exists'
assert_grep_present '^## D-388 ' docs/decisions.md 'phase 226: transaction-integrity decision is recorded'

P226_TMP="$(mktemp -d "${TMPDIR:-/tmp}/harbor-phase-226.XXXXXX")"
trap 'rm -rf "${P226_TMP}"' EXIT

assert_go_tests_pass "${P226_TMP}/protocol.log" '-race -count=1 ./internal/runtime/agentcfg/protocol/' \
    'phase 226: all skill mutation doors refuse stale bases before effects and exactly compensate later failures' \
    TestSkillMutationDoors_StaleExpectationHasNoBodySideEffect \
    TestSkillMutationDoors_RevisionFailureCompensatesBody

assert_go_tests_pass "${P226_TMP}/statestore.log" '-race -count=1 ./internal/agentcfg/drivers/statestore/' \
    'phase 226: ambiguous revision saves clean only exact proven orphans and retain unknown answers' \
    TestSetRevision_RevisionSaveCommittedThenErrored_DeletesExactUnreferencedRecord \
    TestSetRevision_RevisionSaveAbsentOrUnreadable_DistinguishesCleanupOutcome \
    TestSetRevision_CompensationFailure_IsReportedNotSwallowed \
    TestSetRevision_ConcurrentConditionalWriters_ExactlyOneWins \
    TestConditionalWrite_ConcurrentReuse_NoCrossOwnerBleed
smoke_summary
