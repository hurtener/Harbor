#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 250 smoke — same-runtime organization skill publications (HA-68).
# This checkpoint has the domain, persistence, and canonical wire contract;
# the transport/runtime-composition consumer remains pending. These guards
# pin the evidence without implying a live Protocol route or broad preflight.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file \
    "docs/plans/phase-250-same-runtime-skill-publications.md" \
    "phase 250 plan exists"
assert_file \
    "internal/skills/publication/publication.go" \
    "phase 250 publication domain exists"
assert_file \
    "internal/protocol/types/skill_publications.go" \
    "phase 250 canonical wire types exist"

assert_grep_present \
    'type Store interface' \
    "internal/skills/publication/publication.go" \
    "phase 250 publication store contract exists"
assert_grep_present \
    'TestMemoryStore_PublicationLifecycleAndContentFreeProjections' \
    "internal/skills/publication/publication_test.go" \
    "phase 250 lifecycle/content-free test exists"
assert_grep_present \
    'TestMemoryStore_RuntimeAndTenantIsolationFailClosed' \
    "internal/skills/publication/publication_test.go" \
    "phase 250 identity/runtime isolation test exists"
assert_grep_present \
    'TestMemoryStore_ConcurrentResolveN128' \
    "internal/skills/publication/publication_test.go" \
    "phase 250 concurrent resolve test exists"
assert_grep_present \
    'TestStateStoreStore_RestartAndCAS' \
    "internal/skills/publication/publication_test.go" \
    "phase 250 StateStore CAS/restart test exists"
assert_grep_present \
    'TestStateStoreStore_ResponseLossAndUserScopedReferences' \
    "internal/skills/publication/publication_test.go" \
    "phase 250 response-loss/reference test exists"

assert_grep_present \
    'skills.publications.publish' \
    "internal/protocol/methods/methods.go" \
    "phase 250 admin publish method is registered"
assert_grep_present \
    'skills.publications.references.list' \
    "internal/protocol/methods/methods.go" \
    "phase 250 caller reference method is registered"
assert_grep_present \
    'skill_publication_runtime_mismatch' \
    "internal/protocol/errors/errors.go" \
    "phase 250 runtime mismatch error is registered"
if grep -a -qE 'SkillPublication' "docs/site/protocol/types.md"; then
    ok "phase 250 generated types reference is present"
else
    skip "phase 250 generated types reference pending generator guidance rows"
fi
if grep -a -qE 'skills\.publications\.publish' "docs/site/protocol/methods.md"; then
    ok "phase 250 generated methods reference is present"
else
    skip "phase 250 generated methods reference pending generator guidance rows"
fi
if grep -a -qE 'skill_publication_runtime_mismatch' "docs/site/protocol/errors.md"; then
    ok "phase 250 generated errors reference is present"
else
    skip "phase 250 generated errors reference pending generator guidance rows"
fi

assert_grep_present \
    'Organization skill publication' \
    "docs/skills/configure-memory-and-skills/SKILL.md" \
    "phase 250 operator skill documents publication lifecycle"
assert_grep_present \
    'same-runtime' \
    "docs/skills/use-the-harbor-protocol/SKILL.md" \
    "phase 250 Protocol skill documents runtime binding"
assert_grep_present \
    'organization skill publications' \
    "examples/harbor.yaml" \
    "phase 250 example config records publication posture"
assert_grep_present \
    'D-430' \
    "CHANGELOG.md" \
    "phase 250 changelog entry exists"
assert_grep_present \
    'HA-68 publication note' \
    "docs/site/protocol/index.md" \
    "phase 250 protocol-site mirror records pending generated pages"

# The integrated base intentionally does not yet contain these consumers.
assert_grep_present \
    'transport and runtime composition consumers are not established' \
    "docs/plans/phase-250-same-runtime-skill-publications.md" \
    "phase 250 records the pending consumer explicitly"

smoke_summary
