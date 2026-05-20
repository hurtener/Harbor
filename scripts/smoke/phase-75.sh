#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 75 smoke — Console e2e Playwright harness baseline.
#
# The harness is build-time + test-time infrastructure: no HTTP / Protocol
# surface on the runtime side. This smoke asserts the harness files exist,
# the Playwright config targets `harbor console` (NOT `harbor dev` — D-091),
# the npm scripts are declared, the `frontend-e2e` CI job is wired, and no
# spec hand-rolls a `fetch(` call (CLAUDE.md §4.5 #11 + §13).
#
# The whole script degrades gracefully (SKIP, not FAIL) when the Phase 75
# harness itself is absent. The keyed surface is `playwright.config.ts` —
# Phase 72h introduces `web/console/` with the SvelteKit scaffold + Console
# DB module BEFORE Phase 75 lands the Playwright harness; the file-absence
# → SKIP convention (§4.2) must therefore key on the harness file, not the
# `web/console/` directory (which 72h creates first).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if [ ! -f "${ROOT}/web/console/playwright.config.ts" ]; then
  skip "phase 75: Playwright harness not yet present (web/console/playwright.config.ts absent — Phase 75 pending)"
  smoke_summary
  exit 0
fi

# 1. Required harness files exist.
assert_file "${ROOT}/web/console/playwright.config.ts" \
  "phase 75: playwright.config.ts present"
assert_file "${ROOT}/web/console/tests/README.md" \
  "phase 75: tests/README.md authoring guide present"
assert_file "${ROOT}/web/console/tests/harness.spec.ts" \
  "phase 75: tests/harness.spec.ts meta-test present"
assert_file "${ROOT}/web/console/tests/fixtures/page.ts" \
  "phase 75: tests/fixtures/page.ts present"
assert_file "${ROOT}/web/console/tests/fixtures/harbor-runtime.ts" \
  "phase 75: tests/fixtures/harbor-runtime.ts present"
assert_file "${ROOT}/web/console/tests/pages/base-page.ts" \
  "phase 75: tests/pages/base-page.ts present"
assert_file "${ROOT}/web/console/tests/helpers/protocol.ts" \
  "phase 75: tests/helpers/protocol.ts present"
assert_file "${ROOT}/web/console/tests/helpers/identity.ts" \
  "phase 75: tests/helpers/identity.ts present"

# 2. playwright.config.ts targets `harbor console`, NOT `harbor dev` (D-091).
PWCONFIG="${ROOT}/web/console/playwright.config.ts"
if [ -f "${PWCONFIG}" ]; then
  assert_grep_absent "harbor dev" "${PWCONFIG}" \
    "phase 75: playwright.config.ts must NOT target 'harbor dev' (D-091)"
  if grep -q "harbor console" "${PWCONFIG}" 2>/dev/null; then
    ok "phase 75: playwright.config.ts targets 'harbor console' (${PWCONFIG})"
  else
    fail "phase 75: playwright.config.ts must reference 'harbor console' (${PWCONFIG})"
  fi
fi

# 3. package.json declares the three e2e scripts.
PKG="${ROOT}/web/console/package.json"
if [ -f "${PKG}" ]; then
  if command -v jq >/dev/null 2>&1; then
    for script in test:e2e test:e2e:install test:e2e:ui; do
      if [ "$(jq -r --arg s "${script}" '.scripts[$s] // empty' "${PKG}")" != "" ]; then
        ok "phase 75: package.json declares '${script}' script"
      else
        fail "phase 75: package.json missing '${script}' script (${PKG})"
      fi
    done
    if [ "$(jq -r '.devDependencies["@playwright/test"] // empty' "${PKG}")" != "" ]; then
      ok "phase 75: package.json pins '@playwright/test' in devDependencies"
    else
      fail "phase 75: package.json missing '@playwright/test' devDependency (${PKG})"
    fi
  else
    skip "phase 75: jq unavailable — package.json scripts assertions skipped"
  fi
else
  fail "phase 75: web/console/package.json missing (${PKG})"
fi

# 4. CI workflow declares the `frontend-e2e` job.
CI="${ROOT}/.github/workflows/ci.yml"
if [ -f "${CI}" ]; then
  if grep -qE "^  frontend-e2e:" "${CI}" 2>/dev/null; then
    ok "phase 75: ci.yml declares 'frontend-e2e' job"
  else
    fail "phase 75: ci.yml missing 'frontend-e2e' job declaration (${CI})"
  fi
else
  fail "phase 75: .github/workflows/ci.yml missing (${CI})"
fi

# 5. No spec hand-rolls a `fetch(` call — go through the typed Protocol
#    client (CLAUDE.md §4.5 #11 + §13 forbidden practice).
TESTS_DIR="${ROOT}/web/console/tests"
if [ -d "${TESTS_DIR}" ]; then
  hits=$(grep -rEln '\bfetch\(' "${TESTS_DIR}" 2>/dev/null || true)
  if [ -z "${hits}" ]; then
    ok "phase 75: no hand-rolled fetch() in web/console/tests/ specs"
  else
    fail "phase 75: hand-rolled fetch() found in: ${hits} (use typed protocol client per CLAUDE.md §4.5 #11)"
  fi
fi

smoke_summary
