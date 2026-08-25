#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 254 smoke — HA-70 context-bound external execution grants and durable
# content-free attempt receipts. This is a non-vacuous structural gate only;
# it never contacts a provider, PostgreSQL, or a downstream deployment.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file \
    "docs/plans/phase-254-external-execution-grants.md" \
    "phase 254 plan exists"
assert_file \
    "internal/llm/external_grant.go" \
    "external grant contract exists"
assert_file \
    "internal/llm/grant/grant.go" \
    "signed grant verifier exists"
assert_file \
    "internal/llm/receipts/outbox.go" \
    "durable receipt outbox exists"
assert_grep_present \
    '## D-434' \
    "docs/decisions.md" \
    "D-434 decision exists"
assert_grep_present \
    '## HA-70 — context-bound external execution grants' \
    "docs/notes/downstream-asks.md" \
    "HA-70 register entry exists"
assert_grep_present \
    'External execution grant' \
    "docs/glossary.md" \
    "external grant glossary term exists"
assert_grep_present \
    'RegisterExternalGrantWrapper' \
    "internal/llm/registry.go" \
    "grant wrapper registration seam exists"
assert_grep_present \
    'VerifiedGrantContextFrom' \
    "internal/llm/drivers/bifrost/account.go" \
    "Bifrost uses verified grant context"
assert_grep_present \
    'ListKindBounded' \
    "internal/llm/receipts/outbox.go" \
    "outbox enumeration is bounded"
assert_grep_present \
    'TestBindingStore_ConcurrentTwoOrganizationsNoBleed' \
    "internal/llm/grant/grant_test.go" \
    "two-organization isolation test exists"
assert_grep_present \
    'TestOutbox_EnqueueReplayAckAndResponseLossIdempotency' \
    "internal/llm/receipts/outbox_test.go" \
    "receipt replay/idempotency test exists"
assert_grep_present \
    'EstimateRequestTokens' \
    "internal/llm/grant/wrapper.go" \
    "lease reservation reuses the canonical prompt estimator"
assert_grep_present \
    'TestWrap_ReservesCanonicalTotalCallBoundForPromptHeavySuccess' \
    "internal/llm/grant/wrapper_test.go" \
    "prompt-heavy total-call reservation is covered"
assert_grep_present \
    'TestStore_BindingOvershootAndCrashRecoveryAcrossDrivers' \
    "internal/llm/leases/manager_test.go" \
    "lease binding, overshoot, and crash recovery are covered"
assert_grep_present \
    'TestLeaseIntegrity_PostgresAcceptance' \
    "internal/state/drivers/postgres/lease_integrity_test.go" \
    "real-PostgreSQL lease integrity acceptance exists"
assert_grep_present \
    'ExternalGrantRequired' \
    "internal/llm/grant/wrapper.go" \
    "strict mode fails closed"
assert_grep_present \
    'WithAttemptCoordinates' \
    "internal/llm/retry/retry.go" \
    "retry attempts carry coordinates"
assert_grep_present \
    'WithAttemptCoordinates' \
    "internal/llm/output/downgrade.go" \
    "downgrade attempts carry coordinates"
assert_grep_present \
    'WithAttemptCoordinates' \
    "internal/governance/failover.go" \
    "failover attempts carry coordinates"
assert_grep_absent \
    'secret.*receipt\|receipt.*secret\|Secret.*AttemptUsageReceipt' \
    "internal/llm/external_grant.go" \
    "receipt contract does not carry secret bytes"

smoke_summary
