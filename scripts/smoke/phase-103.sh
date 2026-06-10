#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 103 smoke — GitHub Pages docs site (Dockyard parity).
#
# Static-only: asserts the VitePress project under docs/site/ exists and
# is wired — the package manifest with its committed lockfile, the site
# config with the live dead-link gate, the Pages workflow, the Makefile
# target, and one mirrored stub per canonical docs surface (skills,
# recipes, reference). The site BUILD itself is gated by
# .github/workflows/docs.yml on every PR; re-running `npm ci` + a full
# VitePress build inside preflight would double CI's work for no extra
# signal.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if [ ! -d docs/site ]; then
    skip "phase 103: docs/site/ not yet present"
    smoke_summary
    exit 0
fi

# 1. The VitePress project exists with a committed lockfile.
assert_file 'docs/site/package.json'          'phase 103: docs/site/package.json present'
assert_file 'docs/site/package-lock.json'     'phase 103: docs/site/package-lock.json committed'
assert_file 'docs/site/.vitepress/config.ts'  'phase 103: VitePress config present'
assert_file 'docs/site/index.md'              'phase 103: site landing page present'

# 2. The dead-link gate is live (the build fails on a dead internal link).
assert_grep_present 'ignoreDeadLinks' docs/site/.vitepress/config.ts \
    'phase 103: dead-link gate configured'
assert_grep_absent  'ignoreDeadLinks: true' docs/site/.vitepress/config.ts \
    'phase 103: dead-link gate not blanket-disabled'

# 3. The Pages workflow builds on PR + push and deploys on main only.
assert_file '.github/workflows/docs.yml' 'phase 103: docs workflow present'
assert_grep_present 'deploy-pages'   .github/workflows/docs.yml 'phase 103: workflow deploys to Pages'
assert_grep_present 'pull_request'   .github/workflows/docs.yml 'phase 103: workflow builds on PR'
assert_grep_present "github.ref == 'refs/heads/main'" .github/workflows/docs.yml \
    'phase 103: deploy gated to main'
assert_grep_present 'id-token: write' .github/workflows/docs.yml \
    'phase 103: permissions-minimal Pages deploy (id-token)'

# 4. The local build equivalent.
assert_grep_present '^docs:' Makefile 'phase 103: make docs target present'

# 5. Every canonical reference surface has its mirrored page.
for page in reference/rfc.md reference/config.md reference/glossary.md \
            reference/decisions.md reference/master-plan.md \
            reference/productionization-playbook.md reference/changelog.md \
            skills/index.md recipes/index.md; do
    assert_file "docs/site/${page}" "phase 103: ${page} mirrored"
done

# 6. A skill or recipe present in the repo but missing from the site is drift.
for d in docs/skills/*/; do
    slug="$(basename "${d}")"
    if [ -f "docs/site/skills/${slug}/SKILL.md" ]; then
        ok "phase 103: docs/skills/${slug} has a site page"
    else
        fail "phase 103: docs/skills/${slug} has NO site page — add docs/site/skills/${slug}/SKILL.md (+ nav in config.ts)"
    fi
done
for r in docs/recipes/*.md; do
    name="$(basename "${r}")"
    [ "${name}" = "README.md" ] && continue
    if [ -f "docs/site/recipes/${name}" ]; then
        ok "phase 103: docs/recipes/${name} has a site page"
    else
        fail "phase 103: docs/recipes/${name} has NO site page — add docs/site/recipes/${name} (+ nav in config.ts)"
    fi
done

# 7. The §18 drift rule extends to the site's navigation manifest.
assert_grep_present 'docs/site' CLAUDE.md 'phase 103: CLAUDE.md §18 covers the docs site'

smoke_summary
