#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 00 — repo skeleton smoke checks.
# Verifies the doc & mirror invariants. No runtime is required.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file 'AGENTS.md' 'AGENTS.md present'
assert_file 'CLAUDE.md' 'CLAUDE.md present'

if [ -f 'AGENTS.md' ] && [ -f 'CLAUDE.md' ]; then
    if diff -q AGENTS.md CLAUDE.md >/dev/null 2>&1; then
        ok 'AGENTS.md and CLAUDE.md are verbatim mirrors'
    else
        fail 'AGENTS.md and CLAUDE.md drift detected'
    fi
fi

assert_file 'README.md' 'README.md present'
assert_file 'LICENSE' 'LICENSE present'
assert_file 'Makefile' 'Makefile present'
assert_file 'go.mod' 'go.mod present'
assert_file '.golangci.yml' '.golangci.yml present'
assert_file '.editorconfig' '.editorconfig present'
assert_file '.gitignore' '.gitignore present'
assert_file '.markdownlint.yaml' '.markdownlint.yaml present'

assert_file '.github/workflows/ci.yml' 'CI workflow present'
assert_file '.github/dependabot.yml' 'dependabot.yml present'
assert_file '.github/CODEOWNERS' 'CODEOWNERS present'
assert_file '.github/PULL_REQUEST_TEMPLATE.md' 'PR template present'

assert_file 'docs/plans/README.md' 'docs/plans/README.md present'
assert_file 'docs/plans/phase-00-skeleton.md' 'docs/plans/phase-00-skeleton.md present'
assert_file 'docs/plans/_template.md' 'docs/plans/_template.md (phase plan template) present'
assert_file 'docs/rfc/README.md' 'docs/rfc/README.md present'
assert_dir_nonempty 'docs/research' 'docs/research/ has briefs'
assert_file 'docs/research/INDEX.md' 'docs/research/INDEX.md (subsystem→briefs index) present'
assert_file 'docs/glossary.md' 'docs/glossary.md present'
assert_file 'docs/decisions.md' 'docs/decisions.md present'

assert_file 'scripts/preflight.sh' 'scripts/preflight.sh present'
assert_file 'scripts/drift-audit.sh' 'scripts/drift-audit.sh present'
assert_file 'scripts/smoke/common.sh' 'scripts/smoke/common.sh present'
assert_file 'scripts/smoke/phase-00.sh' 'scripts/smoke/phase-00.sh present'
assert_file 'scripts/smoke/_template.sh' 'scripts/smoke/_template.sh present'
assert_file 'scripts/install-hooks.sh' 'scripts/install-hooks.sh present'
assert_file 'scripts/hooks/pre-commit' 'scripts/hooks/pre-commit present'

# Forbidden-name scan on top-level docs. The predecessor project must never appear by name
# in committed text. Research briefs are exempt (they cite source paths under
# ~/Repos/<predecessor>/...) and live in docs/research/, not in this list.
files_to_scan=(
    'AGENTS.md'
    'CLAUDE.md'
    'README.md'
    'Makefile'
    'go.mod'
    'docs/plans/README.md'
    'docs/plans/phase-00-skeleton.md'
    'docs/rfc/README.md'
    '.github/PULL_REQUEST_TEMPLATE.md'
)
forbidden=("Penguiflow" "penguiflow")
scan_failures=0
for f in "${files_to_scan[@]}"; do
    [ -f "$f" ] || continue
    for word in "${forbidden[@]}"; do
        if grep -q -- "${word}" "$f" 2>/dev/null; then
            fail "predecessor name '${word}' present in ${f}"
            scan_failures=$((scan_failures + 1))
        fi
    done
done
if [ "${scan_failures}" -eq 0 ]; then
    ok 'forbidden-name scan clean (top-level docs)'
fi

smoke_summary
