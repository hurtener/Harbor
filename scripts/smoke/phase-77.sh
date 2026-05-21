#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 77 smoke — goroutine-leak conformance harness.
#
# Phase 77 ships a test harness, not a Protocol surface: there is no
# REST endpoint, Protocol method, or CLI subcommand to hit against a
# live server. The smoke is therefore static-only — it verifies the
# harness artefacts exist and are wired, then SKIPs the live-server
# portion (nothing to call).
#
# The harness itself runs under `-race` in the dedicated `leak-harness`
# CI job (and in `make test` / the `go` CI job); this smoke only
# asserts those artefacts are present so the preflight gate flags a
# missing or unwired harness.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ---------------------------------------------------------------------------
# 1. The conformance harness file must exist.
# ---------------------------------------------------------------------------
HARNESS="${ROOT}/test/integration/phase77_goroutine_leak_test.go"
if [ -f "${HARNESS}" ]; then
    ok "phase 77: test/integration/phase77_goroutine_leak_test.go exists"
else
    fail "phase 77: goroutine-leak conformance harness file missing"
fi

# ---------------------------------------------------------------------------
# 2. The harness must declare the table-driven conformance test.
# ---------------------------------------------------------------------------
if [ -f "${HARNESS}" ] && grep -q 'func TestE2E_Phase77_GoroutineLeakConformance' "${HARNESS}"; then
    ok "phase 77: harness declares TestE2E_Phase77_GoroutineLeakConformance"
else
    fail "phase 77: harness does not declare TestE2E_Phase77_GoroutineLeakConformance"
fi

# ---------------------------------------------------------------------------
# 3. The harness must be table-driven (a leakCases slice — one row per
#    long-lived component, so a future component is one new row).
# ---------------------------------------------------------------------------
if [ -f "${HARNESS}" ] && grep -q 'leakCases = \[\]leakCase{' "${HARNESS}"; then
    ok "phase 77: harness is table-driven (leakCases table present)"
else
    fail "phase 77: harness is not table-driven (no leakCases table)"
fi

# ---------------------------------------------------------------------------
# 4. CI must run the harness on every PR — the leak-harness job must be
#    declared in .github/workflows/ci.yml.
# ---------------------------------------------------------------------------
if grep -q 'leak-harness:' "${ROOT}/.github/workflows/ci.yml" 2>/dev/null; then
    ok "phase 77: .github/workflows/ci.yml declares the leak-harness job"
else
    fail "phase 77: .github/workflows/ci.yml does not declare the leak-harness job"
fi

# ---------------------------------------------------------------------------
# Phase 77 adds no live-server surface — nothing to hit on the booted
# dev server. The §4.2 "404/405/501 → SKIP" analogue here is "no
# endpoint → SKIP the live portion".
# ---------------------------------------------------------------------------
skip "phase 77: no Protocol/REST/CLI surface — live-server checks N/A (harness runs under -race in CI)"

smoke_summary
