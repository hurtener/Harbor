#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 75a smoke — Wave 13 wave-end suite (Playwright aggregator + Go
# integration test + page-coverage gate).
#
# Phase 75a is a wave-end aggregator that bundles into the final
# Stage-2.3 PR per CLAUDE.md §17.5. This smoke is static-only: it
# verifies the wave-end artefacts exist and are wired (executable
# coverage-check script, Makefile target, Go integration test file).
# The actual Playwright + Go test runs live in the bundling PR's CI
# (frontend + integration jobs), not in the preflight gate.
#
# Per CLAUDE.md §4.2 convention 4 ("404/405/501 → SKIP so phase-N+1
# scripts coexist with phase-N builds"), the file-existence analogue
# here is "missing artefact → SKIP". Until the bundling Stage-2.3 PR
# lands, this script SKIPs cleanly. Once the bundling PR is merged,
# every assertion below flips to OK.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ---------------------------------------------------------------------------
# 1. The page-coverage check script must exist + be executable.
# ---------------------------------------------------------------------------
COVERAGE_SCRIPT="${ROOT}/scripts/console/check-page-coverage.sh"
if [ -f "${COVERAGE_SCRIPT}" ]; then
    if [ -x "${COVERAGE_SCRIPT}" ]; then
        ok "phase 75a: scripts/console/check-page-coverage.sh exists and is executable"
    else
        fail "phase 75a: scripts/console/check-page-coverage.sh exists but is not executable (chmod +x missing)"
    fi
else
    skip "phase 75a: scripts/console/check-page-coverage.sh not yet present (bundles into final Stage-2.3 PR)"
fi

# ---------------------------------------------------------------------------
# 2. The Makefile must declare the wave13-coverage-check target.
# ---------------------------------------------------------------------------
if grep -q '^wave13-coverage-check:' "${ROOT}/Makefile" 2>/dev/null; then
    ok "phase 75a: Makefile declares wave13-coverage-check target"
else
    skip "phase 75a: Makefile does not yet declare wave13-coverage-check (bundles into final Stage-2.3 PR)"
fi

# ---------------------------------------------------------------------------
# 3. The Go-side wave-end integration test file must exist.
# ---------------------------------------------------------------------------
if [ -f "${ROOT}/test/integration/wave13_test.go" ]; then
    ok "phase 75a: test/integration/wave13_test.go exists"
else
    skip "phase 75a: test/integration/wave13_test.go not yet present (bundles into final Stage-2.3 PR)"
fi

# ---------------------------------------------------------------------------
# 4. When the Playwright test directory exists, the coverage gate must pass.
# ---------------------------------------------------------------------------
if [ -d "${ROOT}/web/console/tests" ] && [ -x "${COVERAGE_SCRIPT}" ]; then
    if "${COVERAGE_SCRIPT}" >/tmp/wave13-coverage.log 2>&1; then
        ok "phase 75a: page coverage check passes (every page-spec has a matching *.spec.ts)"
    else
        fail "phase 75a: page coverage check failed (tail:)"
        tail -30 /tmp/wave13-coverage.log >&2 || true
    fi
else
    skip "phase 75a: web/console/tests/ + coverage script not yet present together (bundles into final Stage-2.3 PR)"
fi

# ---------------------------------------------------------------------------
# 5. The wave-end aggregator Playwright spec must exist once Phase 75a lands.
#
# The gate for "Phase 75a has landed" is the 75a-owned coverage script —
# NOT the mere presence of `web/console/tests/`. Phase 75 (the harness
# baseline) legitimately creates `web/console/tests/` FIRST (its config +
# fixtures + meta-test live there); Phase 75a lands later and adds
# `wave13.spec.ts` + the coverage script. Gating on the directory would
# FAIL the moment Phase 75 ships, before 75a does — the §17.6 cross-phase
# fix bundled with the Phase 75 PR (D-115). Gate on the coverage script,
# the unambiguous 75a artefact, so this assertion SKIPs cleanly until 75a
# actually ships.
# ---------------------------------------------------------------------------
if [ -x "${COVERAGE_SCRIPT}" ]; then
    if [ -f "${ROOT}/web/console/tests/wave13.spec.ts" ]; then
        ok "phase 75a: web/console/tests/wave13.spec.ts exists"
    else
        fail "phase 75a: Phase 75a artefacts present but wave13.spec.ts missing"
    fi
else
    skip "phase 75a: wave13.spec.ts not yet present (Phase 75a bundles into final Stage-2.3 PR)"
fi

smoke_summary
