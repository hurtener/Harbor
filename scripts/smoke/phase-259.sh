#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 259 smoke — bounded external-grant top-up successors.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file "docs/plans/phase-259-external-grant-topup-successor.md" "phase 259 plan exists"
assert_grep_present '^|259 | External-grant top-up successor validation' "docs/plans/README.md" "phase 259 canonical index row exists"
assert_grep_present '## D-439' "docs/decisions.md" "D-439 decision exists"
assert_grep_present '## HA-74' "docs/notes/downstream-asks.md" "HA-74 register entry exists"
assert_grep_present 'ValidateExternalGrantTopUpSuccessor' "internal/llm/external_grant.go" "canonical successor validator exists"
assert_grep_present 'ValidateExternalGrantTopUpSuccessor' "sdk/llm/llm.go" "successor validator is public"
assert_grep_present 'ValidateExternalGrantTopUpSuccessor' "internal/llm/grant/wrapper.go" "wrapper consumes validator"
assert_grep_present 'TestValidateExternalGrantTopUpSuccessor_RejectsEveryImmutableClaimDrift' "internal/llm/external_grant_topup_test.go" "immutable-field matrix exists"
assert_grep_present 'TestValidateExternalGrantTopUpSuccessor_ReplayAndConcurrentValidation' "internal/llm/external_grant_topup_test.go" "replay and concurrent reuse exist"
assert_grep_present 'FuzzValidateExternalGrantTopUpSuccessorImmutableStrings' "internal/llm/external_grant_topup_test.go" "successor fuzz target exists"
assert_grep_present 'TestWrap_TopUpRejectsEveryImmutableSuccessorDriftBeforeProviderCall' "internal/llm/grant/wrapper_test.go" "wrapper no-provider-call matrix exists"
assert_grep_present 'TestWrap_TopUpPreservesLegacyBlankRouteBeforeProviderCall' "internal/llm/grant/wrapper_test.go" "wrapper legacy blank-route proof exists"
assert_grep_present 'TestWrap_TopUpRejectsUntrustedOrInvalidRotatingSignatureBeforeProviderCall' "internal/llm/grant/wrapper_test.go" "wrapper rotating-signature refusal exists"
assert_grep_present 'Stock live top-up remains blocked' "docs/plans/phase-259-external-grant-topup-successor.md" "live top-up boundary remains honest"
assert_grep_present 'No new grant claim, Protocol method, wire type, or Protocol version' "docs/plans/phase-259-external-grant-topup-successor.md" "Protocol boundary stays unchanged"

smoke_summary
