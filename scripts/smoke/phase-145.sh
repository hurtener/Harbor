#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 145 smoke — governance attempt-level cost accounting: the in-band
# attempt-cost tap (D-275). docs/plans/phase-145-governance-attempt-accounting.md
#
# Planned assertions (flip from skeleton as the surface lands):
#   1. go test -race for the attempt-cost tap primitive in internal/llm.
#   2. go test -race for the report-site tests in internal/llm/retry +
#      internal/llm/output (rejected non-final attempts reported; final /
#      propagated attempts never reported).
#   3. go test -race for internal/governance (drain-and-fold exactness,
#      ceiling crossing on intermediate attempts, the mandatory N>=100
#      shared-chain D-025 concurrent-reuse stress, conformance across the
#      three state drivers).
#   4. go test -race for the phase 145 E2E (test/integration/).
#   5. Doc-closure greps: the "Known accounting gap" comment is GONE from
#      internal/governance/wrap.go; the stale "subscribes against this
#      emit site" claim is GONE from internal/llm/drivers/bifrost/cost.go;
#      the attempt-report call is PRESENT in internal/llm/retry/retry.go
#      and internal/llm/output/downgrade.go.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 145: smoke skeleton — governance attempt-cost accounting not yet implemented (D-275); replace with the assertions listed above when the surface lands"

smoke_summary
