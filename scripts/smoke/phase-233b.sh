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
assert_grep_present 'claimed -> revision_committed -> published -> removal_revision_committed -> catalog_unpublished -> teardown_receipted -> removed' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "complete pair-lifetime JTI graph is specified"
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
assert_grep_present 'redirects' "docs/plans/phase-233b-signed-oauth-mcp-capability-registration.md" "bearer redirects are fail-closed"
assert_grep_present 'CanonicalOAuthMCPURL' internal/runtime/agentcfg/protocol/register_signed_oauth_mcp_capability.go "registration derives its URL bytes and sink through the canonical helper"
assert_grep_absent 'AllowWireOAuthDescriptor|allowWireOAuthDescriptor' internal/runtime/agentcfg/protocol/register_signed_oauth_mcp_capability.go "D-401 registration does not consult the development-only wire OAuth descriptor opt-in"

P233B_TMP="$(mktemp -d "${TMPDIR:-/tmp}/harbor-phase-233b.XXXXXX")"
trap 'rm -rf "${P233B_TMP}"' EXIT

assert_go_tests_pass "${P233B_TMP}/go-test.log" '-race -count=1 ./internal/agentcfg ./internal/runtime/agentcfg/protocol' \
    'phase 233b: signed capability authority, recovery, removal, and fence regressions execute under race' \
    TestVerifySignedOAuthMCPAuthority_ExactBindingAndScopeCeiling \
    TestSignedOAuthMCPOperationStore_ClaimsTenantScopedReplayAndTransitions \
    TestSignedOAuthMCPActivationFenceStore_TerminalFenceYieldsToNextOperation \
    TestRegisterOAuthMCPCapability_DurableReplayResumesPublishedOperation \
    TestRegisterOAuthMCPCapability_CommittedRevisionThenError_RecoversExactCandidate \
    TestRegisterOAuthMCPCapability_ConcurrentReplaySharesOnePublication \
    TestRegisterOAuthMCPCapability_ConcurrentMixedIdentityN128 \
    TestRemoveOAuthMCPCapability_ContinuesPairLifetimeReceipt \
    TestSignedOAuthMCPReconciler_Restart_ReattachesOnlyExactPublishedPair \
    TestSignedOAuthMCPReconciler_SQLiteRestart_ReattachesPublishedPair \
    TestSignedOAuthMCPReconciler_RecoversRemovalAfterDetachFault \
    TestSignedOAuthMCPReconciler_ConcurrentReuseN128_CancellationDoesNotLeak

assert_go_tests_pass "${P233B_TMP}/security-repair.log" '-race -count=1 ./internal/agentcfg/drivers/statestore ./internal/tools/auth ./internal/protocol/types ./internal/protocol/transports/stream ./internal/runtime/serve' \
    'phase 233b: authenticated preparation, rollback, scope, and closed-wire regressions execute under race' \
    TestRegisterOAuthMCPCapability_ProductionPathAuthenticatesInitializeAndDiscovery \
    TestRollback_ActiveRevisionReadFailureAbortsBeforePointerMutation \
    TestBuildSignedCapability_RequestedScopeOutsideBootCeilingRejected \
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
