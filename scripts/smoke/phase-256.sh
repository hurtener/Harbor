#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 256 smoke — public external-grant SDK and runtime-default route.
# Static-only: it never contacts a provider, PostgreSQL, or a downstream host.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file "docs/plans/phase-256-external-grant-sdk-runtime-default.md" "phase 256 plan exists"
assert_grep_present '## D-436' "docs/decisions.md" "D-436 decision exists"
assert_grep_present 'HA-70 v1.30.1 compatibility extension' "docs/notes/downstream-asks.md" "HA-70 hotfix register exists"
assert_file "sdk/llm/grant/grant.go" "public grant package exists"
assert_file "sdk/llm/receipts/receipts.go" "public receipt package exists"
assert_file "sdk/assemble/external_grant_sdk_test.go" "external-package consumer test exists"
assert_grep_present 'ExternalGrantRouteRuntimeDefault' "internal/llm/external_grant.go" "runtime-default route is explicit"
assert_grep_present 'ValidateAttemptUsageReceiptAgainstGrant' "internal/llm/external_grant.go" "receipt derivation validator exists"
assert_grep_present 'TestAssemble_ExternalGrantRuntimeDefaultUsesNativeProviderAndReceipts' "internal/runtime/assemble/assemble_test.go" "runtime-default E2E exists"
assert_grep_present 'TestAssemble_ExternalGrantRuntimeDefaultReachesBifrostCustomProvider' "internal/runtime/assemble/assemble_test.go" "runtime-default reaches Bifrost"
assert_grep_present 'TestAccount_GetKeysForProvider_RuntimeDefaultGrantUsesConfiguredNativeKey' "internal/llm/drivers/bifrost/account_test.go" "runtime-default retains native LiveKey"
assert_grep_present 'TestNewAccount_RequiredExternalGrantDoesNotResolveBootSecret' "internal/llm/drivers/bifrost/account_test.go" "legacy blank required mode boots without local key"
assert_grep_present 'TestAccount_GetKeysForProvider_BlankRequiredCoordinatorBoundUsesResolverWithoutBootKey' "internal/llm/drivers/bifrost/account_test.go" "legacy blank coordinator-bound call uses resolver"
assert_grep_present 'TestRuntimeDefaultGrantShapeAndReceiptDerivation' "internal/llm/grant/grant_test.go" "route/forgery tests exist"
assert_grep_present 'does not add a Protocol method' "docs/plans/phase-256-external-grant-sdk-runtime-default.md" "hotfix preserves Protocol boundary"

smoke_summary
