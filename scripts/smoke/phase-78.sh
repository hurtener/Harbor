#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 78 smoke — chaos / fault-injection harness.
#
# Phase 78 ships a test harness, not a Protocol surface: there is no
# REST endpoint, Protocol method, or CLI subcommand to hit against a
# live server. The smoke is therefore static-only — it verifies the
# harness artefacts exist and are wired, then SKIPs the live-server
# portion (nothing to call).
#
# The harness itself runs under `-race` in the dedicated `chaos` CI
# job (and in `make test` / the `go` CI job); this smoke only asserts
# those artefacts are present so the preflight gate flags a missing or
# unwired harness.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ---------------------------------------------------------------------------
# 1. The chaos harness file must exist.
# ---------------------------------------------------------------------------
HARNESS="${ROOT}/test/integration/phase78_chaos_fault_injection_test.go"
if [ -f "${HARNESS}" ]; then
    ok "phase 78: test/integration/phase78_chaos_fault_injection_test.go exists"
else
    fail "phase 78: chaos / fault-injection harness file missing"
fi

# ---------------------------------------------------------------------------
# 2. The fault-injecting decorators file must exist — the decorators
#    wrap real drivers (D-137), they are not registered stubs.
# ---------------------------------------------------------------------------
FAULTS="${ROOT}/test/integration/phase78_faults_test.go"
if [ -f "${FAULTS}" ]; then
    ok "phase 78: test/integration/phase78_faults_test.go (fault-injecting decorators) exists"
else
    fail "phase 78: fault-injecting decorators file missing"
fi

# ---------------------------------------------------------------------------
# 3. The harness must declare the table-driven chaos test.
# ---------------------------------------------------------------------------
if [ -f "${HARNESS}" ] && grep -q 'func TestE2E_Phase78_ChaosFaultInjection' "${HARNESS}"; then
    ok "phase 78: harness declares TestE2E_Phase78_ChaosFaultInjection"
else
    fail "phase 78: harness does not declare TestE2E_Phase78_ChaosFaultInjection"
fi

# ---------------------------------------------------------------------------
# 4. The harness must be table-driven (a chaosCases slice — one row per
#    failure mode, so a future failure mode is one new row).
# ---------------------------------------------------------------------------
if [ -f "${HARNESS}" ] && grep -q 'chaosCases = \[\]chaosCase{' "${HARNESS}"; then
    ok "phase 78: harness is table-driven (chaosCases table present)"
else
    fail "phase 78: harness is not table-driven (no chaosCases table)"
fi

# ---------------------------------------------------------------------------
# 5. CI must run the harness on every PR — the chaos job must be
#    declared in .github/workflows/ci.yml.
# ---------------------------------------------------------------------------
if grep -q 'chaos:' "${ROOT}/.github/workflows/ci.yml" 2>/dev/null; then
    ok "phase 78: .github/workflows/ci.yml declares the chaos job"
else
    fail "phase 78: .github/workflows/ci.yml does not declare the chaos job"
fi

# ---------------------------------------------------------------------------
# Phase 78 adds no live-server surface — nothing to hit on the booted
# dev server. The §4.2 "404/405/501 -> SKIP" analogue here is "no
# endpoint -> SKIP the live portion".
# ---------------------------------------------------------------------------
skip "phase 78: no Protocol/REST/CLI surface — live-server checks N/A (harness runs under -race in CI)"

smoke_summary
