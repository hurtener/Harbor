#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 85k smoke — Harbor operator skills (V1.1.5).
#
# Static-only: asserts every required docs/skills/<slug>/SKILL.md exists,
# the INDEX is present, the frontmatter helper is executable and passes,
# and the §18 drift rule is present in CLAUDE.md (mirror invariant covers
# AGENTS.md separately).
#
# The eleven slugs below are the V1.1.5 cut. `attach-an-mcp-server` is
# deferred to V1.2 once Phase 85a's MCP wire shape stabilises — adding
# it here later is the same PR that ships its surface (§18).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

REQUIRED_SLUGS=(
    scaffold-a-harbor-agent
    define-the-agent-yaml
    add-an-in-process-tool
    wire-the-llm-provider
    configure-memory-and-skills
    run-the-dev-loop
    drive-the-playground
    observe-with-the-console
    validate-and-package
    use-the-harbor-protocol
)

# 1. Every required slug has a SKILL.md.
for slug in "${REQUIRED_SLUGS[@]}"; do
    path="docs/skills/${slug}/SKILL.md"
    if [ -f "${path}" ]; then
        ok "${path} exists"
    else
        fail "${path} missing — phase 85k acceptance criterion violated"
    fi
done

# 2. The INDEX links every slug.
if [ -f docs/skills/INDEX.md ]; then
    ok 'docs/skills/INDEX.md present'
    for slug in "${REQUIRED_SLUGS[@]}"; do
        if grep -q "${slug}/SKILL.md" docs/skills/INDEX.md; then
            ok "INDEX.md references ${slug}"
        else
            fail "INDEX.md does not reference ${slug}"
        fi
    done
else
    fail 'docs/skills/INDEX.md missing'
fi

# 3. The frontmatter helper is executable and passes.
if [ -x scripts/skills/check-frontmatter.sh ]; then
    ok 'scripts/skills/check-frontmatter.sh is executable'
    if scripts/skills/check-frontmatter.sh >/dev/null 2>&1; then
        ok 'frontmatter audit clean across all skills'
    else
        fail 'frontmatter audit failed — re-run scripts/skills/check-frontmatter.sh for details'
    fi
else
    fail 'scripts/skills/check-frontmatter.sh missing or not executable'
fi

# 4. §18 drift rule is present in CLAUDE.md.
if grep -qE '^## 18\. Operator-skill hygiene' CLAUDE.md; then
    ok 'CLAUDE.md §18 operator-skill hygiene rule present'
else
    fail 'CLAUDE.md §18 operator-skill hygiene rule missing — restored by V1.1.5'
fi

# 5. Glossary distinguishes operator-skill vs runtime-skill.
if grep -q '\*\*skill (operator)\*\*' docs/glossary.md && \
   grep -q '\*\*skill (runtime)\*\*' docs/glossary.md; then
    ok 'glossary distinguishes skill (operator) vs skill (runtime)'
else
    fail 'glossary missing the operator-vs-runtime skill clarification'
fi

# 6. README.md points at the operator skills INDEX.
if grep -q 'docs/skills/INDEX.md' README.md; then
    ok 'README.md references docs/skills/INDEX.md'
else
    fail 'README.md does not reference docs/skills/INDEX.md'
fi

smoke_summary
