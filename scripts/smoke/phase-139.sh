#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 139 — Public-site honesty sweep. Docs-only: this phase adds NO new
# runtime surface, so there is nothing to hit on the dev server. The gate is
# a set of static honesty greps over the marketing landing surface and the
# hot-reload test godoc — exactly the "VitePress build + greps" gate the wave
# coordination doc (docs/plans/wave-v18-coordination.md §4, phase 139) names.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

LANDING="docs/site/.vitepress/theme/landingSpec.ts"
HOTRELOAD_TEST="cmd/harbor/cmd_dev_hot_reload_test.go"

# Landing surface reflects the current canonical method count (110), not 109.
assert_grep_present 'canonical Protocol methods' "${LANDING}" \
    "landing: canonical-methods stat present"
assert_grep_present '"110", label: "canonical Protocol methods"' "${LANDING}" \
    "landing: canonical-methods stat reads 110"
assert_grep_present '110 canonical methods' "${LANDING}" \
    "landing: Protocol section reads 110 canonical methods"

# The stale '109' / 'at v1.6' qualifier is gone from the methods claims.
assert_grep_absent '109 canonical methods' "${LANDING}" \
    "landing: no stale '109 canonical methods' claim"
assert_grep_absent 'canonical methods at v1.6' "${LANDING}" \
    "landing: no 'at v1.6' qualifier on the methods claim"

# Cosmetic/unprinted dev-banner artifact removed.
assert_grep_absent '3 drivers registered' "${LANDING}" \
    "landing: unprinted '3 drivers registered' banner removed"

# Hot-reload claim qualified to config/YAML reload (the genuine capability).
assert_grep_present 'config reload on your laptop' "${LANDING}" \
    "landing: hot-reload claim qualified to config reload"

# The stale reference to the non-existent integration test file is gone.
assert_grep_absent 'phase65_hot_reload_test.go' "${HOTRELOAD_TEST}" \
    "hot-reload test: stale phase65_hot_reload_test.go reference removed"

smoke_summary
