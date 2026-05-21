#!/usr/bin/env bash
# scripts/console/check-page-coverage.sh — the Console page-coverage gate
# (Phase 75a / D-131).
#
# The binding rule (operator §12 lock-in #7, recorded in D-131): every
# Console page-spec under `docs/design/console/page-<slug>.md` MUST have
# a matching Playwright spec under `web/console/tests/<slug>-page.spec.ts`.
# This script is that rule expressed as a mechanical gate — it is invoked
# by `make wave13-coverage-check`, by `scripts/smoke/phase-75a.sh`, and by
# the `frontend-e2e` CI job.
#
# Exit 0  — every non-Evaluations page-spec has a matching *-page.spec.ts.
# Exit 1  — at least one page-spec has no matching spec ("missing: <slug>").
#
# The Evaluations page is EXCLUDED — it is a post-V1 subsystem (D-064);
# the carve-out lives in the single grep-visible `EVALUATIONS_EXCLUDED_
# PER_D_064` variable below so the absence of an Evaluations spec never
# false-flags this gate.
#
# When `web/console/tests/` is absent (a pre-Console-scaffold checkout)
# the script exits 0 with a SKIP note — the directory-missing → SKIP
# analogue of the §4.2 "404/405/501 → SKIP" smoke convention.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# The Evaluations page is post-V1 (D-064) — excluded from the coverage
# requirement. Grep-visible so the carve-out is unmistakable.
EVALUATIONS_EXCLUDED_PER_D_064=1

PAGE_DOC_DIR="docs/design/console"
SPEC_DIR="web/console/tests"

if [ ! -d "${SPEC_DIR}" ]; then
    echo "page-coverage: ${SPEC_DIR} absent — SKIP (Console scaffold pending)"
    exit 0
fi

missing=0
checked=0

for doc in "${PAGE_DOC_DIR}"/page-*.md; do
    [ -e "${doc}" ] || continue
    slug="$(basename "${doc}" .md)"
    slug="${slug#page-}"

    # Evaluations carve-out (D-064).
    if [ "${slug}" = "evaluations" ] && [ "${EVALUATIONS_EXCLUDED_PER_D_064}" = "1" ]; then
        echo "page-coverage: skipping 'evaluations' (post-V1, D-064)"
        continue
    fi

    spec="${SPEC_DIR}/${slug}-page.spec.ts"
    checked=$((checked + 1))
    if [ -f "${spec}" ]; then
        echo "page-coverage: OK   ${slug} -> ${spec}"
    else
        echo "page-coverage: MISSING ${slug} -> expected ${spec}"
        missing=$((missing + 1))
    fi
done

# The wave-end aggregator spec must also exist.
if [ -f "${SPEC_DIR}/wave13.spec.ts" ]; then
    echo "page-coverage: OK   wave-end aggregator -> ${SPEC_DIR}/wave13.spec.ts"
else
    echo "page-coverage: MISSING wave-end aggregator -> expected ${SPEC_DIR}/wave13.spec.ts"
    missing=$((missing + 1))
fi

echo "page-coverage: checked ${checked} page-spec(s); ${missing} missing"
if [ "${missing}" -gt 0 ]; then
    echo "page-coverage: FAIL — every docs/design/console/page-<slug>.md needs a web/console/tests/<slug>-page.spec.ts"
    exit 1
fi
echo "page-coverage: PASS — every V1 Console page has a matching Playwright spec"
exit 0
