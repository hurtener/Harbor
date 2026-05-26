#!/usr/bin/env bash
# scripts/skills/check-frontmatter.sh — verify every docs/skills/<slug>/SKILL.md
# has a well-formed Dockyard-style frontmatter.
#
# Invoked by scripts/drift-audit.sh as part of `make drift-audit`. Each skill
# MUST carry exactly this shape:
#
#   ---
#   name: <slug-kebab-case>           # MUST match the parent directory name
#   description: <one sentence>       # MUST contain "Use when" (the framing)
#   license: Apache-2.0               # MUST be exact
#   metadata:
#     framework: harbor               # MUST be exact
#     surface: <one of the canonical set below>
#     verbs: "<harbor cli verbs>"     # may be empty
#   ---
#
# Canonical `surface` values (extend in tandem with §18 of CLAUDE.md):
#   cli / agent-yaml / tools / mcp / llm / memory / playground / console / tasks / protocol
#
# Exit 0 when every skill checks out. Exit 1 on the first violation; the
# violations are printed before the script exits so a contributor sees the
# whole batch.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

SKILLS_DIR="docs/skills"

# A skills directory that doesn't exist yet is fine — phase 85k is what
# creates it. The audit is a no-op until then.
if [ ! -d "${SKILLS_DIR}" ]; then
    echo "[OK]   skills frontmatter: docs/skills/ absent (phase 85k not yet shipped)"
    exit 0
fi

VALID_SURFACES="cli agent-yaml tools mcp llm memory playground console tasks protocol"

declare -i fail_count=0

# require_key <file> <key>
# Greps for `<key>:` at the start of a line inside the frontmatter (first
# 20 lines). Returns 0 if present, 1 otherwise.
require_key() {
    local file="$1" key="$2"
    if ! head -n 20 "${file}" | grep -qE "^${key}:|^[[:space:]]+${key}:"; then
        echo "[FAIL] ${file}: missing required frontmatter key '${key}:'"
        fail_count+=1
        return 1
    fi
    return 0
}

# extract_key <file> <key>
# Returns the value of <key> from the frontmatter (no quoting tricks; the
# value is the everything-after-the-colon, trimmed).
extract_key() {
    local file="$1" key="$2"
    head -n 20 "${file}" \
        | grep -E "^${key}:|^[[:space:]]+${key}:" \
        | head -1 \
        | sed -E "s/^[[:space:]]*${key}:[[:space:]]*//" \
        | sed -E 's/[[:space:]]+$//' \
        | sed -E 's/^"(.*)"$/\1/'
}

# Walk every SKILL.md under docs/skills/*/.
shopt -s nullglob
SKILL_FILES=("${SKILLS_DIR}"/*/SKILL.md)
shopt -u nullglob

if [ ${#SKILL_FILES[@]} -eq 0 ]; then
    echo "[OK]   skills frontmatter: docs/skills/ exists but no SKILL.md files yet"
    exit 0
fi

for skill in "${SKILL_FILES[@]}"; do
    slug="$(basename "$(dirname "${skill}")")"

    # name matches directory slug.
    require_key "${skill}" 'name' || continue
    name_value=$(extract_key "${skill}" 'name')
    if [ "${name_value}" != "${slug}" ]; then
        echo "[FAIL] ${skill}: frontmatter name='${name_value}' does not match directory '${slug}'"
        fail_count+=1
        continue
    fi

    # description present + carries "Use when" framing.
    require_key "${skill}" 'description' || continue
    description_value=$(extract_key "${skill}" 'description')
    if ! grep -qiE 'use when' <<<"${description_value}"; then
        echo "[FAIL] ${skill}: description must contain 'Use when' framing; got: ${description_value:0:80}..."
        fail_count+=1
        continue
    fi

    # license: Apache-2.0
    require_key "${skill}" 'license' || continue
    license_value=$(extract_key "${skill}" 'license')
    if [ "${license_value}" != "Apache-2.0" ]; then
        echo "[FAIL] ${skill}: license='${license_value}' (must be 'Apache-2.0')"
        fail_count+=1
        continue
    fi

    # metadata.framework: harbor
    framework_value=$(extract_key "${skill}" 'framework')
    if [ "${framework_value}" != "harbor" ]; then
        echo "[FAIL] ${skill}: metadata.framework='${framework_value}' (must be 'harbor')"
        fail_count+=1
        continue
    fi

    # metadata.surface in the canonical set.
    surface_value=$(extract_key "${skill}" 'surface')
    if [ -z "${surface_value}" ]; then
        echo "[FAIL] ${skill}: metadata.surface missing"
        fail_count+=1
        continue
    fi
    surface_ok=0
    for valid in ${VALID_SURFACES}; do
        if [ "${surface_value}" = "${valid}" ]; then
            surface_ok=1
            break
        fi
    done
    if [ ${surface_ok} -eq 0 ]; then
        echo "[FAIL] ${skill}: metadata.surface='${surface_value}' is not one of: ${VALID_SURFACES}"
        fail_count+=1
        continue
    fi

    # metadata.verbs key is required (value may be empty string).
    if ! head -n 20 "${skill}" | grep -qE '^[[:space:]]+verbs:'; then
        echo "[FAIL] ${skill}: metadata.verbs key missing (use empty string when no CLI verb applies)"
        fail_count+=1
        continue
    fi

    echo "[OK]   ${skill}: frontmatter well-formed (surface=${surface_value})"
done

if [ ${fail_count} -gt 0 ]; then
    echo ""
    echo "skills frontmatter audit: ${fail_count} violation(s) — see §18 of CLAUDE.md"
    exit 1
fi

echo "[OK]   skills frontmatter audit: ${#SKILL_FILES[@]} skill(s) all well-formed"
