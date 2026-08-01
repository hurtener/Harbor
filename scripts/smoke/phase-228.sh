#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"; cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source scripts/smoke/common.sh
assert_file docs/plans/phase-228-prepared-mcp-activation.md 'phase 228: corrective plan exists'
assert_grep_present '^## D-390 ' docs/decisions.md 'phase 228: prepared-activation decision is recorded'

P228_TMP="$(mktemp -d "${TMPDIR:-/tmp}/harbor-phase-228.XXXXXX")"
trap 'rm -rf "${P228_TMP}"' EXIT

assert_go_tests_pass "${P228_TMP}/prepared-activation.log" '-race -count=1 ./internal/runtime/pauseresume/ ./internal/runtime/agentcfg/protocol/ ./internal/runtime/agentcfg/projection/ ./internal/tools/auth/ ./internal/tools/drivers/mcp/ ./internal/runtime/serve/ ./internal/config/' \
    'phase 228: prepared add, reversible publication, and run-start reconciliation execute as one release gate' \
    TestAddMCPConnection_PreparedRefusalNeverActivates \
    TestAddMCPConnection_PreparedUnknownPointerClosesWithoutPublication \
    TestAddMCPConnection_PreparedConfirmedLandingConvergesThenReturnsStoreError \
    TestAddMCPConnection_SameNameDifferentDescriptorNeverActivates \
    TestAddMCPConnection_ActivationFailureRollsBackDesiredState \
    TestAddMCPConnection_AmbiguousWriteLandingThenActivationFailureRollsBackDesiredState \
    TestAddMCPConnection_InlineOAuthRemainsUnpublishedThroughMCPPrepare \
    TestAddMCPConnection_AuthRequired_ParksOnPauseResume \
    TestAddMCPConnection_AuthContinuationSurvivesRestartAndActivates \
    TestAddMCPConnection_UsesProviderOwnedPauseWithoutDuplicate \
    TestAddMCPConnection_AuthRequiredWriteLandedThenErroredRetainsProducerPause \
    TestAddMCPConnection_ContinuationDescriptorDriftDoesNotPrepareOrPublish \
    TestAddMCPConnection_ContinuationCancellationClosesAndStaysPaused \
    TestContinuation_RestartRehydratesAndRunsExactWork \
    TestContinuation_HandlerOutsideCoordinatorLockAndFailureRetriable \
    TestContinuation_ConcurrentResumeInvokesHandlerOnce \
    TestContinuation_ConcurrentMixedDecisionWaitsForAcceptedWinner \
    TestContinuation_RejectSkipsHandler \
    TestBuildProviders_RestartCompletesDurablePKCEFlowAndUnifiedPause \
    TestCallbackHandler_RestartRoutesStateOnlyToOwningProvider \
    TestFlowStore_RestartLookup_SealsWholeEnvelopeAndRejectsCollision \
    TestFlowStore_ConcurrentReconstructedClaim_ExactlyOneWinner \
    TestFlowStore_CompletedMarker_SaveAckLossReconcilesAndSurvivesFinish \
    TestFlowStore_PutPrunesOnlyExpiredCompletedTombstonesForExactIdentity \
    TestProvider_CompleteFlow_FinishFailureRetryDoesNotReexchange \
    TestProvider_CompleteFlow_ConcurrentReplacementCannotErasePerFlowCompletion \
    TestCallbackHandler_CompletedTombstoneRoutesAfterFinishDeleteAckLoss \
    TestProvider_CompleteFlow_TombstoneRetryCleansPartialFinishPendingRecord \
    TestProvider_CompleteFlow_ExpiredCompletionTombstoneIsForgotten \
    TestCallbackHandler_ReplayWithinRetryHorizon_IdempotentSuccess \
    TestProvider_CompleteFlow_UnrelatedNewerTokenCannotCompleteFlow \
    TestProvider_CompleteFlow_TokenPutAndResumeFailureRetainsTerminalRetry \
    TestProvider_CompleteFlow_RefreshPreservesExactCleanupMarker \
    TestProvider_CompleteFlow_CompetingRejectCannotReportCompletion \
    TestProvider_DenyFlow_FinishFailureRetryConverges \
    TestCallbackHandler_CompletionErrorNeverLogsUntrustedSecret \
    TestCallbackHandler_UnknownDenialCannotCrossContainmentBoundary \
    TestTokenStore_EncryptionAtRest_CiphertextNotPlaintext \
    TestProvider_CompleteFlow_ExchangeFailureReleasesClaimAndKeepsPauseRetryable \
    TestProvider_CompleteFlow_TokenPutFailureRejectsPauseAndConsumesSpentCode \
    TestProvider_CompleteFlow_CancelledTokenPutStillRejectsAndCleansUp \
    TestProvider_PendingFlowPutCancellationRejectsAllocatedPause \
    TestPreparedAttachment_PrepareDoesNotPublishAndCloseIsIdempotent \
    TestPreparedAttachment_SameOwnerOldRegistrationLivesUntilActivation \
    TestPreparedAttachment_RegistryStagesBeforeCatalogDispatchLinearization \
    TestRegistrationSwap_PrivateReservationPreventsCommitInvalidation \
    TestPreparedAttachment_ActivatedToolsEnterSearchIndex \
    TestPreparedAttachment_PublicationRefusalLeavesExactPriorLiveState \
    TestPreparedAttachment_DisplacedCloseFailureWarnsAfterSuccessfulPublication \
    TestPreparationObservations_DoNotContaminatePriorAndTransferToExactStage \
    TestClosePreparedAfterFailure_JoinsCleanupError \
    TestMCPConnectionAttacher_PreparationChallengeIsTypedAuthRequired \
    TestMCPConnectionAttacher_Reattach_SameOwnerChangedDescriptorReplaces \
    TestReattach_CarriesEveryDeclaredDescriptorField \
    TestMCPConnectionAttacher_Reattach_AlreadyRegisteredUnderOwnerIsNoOp \
    TestMCPConnectionAttacher_Reattach_ConcurrentOwners \
    TestReconcileConnections_AttachPassHonoursCancellation \
    TestAddMCPConnection_HTTPURLStrictAndQueryPreserved \
    TestNormalizeMCPHTTPURL_StrictSharedBoundary \
    TestValidate_MCPServerURLUsesStrictSharedBoundary \
    TestConfigValidate_HTTPURLUsesStrictSharedBoundary
smoke_summary
