#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 131a smoke — production identity setup guide (D-263).
#
# The v1.8.0 Adopter-Path P0: ship the operator manual for getting a
# verifiable JWT into a client and attaching it to `harbor serve`, and
# wire it into the published docs site. This is a docs-only phase —
# `harbor serve`'s verifier is untouched (D-220 preserved) — so the
# assertions are static: the page exists, it is nav-mapped (the VitePress
# dead-link gate only protects pages the nav references), it documents
# both attach on-ramps and the claim shape, and the existing auth /
# build-a-client pages plus the use-the-harbor-protocol skill forward-point
# to it (the §18 same-PR skill-drift rule). The VitePress build itself
# (every PR, via .github/workflows/docs.yml) is the dead-link gate.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

GUIDE='docs/site/protocol/production-identity-setup.md'

if [ ! -f "${GUIDE}" ]; then
    skip "phase 131a: ${GUIDE} not yet present"
    smoke_summary
    exit 0
fi

# 1. The guide exists and is registered in the VitePress nav (so it is
#    reachable AND the dead-link gate covers links into it).
assert_file "${GUIDE}" 'phase 131a: production-identity setup guide present'
assert_grep_present 'production-identity-setup' docs/site/.vitepress/config.ts \
    'phase 131a: guide is nav-mapped in config.ts'

# 2. It documents BOTH attach on-ramps (the IdP path + the no-IdP
#    `harbor token` self-issuing path) — the §3 resolution.
assert_grep_present 'harbor token' "${GUIDE}" \
    'phase 131a: guide documents the no-IdP `harbor token` self-issuing on-ramp'
assert_grep_present 'On-ramp A' "${GUIDE}" \
    'phase 131a: guide documents on-ramp A (the IdP path)'
assert_grep_present 'On-ramp B' "${GUIDE}" \
    'phase 131a: guide documents on-ramp B (self-issuing)'

# 3. It documents the claim shape serve verifies + the iss/aud contract.
assert_grep_present 'tenant' "${GUIDE}" 'phase 131a: guide maps the tenant claim'
assert_grep_present 'session' "${GUIDE}" 'phase 131a: guide maps the session claim'
assert_grep_present 'exact-match' "${GUIDE}" \
    'phase 131a: guide states the iss/aud exact-match contract'

# 4. It carries the four IdP snippets.
for idp in Auth0 Okta Keycloak Cognito; do
    assert_grep_present "${idp}" "${GUIDE}" \
        "phase 131a: guide carries the ${idp} snippet"
done

# 5. Cross-links land in the same PR: the auth + build-a-client pages and
#    the use-the-harbor-protocol skill forward-point to the new guide.
assert_grep_present 'production-identity-setup' docs/site/protocol/auth-and-identity.md \
    'phase 131a: auth-and-identity links the guide'
assert_grep_present 'production-identity-setup' docs/site/protocol/build-a-client.md \
    'phase 131a: build-a-client links the guide'
assert_grep_present 'production-identity-setup' \
    docs/skills/use-the-harbor-protocol/SKILL.md \
    'phase 131a: use-the-harbor-protocol skill forward-points to the guide (§18)'

# 6. Honesty: serve's verifier is untouched — the guide says serve mints
#    nothing and the self-issued path is a single-issuer grade.
assert_grep_present 'mints no token' "${GUIDE}" \
    'phase 131a: guide affirms serve mints no token (D-220 preserved)'

smoke_summary
