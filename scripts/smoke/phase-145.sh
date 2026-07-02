#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 145 smoke — governance attempt-level cost accounting: the in-band
# attempt-cost tap (D-275). docs/plans/phase-145-governance-attempt-accounting.md
#
# Assertions:
#   1. go test -race for the attempt-cost tap primitive in internal/llm.
#   2. go test -race for the report-site tests in internal/llm/retry +
#      internal/llm/output (rejected non-final attempts reported; final /
#      propagated / success attempts never reported).
#   3. go test -race for internal/governance (drain-and-fold exactness,
#      ceiling crossing on intermediate attempts, the mandatory N>=100
#      shared-chain D-025 concurrent-reuse stress, conformance across the
#      state drivers).
#   4. go test -race for the phase 145 E2E (test/integration/).
#   5. Doc-closure greps: the "Known accounting gap" comment is GONE from
#      internal/governance/wrap.go; the stale "subscribes against this
#      emit site" claim is GONE from internal/llm/drivers/bifrost/cost.go;
#      the attempt-report call is PRESENT in internal/llm/retry/retry.go
#      and internal/llm/output/downgrade.go.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - This phase has no Protocol/HTTP surface — the Go test surface is the
#     smoke-observable boundary.
#   - Use helpers from scripts/smoke/common.sh.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# --- 1. Attempt-cost tap primitive (internal/llm) --------------------------
if go test -race -count=1 -timeout 180s ./internal/llm/ -run 'TestAttemptCostTap' >/dev/null 2>&1; then
    ok 'phase 145: attempt-cost tap primitive tests pass under -race (report/peek/one-shot-drain/isolation/concurrent)'
else
    fail 'phase 145: internal/llm attempt-cost tap tests failed (run `go test -race ./internal/llm/ -run TestAttemptCostTap`)'
fi

# --- 2. Report-site tests (retry + downgrade) ------------------------------
if go test -race -count=1 -timeout 180s ./internal/llm/retry/ -run 'TestRetry_' >/dev/null 2>&1; then
    ok 'phase 145: retry report-site tests pass (rejected non-final reported; final/propagated/inner-error never reported)'
else
    fail 'phase 145: internal/llm/retry report-site tests failed (run `go test -race ./internal/llm/retry/ -run TestRetry_`)'
fi
if go test -race -count=1 -timeout 180s ./internal/llm/output/ -run 'TestDowngrade_' >/dev/null 2>&1; then
    ok 'phase 145: downgrade report-site tests pass (every errored attempt reported; success propagates unreported)'
else
    fail 'phase 145: internal/llm/output report-site tests failed (run `go test -race ./internal/llm/output/ -run TestDowngrade_`)'
fi

# --- 3. Governance fold + ceiling + D-025 + conformance --------------------
if go test -race -count=1 -timeout 300s ./internal/governance/... >/dev/null 2>&1; then
    ok 'phase 145: internal/governance tests pass under -race (fold exactness, ceiling, N>=100 D-025 shared-chain, conformance)'
else
    fail 'phase 145: internal/governance tests failed (run `go test -race ./internal/governance/...`)'
fi
# State-driver governance conformance (SQLite/Postgres) exercises the fold
# addition across drivers. Postgres self-skips without a DB; that is fine.
if go test -race -count=1 -timeout 300s ./internal/state/drivers/sqlite/ -run 'Governance' >/dev/null 2>&1; then
    ok 'phase 145: SQLite governance conformance (incl. attempt-tap fold) passes'
else
    fail 'phase 145: SQLite governance conformance failed (run `go test -race ./internal/state/drivers/sqlite/ -run Governance`)'
fi

# --- 4. Integration E2E ----------------------------------------------------
if go test -race -count=1 -timeout 300s ./test/integration/ -run 'TestE2E_Phase145' >/dev/null 2>&1; then
    ok 'phase 145: attempt-accounting E2E passes (happy retry + identity propagation, exhaustion accounts all, state-fail surfaces loud)'
else
    fail 'phase 145: phase145 integration test failed (run `go test -race ./test/integration/ -run TestE2E_Phase145`)'
fi

# --- 5. Doc-closure greps --------------------------------------------------
assert_grep_absent 'Known accounting gap' \
    internal/governance/wrap.go \
    'phase 145: the wrap.go "Known accounting gap" comment is replaced by the tap contract'
assert_grep_absent 'subscribes against this emit site' \
    internal/llm/drivers/bifrost/cost.go \
    'phase 145: the stale bifrost "subscribes against this emit site" claim is corrected'
assert_grep_present 'ReportAttemptCost' \
    internal/llm/retry/retry.go \
    'phase 145: the retry wrapper reports consumed non-final attempts into the tap'
assert_grep_present 'ReportAttemptCost' \
    internal/llm/output/downgrade.go \
    'phase 145: the downgrade wrapper reports every errored attempt into the tap'

# No Protocol/HTTP surface at this phase — the Go test surface above is the
# smoke-observable boundary.
skip 'phase 145: no Protocol/HTTP surface — governance accounting is in-process'

smoke_summary
