#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 257 smoke — strict public canonical attempt-receipt parser.
# Static-only: it never contacts a provider, PostgreSQL, or a downstream host.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file "docs/plans/phase-257-canonical-attempt-receipt-parser.md" "phase 257 plan exists"
assert_grep_present '## D-437' "docs/decisions.md" "D-437 decision exists"
assert_grep_present '## HA-72' "docs/notes/downstream-asks.md" "HA-72 register entry exists"
assert_grep_present 'func UnmarshalCanonicalAttemptUsageReceipt\(data \[\]byte\) \(AttemptUsageReceipt, error\)' "internal/llm/external_grant.go" "strict internal parser exists"
assert_grep_present 'UnmarshalCanonicalAttemptUsageReceipt = internal\.UnmarshalCanonicalAttemptUsageReceipt' "sdk/llm/llm.go" "public parser alias exists"
assert_grep_present 'attemptUsageReceiptFromCanonicalWire' "internal/llm/external_grant.go" "canonical wire reverse projection exists"
assert_grep_present 'TestUnmarshalCanonicalAttemptUsageReceipt_RejectsNonCanonicalDocuments' "internal/llm/external_grant_receipt_parse_test.go" "noncanonical adversarial test exists"
assert_grep_present 'TestUnmarshalCanonicalAttemptUsageReceipt_RejectsMalformedReceiptFacts' "internal/llm/external_grant_receipt_parse_test.go" "semantic and hash test exists"
assert_grep_present 'TestUnmarshalCanonicalAttemptUsageReceipt_RoundTripsLegacyBlankRouteMode' "internal/llm/external_grant_receipt_parse_test.go" "legacy canonical hash round-trip test exists"
assert_grep_present 'FuzzUnmarshalCanonicalAttemptUsageReceipt_ExactOrRejected' "internal/llm/external_grant_receipt_parse_test.go" "canonical parser fuzz target exists"
assert_grep_present 'llm\.UnmarshalCanonicalAttemptUsageReceipt' "sdk/assemble/external_grant_sdk_test.go" "external-package parser consumer exists"
assert_grep_present 'legacyParsed' "sdk/assemble/external_grant_sdk_test.go" "external-package legacy parser consumer exists"
assert_grep_present 'adds no coordinator transport' "docs/plans/phase-257-canonical-attempt-receipt-parser.md" "parser preserves transport boundary"

smoke_summary
