#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 131b smoke — `configure-production-identity` operator skill.
#
# The v1.8.0 Adopter-Path skill that operationalizes the 131a production
# identity setup guide: a Claude-Code-style playbook for getting a
# verifiable JWT into a client and attaching it to `harbor serve`. This is
# a docs-only phase — no runtime surface — so the assertions are static:
# the SKILL.md exists with well-formed frontmatter (surface: protocol), it
# documents both attach on-ramps + the claim shape + the iss/aud
# exact-match contract, it is indexed, and it is mirrored into the docs
# site (the include stub + the nav entry) so `phase-103.sh`'s skill→page
# mapping passes and the VitePress dead-link gate covers it. The VitePress
# build itself (every PR, via .github/workflows/docs.yml) is the binding
# dead-link gate; the frontmatter shape is gated by `make drift-audit`.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

SKILL='docs/skills/configure-production-identity/SKILL.md'

if [ ! -f "${SKILL}" ]; then
    skip "phase 131b: ${SKILL} not yet present"
    smoke_summary
    exit 0
fi

# 1. The skill exists with the protocol-surface frontmatter the §18 audit
#    expects (name matches dir, license, framework, surface).
assert_file "${SKILL}" 'phase 131b: configure-production-identity SKILL.md present'
assert_grep_present '^name: configure-production-identity' "${SKILL}" \
    'phase 131b: frontmatter name matches the skill slug'
assert_grep_present 'license: Apache-2.0' "${SKILL}" \
    'phase 131b: license is Apache-2.0'
assert_grep_present 'framework: harbor' "${SKILL}" \
    'phase 131b: metadata.framework is harbor'
assert_grep_present 'surface: protocol' "${SKILL}" \
    'phase 131b: metadata.surface is protocol'

# 2. It documents BOTH attach on-ramps (the IdP path + the no-IdP
#    `harbor token` self-issuing path).
assert_grep_present 'harbor token' "${SKILL}" \
    'phase 131b: skill documents the no-IdP `harbor token` self-issuing on-ramp'
assert_grep_present 'Real IdP' "${SKILL}" \
    'phase 131b: skill documents on-ramp A (the IdP path)'
assert_grep_present 'self-issuing on-ramp' "${SKILL}" \
    'phase 131b: skill documents on-ramp B (self-issuing)'

# 3. It documents the claim shape + the iss/aud exact-match contract.
assert_grep_present 'tenant' "${SKILL}" 'phase 131b: skill maps the tenant claim'
assert_grep_present 'session' "${SKILL}" 'phase 131b: skill maps the session claim'
assert_grep_present 'exact-match' "${SKILL}" \
    'phase 131b: skill states the iss/aud exact-match contract'

# 4. It operationalizes the 131a guide (forward-points to it) and the wire
#    skill it sits beside.
assert_grep_present 'production-identity-setup' "${SKILL}" \
    'phase 131b: skill points at the production identity setup guide'
assert_grep_present 'use-the-harbor-protocol' "${SKILL}" \
    'phase 131b: skill links the use-the-harbor-protocol wire skill'

# 5. Same-PR site mirror (Phase 103 rule): the include stub + the nav entry
#    so phase-103.sh's skill→page mapping passes and the dead-link gate
#    covers the page.
assert_file 'docs/site/skills/configure-production-identity/SKILL.md' \
    'phase 131b: docs-site include stub present'
assert_grep_present 'configure-production-identity/SKILL.md' \
    docs/site/skills/configure-production-identity/SKILL.md \
    'phase 131b: stub includes the canonical SKILL.md'
assert_grep_present 'configure-production-identity' docs/site/.vitepress/config.ts \
    'phase 131b: skill is nav-mapped in config.ts'

# 6. The skill is registered in the operator-skills index.
assert_grep_present 'configure-production-identity' docs/skills/INDEX.md \
    'phase 131b: skill listed in docs/skills/INDEX.md'

smoke_summary
