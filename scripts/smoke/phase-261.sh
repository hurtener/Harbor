#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 261 smoke — stock authenticated external-grant renewal.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file "docs/plans/phase-261-stock-external-grant-renewal.md" "phase 261 plan exists"
assert_grep_present '^|261 | Stock authenticated external-grant renewal' "docs/plans/README.md" "phase 261 canonical index row exists"
assert_grep_present '## D-441' "docs/decisions.md" "D-441 decision exists"
assert_grep_present 'top_up_url' "docs/CONFIG.md" "stock top-up config is documented"
assert_grep_present 'MaxRequestBytes.*128' "sdk/llm/topup/topup.go" "public request bound remains explicit"
assert_grep_present 'func TopUpIdempotencyKey' "sdk/llm/topup/topup.go" "public idempotency helper exists"
assert_grep_present 'func RenewalIdempotencyKey' "sdk/llm/topup/topup.go" "reason-aware public idempotency helper exists"
assert_grep_present 'func ParseCanonicalRequest' "sdk/llm/topup/topup.go" "public strict request parser exists"
assert_grep_present 'ErrIdempotencyMismatch' "sdk/llm/topup/topup.go" "public header/body mismatch is neutral"
assert_grep_present 'VerifyRenewalPredecessor' "internal/llm/grant/grant.go" "reference predecessor verifier exists"
assert_grep_present 'ErrExternalGrantExpired' "internal/llm/external_grant.go" "strict expiry is typed"
assert_grep_present 'ApplySuccessor' "internal/llm/leases/manager.go" "durable successor applier exists"
assert_grep_present 'ResolveSuccessor' "internal/llm/leases/manager.go" "durable current successor resolver exists"
assert_grep_present 'stock_authenticated_http' "internal/runtime/serve/serve.go" "stock readiness truth exists"
assert_grep_present 'TestPublicContract_CanonicalRoundTripAndExpiryOnlyCapacityPreservation' "sdk/llm/topup/topup_test.go" "external consumer covers no-widen expiry"
assert_grep_present 'TestClient_TopUp_AuthenticatedCanonicalResponseLossAndConcurrentReuse' "internal/llm/receipts/httptransport/client_test.go" "stock response-loss/concurrency test exists"
assert_grep_present 'TestStore_ApplySuccessorConformanceAndConcurrentReplay' "internal/llm/leases/manager_test.go" "durable driver conformance exists"
assert_grep_present 'TestLeaseIntegrity_PostgresAcceptance' "internal/state/drivers/postgres/lease_integrity_test.go" "real PostgreSQL acceptance owns successor path"
assert_grep_present 'TestWrap_ExpiredRevokedCredentialMakesZeroRenewalCalls' "internal/llm/grant/wrapper_test.go" "revoked predecessor makes zero renewal calls"
assert_grep_present 'TestWrap_ImmutableRootResolvesCurrentSuccessorAfterSQLiteRestart' "internal/llm/grant/wrapper_test.go" "immutable root survives restart into actual completion"
assert_grep_present 'TestRunLoop_ImmutableBaseGrantReusesAndRenewsDurableSuccessors' "internal/runtime/serve/external_grant_agent_binding_test.go" "real runloop preserves immutable base grant"
assert_grep_present 'TestStore_MixedRequestedUnitSuccessorsChooseOneAndLoserReloads' "internal/llm/leases/manager_test.go" "mixed requested-unit successors converge"

smoke_summary
