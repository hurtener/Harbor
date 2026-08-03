#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# D-401's surface is deliberately unavailable without a boot trust anchor, but
# these are live implementation guards, not planning-only assertions.
assert_file "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "phase 233b plan exists"
assert_grep_present 'agent_config\.register_oauth_mcp_capability' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "canonical atomic capability method is specified"
assert_grep_present 'D-401' "RFC-001-Harbor.md" "RFC carries the D-401 production contract"
assert_grep_present 'production-safe, boot-authorized' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "production path requires boot authorization"
assert_grep_present 'explicit signed-capability production opt-in' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "broker opt-in is explicit"
assert_grep_present 'trust_anchor_name' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "JTI operation key includes the trust anchor"
assert_grep_present 'claimed -> revision_committed -> published -> removal_admitted -> removal_revision_committed -> catalog_unpublished -> teardown_receipted -> removed' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "complete pair-lifetime JTI graph is specified"
assert_grep_present 'pair-history lifetime despite registration-authority expiry' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "published record survives registration-authority expiry"
assert_grep_present 'anti-replay tombstone' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "removed pair cannot be recreated or replayed"
assert_grep_present 'cleanup/maintenance applies only to `claimed`, `revision_committed`,' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "expiry maintenance excludes published and removed records"
assert_grep_present 'SignedOAuthMCPConnectionDescriptor' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "registration uses a closed dedicated descriptor"
assert_grep_present 'removal_revision_committed' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "paired removal has durable recovery phases"
assert_grep_present 'outside the general `ProviderSet`' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "signed provider is pair-owned"
assert_grep_present 'catalog source swap is the sole data-plane dispatch' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "catalog is the only dispatch linearization"
assert_grep_present 'pending-activation/compensation fence' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "first-write security fence is durable"
assert_grep_present 'IDNA2008' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "canonical URL algorithm is pinned"
assert_grep_present 'RFC5952' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "IPv6 canonical form is pinned"
assert_grep_present 'foreign operation' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "activation fence rejects foreign authority mutators"
assert_grep_present 'opaque publisher epoch' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "durable publisher takeover is internal and fail-closed"
assert_grep_present 'redirects' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "bearer redirects are fail-closed"
assert_grep_present 'CanonicalOAuthMCPURL' internal/runtime/agentcfg/protocol/register_signed_oauth_mcp_capability.go "registration derives its URL bytes and sink through the canonical helper"
assert_grep_present 'ActivateUnder' internal/runtime/agentcfg/protocol/register_signed_oauth_mcp_capability.go "initial registration proves authority under the exact staged publication receipt"
assert_grep_present 'ActivateUnder' internal/runtime/agentcfg/protocol/signed_oauth_mcp_reconcile.go "restart reconcile proves authority under the exact staged publication receipt"
assert_grep_absent 'AllowWireOAuthDescriptor|allowWireOAuthDescriptor' internal/runtime/agentcfg/protocol/register_signed_oauth_mcp_capability.go "D-401 registration does not consult the development-only wire OAuth descriptor opt-in"

P233B_TMP="$(mktemp -d "${TMPDIR:-/tmp}/harbor-phase-233b.XXXXXX")"
trap 'rm -rf "${P233B_TMP}"' EXIT

assert_go_tests_pass "${P233B_TMP}/go-test.log" '-race -count=1 ./internal/agentcfg ./internal/runtime/agentcfg/protocol' \
    'phase 233b: signed capability authority, recovery, removal, and fence regressions execute under race' \
    TestVerifySignedOAuthMCPAuthority_ExactBindingAndScopeCeiling \
    TestSignedOAuthMCPOperationStore_ClaimsTenantScopedReplayAndTransitions \
    TestSignedOAuthMCPOperationStore_ExpiryAdmissionRenewalIsExactAndCASBound \
    TestSignedOAuthMCPOperationStore_SQLiteTwoHandleRenewalRaceHasOneWinner \
    TestSignedOAuthMCPActivationFenceStore_TerminalFenceYieldsToNextOperation \
    TestSignedOAuthMCPActivationFenceStore_ReopensOnlyExactRenewedGeneration \
    TestSignedOAuthMCPOperationStore_PublisherEpochCASAndRemovalFenceUse \
    TestRegisterOAuthMCPCapability_DurableReplayResumesPublishedOperation \
    TestRegisterOAuthMCPCapability_StableJTIRecoversClaimedBeforeFenceAndPreservesPrior \
    TestRegisterOAuthMCPCapability_StableJTIRecoversExpiredRevisionCommittedOnce \
    TestRegisterOAuthMCPCapability_ExpiredClaimedMatchingCandidateWithoutFenceFailsClosed \
    TestRegisterOAuthMCPCapability_SQLiteTwoHandleStableJTIExpiryRecovery \
    TestRegisterOAuthMCPCapability_CommittedRevisionThenError_RecoversExactCandidate \
    TestRegisterOAuthMCPCapability_PointerAndCompensationFailure_DoesNotPublishMatchingOrphan \
    TestRegisterOAuthMCPCapability_CrossSessionServiceCannotReplaceDuringRemoval \
    TestRegisterOAuthMCPCapability_CrossServiceRemovalAdmissionBlocksReplacement \
    TestRegisterOAuthMCPCapability_ConcurrentReplaySharesOnePublication \
    TestRegisterOAuthMCPCapability_ConcurrentMixedIdentityN128 \
    TestRemoveOAuthMCPCapability_ContinuesPairLifetimeReceipt \
    TestSignedOAuthMCPReconciler_Restart_ReattachesFrozenOwnerForLaterSubject \
    TestSignedOAuthMCPReconciler_SQLiteRestart_ReattachesPublishedPair \
    TestSignedOAuthMCPReconciler_ExpiredIncompleteNeutralizesCandidate \
    TestSignedOAuthMCPReconciler_ExpiredIncompleteRestoresBootLifecycle \
    TestSignedOAuthMCPReconciler_HistoricalPublishedPairCannotReattach \
    TestSignedOAuthMCPReconciler_RemovalDuringPrepareCannotRepublish \
    TestSignedOAuthMCPReconciler_TwoRegistriesRemovalCannotCrossPublicationFence \
    TestSignedOAuthMCPReconciler_RemovalAdmittedWithActivePairResumesForward \
    TestSignedOAuthMCPReconciler_RecoversRemovalAfterDetachFault \
    TestRemoveOAuthMCPCapability_DefinitiveCASFailureRollsBackAdmissionAndSurfacesFenceCleanup \
    TestRemoveOAuthMCPCapability_PairAbsentCheckpointFailureDirectRetryCompletes \
    TestRemoveOAuthMCPCapability_RemovalAdmittedCarriesNewerSamePairSiblings \
    TestSignedOAuthMCPReconciler_ConcurrentReuseN128_CancellationDoesNotLeak \
    TestSetOAuthProvider_FirstInstallCommitThenErrorRestoresUnsetAgent \
    TestSetOAuthProvider_BootLifecycleCommitThenErrorRestoresExactPrior

assert_go_tests_pass "${P233B_TMP}/security-repair.log" '-race -count=1 ./internal/agentcfg/drivers/statestore ./internal/tasks ./internal/tasks/drivers/durable ./internal/runtime/dispatch ./internal/tools/auth ./internal/tools/auth/drivers/tokenexchange ./internal/tools/drivers/mcp ./internal/protocol ./internal/protocol/types ./internal/protocol/transports/stream ./internal/runtime/serve' \
    'phase 233b: authenticated preparation, selective discovery errors, rollback, scope, and closed-wire regressions execute under race' \
    TestRegisterOAuthMCPCapability_ProductionPathAuthenticatesInitializeAndDiscovery \
    TestBearerInjectingTransport_StaleSignedPublisherNeverReachesNetwork \
    TestMCPConnectionAttacher_SignedPrivateOptionalDiscoveryErrors \
    TestIsJSONRPCMethodNotFound_OnlyCanonicalTypedError \
    TestDetachSourceExpected_CloseFailureIsRetryableAndNeverAbsentSuccess \
    TestRegistry_DeregisterExact_CloseFailureRetainsExactRetryReceiptAndBlocksReplacement \
    TestRegistry_DeregisterExact_PersistentCloseFailureNeverBecomesAbsentSuccess \
    TestRegistry_DeregisterExact_StagedCloseFailureRetainsSameHandleAndBlocksPublish \
    TestRegistry_ExactRemovalFence_CancelCloseFailureRetainsReservationForRetry \
    TestRegistry_ExactStagedPublishVsRemoval_ConcurrentReuseN128 \
    TestPreparedAttachment_AuthorityLostBeforeReservationNeverPublishes \
    TestPreparedAttachment_ExactRemovalAfterReservationInvalidatesPublication \
    TestPreparedAttachment_RegistryStagesBeforeCatalogDispatchLinearization \
    TestPreparedAttachment_PostPublicationAdmissionErrorRetainsLiveGeneration \
    TestProvider_CloseRetriesPairOwnedOAuthUntilPositiveReceipt \
    TestProvider_CloseOwnedTransport_ClosesIdleConnectionAndIsIdempotent \
    TestProvider_CloseOwnedTransport_CancelsAndJoinsActiveExchange \
    TestProvider_CloseCancelsConsentCoordinatorInvocation \
    TestProvider_CloseDeadlineLeavesRetryableClosingWhenCoordinatorIgnoresCancellation \
    TestProvider_ConcurrentTokenClose_N128NoBleedOrLeak \
    TestProvider_CloseSuppliedTransport_DoesNotCrossProviders \
    TestRollback_ActiveRevisionReadFailureAbortsBeforePointerMutation \
    TestDeactivateIfActive_RestoresAbsentAndSurvivesRestart \
    TestDeactivateIfActive_TerminalOrCorruptFailsClosed \
    TestDeactivateIfActive_CASRaceNeverDeletesReplacement \
    TestBuildSignedCapability_RequestedScopeOutsideBootCeilingRejected \
    TestSignedCapability_RegistrarActorAndInvokerSubjectAreSeparated \
    TestAgentReachAdmission_SealedCaptureRestoreAndTamperDenial \
    TestAgentReachAdmission_IdenticalSubjectResealIsIdempotent \
    TestAgentReachAdmission_ConcurrentCaptureNoBleed \
    TestDurable_RestartSurvival_TasksGroupsPatches \
    TestExecutor_SpawnTask_InheritsExactAgentReachAdmission \
    TestPerTaskRunLoopDriver_ForgedSDKAgentIDHasNoCredentialAdmission \
    TestPerTaskRunLoopDriver_StampsEffectiveAgentConfigAdmission \
    TestDispatchStart_NamedAgent_TwoCheckRule \
    TestRegisterOAuthMCPCapabilityWire_FieldSetsAreClosed \
    TestRegisterOAuthMCPCapabilityWire_HasNoCredentialOrSinkConfigurationField \
    TestAgentConfigHandler_RegisterOAuthMCPCapabilityRejectsForbiddenFieldsWithoutSideEffects

if [ -n "${HARBOR_PG_DSN:-}" ]; then
    assert_go_tests_pass "${P233B_TMP}/postgres-reconcile.log" '-race -count=1 ./internal/runtime/agentcfg/protocol' \
        'phase 233b: configured real Postgres two-runtime reconciler executes' \
        TestRegisterOAuthMCPCapability_PostgresTwoIndependentRuntimes
else
    skip "phase 233b: HARBOR_PG_DSN is not configured; real Postgres two-runtime reconciler is CI-gated"
fi

smoke_summary
